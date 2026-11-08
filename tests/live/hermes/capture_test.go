//go:build live

package hermes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	capJudgeMark = "这条来源事件是否值得成为建议"          // inbound-triage/triage.md
	capTitleMark = "为这条来源事件起一个不超过 40 字的建议标题" // inbound-triage/triage-title.md
	capCandMark  = "从这条来源事件里抽取可以成为建议的候选事项"   // inbound-triage/triage-candidates.md
)

// capPack 提供一条后台运行规则，只为拿到一个真实的运行凭据（ACT-CAP-03）。
var capPack = map[string]string{
	"pack.yaml": `name: acc-cap
version: 1.0.0
description: 验收用：拿到后台运行的运行凭据
event_rules:
  - id: cap-bg
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-cap-bg]}
    do: [{action: start_background_run, runner: task, prompt: cap-bg, access: read, executor: work-host.cli, cwd: run_dir}]
`,
	"prompts/cap-bg.md": `---
agent: task
access: read
cwd: run_dir
timeout: 5m
---
CAPBG 你在事项 {item_id} 上后台工作。只用一句话回复"收到"。
`,
}

// TestFlowCapture 对应 flow-capture（ACT-CAP-01..04）：真实 Hermes 会话 + apa-capture 插件 + 真实 /v1/capture。
func TestFlowCapture(t *testing.T) {
	f := setup(t, "")
	defer f.flushEvidence("flow-capture-evidence.json")
	open := f.checkout("acc-open")
	home := f.hermesHome("cap", "core", "inject", "capture")
	proposals := func() map[string]string {
		var out struct {
			Items []struct{ ID, Title string } `json:"items"`
		}
		must(t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--phase", "proposed", "--project", f.Projects["acc-open"]).Stdout), &out))
		m := map[string]string{}
		for _, it := range out.Items {
			m[it.ID] = it.Title
		}
		return m
	}
	extractionRequests := func() int {
		n := 0
		for _, mark := range []string{capJudgeMark, capTitleMark, capCandMark} {
			for _, r := range f.ModelStub.Requests() {
				if strings.Contains(r.BodyText, mark) {
					n++
				}
			}
		}
		return n
	}
	const talk = "下周我要把合同发给小王"

	f.step(t, "ACT-CAP-01 默认关闭时不捕获", func(t *testing.T) {
		f.ModelStub.Reset()
		s := f.session(home, open, f.DEL)
		s.say(talk + "。先别用任何命令，只回复：好的")
		time.Sleep(15 * time.Second) // 捕获是回合结束后的异步 POST；给足时间再断言“没有”
		eq(t, "MODEL_STUB 的抽取请求数", extractionRequests(), 0)
		eq(t, "建议流新增", len(proposals()), 0)
	})

	f.step(t, "ACT-CAP-02 开启后抽取候选", func(t *testing.T) {
		f.webOK("PATCH", "/projects/"+f.Projects["acc-open"], `{"capture":true}`, 200)
		f.ModelStub.Reset()
		f.ModelStub.When(capJudgeMark, replyJSON(`{"choice":"task","confidence":0.9}`))
		f.ModelStub.When(capTitleMark, replyJSON(`{"insufficient_info":false,"title":"CAP2 把合同发给小王","summary":"下周把合同发给小王"}`))
		f.ModelStub.When(capCandMark, replyJSON(`{"insufficient_info":false,"candidates":[{"title":"CAP2 把合同发给小王","kind":"task","summary":"下周把合同发给小王"}]}`))
		s := f.session(home, open, f.DEL)
		s.say(talk + "。先别用任何命令，只回复：好的")
		f.waitFor("建议流出现 proposed 事项", 120*time.Second, func() bool { return len(proposals()) > 0 })
		got := proposals()
		for id := range got {
			eq(t, "候选阶段", f.phase(id), "proposed")
		}
		time.Sleep(5 * time.Second)
		n := len(proposals())
		// 同一段对话不重复提交：同一会话再次提交相同增量，不产生新的候选。
		sid := s.ID
		body, _ := json.Marshal(map[string]string{"text": "user: " + talk + "。先别用任何命令，只回复：好的\nassistant: 好的", "project": f.Projects["acc-open"]})
		for i := 0; i < 2; i++ {
			r := f.capturePost(f.DEL, sid, string(body))
			if i == 1 && !strings.Contains(r, `"captured":false`) && len(proposals()) != n {
				t.Errorf("重复提交不应新增候选: %s", r)
			}
		}
		time.Sleep(10 * time.Second)
		eq(t, "重复提交后的候选数", len(proposals()), n)
		f.evidence["ACT-CAP-02"] = map[string]any{"proposals": got, "extraction_requests": extractionRequests()}
		// 不产生投递：这些候选不应有决策投递。
		if strings.Contains(f.apa(f.OBS, 0, "context", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout, "- 提醒 ") {
			t.Errorf("候选捕获不应产生投递")
		}
	})

	f.step(t, "ACT-CAP-03 运行身份下不生效", func(t *testing.T) {
		runTok := f.startRunAndGetToken(t)
		before := len(proposals())
		reqs := extractionRequests()
		runs := f.apa(f.OBS, 0, "run", "list").Stdout
		body, _ := json.Marshal(map[string]string{"text": "user: CAP3 记得下周三交报销\nassistant: 好的", "project": f.Projects["acc-open"]})
		resp := f.capturePost(runTok, "run-session", string(body))
		contains(t, "运行身份下的 /v1/capture", resp, `"captured":false`)
		time.Sleep(10 * time.Second)
		eq(t, "候选数", len(proposals()), before)
		eq(t, "抽取请求数", extractionRequests(), reqs)
		_ = runs
	})

	f.step(t, "ACT-CAP-04 不强制续跑", func(t *testing.T) {
		f.ModelStub.Reset()
		// 抽取故意慢 20 秒：回合应先正常结束，捕获在回合之后异步完成。
		f.ModelStub.When(capJudgeMark, modelstubDelayed(`{"choice":"task","confidence":0.9}`))
		f.ModelStub.When(capTitleMark, modelstubDelayed(`{"insufficient_info":false,"title":"CAP4 给小李回电话","summary":"回电话"}`))
		f.ModelStub.When(capCandMark, modelstubDelayed(`{"insufficient_info":false,"candidates":[{"title":"CAP4 给小李回电话","kind":"task","summary":"回电话"}]}`))
		before := len(proposals())
		s := f.session(home, open, f.DEL)
		r := s.say("明天上午要给小李回电话。先别用任何命令，只回复：好的")
		if r.Final == "" || len(r.Tools) != 0 {
			t.Errorf("回合应正常结束且没有被续跑: %q tools=%v", r.Final, r.Tools)
		}
		if got := len(proposals()); got != before {
			t.Errorf("回合结束时抽取（20 秒延迟）不应已完成: %d -> %d", before, got)
		}
		f.waitFor("捕获异步完成", 120*time.Second, func() bool { return len(proposals()) > before })
	})
}

func (f *fx) capturePost(token, session, body string) string {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.URL+"/v1/capture", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APA-Session", session)
	resp, err := http.DefaultClient.Do(req)
	must(f.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		f.t.Fatalf("POST /v1/capture: %d %s", resp.StatusCode, b)
	}
	return string(b)
}

// startRunAndGetToken 经后台运行规则拉起一次运行，从运行子进程的环境里取出运行凭据（只在测试里使用，不落盘）。
func (f *fx) startRunAndGetToken(t *testing.T) string {
	t.Helper()
	for name, body := range capPack {
		p := filepath.Join(f.Home, "packs", "acc-cap", name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	// 包是热加载的：等规则出现后切 auto。
	f.waitFor("验收包规则加载", 60*time.Second, func() bool {
		return f.Web("PATCH", "/v1/rules/"+"acc-cap%2Fevent_rules%2Fcap-bg"+"/mode", `{"mode":"auto"}`).Code == 200
	})
	envFile := filepath.Join(f.Root, "run-env.txt")
	wrapper := filepath.Join(f.Root, "hermes-run.sh")
	runHome := f.hermesHome("run", "core", "tools", "headless") // 隔离的 HERMES_HOME，绝不碰 ~/.hermes
	script := fmt.Sprintf("#!/bin/sh\nenv | grep '^APA_' > %q\nexport HERMES_HOME=%q\nexport PATH=%q:\"$PATH\"\nexport APA_BIN=%q\nexec %q \"$@\"\n",
		envFile, runHome, f.shimDir, f.Bin["apa"], f.hermesBN)
	must(t, os.WriteFile(wrapper, []byte(script), 0o755))
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "hermes", "headless", "declaration.json"))
	must(t, err)
	var decl struct {
		Harness  string         `json:"harness"`
		Headless map[string]any `json:"headless"`
	}
	must(t, json.Unmarshal(raw, &decl))
	cmd := decl.Headless["command"].([]any)
	decl.Headless["command"] = append([]any{wrapper}, cmd[1:]...)
	b, _ := json.Marshal(map[string]any{"harness": harness, "headless": decl.Headless})
	if r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "真实 Hermes 的无头运行声明"); r.Code != 0 {
		t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	ver := "1" // 本流程里 hermes 的第一份声明
	f.webOK("POST", "/settings/declarations/"+harness+"/"+ver+"/confirm", "", 200)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	f.add("acc-open", "CAP3 后台运行触发", "", "acc-cap-bg", "work-host.cli")
	var tok string
	f.waitFor("运行子进程写出环境", 120*time.Second, func() bool {
		b, err := os.ReadFile(envFile)
		if err != nil {
			return false
		}
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "APA_TOKEN="); ok && v != "" {
				tok = v
			}
		}
		return tok != ""
	})
	return tok
}
