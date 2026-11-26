//go:build e2e

package sourcesv2

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/tools"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// setupOutputLimit 同 setup，但内核以 max_source_output_mb: 2、max_source_event_mb: 1 启动（验收 ACT-MAD-04 的前置）。
// 验收写的是 max_source_output_mb=1，但内核要求 max_source_event_mb 严格小于它，而 max_source_event_mb 最小是 1，
// 所以 1 起不来（见 PR 说明）；取能起来的最小值 2。
func setupOutputLimit(t *testing.T) *kit {
	t.Helper()
	dir := programs(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+hostPathWithout(t, "himalaya"))
	e := baseline.New(t)
	e.Init()
	e.ServeWith("max_source_output_mb: 2\nmax_source_event_mb: 1\n")
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	e.WireEndpoints()
	e.BatchAgent("")
	e.InstallToolStub()
	return &kit{Env: e, t: t}
}

// rawRead 是 GET /source-events/{id} 的响应里本测试关心的部分（Web 登录读全文）。
type rawRead struct {
	Body    *string        `json:"body"`
	Headers map[string]any `json:"headers"`
	Content struct {
		State   string `json:"state"`
		Missing []struct {
			Part   string `json:"part"`
			Reason string `json:"reason"`
		} `json:"missing"`
	} `json:"content"`
	Original    json.RawMessage `json:"original"`
	Attachments json.RawMessage `json:"attachments"`
}

func (k *kit) rawRead(eventID string) rawRead {
	k.t.Helper()
	var r rawRead
	if err := json.Unmarshal([]byte(k.webOK("GET", "/source-events/"+eventID, "", 200)), &r); err != nil {
		k.t.Fatal(err)
	}
	return r
}

// e2e-source-raw（himalaya 部分）：ACT-MAD-02 取全正文与固定信头、original/attachments 以 partial 自报，只读命令；
// 事件 id 仍是 Message-ID。
func TestFlowMailAdaptersRawHimalayaLong(t *testing.T) {
	k := setup(t)
	const src = "src_himalaya_long"
	k.SetToolCase("himalaya-long")
	k.webSource(map[string]any{"id": src, "name": src, "type": "script", "command": filepath.Join(progDir, "mail-himalaya"), "schedule": "1s", "timeout": 20,
		"source_type": "mail", "dedupe_domain": "acc-mail", "default_project": k.Projects["acc-home"],
		"config": map[string]any{"account": "acc@example.com", "lookback_days": 36500}})
	k.clearToolLog()

	const mid = "<mid-long@example.com>"
	var ev SrcEvent
	k.until("长邮件入账", func() bool { var ok bool; ev, ok = k.event(src, mid); return ok })
	if ev.DedupeKey.EventID != mid {
		t.Errorf("事件 id 应仍是 Message-ID: %q", ev.DedupeKey.EventID)
	}
	r := k.rawRead(ev.ID)

	// body 全文不截断（> 旧的 16 KiB 上限），与样本逐字一致。
	want := tools.LongBody(mid, tools.LongBodyBytes)
	if r.Body == nil || strings.TrimRight(*r.Body, "\n") != want || len(*r.Body) <= 16<<10 {
		n := -1
		if r.Body != nil {
			n = len(*r.Body)
		}
		t.Errorf("正文应是完整的 %d 字节，实际 %d", len(want), n)
	}
	for h, v := range map[string]string{"From": "AgentMail <agent@example.com>", "To": "me@example.com", "Cc": "team@example.com", "Reply-To": "noreply@example.com",
		"Subject": "[APA-LIVE-TEST] long mail", "Message-Id": mid, "In-Reply-To": "<mid-t2@example.com>", "References": "<mid-t1@example.com>",
		"List-Id": "<news.example.com>", "List-Unsubscribe": "<mailto:unsub@example.com>", "Content-Type": "text/plain; charset=utf-8"} {
		if r.Headers[h] != v {
			t.Errorf("headers[%s] = %v，期望 %q（全部: %v）", h, r.Headers[h], v, r.Headers)
		}
	}
	if r.Headers["Date"] == nil {
		t.Errorf("headers 缺 Date: %v", r.Headers)
	}
	parts := map[string]string{}
	for _, m := range r.Content.Missing {
		parts[m.Part] = m.Reason
	}
	if r.Content.State != "partial" || parts["original"] == "" || parts["attachments"] == "" || len(parts) != 2 {
		t.Errorf("content 应为 partial 并列出 original 与 attachments 及原因: %+v", r.Content)
	}
	if len(r.Original) != 0 && string(r.Original) != "null" || len(r.Attachments) != 0 && string(r.Attachments) != "null" {
		t.Errorf("没取到的部分不应凭空给出: original=%s attachments=%s", r.Original, r.Attachments)
	}

	// TOOL_LOG：读取命令都带 --preview，没有 message export。
	var reads int
	for _, argv := range k.toolCalls("himalaya") {
		j := strings.Join(argv, " ")
		if strings.Contains(j, "export") {
			t.Errorf("不应调用 message export: %v", argv)
		}
		if len(argv) >= 2 && argv[0] == "message" && argv[1] == "read" {
			reads++
			if !strings.Contains(j, "--preview") {
				t.Errorf("message read 缺 --preview（会置已读）: %v", argv)
			}
			for _, h := range []string{"From", "To", "Cc", "Reply-To", "Date", "Subject", "Message-ID", "In-Reply-To", "References", "List-Id", "Content-Type"} {
				if !strings.Contains(j, "-H "+h+" ") {
					t.Errorf("message read 没有请求信头 %s: %v", h, argv)
				}
			}
		}
	}
	if reads == 0 {
		t.Error("应有 message read 调用")
	}
}

// e2e-source-raw（himalaya 部分）：ACT-MAD-04 max_source_output_mb=2，20 封各约 110 KiB（合计超过上限）：每轮输出停在 1.5 MiB 内、
// 游标停在已输出的最后一条、多轮后全部入账、没有一轮因超限记为临时失败。
func TestFlowMailAdaptersRawByteBatching(t *testing.T) {
	k := setupOutputLimit(t)
	const src = "src_himalaya_many"
	k.SetToolCase("mail-many-long")
	// 适配器不知道内核的上限（内核不传），config.max_output_mb 与 max_source_output_mb 取同一个数。
	k.webSource(map[string]any{"id": src, "name": src, "type": "script", "command": filepath.Join(progDir, "mail-himalaya"), "schedule": "1s", "timeout": 20,
		"source_type": "mail", "dedupe_domain": "acc-mail", "default_project": k.Projects["acc-home"],
		"config": map[string]any{"account": "acc@example.com", "lookback_days": 36500, "max_output_mb": 2}})

	// 每封事件的输出约 110.4 KiB，1.5 MiB 预算最多放 13 封；全部 20 封合计约 2.15 MiB，不分批一次放不下。
	const perRun = 13
	prevEvents, prevRuns, steps := 0, 0, 0
	for i := 0; i < 40 && prevEvents < tools.ManyLongCount; i++ {
		s := k.cycle(src, 1)
		n := len(k.events(src))
		if s.Stats.ConsecutiveFailures != 0 || s.Stats.LastError != "" {
			t.Fatalf("不应有一轮记为失败（超限按临时失败计）: %+v", s.Stats)
		}
		if runs := s.Stats.Runs - prevRuns; n-prevEvents > perRun*max(runs, 1) {
			t.Errorf("本轮新增 %d 封（%d 次运行），超过每轮 %d 封", n-prevEvents, runs, perRun)
		}
		if n > prevEvents {
			steps++
		}
		prevEvents, prevRuns = n, s.Stats.Runs
	}
	if prevEvents != tools.ManyLongCount {
		t.Fatalf("20 封应全部入账，实际 %d", prevEvents)
	}
	if steps < 2 {
		t.Errorf("20 封 × 110 KiB 在 2 MiB 上限下至少要分 2 轮，实际 %d", steps)
	}
	for _, e := range k.events(src) {
		if !strings.HasPrefix(e.DedupeKey.EventID, "<mid-ml") {
			t.Errorf("事件 id 应是 Message-ID: %q", e.DedupeKey.EventID)
		}
	}
	for _, rj := range k.rejects(src) {
		t.Errorf("不应有被拒收的行: %s", rj)
	}
	if c := k.show(src).Cursor; c != "2026-11-07T19:00:00Z" {
		t.Errorf("全部取完后游标应停在最后一封的时间: %q", c)
	}
}
