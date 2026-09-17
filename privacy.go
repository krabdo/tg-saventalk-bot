package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// Context contains only what the model needs; Telegram users/media never enter it.
type ContextText struct {
	ID   int64  `json:"message_id"`
	Date int64  `json:"date"`
	Text string `json:"text"`
	Role string `json:"role"`
}

func contextEnabled(s State, p Policy) bool {
	return !p.Paused && !s.Paused && (s.Limit == nil || *s.Limit > 0) && (s.Limit != nil || p.Limit > 0)
}
func trimContext(q queryer, chat string) {
	budget := 24000
	count := 0
	for _, r := range rows(q, "SELECT key,body FROM messages WHERE chat=? AND body IS NOT NULL ORDER BY json_extract(body,'$.date') DESC,json_extract(body,'$.message_id') DESC", chat) {
		if count >= 20 || budget == 0 {
			exec(q, "UPDATE messages SET body=NULL WHERE chat=? AND key=?", chat, r["key"])
			continue
		}
		v := decode[ContextText](stringVal(r["body"]))
		if v.Text == "" {
			exec(q, "UPDATE messages SET body=NULL WHERE chat=? AND key=?", chat, r["key"])
			continue
		}
		units := utf16.Encode([]rune(v.Text))
		if len(units) > budget {
			units = units[len(units)-budget:]
			if len(units) > 0 && units[0] >= 0xdc00 && units[0] <= 0xdfff {
				units = units[1:]
			}
			v.Text = string(utf16.Decode(units))
			exec(q, "UPDATE messages SET body=? WHERE chat=? AND key=?", raw(v), chat, r["key"])
		}
		budget -= len(units)
		count++
	}
}
func (b *Bot) clearContact(q queryer, s *State, at int64) {
	exec(q, "UPDATE messages SET body=NULL WHERE chat=?", s.Chat)
	s.Pending = 0
	s.FirstPending = 0
	s.PendingPolicy = 0
	s.Retry = nil
	s.AIError = ""
	s.LastIncoming = 0
	s.Connection = ""
	s.ContextSince = at
	s.Revision++
	if s.Topic != 0 {
		s.Name = ""
	}
	if s.Flight != nil && s.Flight.Phase == "generating" {
		s.Flight = nil
	}
	if cancel := b.aiCancel[s.Chat]; cancel != nil {
		cancel()
	}
}
func (b *Bot) clearAll(q queryer, at int64) {
	b.contextSince = at
	for _, r := range rows(q, "SELECT state FROM contacts") {
		s := decode[State](stringVal(r["state"]))
		b.clearContact(q, &s, at)
		save(q, s)
	}
	// These are forwarding metadata, not model context. Drop completed-message
	// references at explicit cleanup; pending jobs still need their source keys.
	exec(q, "DELETE FROM messages WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE jobs.chat=messages.chat AND jobs.source=messages.key)")
	exec(q, "DELETE FROM versions WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE jobs.chat=versions.chat AND substr(versions.key,1,length(jobs.source)+1)=jobs.source||':')")
	exec(q, "DELETE FROM commands")
	exec(q, "DELETE FROM inbox WHERE done=1")
}
func (b *Bot) maintenance(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	day := at.Format("2006-01-02")
	if b.CleanupMidnight && doc(b.Store.db, "lastCleanupDate", "") != day {
		transaction(b.Store.db, func(q queryer) { b.clearAll(q, at.Unix()); put(q, "lastCleanupDate", day) })
	}
	if at.Unix()-b.lastPrune < 60 {
		return
	}
	b.lastPrune = at.Unix()
	// Bound forwarding-only metadata in RAM even when nightly cleanup is disabled.
	cutoff := at.Add(-24*time.Hour).Unix() * 2
	exec(b.Store.db, "DELETE FROM messages WHERE body IS NULL AND version<? AND NOT EXISTS(SELECT 1 FROM jobs WHERE jobs.chat=messages.chat AND jobs.source=messages.key)", cutoff)
	exec(b.Store.db, "DELETE FROM versions WHERE NOT EXISTS(SELECT 1 FROM messages WHERE messages.chat=versions.chat AND substr(versions.key,1,length(messages.key)+1)=messages.key||':')")
	exec(b.Store.db, "DELETE FROM inbox WHERE done=1 AND id<?", doc(b.Store.db, "offset", int64(0))-1000)
	exec(b.Store.db, "DELETE FROM commands WHERE id<?", doc(b.Store.db, "offset", int64(0))-1000)
}
func (b *Bot) privacyStartup() {
	transaction(b.Store.db, func(q queryer) {
		b.clearAll(q, time.Now().Unix())
		p := policy(q)
		if p.Connection != nil {
			p.Connection.User = User{ID: p.Connection.User.ID}
		}
		if !b.API.configured() {
			p.Paused = true
		}
		put(q, "policy", p)
		for _, r := range rows(q, "SELECT id,body FROM connections") {
			c := decode[Connection](stringVal(r["body"]))
			c.User = User{ID: c.User.ID}
			exec(q, "UPDATE connections SET body=? WHERE id=?", raw(c), r["id"])
		}
		put(q, "lastCleanupDate", time.Now().Format("2006-01-02"))
	})
	// Reclaim old historical pages and truncate the WAL during the upgrade.
	exec(b.Store.db, "PRAGMA wal_checkpoint(TRUNCATE)")
	exec(b.Store.db, "VACUUM")
	exec(b.Store.db, "PRAGMA wal_checkpoint(TRUNCATE)")
}
func (b *Bot) maintenanceLoop(ctx context.Context) {
	defer safePanic()
	defer b.wg.Done()
	for ctx.Err() == nil {
		b.maintenance(time.Now())
		if !wait(ctx, time.Second) {
			return
		}
	}
}

// Only legacy databases owned by this configured account are removed, after
// importing durable settings. External backups are outside this app's control.
func eraseLegacy(dir, owner string) {
	for _, pattern := range []string{"account-" + owner + ".sqlite", "chat-" + owner + "_*.sqlite", "ai-" + owner + "_*.sqlite"} {
		files, e := filepath.Glob(filepath.Join(dir, pattern))
		must(e)
		for _, file := range files {
			base := filepath.Base(file)
			if base != "account-"+owner+".sqlite" {
				peer := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(base, "chat-"+owner+"_"), "ai-"+owner+"_"), ".sqlite")
				if !isDigits(peer) {
					continue
				}
			}
			info, e := os.Lstat(file)
			must(e)
			if !info.Mode().IsRegular() {
				continue
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				e = os.Remove(file + suffix)
				if e != nil && !os.IsNotExist(e) {
					must(e)
				}
			}
		}
	}
}
