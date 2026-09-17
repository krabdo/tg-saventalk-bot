package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Bot struct {
	mu              sync.Mutex
	Store           *Store
	API             *API
	Owner           int64
	Dir             string
	Username        string
	busy            map[string]bool
	aiCancel        map[string]context.CancelFunc
	CleanupMidnight bool
	lastPrune       int64
	contextSince    int64
	wg              sync.WaitGroup
}

func (b *Bot) notify(ctx context.Context) {
	b.mu.Lock()
	last := doc(b.Store.db, "lastAlert", int64(0))
	send := now()-last > 600000
	if send {
		put(b.Store.db, "lastAlert", now())
	}
	b.mu.Unlock()
	if send {
		_ = b.API.telegram(ctx, "sendMessage", Obj{"chat_id": b.Owner, "text": "归档或 AI 出现异常。请 /status all 或在联系人话题 /status 核对；失败的转发任务仅暂存内存，重启会丢失。不确定的发送不会自动重试。"}, nil)
	}
}
func (b *Bot) verifyConnection(ctx context.Context, id string) (*Connection, error) {
	b.mu.Lock()
	r := rows(b.Store.db, "SELECT body FROM connections WHERE id=?", id)
	b.mu.Unlock()
	if len(r) > 0 {
		c := decode[Connection](stringVal(r[0]["body"]))
		return &c, nil
	}
	var c Connection
	if e := b.API.telegram(ctx, "getBusinessConnection", Obj{"business_connection_id": id}, &c); e != nil {
		return nil, e
	}
	if c.User.ID != b.Owner {
		return nil, nil
	}
	b.mu.Lock()
	b.recordConnection(b.Store.db, c, -1)
	b.mu.Unlock()
	return &c, nil
}
func (b *Bot) recordConnection(q queryer, c Connection, version int64) {
	if c.User.ID != b.Owner {
		return
	}
	c.User = User{ID: c.User.ID}
	r := rows(q, "SELECT version FROM connections WHERE id=?", c.ID)
	if len(r) > 0 && integer(r[0]["version"]) >= version {
		return
	}
	exec(q, "INSERT OR REPLACE INTO connections VALUES(?,?,?)", c.ID, raw(c), version)
	p := policy(q)
	if p.Connection == nil || p.Connection.ID == c.ID || p.Connection.Date <= c.Date {
		p.Connection = &c
		p.Version++
		put(q, "policy", p)
	}
}
func (b *Bot) record(q queryer, s *State, m Message, id int64, edited bool, p Policy) {
	key := m.Connection + ":" + str(m.ID)
	versionKey := key + ":original"
	if edited {
		hash := sha256.Sum256([]byte(raw(m)))
		versionKey = key + ":edit:" + str(m.EditDate) + ":" + hex.EncodeToString(hash[:])
	}
	if len(rows(q, "SELECT key FROM versions WHERE chat=? AND key=?", s.Chat, versionKey)) > 0 {
		return
	}
	exec(q, "INSERT INTO versions VALUES(?,?)", s.Chat, versionKey)
	old := rows(q, "SELECT * FROM messages WHERE chat=? AND key=?", s.Chat, key)
	var anchor int64
	deleted := false
	version := m.Date * 2
	if edited {
		version = m.EditDate*2 + 1
	}
	current := len(old) == 0 || integer(old[0]["version"]) == 0 || version > integer(old[0]["version"]) || (version == integer(old[0]["version"]) && id > integer(old[0]["update_id"]))
	if len(old) > 0 {
		anchor = integer(old[0]["anchor"])
		deleted = integer(old[0]["deleted"]) != 0
	}
	if current {
		var body any
		if contextEnabled(*s, p) && b.API.configured() && m.Text != "" && !deleted && m.Date >= s.ContextSince {
			role := "user"
			if m.owner(b.Owner) || m.SenderBot != nil {
				role = "assistant"
			}
			body = raw(ContextText{ID: m.ID, Date: m.Date, Text: m.Text, Role: role})
		}
		exec(q, `INSERT INTO messages(chat,key,body,version,update_id) VALUES(?,?,?,?,?) ON CONFLICT(chat,key) DO UPDATE SET body=excluded.body,version=excluded.version,update_id=excluded.update_id`, s.Chat, key, body, version, id)
		s.Revision++
	}
	parts := archiveParts(m, b.Owner, edited)
	if edited && anchor != 0 {
		parts[0].Body["reply_parameters"] = Obj{"message_id": anchor, "allow_sending_without_reply": true}
	}
	if deleted {
		parts[0].Body["text"] = fmt.Sprint(parts[0].Body["text"]) + "\n🗑 原消息已删除（迟到的更新）"
	}
	addJob(q, s.Chat, key, parts)
	if m.owner(b.Owner) && m.SenderBot == nil && !edited {
		s.Paused = true
		b.clearContact(q, s, time.Now().Unix())
	} else if contextEnabled(*s, p) && b.API.configured() && m.Date >= s.ContextSince && !m.owner(b.Owner) && m.SenderBot == nil && (m.From == nil || !m.From.Bot) && !edited && !deleted {
		s.LastIncoming = max(s.LastIncoming, m.Date*1000)
		s.Connection = m.Connection
		if m.Text != "" && !s.Paused && !p.Paused {
			if s.FirstPending == 0 {
				s.FirstPending = now()
			}
			s.Pending = min(now()+3000, s.FirstPending+10000)
			s.PendingPolicy = p.Version
		}
	}
	trimContext(q, s.Chat)
}
func (b *Bot) ingest(q queryer, u Update) {
	m := u.Message
	if m == nil {
		m = u.Edited
	}
	var chat Chat
	if m != nil {
		chat = m.Chat
	} else {
		chat = u.Deleted.Chat
	}
	s := state(q, str(chat.ID))
	s.ContextSince = max(s.ContextSince, b.contextSince)
	if s.Topic == 0 {
		s.Name = strings.TrimSpace(chat.First + " " + chat.Last)
		if s.Name == "" {
			s.Name = chat.Username
		}
		if s.Name == "" {
			s.Name = s.Chat
		}
	} else {
		s.Name = ""
	}
	p := policy(q)
	if m != nil {
		b.record(q, &s, *m, u.ID, u.Edited != nil, p)
	} else {
		for _, id := range u.Deleted.IDs {
			key := u.Deleted.Connection + ":" + str(id)
			old := rows(q, "SELECT body,anchor,version FROM messages WHERE chat=? AND key=?", s.Chat, key)
			text := fmt.Sprintf("🗑 原消息 #%d 已删除；未收到原消息或缓存已清理，已有话题副本不受影响", id)
			if len(old) > 0 && integer(old[0]["version"]) > 0 {
				text = fmt.Sprintf("🗑 原消息 #%d 已删除；已收到原消息，尚未确认转发完成", id)
			}
			body := Obj{}
			if len(old) > 0 && integer(old[0]["anchor"]) != 0 {
				text = fmt.Sprintf("🗑 原消息 #%d 已删除；保留归档副本", id)
				if anchor := integer(old[0]["anchor"]); anchor != 0 {
					body["reply_parameters"] = Obj{"message_id": anchor, "allow_sending_without_reply": true}
				}
			}
			body["text"] = text
			exec(q, "INSERT INTO messages(chat,key,deleted) VALUES(?,?,1) ON CONFLICT(chat,key) DO UPDATE SET deleted=1,body=NULL", s.Chat, key)
			addJob(q, s.Chat, key, []Part{{Method: "sendMessage", Body: body}})
		}
		s.Revision++
	}
	save(q, s)
}
func (b *Bot) routeOne(ctx context.Context) bool {
	b.mu.Lock()
	pending := rows(b.Store.db, "SELECT id,body,attempts FROM inbox WHERE done=0 AND next_at<=? ORDER BY id LIMIT 1", now())
	b.mu.Unlock()
	if len(pending) == 0 {
		return false
	}
	r := pending[0]
	u := decode[Update](stringVal(r["body"]))
	var cid string
	var typ string
	if u.Message != nil {
		cid = u.Message.Connection
		typ = u.Message.Chat.Type
	} else if u.Edited != nil {
		cid = u.Edited.Connection
		typ = u.Edited.Chat.Type
	} else if u.Deleted != nil {
		cid = u.Deleted.Connection
		typ = u.Deleted.Chat.Type
	}
	authorized := false
	if cid != "" && typ == "private" {
		c, e := b.verifyConnection(ctx, cid)
		if e != nil {
			b.mu.Lock()
			delay := min(int64(300000), int64(1000)<<min(integer(r["attempts"])+1, 8))
			if a, ok := e.(*APIError); ok {
				delay = max(delay, int64(a.Retry)*1000)
			}
			exec(b.Store.db, "UPDATE inbox SET attempts=attempts+1,next_at=? WHERE id=?", now()+delay, u.ID)
			b.mu.Unlock()
			return true
		}
		authorized = c != nil && c.User.ID == b.Owner
	}
	var answer string
	b.mu.Lock()
	transaction(b.Store.db, func(q queryer) {
		if u.Connection != nil {
			b.recordConnection(q, *u.Connection, u.ID)
		} else if authorized {
			b.ingest(q, u)
		} else if u.Command != nil {
			answer = b.command(q, u.ID, *u.Command)
		}
		exec(q, "UPDATE inbox SET done=1,body='{}' WHERE id=?", u.ID)
	})
	b.mu.Unlock()
	if answer != "" {
		body := Obj{"chat_id": b.Owner}
		if u.Command.Thread != 0 {
			body["message_thread_id"] = u.Command.Thread
		}
		for i, text := range chunks(answer, 4000) {
			if i > 0 && !wait(ctx, time.Second) {
				break
			}
			body["text"] = text
			if b.API.telegram(ctx, "sendMessage", body, nil) != nil {
				break
			}
		}
	}
	return true
}

var allowed = []string{"business_connection", "business_message", "edited_business_message", "deleted_business_messages", "message"}

func (b *Bot) poll(ctx context.Context) error {
	for ctx.Err() == nil {
		b.mu.Lock()
		offset := doc(b.Store.db, "offset", int64(0))
		load := rows(b.Store.db, "SELECT (SELECT COUNT(*) FROM jobs)+(SELECT COUNT(*) FROM inbox WHERE done=0) AS n,(SELECT COALESCE(SUM(length(parts)),0) FROM jobs)+(SELECT COALESCE(SUM(length(body)),0) FROM inbox WHERE done=0) AS bytes")[0]
		full := integer(load["n"]) >= 1024 || integer(load["bytes"]) >= 8*1024*1024
		b.mu.Unlock()
		if full {
			must(os.WriteFile(filepath.Join(b.Dir, "health"), []byte(str(now())), 0600))
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		var batch []Update
		e := b.API.telegram(ctx, "getUpdates", Obj{"offset": offset, "timeout": 30, "limit": 100, "allowed_updates": allowed}, &batch)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			if a, ok := e.(*APIError); ok {
				if a.Code == 401 || a.Code == 409 {
					return fmt.Errorf("polling stopped (%d): check Token and other instances", a.Code)
				}
				if !wait(ctx, time.Duration(max(5, a.Retry))*time.Second) {
					return nil
				}
			}
			continue
		}
		b.mu.Lock()
		transaction(b.Store.db, func(q queryer) {
			for _, u := range batch {
				exec(q, "INSERT OR IGNORE INTO inbox(id,body) VALUES(?,?)", u.ID, raw(u))
				offset = max(offset, u.ID+1)
			}
			if len(batch) > 0 {
				put(q, "offset", offset)
			}
		})
		b.mu.Unlock()
		must(os.WriteFile(filepath.Join(b.Dir, "health"), []byte(str(now())), 0600))
	}
	return nil
}
func (b *Bot) startWorkers(ctx context.Context) {
	b.busy = map[string]bool{}
	b.aiCancel = map[string]context.CancelFunc{}
	b.wg.Add(4)
	go b.maintenanceLoop(ctx)
	go func() {
		defer safePanic()
		defer b.wg.Done()
		for ctx.Err() == nil {
			if !b.routeOne(ctx) {
				if !wait(ctx, 200*time.Millisecond) {
					return
				}
			}
		}
	}()
	go func() {
		defer safePanic()
		defer b.wg.Done()
		for ctx.Err() == nil {
			b.archiveStep(ctx)
			if !wait(ctx, 200*time.Millisecond) {
				return
			}
		}
	}()
	go func() {
		defer safePanic()
		defer b.wg.Done()
		for ctx.Err() == nil {
			b.launchAI(ctx)
			if !wait(ctx, 200*time.Millisecond) {
				return
			}
		}
	}()
}
func (b *Bot) setup(ctx context.Context) error {
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Business bool   `json:"can_connect_to_business"`
		Topics   bool   `json:"has_topics_enabled"`
	}
	if e := b.API.telegram(ctx, "getMe", Obj{}, &me); e != nil {
		return e
	}
	if !me.Business || !me.Topics {
		return fmt.Errorf("enable Secretary Mode and private topics in BotFather")
	}
	b.Username = me.Username
	b.mu.Lock()
	identity := doc(b.Store.db, "identity", "")
	if identity != "" && identity != fmt.Sprintf("%d:%d", me.ID, b.Owner) {
		b.mu.Unlock()
		return fmt.Errorf("data volume belongs to another bot or owner")
	}
	put(b.Store.db, "identity", fmt.Sprintf("%d:%d", me.ID, b.Owner))
	b.mu.Unlock()
	if e := b.API.telegram(ctx, "deleteWebhook", Obj{"drop_pending_updates": false}, nil); e != nil {
		return e
	}
	commands := []Obj{}
	for _, name := range []string{"help", "status", "pause", "resume", "limit", "reset", "prompt", "prompt_show", "prompt_reset", "retry"} {
		commands = append(commands, Obj{"command": name, "description": name})
	}
	return b.API.telegram(ctx, "setMyCommands", Obj{"scope": Obj{"type": "chat", "chat_id": b.Owner}, "commands": commands}, nil)
}
func safePanic() {
	if recover() != nil {
		log.Print("Fatal storage/runtime failure; stopping to preserve pending tasks")
		os.Exit(1)
	}
}
