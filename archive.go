package main

import (
	"context"
	"fmt"
	"strings"
)

func jobFrom(r map[string]any) Job {
	return Job{ID: integer(r["id"]), Chat: stringVal(r["chat"]), Source: stringVal(r["source"]), Parts: decode[[]Part](stringVal(r["parts"])), Step: int(integer(r["step"])), Attempts: int(integer(r["attempts"])), Status: stringVal(r["status"]), Next: integer(r["next_at"]), Anchor: integer(r["anchor"])}
}
func advance(q queryer, j Job, message int64) {
	status := "pending"
	if j.Step+1 >= len(j.Parts) {
		status = "done"
	}
	if status == "done" {
		exec(q, "DELETE FROM jobs WHERE chat=? AND id=?", j.Chat, j.ID)
	} else {
		j.Parts[j.Step] = Part{} // Release the already forwarded portion immediately.
		exec(q, "UPDATE jobs SET parts=?,status=?,step=step+1,attempts=0,anchor=COALESCE(anchor,?) WHERE chat=? AND id=?", raw(j.Parts), status, message, j.Chat, j.ID)
	}
	if j.Step == 0 {
		exec(q, "UPDATE messages SET anchor=COALESCE(anchor,?) WHERE chat=? AND key=?", message, j.Chat, j.Source)
	}
}
func (b *Bot) ensureTopic(ctx context.Context, chat string) (int64, error) {
	b.mu.Lock()
	s := state(b.Store.db, chat)
	if s.Topic != 0 {
		topic := s.Topic
		b.mu.Unlock()
		return topic, nil
	}
	if s.TopicState != "" {
		s.TopicState = "uncertain"
		save(b.Store.db, s)
		b.mu.Unlock()
		return 0, &APIError{Uncertain: true}
	}
	s.TopicState = "creating"
	save(b.Store.db, s)
	name := clip(s.Name, 110-len(s.Chat)) + " · " + s.Chat
	b.mu.Unlock()
	var result struct {
		Thread int64 `json:"message_thread_id"`
	}
	e := b.API.telegram(ctx, "createForumTopic", Obj{"chat_id": b.Owner, "name": name}, &result)
	b.mu.Lock()
	defer b.mu.Unlock()
	s = state(b.Store.db, chat)
	if e == nil && result.Thread == 0 {
		e = &APIError{Uncertain: true}
	}
	if e != nil {
		s.TopicState = "uncertain"
		if a, ok := e.(*APIError); ok && !a.Uncertain {
			s.TopicState = ""
		}
		save(b.Store.db, s)
		return 0, e
	}
	s.Topic = result.Thread
	s.TopicState = ""
	s.Name = ""
	transaction(b.Store.db, func(q queryer) { save(q, s); exec(q, "INSERT OR REPLACE INTO topics VALUES(?,?)", chat, s.Topic) })
	return s.Topic, nil
}
func (b *Bot) archiveStep(ctx context.Context) {
	b.mu.Lock()
	if doc(b.Store.db, "archiveNext", int64(0)) > now() {
		b.mu.Unlock()
		return
	}
	r := rows(b.Store.db, "SELECT * FROM jobs WHERE status='pending' AND next_at<=? ORDER BY next_at,id LIMIT 1", now())
	if len(r) == 0 {
		b.mu.Unlock()
		return
	}
	j := jobFrom(r[0])
	put(b.Store.db, "archiveNext", now()+3100)
	b.mu.Unlock()
	topic, e := b.ensureTopic(ctx, j.Chat)
	part := j.Parts[j.Step]
	if e == nil {
		b.mu.Lock()
		exec(b.Store.db, "UPDATE jobs SET status='sending' WHERE chat=? AND id=?", j.Chat, j.ID)
		b.mu.Unlock()
		body := Obj{}
		for k, v := range part.Body {
			body[k] = v
		}
		body["chat_id"] = b.Owner
		body["message_thread_id"] = topic
		body["disable_notification"] = true
		var sent Message
		e = b.API.telegram(ctx, part.Method, body, &sent)
		if e == nil && sent.ID == 0 {
			e = &APIError{Uncertain: true}
		}
		if e == nil {
			b.mu.Lock()
			transaction(b.Store.db, func(q queryer) { advance(q, j, sent.ID) })
			b.mu.Unlock()
			return
		}
	}
	a, ok := e.(*APIError)
	if !ok {
		a = &APIError{Uncertain: true}
	}
	status := "pending"
	if a.Uncertain {
		status = "uncertain"
	} else if j.Attempts >= 4 {
		status = "failed"
	}
	delay := max(int64(a.Retry)*1000, min(int64(300000), int64(1000)<<min(j.Attempts+1, 8)))
	description := strings.ToUpper(a.Description)
	b.mu.Lock()
	transaction(b.Store.db, func(q queryer) {
		if !a.Uncertain && strings.Contains(description, "TOPIC_CLOSED") {
			status = "failed"
		} else if !a.Uncertain && (strings.Contains(description, "MESSAGE THREAD NOT FOUND") || strings.Contains(description, "TOPIC_DELETED") || strings.Contains(description, "MESSAGE_THREAD_INVALID")) {
			s := state(q, j.Chat)
			s.Topic = 0
			s.TopicState = ""
			save(q, s)
			delete(j.Parts[j.Step].Body, "reply_parameters")
			notice := Part{Method: "sendMessage", Body: Obj{"text": "⚠️ 原话题已删除，以下内容继续保存到新话题；旧副本无法恢复。"}}
			j.Parts = append(j.Parts[:j.Step], append([]Part{notice}, j.Parts[j.Step:]...)...)
		} else if !a.Uncertain && a.Code == 400 && part.Media {
			j.Parts[j.Step] = Part{Method: "sendMessage", Body: Obj{"text": clip(fmt.Sprintf("⚠️ 媒体未备份：Telegram 拒绝重发。源 %s，类型 %s。\n说明：%v", j.Source, part.Method, part.Body["caption"]), 4000)}}
			status = "pending"
		}
		exec(q, "UPDATE jobs SET parts=?,status=?,attempts=attempts+1,next_at=? WHERE chat=? AND id=?", raw(j.Parts), status, now()+delay, j.Chat, j.ID)
		if a.Retry > 0 {
			put(q, "archiveNext", max(doc(q, "archiveNext", int64(0)), now()+int64(a.Retry)*1000))
		}
	})
	b.mu.Unlock()
	b.notify(ctx)
}
