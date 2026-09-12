package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const help = "通过 BotFather 开启 Secretary Mode 和私聊话题，在 Telegram 聊天自动化连接本 Bot。归档在你与 Bot 私聊的联系人话题中。\n/status all|用户ID\n/pause all|用户ID\n/resume all|用户ID\n/limit N all|用户ID\n/reset 用户ID\n/prompt 文本（也可回复文字执行）\n/prompt_show /prompt_reset\n/retry（联系人话题内）\n话题内可省略目标，其他位置必须指定。初始 AI 暂停，默认每人累计 10 条；原私聊人工回复自动暂停该联系人。话题备注不会代发。"

var commandPattern = regexp.MustCompile(`(?s)^/([a-z_]+)(?:@([A-Za-z0-9_]+))?(?:\s+(.*))?$`)

func (b *Bot) command(q queryer, id int64, m Message) string {
	if !m.owner(b.Owner) || m.SenderChat != nil || m.Chat.Type != "private" || m.Chat.ID != b.Owner {
		return ""
	}
	match := commandPattern.FindStringSubmatch(m.Text)
	if match == nil || match[2] != "" && !strings.EqualFold(match[2], b.Username) {
		return ""
	}
	if len(rows(q, "SELECT id FROM commands WHERE id=?", id)) > 0 {
		return ""
	}
	answer := b.execute(q, match[1], strings.TrimSpace(match[3]), m)
	exec(q, "INSERT INTO commands VALUES(?,?)", id, answer)
	return answer
}
func (b *Bot) execute(q queryer, cmd, args string, m Message) string {
	p := policy(q)
	switch cmd {
	case "start", "help":
		return help
	case "prompt_show":
		return p.Prompt
	case "prompt", "prompt_reset":
		text := args
		if cmd == "prompt_reset" {
			text = defaultPrompt
		} else if text == "" && m.Reply != nil {
			text = m.Reply.Text
		}
		if text == "" || len([]rune(text)) > 12000 {
			return "请提供 1–12000 字符的提示词。"
		}
		p.Prompt = text
		p.Version++
		put(q, "policy", p)
		return "提示词已更新，旧生成结果失效。"
	}
	target := args
	limit := -1
	parts := strings.Fields(args)
	if cmd == "limit" {
		if len(parts) == 0 || len(parts) > 2 {
			return "用法：/limit N [all|用户ID]"
		}
		n, e := strconv.Atoi(parts[0])
		if e != nil || n < 0 || n > 1000000 {
			return "额度应为 0–1000000 整数。"
		}
		limit = n
		target = ""
		if len(parts) == 2 {
			target = parts[1]
		}
	}
	if cmd == "topic_bind" {
		if !isDigits(args) || m.Thread == 0 {
			return "在正确的联系人话题执行 /topic_bind 用户ID。"
		}
		s := state(q, args)
		if len(rows(q, "SELECT chat FROM contacts WHERE chat=?", args)) == 0 {
			return "没有该联系人记录。"
		}
		if s.TopicState == "creating" {
			return "话题仍在创建，稍后再试。"
		}
		r := rows(q, "SELECT chat FROM topics WHERE topic=?", m.Thread)
		if len(r) > 0 && stringVal(r[0]["chat"]) != args {
			return "话题已属于其他联系人。"
		}
		s.Topic = m.Thread
		s.TopicState = ""
		save(q, s)
		exec(q, "INSERT OR REPLACE INTO topics VALUES(?,?)", args, m.Thread)
		return "话题已绑定，可 /retry；待核对任务用 /archive_resolve。"
	}
	if cmd == "archive_resolve" {
		target = ""
	}
	if target == "" && m.Thread != 0 {
		r := rows(q, "SELECT chat FROM topics WHERE topic=?", m.Thread)
		if len(r) > 0 {
			target = stringVal(r[0]["chat"])
		}
	}
	if target == "all" {
		switch cmd {
		case "status":
			contacts := rows(q, "SELECT chat FROM contacts ORDER BY chat")
			ids := []string{}
			for _, r := range contacts {
				ids = append(ids, stringVal(r["chat"]))
			}
			connection := "未连接/停用"
			if p.Connection != nil && p.Connection.Enabled {
				connection = "已启用"
			}
			return fmt.Sprintf("连接：%s\n全局暂停：%t\n默认额度：%d\nAI 配置：%t\n联系人：%s", connection, p.Paused, p.Limit, b.API.configured(), strings.Join(ids, ", "))
		case "pause":
			p.Paused = true
		case "resume":
			if !b.API.configured() {
				return "请先配置 HTTPS AI_BASE_URL、AI_MODEL、AI_API_KEY。"
			}
			p.Paused = false
		case "limit":
			p.Limit = limit
		default:
			return "该命令不支持 all。"
		}
		p.Version++
		put(q, "policy", p)
		return "全局设置已更新；联系人暂停与计数保持。"
	}
	if !isDigits(target) {
		return "请指定 all 或用户 ID；联系人话题内可省略目标。"
	}
	s := state(q, target)
	switch cmd {
	case "status":
		counts := rows(q, "SELECT status,COUNT(*) AS n FROM jobs WHERE chat=? GROUP BY status", target)
		issues := rows(q, "SELECT id,step,status FROM jobs WHERE chat=? AND status IN ('failed','uncertain') ORDER BY id LIMIT 15", target)
		quota := p.Limit
		if s.Limit != nil {
			quota = *s.Limit
		}
		phase := s.AIError
		if s.Flight != nil {
			phase = s.Flight.Phase
		}
		return fmt.Sprintf("联系人：%s (%s)\n全局暂停：%t；联系人暂停：%t\n额度（含待核对）：%d/%d\n话题：%d %s\nAI：%s\n任务：%v\n异常任务：%v", s.Name, target, p.Paused, s.Paused, s.Used, quota, s.Topic, s.TopicState, phase, counts, issues)
	case "pause":
		s.Paused = true
		s.Pending = 0
		s.FirstPending = 0
	case "resume":
		s.Paused = false
	case "limit":
		s.Limit = &limit
	case "reset":
		s.Used = 0
		s.Epoch++
		if s.Flight != nil && s.Flight.Phase != "sending" {
			s.Flight = nil
		}
		s.Retry = nil
	case "retry":
		if m.Thread == 0 {
			return "仅可在联系人话题内 /retry。"
		}
		exec(q, "UPDATE jobs SET status='pending',attempts=0,next_at=0 WHERE chat=? AND status='failed'", target)
	case "archive_resolve":
		if m.Thread == 0 || len(parts) < 2 || len(parts) > 3 || !isDigits(parts[0]) {
			return "用法：/archive_resolve 任务ID sent 归档消息ID，或 /archive_resolve 任务ID retry"
		}
		r := rows(q, "SELECT * FROM jobs WHERE chat=? AND id=? AND status='uncertain'", target, num(parts[0]))
		if len(r) == 0 {
			return "未找到待核对任务。"
		}
		job := jobFrom(r[0])
		if parts[1] == "retry" && len(parts) == 2 {
			exec(q, "UPDATE jobs SET status='pending',attempts=0,next_at=0 WHERE chat=? AND id=?", target, job.ID)
		} else if parts[1] == "sent" && len(parts) == 3 && num(parts[2]) > 0 {
			advance(q, job, num(parts[2]))
		} else {
			return "参数无效。"
		}
		return "已按核对结果更新归档任务。"
	default:
		return help
	}
	s.Revision++
	save(q, s)
	return "联系人设置已更新。恢复不清零，清零不解除暂停；新文字可触发 AI。"
}
