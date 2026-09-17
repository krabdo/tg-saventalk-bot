package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"unicode/utf16"
)

func eligible(s State, p Policy) bool {
	limit := p.Limit
	if s.Limit != nil {
		limit = *s.Limit
	}
	return !s.Paused && !p.Paused && s.Used < limit && p.Connection != nil && p.Connection.Enabled && p.Connection.Rights.Reply && p.Connection.ID == s.Connection && s.LastIncoming > 0 && now()-s.LastIncoming < 86400000
}
func (b *Bot) prepare(chat string) *Ticket {
	p := policy(b.Store.db)
	s := state(b.Store.db, chat)
	if s.Flight != nil || s.Pending == 0 || s.Pending > now() {
		return nil
	}
	s.Pending = 0
	s.FirstPending = 0
	if !eligible(s, p) || s.PendingPolicy != p.Version {
		save(b.Store.db, s)
		return nil
	}
	messages := []Text{}
	budget := 24000
	for _, r := range rows(b.Store.db, "SELECT body FROM messages WHERE chat=? AND deleted=0 AND body IS NOT NULL AND json_extract(body,'$.text') IS NOT NULL ORDER BY json_extract(body,'$.date') DESC,json_extract(body,'$.message_id') DESC LIMIT 20", chat) {
		m := decode[ContextText](stringVal(r["body"]))
		if m.Text == "" {
			continue
		}
		text := utf16.Encode([]rune(m.Text))
		if len(text) > budget {
			text = text[len(text)-budget:]
			if len(text) > 0 && text[0] >= 0xDC00 && text[0] <= 0xDFFF {
				text = text[1:]
			}
		}
		budget -= len(text)
		role := m.Role
		messages = append(messages, Text{role, string(utf16.Decode(text))})
		if budget == 0 {
			break
		}
	}
	if len(messages) == 0 {
		save(b.Store.db, s)
		return nil
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	var token [16]byte
	_, e := rand.Read(token[:])
	must(e)
	t := Ticket{Token: hex.EncodeToString(token[:]), Revision: s.Revision, PolicyVersion: p.Version, Connection: s.Connection, Messages: append([]Text{{"system", p.Prompt}}, messages...)}
	s.Flight = &Flight{Token: t.Token, Phase: "generating", Revision: s.Revision, Epoch: s.Epoch}
	s.AIError = ""
	save(b.Store.db, s)
	return &t
}
func (b *Bot) launchAI(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.busy) >= 4 {
		return
	}
	for _, r := range rows(b.Store.db, "SELECT chat,state FROM contacts WHERE json_extract(state,'$.pendingAt')>0 OR json_extract(state,'$.retry.At')>0") {
		chat := stringVal(r["chat"])
		if b.busy[chat] {
			continue
		}
		s := decode[State](stringVal(r["state"]))
		var t *Ticket
		var text string
		attempt := 0
		if s.Retry != nil {
			if s.Retry.At > now() {
				continue
			}
			t = &s.Retry.Ticket
			text = s.Retry.Text
			attempt = s.Retry.Attempts
		} else {
			t = b.prepare(chat)
		}
		if t == nil {
			continue
		}
		b.busy[chat] = true
		if b.aiCancel == nil {
			b.aiCancel = map[string]context.CancelFunc{}
		}
		requestCtx, cancel := context.WithCancel(ctx)
		b.aiCancel[chat] = cancel
		b.wg.Add(1)
		go func(ctx context.Context, chat string, t Ticket, text string, attempt int) {
			defer safePanic()
			defer b.wg.Done()
			defer func() { cancel(); b.mu.Lock(); delete(b.busy, chat); delete(b.aiCancel, chat); b.mu.Unlock() }()
			if text == "" {
				var e error
				text, e = b.API.complete(ctx, t.Messages)
				if e != nil {
					b.mu.Lock()
					s := state(b.Store.db, chat)
					if s.Flight != nil && s.Flight.Token == t.Token {
						s.Flight = nil
						s.Retry = nil
						s.AIError = "模型失败或超时，等待新文字"
						save(b.Store.db, s)
					}
					b.mu.Unlock()
					b.notify(ctx)
					return
				}
			}
			b.finishAI(ctx, chat, t, text, attempt)
		}(requestCtx, chat, *t, text, attempt)
		if len(b.busy) >= 4 {
			return
		}
	}
}
func (b *Bot) finishAI(ctx context.Context, chat string, t Ticket, text string, attempt int) {
	b.mu.Lock()
	s := state(b.Store.db, chat)
	p := policy(b.Store.db)
	if s.Flight == nil || s.Flight.Token != t.Token || s.Flight.Phase != "generating" {
		b.mu.Unlock()
		return
	}
	if !eligible(s, p) || s.Revision != t.Revision || p.Version != t.PolicyVersion || p.Connection.ID != t.Connection {
		s.Flight = nil
		s.Retry = nil
		save(b.Store.db, s)
		b.mu.Unlock()
		return
	}
	s.Used++
	s.Flight.Phase = "sending"
	s.Retry = nil
	save(b.Store.db, s)
	b.mu.Unlock()
	// Recheck after the durable send reservation and immediately before submission.
	b.mu.Lock()
	s = state(b.Store.db, chat)
	p = policy(b.Store.db)
	check := s
	check.Used = max(0, check.Used-1)
	if s.Flight == nil || s.Flight.Token != t.Token {
		b.mu.Unlock()
		return
	}
	if ctx.Err() != nil || !eligible(check, p) || s.Revision != t.Revision || p.Version != t.PolicyVersion {
		if s.Flight.Epoch == s.Epoch {
			s.Used = max(0, s.Used-1)
		}
		s.Flight = nil
		save(b.Store.db, s)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	var sent Message
	e := b.API.telegram(ctx, "sendMessage", Obj{"business_connection_id": t.Connection, "chat_id": num(chat), "text": clip(text, 4000)}, &sent)
	if e == nil && sent.ID == 0 {
		e = &APIError{Uncertain: true}
	}
	b.mu.Lock()
	transaction(b.Store.db, func(q queryer) {
		s = state(q, chat)
		if e == nil {
			sent.Connection = t.Connection
			if sent.SenderBot == nil {
				sent.SenderBot = &User{First: "AI", Bot: true}
			}
			if sent.From == nil {
				sent.From = &User{ID: b.Owner, First: "我"}
			}
			p := policy(q)
			if s.Revision != t.Revision || p.Version != t.PolicyVersion {
				p.Paused = true
			}
			b.record(q, &s, sent, 0, false, p)
			if s.Flight != nil && s.Flight.Token == t.Token {
				s.Flight = nil
			}
			s.Retry = nil
			save(q, s)
			return
		}
		if s.Flight == nil || s.Flight.Token != t.Token {
			return
		}
		if a, ok := e.(*APIError); ok && !a.Uncertain {
			if s.Flight.Epoch == s.Epoch {
				s.Used = max(0, s.Used-1)
			}
			if a.Code == 429 && attempt < 4 && s.Revision == t.Revision && policy(q).Version == t.PolicyVersion && contextEnabled(s, policy(q)) {
				s.Flight.Phase = "generating"
				s.Retry = &Retry{Ticket: t, Text: text, Attempts: attempt + 1, At: now() + int64(max(1, a.Retry))*1000}
				s.AIError = "Telegram 限流，等待重试"
			} else {
				s.Flight = nil
				s.AIError = "发送明确失败，未扣额度；等待新文字"
			}
		} else {
			s.Flight.Phase = "uncertain"
			s.AIError = "发送结果不确定，请核对后 /reset"
		}
		save(q, s)
	})
	b.mu.Unlock()
	if e != nil {
		b.notify(ctx)
	}
}
