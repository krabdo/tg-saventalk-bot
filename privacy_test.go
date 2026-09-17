package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func assertNoDiskPayload(t *testing.T, f *fixture, markers ...string) {
	t.Helper()
	files, e := filepath.Glob(filepath.Join(f.b.Dir, "*"))
	must(e)
	for _, file := range files {
		data, e := os.ReadFile(file)
		must(e)
		for _, marker := range markers {
			check(t, !strings.Contains(string(data), marker), "private payload found in", filepath.Base(file))
		}
	}
}
func TestNoMessagePayloadOnDiskWithAIOnOrOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(str(int64(map[bool]int{false: 0, true: 1}[enabled])), func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			if enabled {
				f.command("/resume all", 0)
			}
			m := message(1)
			m.Text = "private-message-marker-49021"
			m.Chat.First = "private-contact-name-3911"
			f.ingest(m)
			check(t, len(rows(f.b.Store.db, "SELECT * FROM main.sqlite_master WHERE type='table' AND name IN ('messages','jobs','inbox','versions','commands')")) == 0)
			check(t, integer(rows(f.b.Store.db, "PRAGMA temp_store")[0]["temp_store"]) == 2)
			assertNoDiskPayload(t, f, m.Text, m.Chat.First)
			bodies := rows(f.b.Store.db, "SELECT body FROM messages WHERE body IS NOT NULL")
			check(t, (len(bodies) == 1) == enabled)
			f.step()
			f.step()
			check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) == 0)
			assertNoDiskPayload(t, f, m.Text, m.Chat.First)
		})
	}
}
func TestDiskQuotaAndTopicSurviveButCachesDoNot(t *testing.T) {
	f := newFixture(t)
	f.connect()
	ticket := f.ready()
	s := state(f.b.Store.db, "200")
	s.Topic = 77
	s.Used = 3
	s.Retry = &Retry{Ticket: *ticket, Text: "retry-secret-marker", At: now() + 1000}
	save(f.b.Store.db, s)
	exec(f.b.Store.db, "INSERT INTO topics VALUES('200',77)")
	assertNoDiskPayload(t, f, "retry-secret-marker", "你好")
	f.b.Store.db.Close()
	f.b.Store = newStore(f.b.Dir)
	s = state(f.b.Store.db, "200")
	check(t, s.Topic == 77 && s.Used == 3 && s.Retry == nil && s.Flight == nil && s.Name == "" && s.LastIncoming == 0)
	for _, table := range []string{"jobs", "messages", "inbox", "versions", "commands"} {
		check(t, len(rows(f.b.Store.db, "SELECT * FROM "+table)) == 0, table)
	}
}
func TestAIOffAndManualTakeoverEraseContext(t *testing.T) {
	for _, mode := range []string{"global", "contact", "limit", "manual"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			ticket := f.ready()
			s := state(f.b.Store.db, "200")
			s.Retry = &Retry{Ticket: *ticket, Text: "cached answer", At: now() + 1000}
			save(f.b.Store.db, s)
			pending := len(rows(f.b.Store.db, "SELECT * FROM jobs"))
			switch mode {
			case "global":
				f.command("/pause all", 0)
			case "contact":
				f.command("/pause 200", 0)
			case "limit":
				f.command("/limit 0 200", 0)
			case "manual":
				m := message(2)
				m.From = &User{ID: 100}
				f.ingest(m)
			}
			s = state(f.b.Store.db, "200")
			check(t, s.Retry == nil && s.Flight == nil && s.Pending == 0 && s.LastIncoming == 0)
			check(t, len(rows(f.b.Store.db, "SELECT * FROM messages WHERE body IS NOT NULL")) == 0)
			check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) >= pending, "forwarding must continue")
			f.b.finishAI(context.Background(), "200", *ticket, "stale", 0)
			check(t, state(f.b.Store.db, "200").Used == 0)
		})
	}
}
func TestMidnightCleanupIsOptionalAndUsesLocalDate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(str(int64(map[bool]int{false: 0, true: 1}[enabled])), func(t *testing.T) {
			f := newFixture(t)
			f.connect()
			ticket := f.ready()
			f.b.CleanupMidnight = enabled
			zone := time.FixedZone("server", 8*3600)
			day := time.Date(2026, 9, 17, 23, 59, 59, 0, zone)
			put(f.b.Store.db, "lastCleanupDate", day.Format("2006-01-02"))
			f.b.maintenance(day)
			check(t, len(rows(f.b.Store.db, "SELECT * FROM messages WHERE body IS NOT NULL")) == 1)
			f.b.maintenance(day.Add(time.Second))
			check(t, (len(rows(f.b.Store.db, "SELECT * FROM messages WHERE body IS NOT NULL")) == 0) == enabled)
			check(t, len(rows(f.b.Store.db, "SELECT * FROM jobs")) == 1)
			check(t, !policy(f.b.Store.db).Paused)
			if enabled {
				f.b.finishAI(context.Background(), "200", *ticket, "stale", 0)
				check(t, state(f.b.Store.db, "200").Used == 0)
				revision := state(f.b.Store.db, "200").Revision
				f.b.maintenance(day.Add(2 * time.Second))
				check(t, state(f.b.Store.db, "200").Revision == revision)
			}
		})
	}
}
func TestStoredContextIsBoundedAndMinimal(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.command("/resume all", 0)
	for id := int64(1); id < 30; id++ {
		m := message(id)
		m.Text = strings.Repeat("😀", 2000)
		m.Extra = Obj{"caption": "not-context", "document": Obj{"file_id": "not-context-file"}}
		f.ingest(m)
	}
	r := rows(f.b.Store.db, "SELECT body FROM messages WHERE body IS NOT NULL")
	check(t, len(r) <= 20)
	units := 0
	for _, row := range r {
		s := stringVal(row["body"])
		v := decode[ContextText](s)
		units += len(utf16.Encode([]rune(v.Text)))
		check(t, !strings.Contains(s, "caption") && !strings.Contains(s, "file_id") && !strings.Contains(s, "first_name"))
	}
	check(t, units <= 24000)
}
func TestUpgradeScrubsHistoricalDatabase(t *testing.T) {
	dir := t.TempDir()
	db := openDB(filepath.Join(dir, "bot.sqlite"), false)
	exec(db, `CREATE TABLE contacts(chat TEXT PRIMARY KEY,state TEXT);CREATE TABLE messages(body TEXT);CREATE TABLE jobs(parts TEXT);CREATE TABLE documents(key TEXT PRIMARY KEY,value TEXT);`)
	marker := "legacy-secret-marker-13379"
	exec(db, "INSERT INTO messages VALUES(?)", marker)
	exec(db, "INSERT INTO jobs VALUES(?)", marker)
	s := State{Chat: "200", Name: marker, Used: 4, Topic: 77, Retry: &Retry{Text: marker}}
	exec(db, "INSERT INTO contacts VALUES('200',?)", raw(s))
	db.Close()
	store := newStore(dir)
	defer store.db.Close()
	f := &fixture{b: &Bot{Store: store, Dir: dir, Owner: 100, API: newAPI("1:t", "", "", "")}}
	f.b.privacyStartup()
	assertNoDiskPayload(t, f, marker)
	check(t, state(store.db, "200").Used == 4 && state(store.db, "200").Topic == 77)
	check(t, len(rows(store.db, "SELECT * FROM jobs")) == 0)
}
func TestPauseCancelsModelRequest(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.command("/resume all", 0)
	f.ingest(message(1))
	s := state(f.b.Store.db, "200")
	s.Pending = 1
	save(f.b.Store.db, s)
	entered := make(chan struct{})
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer model.Close()
	f.b.API.Client = model.Client()
	f.b.API.Base = model.URL
	f.b.launchAI(context.Background())
	<-entered
	f.command("/pause all", 0)
	done := make(chan struct{})
	go func() { f.b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model request was not canceled")
	}
	check(t, len(rows(f.b.Store.db, "SELECT * FROM messages WHERE body IS NOT NULL")) == 0)
	check(t, state(f.b.Store.db, "200").Retry == nil)
}
