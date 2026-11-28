//go:build e2e

package proposals

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// apaRun 以运行身份（运行凭据 APA_TOKEN + APA_RUN）运行 apa。
func (k *kit) apaRun(token, run string, args ...string) baseline.Result {
	k.t.Helper()
	cmd := exec.Command(k.Bin["apa"], args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+k.Home, "APA_URL="+k.URL, "APA_TOKEN="+token, "APA_RUN="+run)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	r := baseline.Result{}
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			k.t.Fatalf("运行 apa %v: %v", args, err)
		}
		r.Code = ee.ExitCode()
	}
	r.Stdout, r.Stderr = out.String(), errb.String()
	return r
}

// runCredential 是 ACT-PROP-08：在 acc-open 建一个带后台运行标签的事项，让 STUB_HARNESS（声明 acc-stub 同款、read 档）
// 以"挂起不退出"模式运行，从运行目录的环境变量记录里取出 APA_TOKEN（RUN_TOK）与 APA_RUN（RUN_ID）。
func (k *kit) runCredential() (tok, runID, item string) {
	k.t.Helper()
	decl, _ := json.Marshal(map[string]any{"harness": "acc-hang", "headless": map[string]any{
		"command": []string{"env", "STUB_HARNESS_MODE=hang", "STUB_HARNESS_HANG_FOR=2m", k.Bin["stub-harness"], "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}}})
	if r := k.Apa(k.DEL, string(decl), "declare", "-"); r.Code != 0 {
		k.t.Fatalf("apa declare acc-hang: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	k.webOK("POST", "/settings/declarations/acc-hang/1/confirm", "", 200)
	k.webOK("PUT", "/settings/policy", `{"default_harness":"acc-hang"}`, 200)
	k.webOK("PUT", "/settings/agents/legacy:acc-hang/card", `{"groups":{"view":{},"organize":{},"source_events":{}},"tool":"edit"}`, 200)
	k.webOK("PATCH", "/rules/background-run%2Fevent_rules%2Fhandle-labelled/mode", `{"mode":"auto"}`, 200)
	item = k.idOf(k.apa(k.OBS, 0, "propose", "--kind", "task", "--title", "prop-run 后台运行", "--project", k.Projects["acc-open"], "--tags", "agent-ok"))
	k.webOK("POST", "/items/"+item+"/accept", `{}`, 200)
	if !k.WaitFor(30*time.Second, func() bool {
		envs, _ := filepath.Glob(filepath.Join(k.Home, "runs", "*", "stub-harness-env.json"))
		for _, p := range envs {
			var e map[string]string
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &e) == nil && e["APA_RUN"] != "" {
				tok, runID = e["APA_TOKEN"], e["APA_RUN"]
				if tok == "" && e["APA_RUN_CREDENTIAL_FILE"] != "" {
					b, _ := os.ReadFile(e["APA_RUN_CREDENTIAL_FILE"])
					tok = strings.TrimSpace(string(b))
				}
				return tok != ""
			}
		}
		return false
	}) {
		k.t.Fatalf("后台运行没有拿到运行凭据\n%s", k.ServiceLog())
	}
	return tok, runID, item
}

// stopRuns 让 STUB_HARNESS 退出（挂起的进程只属于本验收环境的二进制）。
func (k *kit) stopRuns() { _ = exec.Command("pkill", "-f", k.Bin["stub-harness"]).Run() }

// TestFlowProposalTriage 按验收文档 flow-proposal-triage 的顺序执行 ACT-PROP-01..10：
// 建议先进建议流，由本人接受、补到已有事项或丢弃，且不打扰本人。
func TestFlowProposalTriage(t *testing.T) {
	k := newKit(t)
	var a, d, open string
	var runTok, runID string

	k.step("ACT-PROP-01 observer 以 proposed 新建建议，进建议流而不进决策待办", func(t *testing.T) {
		a = k.proposal("prop-a")
		eq(t, "prop-a 阶段", k.phase(a), "proposed")
		if !k.inStream(a) {
			t.Error("prop-a 应在 Web 建议流（GET /items?phase=proposed）")
		}
		pending, suggestions := k.inbox()
		if !strings.Contains(suggestions, a) || strings.Contains(pending, a) {
			t.Errorf("apa inbox 应把 prop-a 列在建议里、不在决策待办里: %s%s", pending, suggestions)
		}
		if needs := k.webOK("GET", "/items?phase=needs_input", "", 200); strings.Contains(needs, a) {
			t.Errorf("决策待办（needs_input）不应有 prop-a: %s", needs)
		}
	})

	k.step("ACT-PROP-02 建议不产生投递", func(t *testing.T) {
		before := k.unread()
		if r := k.APIRaw("GET", "/v1/deliveries/next?wait=1", "", k.CHAN); r.Code != http.StatusNoContent {
			t.Errorf("CHAN 不应领到投递: %d %s", r.Code, r.Body)
		}
		eq(t, "Web 未读数", k.unread(), before)
		ctx := k.apa(k.DEL, 0, "context", "--session", "s1", "--scope", "project", "--project", k.Projects["acc-open"], "--format", "inject").Stdout
		if !strings.Contains(ctx, a) || !strings.Contains(ctx, "阶段 proposed") {
			t.Errorf("context 的事项增量应带 prop-a: %s", ctx)
		}
		if pending, _ := k.inbox(); strings.Contains(pending, a) {
			t.Error("待决不应包含建议")
		}
		if r := k.APIRaw("GET", "/v1/deliveries/next?wait=1", "", k.CHAN); r.Code != http.StatusNoContent {
			t.Errorf("context 之后 CHAN 仍不应领到投递: %d %s", r.Code, r.Body)
		}
	})

	k.step("ACT-PROP-03 本人在 Web 接受建议", func(t *testing.T) {
		k.webOK("POST", "/items/"+a+"/accept", `{}`, 200)
		eq(t, "阶段", k.phase(a), "open")
		if k.inStream(a) {
			t.Error("接受后应离开建议流")
		}
		ev := k.last(a, "item.phase_changed")
		eq(t, "Event via", ev.Actor.Via, "web")
		eq(t, "Event 操作者类型", ev.Actor.Type, "human")
	})

	k.step("ACT-PROP-04 本人在 Web 丢弃建议并填原因，可重开", func(t *testing.T) {
		b := k.proposal("prop-b")
		k.webOK("POST", "/items/"+b+"/dismiss", `{"reason":"重复了"}`, 200)
		eq(t, "阶段", k.phase(b), "dropped")
		eq(t, "reason", k.last(b, "item.phase_changed").Reason, "重复了")
		if k.inStream(b) {
			t.Error("丢弃后应离开建议流")
		}
		k.webOK("POST", "/items/"+b+"/transition", `{"to":"open","reason":"重开"}`, 200)
		eq(t, "重开后阶段", k.phase(b), "open")
	})

	// 2026-11-20 改写（权限模型第二版）：接受建议是二档；DEL 所属 Agent 的档位已放开，直接生效，记成 Agent 自己做的。
	k.step("ACT-PROP-05 档位放开的 Agent 代为接受", func(t *testing.T) {
		c := k.proposal("prop-c")
		r := k.Apa(k.DEL, "", "accept", c)
		if r.Code != 0 {
			t.Fatalf("apa accept 退出码 %d: %s%s", r.Code, r.Stdout, r.Stderr)
		}
		eq(t, "阶段", k.phase(c), "open")
		ev := k.last(c, "item.phase_changed")
		if ev.Actor.Type != "agent" || ev.Actor.OnBehalfOf != "" || ev.Ev != "" {
			t.Errorf("Event 应记 Agent 自己、不带代本人与原话: %+v", ev)
		}
		eq(t, "依据", ev.Basis.Via, "allowance")
		eq(t, "依据里的 Agent", ev.Basis.Agent, k.AgentID)
		found := false
		for _, s := range k.agentChanges() {
			found = found || s.Seq == ev.Seq
		}
		if !found {
			t.Error("Web 的 agent 变更视图里没有这次代答")
		}
	})

	// 2026-11-20 改写：依据不再是原话或授权单，而是 Agent 的档位；只整理的钥匙（DELO）处理建议得 23（tier_closed）。
	k.step("ACT-PROP-06 档位未放开的代答被拒绝，Web 建议流标明被拒绝的处理", func(t *testing.T) {
		d = k.proposal("prop-d")
		open = k.openItem("prop-open")
		k.apa(k.DELO, exitBasis, "accept", d)
		k.apa(k.DELO, exitBasis, "dismiss", d, "--reason", "重复")
		k.apa(k.DELO, exitBasis, "attach", d, "--to", open)
		eq(t, "阶段", k.phase(d), "proposed")
		items, rejections := k.streamItems()
		var inStream bool
		for _, it := range items {
			inStream = inStream || it["id"] == d
		}
		if !inStream {
			t.Error("prop-d 应仍在建议流")
		}
		rej := rejections[d]
		if rej == nil || rej["code"] == nil || rej["why"] == nil {
			t.Fatalf("Web 建议流应标明 prop-d 被拒绝的处理（last_rejections）: %v", rejections)
		}
		var rejectedCmds int
		for _, e := range k.events(d, "command.rejected") {
			if e.Payload["exit"] == float64(exitBasis) {
				rejectedCmds++
			}
		}
		eq(t, "退出码 23 的拒绝记录数", rejectedCmds, 3)
	})

	k.step("ACT-PROP-07 observer 和 worker 不能处理建议", func(t *testing.T) {
		for name, tok := range map[string]string{"OBS": k.OBS, "WRK": k.WRK} {
			k.apa(tok, exitDenied, "accept", d)
			k.apa(tok, exitDenied, "dismiss", d, "--reason", "x")
			k.apa(tok, exitDenied, "attach", d, "--to", open)
			eq(t, name+" 之后阶段", k.phase(d), "proposed")
		}
	})

	k.step("ACT-PROP-08 取得一份运行凭据", func(t *testing.T) {
		var item string
		runTok, runID, item = k.runCredential()
		if r := k.apaRun(runTok, runID, "item", "show", item, "--full"); r.Code != 0 {
			t.Fatalf("运行凭据读不了自己的事项: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
	})

	k.step("ACT-PROP-09 运行身份随所属 Agent，没开拍板不能处理建议", func(t *testing.T) {
		defer k.stopRuns()
		before := len(k.events("", "proposal.attached"))
		for _, args := range [][]string{
			{"accept", d},
			{"dismiss", d, "--reason", "x"},
			{"attach", d, "--to", open},
		} {
			if r := k.apaRun(runTok, runID, args...); r.Code != 23 {
				t.Errorf("运行凭据 apa %s 退出码 %d，期望 23\n%s%s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
			}
		}
		// 批量等于逐条（2026-11-20）：每条都被拒，整批什么也不处理；没有可处理的条目时为 0。
		if r := k.apaRun(runTok, runID, "proposals", "apply", "--recommended", "--source", "src_none"); r.Code != 23 && r.Code != 0 {
			t.Errorf("运行凭据 apa proposals apply --recommended 退出码 %d\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		eq(t, "prop-d 阶段", k.phase(d), "proposed")
		eq(t, "新增 proposal.attached", len(k.events("", "proposal.attached")), before)
		items, _ := k.streamItems()
		var ids []string
		for _, it := range items {
			ids = append(ids, it["id"].(string))
		}
		if !strings.Contains(strings.Join(ids, ","), d) {
			t.Errorf("没有任何建议被处理，prop-d 应仍在建议流: %v", ids)
		}
	})

	k.step("ACT-PROP-10 Web 建议流提供三种处置与合并展开，agent 直接提出的建议不显示推荐", func(t *testing.T) {
		// 数据面：prop-d 由 agent 直接提出，没有 recommend。
		if p := k.proposalOf(d); p != nil && p["recommend"] != nil {
			t.Errorf("prop-d 不应带推荐: %v", p)
		}
		// 页面面：内核提供的 Web 页面脚本里有建议流的三个处置、窄屏「更多」与「合并自 N 条」；「按推荐处理」已下线（决定 93）。
		html := k.WebRaw("GET", "/", "", false)
		js := regexp.MustCompile(`/assets/[^"]+\.js`).FindString(html.Body)
		if js == "" {
			t.Fatalf("Web 首页没有脚本入口: %.200s", html.Body)
		}
		bundle := k.WebRaw("GET", js, "", false).Body
		for _, want := range []string{"接受", "补到已有事项", "忽略", "更多", "合并自 ", "推荐："} {
			if !strings.Contains(bundle, want) {
				t.Errorf("Web 页面脚本缺少 %q", want)
			}
		}
		if strings.Contains(bundle, "按推荐处理") {
			t.Error("Web 页面脚本不应再有「按推荐处理」入口")
		}
	})
}
