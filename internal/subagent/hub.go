package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// hub wait 的时间窗：默认 30 秒，最长 120 秒。等待只是「让出一次模型调用」，
// 不该变成长时间挂起——真正的长任务靠后台作业自动投递，不靠轮询。
const (
	hubWaitDefault = 30 * time.Second
	hubWaitMax     = 120 * time.Second
	hubWaitPoll    = 250 * time.Millisecond
)

// hubTool 是 peer 协调工具：看名册、发消息、收消息、等事件、看作业、取消作业。
// 主 agent 的地址固定是 Main，子 agent 用自己的运行名。
type hubTool struct {
	mgr  *Manager
	self string
}

// NewHubTool 构造 hub 工具；self 是调用者在名册里的地址。
func NewHubTool(mgr *Manager, self string) tool.Tool { return hubTool{mgr: mgr, self: self} }

func (hubTool) Name() string { return "hub" }

func (h hubTool) Description() string {
	return "与其它 agent 协调：看名册、发消息、收消息、等事件、查/取消后台作业。你的地址是 " + h.self + "；主 agent 是 Main。\n" +
		"- list：列出可寻址的 peer（名字、agent、状态、当前工具）。只能用名册里的确切名字，不要自己编。\n" +
		"- send：给某个 peer 发一句话（to + text）。不阻塞；对方在跑就作为提示注入它的下一步，已结束就把它唤醒续跑。\n" +
		"- inbox：把发给你的消息一次性取走（不阻塞）。\n" +
		"- wait：完全没事可做时才用（可选 timeout 秒、ids 指定关注的作业）；一有消息或作业结束就返回。\n" +
		"- jobs：后台作业快照；已结束的作业结果会随这次调用一起给你（之后不会再单独送一遍）。\n" +
		"- cancel：按 ids 取消后台作业。\n" +
		"只用来协调，不要传长内容：文件用路径，子 agent 产出用 agent://<名字>，被截断的输出用 artifact://<id>。"
}

func (hubTool) Parameters() map[string]any {
	return map[string]any{
		"op": map[string]any{
			"type": "string", "enum": []string{"list", "send", "inbox", "wait", "jobs", "cancel"},
			"description": "要做的操作",
		},
		"to":      map[string]any{"type": "string", "description": "send 的收件人（名册里的确切名字，主 agent 是 Main）"},
		"text":    map[string]any{"type": "string", "description": "send 的正文：一句人话，别放 JSON 或大段内容"},
		"replyTo": map[string]any{"type": "string", "description": "可选：回复谁的消息"},
		"ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "cancel/wait 关注的作业 id（= 运行名）"},
		"timeout": map[string]any{"type": "integer", "description": "wait 的秒数，默认 30，最长 120"},
	}
}

func (hubTool) Required() []string            { return []string{"op"} }
func (hubTool) Tier() permission.Tier         { return permission.TierRead }
func (hubTool) Concurrency() tool.Concurrency { return tool.ConcurrencyShared }

func (h hubTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	op := strings.TrimSpace(argString(args, "op"))
	switch op {
	case "list":
		sink.Write([]byte(h.renderRoster()))
		return nil
	case "send":
		to, text := strings.TrimSpace(argString(args, "to")), strings.TrimSpace(argString(args, "text"))
		if to == "" || text == "" {
			return fmt.Errorf("send 需要 to 与 text")
		}
		res, err := h.mgr.Deliver(h.self, to, text, strings.TrimSpace(argString(args, "replyTo")))
		if err != nil {
			return err
		}
		sink.Write([]byte(res))
		return nil
	case "inbox":
		sink.Write([]byte(renderMails(h.mgr.box(h.self).drain())))
		return nil
	case "wait":
		sink.Write([]byte(h.wait(ctx, stringsOf(args["ids"]), waitDuration(args))))
		return nil
	case "jobs":
		sink.Write([]byte(renderJobs(h.mgr.Jobs())))
		return nil
	case "cancel":
		ids := stringsOf(args["ids"])
		if len(ids) == 0 {
			return fmt.Errorf("cancel 需要 ids")
		}
		fmt.Fprintf(sink, "已取消 %d 个作业", h.mgr.Cancel(ids))
		return nil
	}
	return fmt.Errorf("未知 op %q，可用：list/send/inbox/wait/jobs/cancel", op)
}

// wait 阻塞到「第一个事件」：收到消息 / 关注的作业结束 / 超时 / 被取消。
func (h hubTool) wait(ctx context.Context, ids []string, d time.Duration) string {
	box := h.mgr.box(h.self)
	deadline := time.After(d)
	ticker := time.NewTicker(hubWaitPoll)
	defer ticker.Stop()
	for {
		if mails := box.drain(); len(mails) > 0 {
			return "收到消息：\n" + renderMails(mails)
		}
		if done := h.settledAmong(ids); done != "" {
			return done
		}
		select {
		case <-box.waitCh():
		case <-ticker.C:
		case <-deadline:
			return fmt.Sprintf("等待 %s 内没有新消息或作业结束；继续做别的事，别空等。", d)
		case <-ctx.Done():
			return "等待被取消。"
		}
	}
}

// settledAmong 检查关注的作业（ids 为空 = 全部后台作业）是否有已结束的；有就消费投递并返回结果。
func (h hubTool) settledAmong(ids []string) string {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var keep, hit []JobResult
	for _, s := range h.mgr.TakeSettled() {
		if len(want) == 0 || want[s.JobID] {
			hit = append(hit, s)
		} else {
			keep = append(keep, s)
		}
	}
	if len(keep) > 0 {
		h.mgr.putBack(keep) // 不关注的作业留给正常投递通道
	}
	if len(hit) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("作业已结束：\n")
	for _, s := range hit {
		sb.WriteString(renderResult(s.Result))
		sb.WriteString("\n")
	}
	return sb.String()
}

func waitDuration(args map[string]any) time.Duration {
	d := hubWaitDefault
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		d = time.Duration(v) * time.Second
	}
	if d > hubWaitMax {
		d = hubWaitMax
	}
	return d
}

func (h hubTool) renderRoster() string {
	rows := h.mgr.Roster()
	var sb strings.Builder
	sb.WriteString("可寻址的 peer：\n")
	n := 0
	if h.self != MainName {
		sb.WriteString("- Main（主 agent，派你来的那个）\n")
		n++
	}
	for _, r := range rows {
		if r.Name == h.self {
			continue
		}
		activity := r.CurrentTool
		if activity == "" {
			activity = "-"
		}
		fmt.Fprintf(&sb, "- %s（%s，%s，当前工具 %s，requests=%d）\n", r.Name, r.Agent, r.Status, activity, r.Requests)
		n++
	}
	if n == 0 {
		return "当前没有其它 peer。"
	}
	return sb.String()
}

func renderMails(mails []Mail) string {
	if len(mails) == 0 {
		return "收件箱为空。"
	}
	var sb strings.Builder
	for _, m := range mails {
		fmt.Fprintf(&sb, "- 来自 %s", m.From)
		if m.ReplyTo != "" {
			fmt.Fprintf(&sb, "（回复 %s）", m.ReplyTo)
		}
		fmt.Fprintf(&sb, "：%s\n", m.Text)
	}
	return sb.String()
}

func renderJobs(jobs []JobInfo) string {
	if len(jobs) == 0 {
		return "没有后台作业。"
	}
	var sb strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&sb, "- %s（%s）[%s]", j.ID, j.Agent, j.Status)
		if !j.Settled.IsZero() {
			fmt.Fprintf(&sb, " 用时 %s", j.Settled.Sub(j.Started).Round(time.Second))
		}
		sb.WriteString("\n")
		if j.Summary != "" {
			sb.WriteString(j.Summary)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
