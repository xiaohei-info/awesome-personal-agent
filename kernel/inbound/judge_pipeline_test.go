package inbound

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

type spyClassifier struct {
	mu      sync.Mutex
	batches [][]model.Question
	fn      func(qs []model.Question) model.JudgeResult
}

func (c *spyClassifier) Judge(_ context.Context, _ policy.JudgeEndpoint, qs []model.Question, _ model.JudgeOptions) (model.JudgeResult, error) {
	c.mu.Lock()
	cp := append([]model.Question(nil), qs...)
	c.batches = append(c.batches, cp)
	c.mu.Unlock()
	return c.fn(qs), nil
}

type spyChat struct {
	mu    sync.Mutex
	calls []model.Request
	fn    func(req model.Request) (model.Response, error)
}

func (c *spyChat) Complete(_ context.Context, _ policy.GenerateEndpoint, req model.Request) (model.Response, error) {
	c.mu.Lock()
	c.calls = append(c.calls, req)
	c.mu.Unlock()
	return c.fn(req)
}

func setupUnifiedJudgeEnv(t *testing.T, cls *spyClassifier, chat *spyChat, extraPackYAML string, judgePrompt string) *env {
	t.Helper()
	e := newEnv(t, false)
	e.noAutoBatch = true
	// 用真实的内置 inbound-triage 包，外加可选自定义包（如 chat-patrol）
	userDir := ""
	if extraPackYAML != "" {
		userDir = t.TempDir()
		cpDir := filepath.Join(userDir, "chat-patrol")
		if err := os.MkdirAll(cpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cpDir, "pack.yaml"), []byte(extraPackYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.loadr = packs.New(packs.Options{Builtin: rootpacks.FS, DataDir: userDir, Kinds: items.NewKinds()})
	if rej, err := e.loadr.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load packs: %v %+v", err, rej)
	}
	e.svc.o.Packs = e.loadr
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test endpoints", func(p *policy.Policy) error {
			if cls != nil {
				p.JudgeEndpoint = &policy.JudgeEndpoint{URL: "http://judge.local", Protocol: policy.ProtocolProbability, Locality: policy.LocalityLocal}
			}
			if chat != nil {
				p.GenerateEndpoint = policy.GenerateEndpoint{URL: "http://chat.local", Model: "m", Locality: policy.LocalityLocal}
			}
			p.SourcePipeline.JudgePrompt = judgePrompt
			p.SourcePipeline.OwnerIdentity = "昵称阿江"
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ds, err := direct.New(direct.Options{
		Store:      e.st,
		Packs:      e.loadr,
		Model:      chat,
		Classifier: cls,
		Now:        func() time.Time { return e.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.o.Judge = ds
	return e
}

func ingestOne(t *testing.T, e *env, srcID, sourceType, evID, evType, subject string) string {
	t.Helper()
	if !IsCaptureSource(srcID) {
		var exists bool
		_ = e.st.View(bg, func(tx *store.Tx) error {
			_, err := registry.Get(tx, srcID)
			exists = err == nil
			return nil
		})
		if !exists {
			e.must(registry.CmdRegisterExec, map[string]any{"id": srcID, "type": "script", "source_type": sourceType, "command": "/bin/true",
				"schedule": "1m", "timeout": 30})
		}
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified, ingested_at) VALUES (?, ?, ?, ?, 'r', ?, ?, 'sum', 'body', '', ?, '[]', 'alice@example.com', 1, ?)`,
			srcID, evID, evID, srcID, evType, subject, e.now.UnixNano(), e.now.UnixNano())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return evID
}

// B2 验收 1：一步两题一次分类端点请求；两题各自未定时的保守结局（第一题未定→进队列，第二题未定→普通）；
// 第一题判否→忽略且不追问对话端点第二题；自定义判断标准附在判据里。
func TestUnifiedJudgeTwoQuestionsAndUndecidedDefaults(t *testing.T) {
	cls := &spyClassifier{fn: func(qs []model.Question) model.JudgeResult {
		ans := make([]model.Answer, len(qs))
		for i, q := range qs {
			in := string(q.Input)
			switch {
			case strings.Contains(in, "urgent-mail"):
				ans[i] = model.Answer{ID: q.ID, PYes: 0.90, Source: "classifier"} // 两题都高分 -> urgent
			case strings.Contains(in, "ignore-mail"):
				if strings.HasPrefix(q.Question, "这条来源事件需要今天之内") {
					ans[i] = model.Answer{ID: q.ID, PYes: 0.40, Source: "classifier"} // 落在不确定带，但 Q1 为否，不应触发 chat
				} else {
					ans[i] = model.Answer{ID: q.ID, PYes: 0.05, Source: "classifier"}
				}
			case strings.Contains(in, "both-undecided"):
				ans[i] = model.Answer{ID: q.ID, PYes: 0.35, Source: "classifier"} // 两题都落不确定带 -> 交 chat
			case strings.Contains(in, "q2-undecided"):
				if strings.HasPrefix(q.Question, "这条来源事件需要今天之内") {
					ans[i] = model.Answer{ID: q.ID, PYes: 0.35, Source: "classifier"} // Q2 不确定 -> 交 chat
				} else {
					ans[i] = model.Answer{ID: q.ID, PYes: 0.90, Source: "classifier"} // Q1 直接通过
				}
			}
		}
		return model.JudgeResult{Answers: ans, Healthy: true}
	}}
	chat := &spyChat{fn: func(req model.Request) (model.Response, error) {
		// 对话端点一律自报资料不足 -> 触发各自保守默认（Q1: yes 进队列；Q2: no 不急）
		raw, _ := json.Marshal(map[string]any{"answer": "no", "sufficient": false})
		return model.Response{Content: string(raw)}, nil
	}}
	e := setupUnifiedJudgeEnv(t, cls, chat, "", "发票和报销单都算需要处理")

	ingestOne(t, e, "src_mail", "mail", "ev_urgent", "mail.received", "urgent-mail")
	ingestOne(t, e, "src_mail", "mail", "ev_ignore", "mail.received", "ignore-mail")
	ingestOne(t, e, "src_mail", "mail", "ev_both_u", "mail.received", "both-undecided")
	ingestOne(t, e, "src_mail", "mail", "ev_q2_u", "mail.received", "q2-undecided")

	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}

	// 验证 4 条事件共 8 道题合并在一次分类端点请求里发完，且自定义判断标准附在 criteria 里。
	if len(cls.batches) != 1 || len(cls.batches[0]) != 8 {
		t.Fatalf("expected 1 batched classifier request with 8 questions (4 events x 2), got %d batches (%v)", len(cls.batches), len(cls.batches[0]))
	}
	for i, q := range cls.batches[0] {
		// 本人改过的判断标准整段代替第一题的内置判据；第二题用自己的判据。题面里的安全约定不受影响。
		urgent := strings.HasPrefix(q.Question, "这条来源事件需要今天之内")
		if strings.Contains(q.Criteria, "昵称阿江") == false {
			t.Fatalf("question %d: owner identity is shared by both questions: %q", i, q.Criteria)
		}
		if strings.HasPrefix(q.Criteria, "发票和报销单都算需要处理") == urgent || !strings.Contains(q.Question, "安全约定") ||
			urgent && !strings.Contains(q.Criteria, "已经恢复的告警") {
			t.Fatalf("question %d: judge_prompt applies to Q1 only: %q / %q", i, q.Criteria, q.Question)
		}
	}
	// ev_ignore 的 Q1 判否，不得再问 chat（chat 只被 both-undecided 的 Q1/Q2 与 q2-undecided 的 Q2 调了 3 次）。
	if len(chat.calls) != 3 {
		t.Fatalf("expected 3 chat fallback calls (none for ignored event), got %d", len(chat.calls))
	}

	check := func(id string, wantDisp domain.Disposition, wantQueue, wantLevel string) {
		t.Helper()
		var ev domain.SourceEvent
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, id); return })
		if ev.Disposition != wantDisp || ev.Queue != wantQueue || ev.QueueLevel != wantLevel {
			t.Fatalf("%s: got disp=%s queue=%s level=%s, want disp=%s queue=%s level=%s",
				id, ev.Disposition, ev.Queue, ev.QueueLevel, wantDisp, wantQueue, wantLevel)
		}
	}
	check("ev_urgent", domain.DispositionPending, runner.QueueQueued, runner.LevelUrgent)
	check("ev_ignore", domain.DispositionIgnored, "", "")
	check("ev_both_u", domain.DispositionPending, runner.QueueQueued, runner.LevelNormal)
	check("ev_q2_u", domain.DispositionPending, runner.QueueQueued, runner.LevelNormal)
}

// B2 验收 2：规则直通（then: proposed，如日历、提醒）跳过第一题，只问第二题；没有模型端点时也照常以 normal 进队列。
func TestRulePassthroughSkipsFirstQuestion(t *testing.T) {
	cls := &spyClassifier{fn: func(qs []model.Question) model.JudgeResult {
		ans := make([]model.Answer, len(qs))
		for i, q := range qs {
			ans[i] = model.Answer{ID: q.ID, PYes: 0.90, Source: "classifier"}
		}
		return model.JudgeResult{Answers: ans, Healthy: true}
	}}
	e := setupUnifiedJudgeEnv(t, cls, nil, "", "")
	ingestOne(t, e, "src_cal", "calendar", "ev_cal", "event.created", "下午评审会")
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if len(cls.batches) != 1 || len(cls.batches[0]) != 1 || !strings.Contains(cls.batches[0][0].Question, "今天之内") {
		t.Fatalf("passthrough must skip Q1 and only ask Q2, got batches=%+v", cls.batches)
	}
	var ev domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, "ev_cal"); return })
	if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || ev.QueueLevel != runner.LevelUrgent {
		t.Fatalf("calendar passthrough: %+v", ev)
	}

	// 无模型端点时规则直通仍按 normal 进队列，不落 unjudged。
	e2 := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	ingestOne(t, e2, "src_rem", "reminder", "ev_rem", "reminder.due", "续保")
	if err := e2.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	_ = e2.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, "ev_rem"); return })
	if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || ev.QueueLevel != runner.LevelNormal {
		t.Fatalf("passthrough without model: %+v", ev)
	}
}

// B2 验收 3：对话捕获、聊天（then: process）、邮件全部走同一段统一 judge 并进队列。
func TestCaptureChatAndMailShareUnifiedJudge(t *testing.T) {
	chatPatrolYAML := `name: chat-patrol
version: 1.0.0
description: chat patrol unified process rule test
source_rules:
  - id: chat-mention
    match: {source_type: chat, event_type: [chat.mention]}
    then: process
  - id: chat-topic
    match: {source_type: chat, event_type: [chat.topic]}
    then: ignore
`
	cls := &spyClassifier{fn: func(qs []model.Question) model.JudgeResult {
		ans := make([]model.Answer, len(qs))
		for i, q := range qs {
			p := 0.85
			if strings.HasPrefix(q.Question, "这条来源事件需要今天之内") {
				p = 0.05
			}
			ans[i] = model.Answer{ID: q.ID, PYes: p, Source: "classifier"}
		}
		return model.JudgeResult{Answers: ans, Healthy: true}
	}}
	e := setupUnifiedJudgeEnv(t, cls, nil, chatPatrolYAML, "")
	if _, ok := e.loadr.Get("chat-patrol"); !ok {
		t.Fatal("chat-patrol pack failed to load")
	}
	// 1) 邮件
	ingestOne(t, e, "src_mail", "mail", "ev_mail", "mail.received", "合同审批")
	// 2) 聊天（命中 chat-patrol 的 then: extract 与 then: ignore）
	ingestOne(t, e, "src_chat", "chat", "ev_chat_m", "chat.mention", "@我 明天同步")
	ingestOne(t, e, "src_chat", "chat", "ev_chat_t", "chat.topic", "纯主题群")
	// 3) 对话捕获（capture:<project> 内部来源）
	ingestOne(t, e, capturePrefix+e.mail, "capture", "ev_cap", "conversation.delta", "对话里托办的事")

	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	// 3 条走统一 judge（邮件、聊天 mention、对话捕获）共 6 题在一次批量请求里完成；chat.topic 被规则直接 ignore 不调模型。
	if len(cls.batches) != 1 || len(cls.batches[0]) != 6 {
		t.Fatalf("expected 1 unified judge batch with 6 questions (mail, chat, capture x 2), got %d batches (%v)", len(cls.batches), cls.batches)
	}
	for _, id := range []string{"ev_mail", "ev_chat_m", "ev_cap"} {
		var ev domain.SourceEvent
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, id); return })
		if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || ev.QueueLevel != runner.LevelNormal {
			t.Fatalf("%s should be queued via unified judge, got disp=%s queue=%s level=%s", id, ev.Disposition, ev.Queue, ev.QueueLevel)
		}
	}
	var topic domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { topic, err = runner.Event(tx, "ev_chat_t"); return })
	if topic.Disposition != domain.DispositionIgnored {
		t.Fatalf("chat.topic should still be ignored by rule, got %s", topic.Disposition)
	}
}

// 两题的输入带本机当前时间；邮件的收件信头只随全文资料送出；急不急一题放行上限 0.80，0.62 不再直接判急。
func TestJudgeInputAndUrgentBand(t *testing.T) {
	cls := &spyClassifier{fn: func(qs []model.Question) model.JudgeResult {
		ans := make([]model.Answer, len(qs))
		for i, q := range qs {
			p := 0.90
			if strings.HasPrefix(q.Question, "这条来源事件需要今天之内") {
				p = 0.62
			}
			ans[i] = model.Answer{ID: q.ID, PYes: p, Source: "classifier"}
		}
		return model.JudgeResult{Answers: ans, Healthy: true}
	}}
	e := setupUnifiedJudgeEnv(t, cls, nil, "", "")
	ingestOne(t, e, "src_mail", "mail", "ev_to", "mail.received", "周会纪要")
	headers := `{"To":"me@example.test","Cc":["a@example.test","b@example.test"],"List-Id":"<team.example.test>","Received":"hop"}`
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES ('ev_to', 'headers', 'application/json', ?, '', ?)`,
			len(headers), []byte(headers))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if len(cls.batches) != 1 || len(cls.batches[0]) != 2 {
		t.Fatalf("batches %+v", cls.batches)
	}
	for _, q := range cls.batches[0] {
		var in map[string]any
		_ = json.Unmarshal(q.Input, &in)
		now := e.now.In(time.Local).Format("2006-01-02T15:00-07:00 Monday")
		if in["now"] != now || in["stats"] != nil {
			t.Fatalf("input %s, want now %s and no stats", q.Input, now)
		}
		if !strings.HasPrefix(q.Material, "to: me@example.test\ncc: a@example.test, b@example.test\nlist_id: <team.example.test>\n") || strings.Contains(q.Material, "hop") {
			t.Fatalf("material %q", q.Material)
		}
	}
	var ev domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, "ev_to"); return })
	if ev.QueueLevel != runner.LevelNormal {
		t.Fatalf("p=0.62 must not be urgent on its own: %+v", ev)
	}
	if pv := OutboundPreview(ev, "to: me@example.test\n"); !strings.Contains(pv["full"].Material, "to: me@") || strings.Contains(pv["excerpt"].Material+pv["meta"].Material, "to: me@") {
		t.Fatalf("recipients leave only with the full scope: %+v", pv)
	}
}
