package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

type call struct {
	Method string
	Body   Obj
}
type fixture struct {
	b       *Bot
	calls   []call
	mu      sync.Mutex
	handler func(string, Obj) (any, int)
	seq     int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{seq: 100}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Obj
		_ = json.NewDecoder(r.Body).Decode(&body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls = append(f.calls, call{method, body})
		handler := f.handler
		n := len(f.calls)
		f.mu.Unlock()
		var result any
		status := 200
		if handler != nil {
			result, status = handler(method, body)
		}
		if result == nil {
			switch method {
			case "getMe":
				result = Obj{"id": 900, "username": "secretary_bot", "has_topics_enabled": true, "can_connect_to_business": true}
			case "getBusinessConnection":
				result = connection()
			case "createForumTopic":
				result = Obj{"message_thread_id": 77}
			default:
				result = Obj{"message_id": 1000 + n, "date": time.Now().Unix(), "chat": Obj{"id": 200, "type": "private"}, "from": Obj{"id": 100}, "text": body["text"]}
			}
		}
		w.WriteHeader(status)
		if status == 200 {
			_ = json.NewEncoder(w).Encode(Obj{"ok": true, "result": result})
		} else {
			_ = json.NewEncoder(w).Encode(result)
		}
	}))
	dir := t.TempDir()
	f.b = &Bot{Store: newStore(dir), Owner: 100, Dir: dir, Username: "secretary_bot", API: newAPI("1:test", "https://model.example/v1", "model", "key"), busy: map[string]bool{}}
	f.b.API.TelegramURL = server.URL
	t.Cleanup(func() { server.Close(); f.b.Store.db.Close() })
	return f
}
func connection() Connection {
	c := Connection{ID: "conn", User: User{ID: 100}, Date: 1, Enabled: true}
	c.Rights.Reply = true
	return c
}
func message(id int64) Message {
	return Message{ID: id, Date: time.Now().Unix(), Chat: Chat{ID: 200, Type: "private", First: "Alice"}, From: &User{ID: 200}, Connection: "conn", Text: "你好", Extra: Obj{}}
}
func (f *fixture) route(u Update) {
	if u.ID == 0 {
		f.seq++
		u.ID = f.seq
	}
	exec(f.b.Store.db, "INSERT OR IGNORE INTO inbox(id,body) VALUES(?,?)", u.ID, raw(u))
	f.b.routeOne(context.Background())
}
func (f *fixture) connect() { c := connection(); f.route(Update{Connection: &c}) }
func (f *fixture) command(text string, thread int64) {
	m := message(1)
	m.From = &User{ID: 100}
	m.Chat = Chat{ID: 100, Type: "private"}
	m.Text = text
	m.Thread = thread
	f.route(Update{Command: &m})
}
func (f *fixture) ingest(m Message) { f.route(Update{Message: &m}) }
func (f *fixture) ready() *Ticket {
	f.command("/resume all", 0)
	f.ingest(message(1))
	s := state(f.b.Store.db, "200")
	s.Pending = 1
	save(f.b.Store.db, s)
	return f.b.prepare("200")
}
func (f *fixture) step() {
	put(f.b.Store.db, "archiveNext", int64(0))
	exec(f.b.Store.db, "UPDATE jobs SET next_at=0")
	f.b.archiveStep(context.Background())
}
func check(t *testing.T, yes bool, v ...any) {
	t.Helper()
	if !yes {
		t.Fatal(v...)
	}
}
func TestOwnerAuthorization(t *testing.T) {
	f := newFixture(t)
	c := connection()
	c.User.ID = 999
	f.route(Update{Connection: &c})
	check(t, policy(f.b.Store.db).Connection == nil)
	for _, kind := range []string{"other", "group", "anonymous", "note"} {
		m := message(1)
		m.Chat = Chat{ID: 100, Type: "private"}
		m.From = &User{ID: 100}
		m.Text = "/resume all"
		switch kind {
		case "other":
			m.From.ID = 999
		case "group":
			m.Chat.Type = "supergroup"
		case "anonymous":
			m.SenderChat = &Chat{ID: -1}
		case "note":
			m.Text = "备注"
		}
		f.route(Update{Command: &m})
		check(t, policy(f.b.Store.db).Paused, kind)
	}
}
func TestUnknownConnectionAndOrdering(t *testing.T) {
	f := newFixture(t)
	f.handler = func(method string, _ Obj) (any, int) {
		if method == "getBusinessConnection" {
			c := connection()
			c.User.ID = 999
			return c, 200
		}
		return nil, 200
	}
	f.ingest(message(1))
	check(t, len(rows(f.b.Store.db, "SELECT * FROM contacts")) == 0)
	f.handler = nil
	c := connection()
	c.Enabled = false
	f.route(Update{ID: 300, Connection: &c})
	c.Enabled = true
	f.route(Update{ID: 200, Connection: &c})
	check(t, !policy(f.b.Store.db).Connection.Enabled)
}
func TestArchiveVersionsDeletionAndDuplicate(t *testing.T) {
	f := newFixture(t)
	f.connect()
	m := message(1)
	f.route(Update{ID: 2, Message: &m})
	f.route(Update{ID: 2, Message: &m})
	check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) == 1)
	m.Text = "新版本"
	m.EditDate = m.Date + 1
	f.route(Update{ID: 3, Edited: &m})
	f.route(Update{Deleted: &Deleted{Connection: "conn", Chat: m.Chat, IDs: []int64{1, 9}}})
	m = message(1)
	f.ingest(m)
	check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) == 4)
	r := rows(f.b.Store.db, "SELECT * FROM messages WHERE key='conn:1'")[0]
	check(t, integer(r["deleted"]) == 1 && strings.Contains(stringVal(r["body"]), "新版本"))
	check(t, strings.Contains(stringVal(rows(f.b.Store.db, "SELECT parts FROM jobs WHERE id=4")[0]["parts"]), "未收到原消息"))
}
func TestDeleteBeforeMessageExcludedFromAI(t *testing.T) {
	f := newFixture(t)
	f.connect()
	m := message(1)
	f.route(Update{Deleted: &Deleted{Connection: "conn", Chat: m.Chat, IDs: []int64{1}}})
	f.ingest(m)
	s := state(f.b.Store.db, "200")
	check(t, s.Pending == 0)
	check(t, integer(rows(f.b.Store.db, "SELECT deleted FROM messages")[0]["deleted"]) == 1)
}
func TestAllMediaAndEntities(t *testing.T) {
	for _, kind := range []string{"photo", "video", "document", "voice", "audio", "sticker", "video_note", "animation", "unsupported", "protected", "text"} {
		t.Run(kind, func(t *testing.T) {
			m := message(1)
			m.Text = ""
			m.Extra = Obj{"caption": "说明", "caption_entities": []any{Obj{"type": "bold", "offset": 0, "length": 2}}, "media_group_id": "album"}
			v := Obj{"file_id": "file"}
			if kind == "photo" {
				m.Extra[kind] = []any{map[string]any(v)}
			} else {
				m.Extra[kind] = map[string]any(v)
			}
			if kind == "protected" {
				m.Extra["has_protected_content"] = true
			}
			if kind == "text" {
				m.Text = "文字"
				m.Extra["entities"] = []any{Obj{"type": "bold", "offset": 0, "length": 2}}
			}
			p := archiveParts(m, 100, false)
			check(t, len(p) == 2)
			check(t, strings.Contains(fmt.Sprint(p[0].Body["text"]), "album"))
			if kind == "unsupported" || kind == "protected" {
				check(t, strings.Contains(fmt.Sprint(p[1].Body["text"]), "媒体未备份"))
			} else if kind != "text" {
				check(t, p[1].Body[kind] == "file")
			} else {
				check(t, p[1].Body["entities"] != nil)
			}
		})
	}
}
func TestArchiveSendAndTopicOnce(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.ingest(message(1))
	m := message(2)
	m.From = &User{ID: 100}
	f.ingest(m)
	for range 4 {
		f.step()
	}
	n := 0
	for _, c := range f.calls {
		if c.Method == "createForumTopic" {
			n++
		}
		if strings.HasPrefix(c.Method, "send") && c.Body["disable_notification"] == true {
			check(t, c.Body["chat_id"] == float64(100) && c.Body["message_thread_id"] == float64(77))
			check(t, c.Body["business_connection_id"] == nil)
		}
	}
	check(t, n == 1)
	check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs WHERE status='done'")) == 2)
}
func TestArchiveFailures(t *testing.T) {
	for _, scenario := range []string{"429", "403", "uncertain", "closed", "deleted", "media"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			m := message(1)
			if scenario == "media" {
				m.Text = ""
				m.Extra = Obj{"photo": []any{Obj{"file_id": "file"}}}
			}
			f.ingest(m)
			if scenario == "media" {
				f.step()
			}
			f.handler = func(method string, _ Obj) (any, int) {
				if strings.HasPrefix(method, "send") {
					switch scenario {
					case "429":
						return Obj{"ok": false, "error_code": 429, "parameters": Obj{"retry_after": 30}}, 429
					case "403":
						return Obj{"ok": false, "error_code": 403}, 403
					case "uncertain":
						return Obj{"ok": false}, 500
					case "closed":
						return Obj{"ok": false, "error_code": 400, "description": "TOPIC_CLOSED"}, 400
					case "deleted":
						return Obj{"ok": false, "error_code": 400, "description": "message thread not found"}, 400
					case "media":
						return Obj{"ok": false, "error_code": 400}, 400
					}
				}
				return nil, 200
			}
			f.step()
			j := jobFrom(rows(f.b.Store.db, "SELECT * FROM jobs")[0])
			switch scenario {
			case "429":
				check(t, j.Next >= now()+29000)
			case "403":
				for range 4 {
					f.step()
				}
				check(t, stringVal(rows(f.b.Store.db, "SELECT status FROM jobs")[0]["status"]) == "failed")
			case "uncertain":
				check(t, j.Status == "uncertain")
			case "closed":
				check(t, j.Status == "failed")
			case "deleted":
				check(t, state(f.b.Store.db, "200").Topic == 0 && len(j.Parts) == 3)
			case "media":
				check(t, strings.Contains(raw(j.Parts), "媒体未备份"))
			}
		})
	}
}
func TestAIQuotaAndEcho(t *testing.T) {
	f := newFixture(t)
	f.connect()
	ticket := f.ready()
	check(t, ticket != nil)
	f.command("/limit 1 200", 0)
	s := state(f.b.Store.db, "200")
	s.Flight = nil
	s.Pending = 1
	save(f.b.Store.db, s)
	ticket = f.b.prepare("200")
	f.b.finishAI(context.Background(), "200", *ticket, "答复", 0)
	s = state(f.b.Store.db, "200")
	check(t, s.Used == 1 && s.Flight == nil && !s.Paused)
	r := rows(f.b.Store.db, "SELECT body FROM messages WHERE json_extract(body,'$.sender_business_bot') IS NOT NULL")
	m := decode[Message](stringVal(r[0]["body"]))
	before := len(rows(f.b.Store.db, "SELECT * FROM jobs"))
	f.ingest(m)
	check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) == before)
	s = state(f.b.Store.db, "200")
	s.Pending = 1
	save(f.b.Store.db, s)
	check(t, f.b.prepare("200") == nil)
}
func TestAIStaleResults(t *testing.T) {
	for _, reason := range []string{"pause all", "pause 200", "prompt 新提示词", "limit 0 200", "manual", "delete", "disconnect"} {
		t.Run(reason, func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			ticket := f.ready()
			switch reason {
			case "manual":
				m := message(2)
				m.From = &User{ID: 100}
				f.ingest(m)
			case "delete":
				f.route(Update{Deleted: &Deleted{Connection: "conn", Chat: message(1).Chat, IDs: []int64{1}}})
			case "disconnect":
				c := connection()
				c.Enabled = false
				f.route(Update{Connection: &c})
			default:
				f.command("/"+reason, 0)
			}
			f.b.finishAI(context.Background(), "200", *ticket, "不应发出", 0)
			check(t, state(f.b.Store.db, "200").Used == 0)
			for _, c := range f.calls {
				check(t, c.Body["business_connection_id"] == nil)
			}
		})
	}
}
func TestAITextOnlyAndDebounce(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.command("/resume all", 0)
	m := message(1)
	m.Text = ""
	m.Extra = Obj{"caption": "不触发", "photo": []any{Obj{"file_id": "f"}}}
	f.ingest(m)
	check(t, state(f.b.Store.db, "200").Pending == 0)
	f.ingest(message(2))
	s := state(f.b.Store.db, "200")
	check(t, s.Pending >= now()+2900 && s.Pending <= now()+3000)
	s.FirstPending = now() - 9000
	save(f.b.Store.db, s)
	f.ingest(message(3))
	s = state(f.b.Store.db, "200")
	check(t, s.Pending <= now()+1000)
}
func TestAIContextAndPermissions(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.command("/resume all", 0)
	for i := int64(1); i <= 25; i++ {
		m := message(i)
		m.Text = strings.Repeat("😀", 2000)
		f.ingest(m)
	}
	s := state(f.b.Store.db, "200")
	s.Pending = 1
	save(f.b.Store.db, s)
	ticket := f.b.prepare("200")
	count := 0
	for _, m := range ticket.Messages[1:] {
		count += len(utf16.Encode([]rune(m.Content)))
	}
	check(t, len(ticket.Messages) <= 21 && count <= 24000)
	p := policy(f.b.Store.db)
	s = state(f.b.Store.db, "200")
	s.LastIncoming = now() - 86400001
	check(t, !eligible(s, p))
	s.LastIncoming = now()
	p.Connection.Rights.Reply = false
	check(t, !eligible(s, p))
}
func TestAISendFailuresAndRetry(t *testing.T) {
	for _, code := range []int{429, 403, 500} {
		t.Run(str(int64(code)), func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			ticket := f.ready()
			f.handler = func(method string, body Obj) (any, int) {
				if method == "sendMessage" && body["business_connection_id"] != nil {
					return Obj{"ok": false, "error_code": code, "parameters": Obj{"retry_after": 30}}, code
				}
				return nil, 200
			}
			f.b.finishAI(context.Background(), "200", *ticket, "答复", 0)
			s := state(f.b.Store.db, "200")
			if code == 500 {
				check(t, s.Used == 1 && s.Flight.Phase == "uncertain")
			} else {
				check(t, s.Used == 0)
			}
			if code == 429 {
				check(t, s.Retry != nil && s.Retry.At > now()+29000)
				f.handler = nil
				f.b.finishAI(context.Background(), "200", s.Retry.Ticket, s.Retry.Text, 1)
				check(t, state(f.b.Store.db, "200").Used == 1)
			}
		})
	}
}
func TestPausePromptResetIdempotency(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.ingest(message(1))
	exec(f.b.Store.db, "INSERT INTO topics VALUES('200',77)")
	f.command("/pause", 77)
	f.command("/resume all", 0)
	check(t, state(f.b.Store.db, "200").Paused)
	f.command("/limit 2", 77)
	f.command("/reset", 77)
	check(t, state(f.b.Store.db, "200").Paused)
	p := policy(f.b.Store.db)
	f.command("/prompt 新提示词", 0)
	check(t, policy(f.b.Store.db).Version > p.Version)
	m := message(1)
	m.From = &User{ID: 100}
	m.Chat = Chat{ID: 100, Type: "private"}
	m.Text = "/reset 200"
	f.route(Update{ID: 500, Command: &m})
	s := state(f.b.Store.db, "200")
	s.Used = 3
	save(f.b.Store.db, s)
	f.route(Update{ID: 500, Command: &m})
	check(t, state(f.b.Store.db, "200").Used == 3)
}
func TestRestartRecoveryAndTransactions(t *testing.T) {
	f := newFixture(t)
	f.connect()
	_ = f.ready()
	s := state(f.b.Store.db, "200")
	s.Flight.Phase = "sending"
	s.Used = 1
	s.TopicState = "creating"
	save(f.b.Store.db, s)
	exec(f.b.Store.db, "UPDATE jobs SET status='sending'")
	f.b.Store.db.Close()
	f.b.Store = newStore(f.b.Dir)
	f.b.Store.recover()
	s = state(f.b.Store.db, "200")
	check(t, s.Flight.Phase == "uncertain" && s.Used == 1 && s.TopicState == "uncertain")
	check(t, stringVal(rows(f.b.Store.db, "SELECT status FROM jobs")[0]["status"]) == "uncertain")
	func() {
		defer func() { _ = recover() }()
		transaction(f.b.Store.db, func(q queryer) { put(q, "bad", true); panic("crash") })
	}()
	check(t, !doc(f.b.Store.db, "bad", false))
}
func TestPollingDurableOffset(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	f.handler = func(method string, body Obj) (any, int) {
		if method == "getUpdates" {
			n++
			if n == 1 {
				return []Update{{ID: 5}, {ID: 7}}, 200
			}
			check(t, body["offset"] == float64(8))
			check(t, len(rows(f.b.Store.db, "SELECT * FROM inbox")) == 2)
			cancel()
			return []Update{}, 200
		}
		return nil, 200
	}
	check(t, f.b.poll(ctx) == nil)
	check(t, doc(f.b.Store.db, "offset", int64(0)) == 8)
}
func TestSetupAutomaticWebhookRemoval(t *testing.T) {
	f := newFixture(t)
	check(t, f.b.setup(context.Background()) == nil)
	for _, c := range f.calls {
		if c.Method == "deleteWebhook" {
			check(t, c.Body["drop_pending_updates"] == false)
			return
		}
	}
	t.Fatal("no deleteWebhook")
}
func TestNodeMigration(t *testing.T) {
	dir := t.TempDir()
	a := openDB(filepath.Join(dir, "account-100.sqlite"), false)
	exec(a, `CREATE TABLE documents(key TEXT PRIMARY KEY,value TEXT);CREATE TABLE connections(id TEXT,body TEXT,version INTEGER);CREATE TABLE inbox(id INTEGER,body TEXT,attempts INTEGER,next_at INTEGER,done INTEGER);CREATE TABLE command_results(id INTEGER,result TEXT);CREATE TABLE topics(chat_id TEXT,topic INTEGER);`)
	put(a, "policy", Policy{Limit: 7, Prompt: "迁移提示词", Paused: true})
	put(a, "offset", int64(123))
	exec(a, "INSERT INTO topics VALUES('200',77)")
	a.Close()
	c := openDB(filepath.Join(dir, "chat-100_200.sqlite"), false)
	exec(c, `CREATE TABLE documents(key TEXT PRIMARY KEY,value TEXT);CREATE TABLE messages(key TEXT,body TEXT,deleted INTEGER,version INTEGER,update_id INTEGER,anchor INTEGER);CREATE TABLE versions(key TEXT);CREATE TABLE archive_jobs(id INTEGER,source TEXT,parts TEXT,step INTEGER,attempts INTEGER,status TEXT,next_at INTEGER,anchor INTEGER);CREATE TABLE controls(id INTEGER,result TEXT);`)
	put(c, "state", State{Chat: "200", Name: "Alice", Used: 4, Topic: 77})
	exec(c, "INSERT INTO messages VALUES(?,?,0,2,1,5)", "conn:1", raw(message(1)))
	exec(c, "INSERT INTO archive_jobs VALUES(1,'conn:1',?,0,0,'pending',0,NULL)", raw(archiveParts(message(1), 100, false)))
	c.Close()
	s := newStore(dir)
	defer s.db.Close()
	s.migrate(dir, "100")
	check(t, policy(s.db).Limit == 7 && state(s.db, "200").Used == 4 && doc(s.db, "offset", int64(0)) == 123)
	check(t, len(rows(s.db, "SELECT * FROM messages")) == 1 && len(rows(s.db, "SELECT * FROM jobs")) == 1)
	s.migrate(dir, "100")
	check(t, len(rows(s.db, "SELECT * FROM jobs")) == 1)
}

func TestSlowModelDoesNotBlockArchive(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.command("/resume all", 0)
	f.ingest(message(1))
	s := state(f.b.Store.db, "200")
	s.Pending = 1
	save(f.b.Store.db, s)
	entered := make(chan struct{})
	release := make(chan struct{})
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(Obj{"choices": []any{Obj{"message": Obj{"content": "AI 回复"}}}})
	}))
	defer model.Close()
	defer func() { f.b.wg.Wait() }()
	f.b.API.Base = model.URL
	f.b.API.Client = model.Client()
	f.b.launchAI(context.Background())
	<-entered
	f.step()
	check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs WHERE step=1")) == 1)
	close(release)
	f.b.wg.Wait()
	check(t, state(f.b.Store.db, "200").Used == 1)
}
func TestConcurrentAIQuotaReservation(t *testing.T) {
	f := newFixture(t)
	f.connect()
	ticket := f.ready()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { f.b.finishAI(context.Background(), "200", *ticket, "回复", 0) })
	}
	wg.Wait()
	check(t, state(f.b.Store.db, "200").Used == 1)
}
func TestRepairCommands(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.ingest(message(1))
	s := state(f.b.Store.db, "200")
	s.TopicState = "uncertain"
	save(f.b.Store.db, s)
	f.command("/topic_bind 200", 77)
	check(t, state(f.b.Store.db, "200").Topic == 77)
	exec(f.b.Store.db, "UPDATE jobs SET status='uncertain'")
	f.command("/archive_resolve 1 sent 500", 77)
	j := jobFrom(rows(f.b.Store.db, "SELECT * FROM jobs")[0])
	check(t, j.Step == 1 && j.Anchor == 500 && j.Status == "pending")
	exec(f.b.Store.db, "UPDATE jobs SET status='failed'")
	f.command("/retry", 77)
	check(t, stringVal(rows(f.b.Store.db, "SELECT status FROM jobs")[0]["status"]) == "pending")
}
