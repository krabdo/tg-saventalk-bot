package main

import (
	"database/sql"
	"errors"
	_ "github.com/mattn/go-sqlite3"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Store struct{ db *sql.DB }
type queryer interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func openDB(path string, readOnly bool) *sql.DB {
	if filepath.VolumeName(path) != "" {
		path = "/" + filepath.ToSlash(path)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("_busy_timeout", "5000")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("_journal_mode", "WAL")
		q.Set("_synchronous", "FULL")
		q.Set("_secure_delete", "on")
	}
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite3", u.String())
	must(e)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1) // TEMP tables must remain on this one connection.
	must(db.Ping())
	return db
}
func exec(q queryer, s string, args ...any) sql.Result { r, e := q.Exec(s, args...); must(e); return r }
func rows(q queryer, s string, args ...any) []map[string]any {
	out := []map[string]any{}
	eachRow(q, s, func(row map[string]any) { out = append(out, row) }, args...)
	return out
}
func eachRow(q queryer, s string, visit func(map[string]any), args ...any) {
	r, e := q.Query(s, args...)
	must(e)
	defer r.Close()
	cols, e := r.Columns()
	must(e)
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		must(r.Scan(ptrs...))
		m := map[string]any{}
		for i, k := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			m[k] = vals[i]
		}
		visit(m)
	}
	must(r.Err())
}
func integer(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}
func stringVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func doc[T any](q queryer, key string, fallback T) T {
	var s string
	e := q.QueryRow("SELECT value FROM documents WHERE key=?", key).Scan(&s)
	if errors.Is(e, sql.ErrNoRows) {
		return fallback
	}
	must(e)
	return decode[T](s)
}
func put(q queryer, key string, v any) {
	exec(q, "INSERT OR REPLACE INTO documents VALUES(?,?)", key, raw(v))
}
func transaction(db *sql.DB, fn func(queryer)) {
	tx, e := db.Begin()
	must(e)
	defer tx.Rollback()
	fn(tx)
	must(tx.Commit())
}
func newStore(dir string) *Store {
	must(os.MkdirAll(dir, 0700))
	db := openDB(filepath.Join(dir, "bot.sqlite"), false)
	s := &Store{db}
	exec(db, "PRAGMA temp_store=MEMORY; PRAGMA secure_delete=ON")
	// Delete historical payload tables from older releases before creating
	// memory-only replacements. No disk table remains as a fallback if the
	// connection is lost: a missing TEMP table then fails closed.
	transaction(db, func(q queryer) {
		for _, table := range []string{"messages", "versions", "jobs", "inbox", "commands"} {
			if len(rows(q, "SELECT name FROM main.sqlite_master WHERE type='table' AND name=?", table)) > 0 {
				exec(q, "DELETE FROM main."+table)
				exec(q, "DROP TABLE main."+table)
			}
		}
	})
	exec(db, `
 CREATE TABLE IF NOT EXISTS documents(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS contacts(chat TEXT PRIMARY KEY,state TEXT NOT NULL);
 CREATE TEMP TABLE contacts(chat TEXT PRIMARY KEY,state TEXT NOT NULL);
 CREATE INDEX temp.contact_pending ON contacts(json_extract(state,'$.pendingAt'));
 CREATE INDEX temp.contact_retry ON contacts(json_extract(state,'$.retry.At'));
 CREATE TABLE IF NOT EXISTS connections(id TEXT PRIMARY KEY,body TEXT NOT NULL,version INTEGER NOT NULL);
 CREATE TEMP TABLE inbox(id INTEGER PRIMARY KEY,body TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL DEFAULT 0,done INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS pending_inbox ON inbox(done,next_at,id);
 CREATE TEMP TABLE messages(chat TEXT NOT NULL,key TEXT NOT NULL,body TEXT,deleted INTEGER DEFAULT 0,version INTEGER DEFAULT 0,update_id INTEGER DEFAULT 0,anchor INTEGER,PRIMARY KEY(chat,key));
 CREATE INDEX IF NOT EXISTS context_messages ON messages(chat,deleted,json_extract(body,'$.date') DESC,json_extract(body,'$.message_id') DESC) WHERE body IS NOT NULL AND json_extract(body,'$.text') IS NOT NULL;
 CREATE TEMP TABLE versions(chat TEXT NOT NULL,key TEXT NOT NULL,PRIMARY KEY(chat,key));
 CREATE TEMP TABLE jobs(chat TEXT NOT NULL,id INTEGER NOT NULL,source TEXT NOT NULL,parts TEXT NOT NULL,step INTEGER DEFAULT 0,attempts INTEGER DEFAULT 0,status TEXT DEFAULT 'pending',next_at INTEGER DEFAULT 0,anchor INTEGER,PRIMARY KEY(chat,id));
 CREATE INDEX IF NOT EXISTS pending_jobs ON jobs(status,next_at,chat,id);
 CREATE TEMP TABLE commands(id INTEGER PRIMARY KEY,result TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS topics(chat TEXT PRIMARY KEY,topic INTEGER UNIQUE NOT NULL);
 `)
	transaction(db, func(q queryer) {
		for _, r := range rows(q, "SELECT state FROM main.contacts") {
			v := decode[State](stringVal(r["state"]))
			v = minimalState(v)
			save(q, v)
		}
	})
	return s
}
func state(q queryer, chat string) State {
	var s string
	e := q.QueryRow("SELECT state FROM contacts WHERE chat=?", chat).Scan(&s)
	if errors.Is(e, sql.ErrNoRows) {
		return State{Chat: chat, Name: chat}
	}
	must(e)
	return decode[State](s)
}
func minimalState(s State) State {
	v := State{Chat: s.Chat, Paused: s.Paused, Limit: s.Limit, Used: s.Used, Topic: s.Topic, TopicState: s.TopicState}
	if s.Flight != nil && (s.Flight.Phase == "sending" || s.Flight.Phase == "uncertain") {
		v.Flight = &Flight{Phase: "uncertain"}
	}
	return v
}
func save(q queryer, s State) {
	exec(q, "INSERT OR REPLACE INTO temp.contacts VALUES(?,?)", s.Chat, raw(s))
	exec(q, "INSERT OR REPLACE INTO main.contacts VALUES(?,?)", s.Chat, raw(minimalState(s)))
}
func policy(q queryer) Policy {
	return doc(q, "policy", Policy{Paused: true, Limit: 10, Prompt: defaultPrompt})
}
func addJob(q queryer, chat, key string, parts []Part) {
	id := doc(q, "jobSequence", int64(0)) + 1
	for _, r := range rows(q, "SELECT COALESCE(MAX(id),0)+1 AS next FROM jobs") {
		id = max(id, integer(r["next"]))
	}
	put(q, "jobSequence", id)
	exec(q, "INSERT INTO jobs(chat,id,source,parts) VALUES(?,?,?,?)", chat, id, key, raw(parts))
}
func (s *Store) recover() {
	transaction(s.db, func(q queryer) {
		exec(q, "UPDATE jobs SET status='uncertain' WHERE status='sending'")
		for _, r := range rows(q, "SELECT state FROM contacts") {
			v := decode[State](stringVal(r["state"]))
			if v.TopicState == "creating" {
				v.TopicState = "uncertain"
			}
			if v.Flight != nil {
				if v.Flight.Phase == "sending" {
					v.Flight.Phase = "uncertain"
				} else if v.Flight.Phase == "generating" && v.Retry == nil {
					v.Flight = nil
					v.AIError = "生成任务被重启中断，等待新文字"
				}
			}
			save(q, v)
		}
	})
}

// Import Node v1 files once, in one destination transaction; source files remain untouched.
func (s *Store) migrate(dir, owner string) {
	if doc(s.db, "nodeImported", false) {
		return
	}
	path := filepath.Join(dir, "account-"+owner+".sqlite")
	if _, e := os.Stat(path); e != nil {
		return
	}
	old := openDB(path, true)
	defer old.Close()
	transaction(s.db, func(q queryer) {
		eachRow(old, "SELECT * FROM documents", func(r map[string]any) {
			exec(q, "INSERT OR IGNORE INTO documents VALUES(?,?)", r["key"], r["value"])
		})
		eachRow(old, "SELECT * FROM connections", func(r map[string]any) {
			exec(q, "INSERT OR IGNORE INTO connections VALUES(?,?,?)", r["id"], r["body"], r["version"])
		})
		eachRow(old, "SELECT * FROM inbox", func(r map[string]any) {
			exec(q, "INSERT OR IGNORE INTO inbox VALUES(?,?,?,?,?)", r["id"], r["body"], r["attempts"], r["next_at"], r["done"])
		})
		eachRow(old, "SELECT * FROM topics", func(r map[string]any) {
			exec(q, "INSERT OR IGNORE INTO topics VALUES(?,?)", r["chat_id"], r["topic"])
		})
		files, e := filepath.Glob(filepath.Join(dir, "chat-"+owner+"_*.sqlite"))
		must(e)
		for _, file := range files {
			func() {
				c := openDB(file, true)
				defer c.Close()
				v := doc(c, "state", State{})
				if v.Chat == "" {
					return
				}
				save(q, minimalState(v))
				eachRow(c, "SELECT * FROM archive_jobs WHERE status!='done'", func(r map[string]any) {
					exec(q, "INSERT OR IGNORE INTO jobs VALUES(?,?,?,?,?,?,?,?,?)", v.Chat, r["id"], r["source"], r["parts"], r["step"], r["attempts"], r["status"], r["next_at"], r["anchor"])
				})
			}()
		}
		put(q, "nodeImported", true)
	})
}
func isDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
