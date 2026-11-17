//go:build e2e

package hostplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const (
	cdSrc       = "src_cd_chat" // 脚本来源 id 须形如 src_<小写名>
	cdOwnerWord = "按建议处理"       // 本人这一回合的原话（不含空格：STUB_HARNESS 按空白切分回执命令）
	cdExitBasis = 23            // 替本人做决定的依据不足（§14.5）
	cdListed    = "acc-own"     // 本人在 APA_DIRECT_PLATFORMS 里声明的平台
	cdSourceSh  = "#!/bin/sh\nQ=\"$1\"\ncat > \"$Q/last-stdin.json\"\nfor f in \"$Q\"/in/*.jsonl; do\n  [ -e \"$f\" ] || continue\n  cat \"$f\"\n  mv \"$f\" \"$Q/done/\"\ndone\necho '{\"cursor\":\"c'\"$(ls \"$Q/done\" | wc -l | tr -d ' ')\"'\"}'\n"
)

// cdEvent 是审计页（GET /v1/audit/events）里的一条事件，只列验收关心的字段。
type cdEvent struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Ev      string         `json:"evidence"`
	Payload map[string]any `json:"payload"`
	Actor   struct {
		Type        string `json:"type"`
		OnBehalfOf  string `json:"on_behalf_of"`
		Session     string `json:"session"`
		SessionKind string `json:"session_kind"`
	} `json:"actor"`
	Grant string `json:"grant"`
}

func (f *fx) cdEvents(object, typ string) []cdEvent {
	f.t.Helper()
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + object
	}
	if typ != "" {
		path += "&type=" + typ
	}
	var out struct {
		Events []cdEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", path, "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

// cdLedger 是账本里"代理改了东西"的事件数：只数 actor.type=agent 且不是 command.rejected 的事件。
// 后台系统事件（内核代写的待决、定时器、来源拉取等）不是代理的变更，且会异步出现，不计。
func (f *fx) cdLedger() (n int, last int64) {
	f.t.Helper()
	for _, e := range f.cdEvents("", "") {
		if e.Actor.Type == "agent" && e.Type != "command.rejected" {
			n++
		}
		last = max(last, e.Seq)
	}
	return
}

func (f *fx) cdPhase(id string) string {
	f.t.Helper()
	var out struct {
		Item struct {
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item.Phase
}

// cdRegisterSource 登记一个每秒拉取的脚本来源，事件由 cdEmit 放进队列。
func (f *fx) cdRegisterSource() string {
	f.t.Helper()
	q := filepath.Join(f.Root, "src-cd")
	for _, d := range []string{"in", "done"} {
		if err := os.MkdirAll(filepath.Join(q, d), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	script := filepath.Join(q, "source.sh")
	if err := os.WriteFile(script, []byte(cdSourceSh), 0o755); err != nil {
		f.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": cdSrc, "name": cdSrc, "type": "script", "source_type": "acc",
		"command": script + " " + q, "schedule": "1s", "timeout": 30, "default_project": f.Projects["acc-open"]})
	f.webOK("POST", "/sources", string(body), 201)
	return q
}

// cdProposal 让来源输出一条事件，等它被起草成带推荐的建议，返回建议事项 id。
func (f *fx) cdProposal(queue string, seq int, subject string, draft map[string]any) string {
	f.t.Helper()
	d, _ := json.Marshal(draft)
	f.ModelStub.When(`subject\":\"`+subject+`\"`, modelstub.Reply{Content: string(d)})
	ev, _ := json.Marshal(map[string]any{"id": subject, "type": "cd.message", "subject": subject, "sender_verified": true,
		"occurred_at": time.Now().UTC().Format(time.RFC3339)})
	tmp := filepath.Join(queue, fmt.Sprintf("tmp-%d", seq))
	if err := os.WriteFile(tmp, append(ev, '\n'), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(queue, "in", fmt.Sprintf("%04d.jsonl", seq))); err != nil {
		f.t.Fatal(err)
	}
	var item string
	if !f.WaitFor(60*time.Second, func() bool {
		var out struct {
			Events []struct {
				Disposition string `json:"disposition"`
				Item        string `json:"item"`
				DedupeKey   struct {
					EventID string `json:"event_id"`
				} `json:"dedupe_key"`
			} `json:"events"`
		}
		if json.Unmarshal([]byte(f.webOK("GET", "/sources/"+cdSrc+"/events?limit=500", "", 200)), &out) != nil {
			return false
		}
		for _, e := range out.Events {
			if e.DedupeKey.EventID == subject && e.Disposition == "proposed" {
				item = e.Item
			}
		}
		return item != ""
	}) {
		f.t.Fatalf("事件 %s 在 60 秒内没有成为建议\n%s", subject, f.ServiceLog())
	}
	return item
}

// cdTool 在 bound 描述的聊天会话里经插件包的原生工具运行一次调用（真实的会话类型判定与真 apa）。
// directPlatforms 是本人声明的 APA_DIRECT_PLATFORMS。APA_BIN 是记录调用环境后转给真 apa 的脚本，记录可经 wrapperEnvLog 读到。
func (f *fx) cdTool(bound map[string]string, directPlatforms, tool string, args map[string]any) wrapperResult {
	f.t.Helper()
	shim := filepath.Join(f.Root, "apa-recorder.sh")
	body := fmt.Sprintf("#!/bin/sh\nenv | grep -E '^APA_(HARNESS|SESSION|SESSION_KIND)=' | sort > %q\nexec %q \"$@\"\n", f.wrapperLogPath(), f.Bin["apa"])
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(bound)
	a, _ := json.Marshal(args)
	cmd := exec.Command("python3", "-I", filepath.Join(repoRoot(), "tests", "e2e", "hostplugin", "testdata", "tool_driver.py"),
		filepath.Join(repoRoot(), "plugins", "hermes"), "cd-session", string(b), tool, string(a))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Root, "APA_HOME=" + f.Home, "APA_URL=" + f.URL, "APA_TOKEN=" + f.DEL, "APA_BIN=" + shim}
	if directPlatforms != "" {
		cmd.Env = append(cmd.Env, "APA_DIRECT_PLATFORMS="+directPlatforms)
	}
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("tool_driver %s: %v\n%s", tool, err, out)
	}
	var r wrapperResult
	if err := json.Unmarshal(out, &r); err != nil {
		f.t.Fatalf("工具结果不是 JSON: %v\n%s", err, out)
	}
	if r.Error != "" {
		f.t.Fatalf("工具 %s 报错: %s", tool, r.Error)
	}
	return r
}

// cdStub 以 STUB_HARNESS 的一种会话入口（terminal、dm、group、listed）运行一条回执命令（apa 的一个子命令），返回替身退出码：
// 0 表示命令成功，3 表示命令被内核拒绝（回执命令失败）。extra 覆盖入口预设（STUB_HARNESS_CHAT_TYPE 等）。
func (f *fx) cdStub(entry string, extra []string, apaArgs ...string) int {
	f.t.Helper()
	dir := f.t.TempDir()
	cmd := exec.Command(f.Bin["stub-harness"])
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "APA_HOME=" + f.Home, "APA_URL=" + f.URL, "APA_TOKEN=" + f.DEL,
		"STUB_HARNESS_RUN_DIR=" + dir, "STUB_HARNESS_ENTRY=" + entry, "STUB_HARNESS_SESSION_ID=cd-" + entry,
		"STUB_HARNESS_RECEIPT_CMD=" + f.Bin["apa"] + " " + strings.Join(apaArgs, " ")}, extra...)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		f.t.Fatal(err)
	}
	return 0
}

// TestFlowChatDispose 对应验收文档 flow-chat-dispose（替身版，ACT-CHT-01..05）：
// 真实 apa 二进制 + 插件包的三个原生代答工具（宿主上下文替身绑定聊天类型）+ STUB_HARNESS 的四种会话入口。
// 真实模型与真实聊天渠道上的体验由本机升级验证 L14–L24 承接，不在此节点。
func TestFlowChatDispose(t *testing.T) {
	f := newKernel(t, "")
	f.WireEndpoints()
	queue := f.cdRegisterSource()
	yes := 0.9
	f.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "cd-"}, PYes: &yes})
	f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "cd-"}, Answer: "yes"})

	dm := map[string]string{"platform": "telegram", "chat_type": "dm", "user_id": "42"}
	group := map[string]string{"platform": cdListed, "chat_type": "group", "user_id": "42"}

	var target, prAcc, prDis, prAtt, prNone, prList1, prList2, prList3 string
	var seq int

	f.step("ACT-CHT-01 插件提供三个代答工具及说明", func(t *testing.T) {
		pytest(t, "plugins/hermes/tests/tools/test_tools.py",
			"test_chat_dispose_refusal_exit_23_is_exposed_not_retried")
		out, code := py(t, "-m", "pytest", "plugins/hermes/tests")
		if code != 0 {
			t.Fatalf("python3 -m pytest plugins/hermes/tests 退出码 %d\n%s", code, out)
		}
		// 真实注册的工具：名称、说明里的要求（本回合原话、退出码 23 不换说法重试、告知原因与 Web 链接）。
		r := exec.Command("python3", "-I", filepath.Join(repoRoot(), "tests", "e2e", "hostplugin", "testdata", "tool_driver.py"),
			filepath.Join(repoRoot(), "plugins", "hermes"), "s", "{}", "--list")
		r.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Root}
		raw, err := r.Output()
		if err != nil {
			t.Fatalf("列出已注册工具: %v", err)
		}
		var tools map[string]string
		if err := json.Unmarshal(raw, &tools); err != nil {
			t.Fatal(err)
		}
		for name, cli := range map[string]string{"apa_attach": "apa attach", "apa_proposals_apply": "apa proposals apply", "apa_item_progress": "apa item progress"} {
			desc, ok := tools[name]
			if !ok {
				t.Errorf("没有注册工具 %s", name)
				continue
			}
			contains(t, name+" 的说明", desc, cli, "this turn", "evidence", "exit 23", "do not rephrase and retry", "Web link")
		}
		contains(t, "apa_proposals_apply 的说明", tools["apa_proposals_apply"], "recommended", "source", "project")
	})

	f.step("ACT-CHT-02 会话类型按顺序判定，多人优先（STUB_HARNESS 四种入口加 APA_DIRECT_PLATFORMS）", func(t *testing.T) {
		target = f.add("acc-open", "cd-target", "open")
		cases := []struct {
			name   string
			entry  string
			extra  []string
			direct bool
		}{
			{"终端", "terminal", nil, true},
			{"私聊（dm 带发送者）", "dm", nil, true},
			{"名单里的平台（dm 或空聊天类型）", "listed", nil, true},
			{"名单里的平台加多人聊天类型仍是 group", "listed", []string{"STUB_HARNESS_CHAT_TYPE=group"}, false},
			{"多人会话", "group", nil, false},
		}
		for _, c := range cases {
			p := f.apaProposal("cd-order-" + c.entry + fmt.Sprint(len(c.extra)))
			before, _ := f.cdLedger()
			code := f.cdStub(c.entry, c.extra, "dismiss", p, "--evidence", cdOwnerWord)
			if c.direct {
				if code != 0 || f.cdPhase(p) != "dropped" {
					t.Errorf("%s：应判为 direct，凭原话的处理应成功（替身退出码 %d，阶段 %s）", c.name, code, f.cdPhase(p))
				}
				ev := f.cdEvents(p, "item.phase_changed")
				if len(ev) == 0 || ev[len(ev)-1].Actor.SessionKind != "direct" || ev[len(ev)-1].Ev != cdOwnerWord {
					t.Errorf("%s：事件应带原话与 direct: %+v", c.name, ev)
				}
			} else {
				if code != 3 || f.cdPhase(p) != "proposed" {
					t.Errorf("%s：应判为 group，凭原话的处理应被拒（替身退出码 %d，阶段 %s）", c.name, code, f.cdPhase(p))
				}
				if after, _ := f.cdLedger(); after != before {
					t.Errorf("%s：被拒的处理不应改账本（%d → %d）", c.name, before, after)
				}
			}
		}
		// 工具一侧的同一判定：名单里的平台加多人聊天类型报 group；不设名单时名单平台取不到类型。
		if r := f.cdTool(group, cdListed, "apa_dismiss", map[string]any{"id": f.apaProposal("cd-order-tool"), "evidence": cdOwnerWord}); r.ExitCode != cdExitBasis ||
			!strings.Contains(f.wrapperEnvLog(), "APA_SESSION_KIND=group") {
			t.Errorf("工具侧：名单平台加多人聊天类型应报 group 并被拒: %+v\n%s", r, f.wrapperEnvLog())
		}
		if r := f.cdTool(map[string]string{"platform": cdListed}, "", "apa_dismiss", map[string]any{"id": f.apaProposal("cd-order-tool2"), "evidence": cdOwnerWord}); r.ExitCode != cdExitBasis ||
			!strings.Contains(f.wrapperEnvLog(), "APA_SESSION_KIND=unknown") {
			t.Errorf("工具侧：不设名单时名单平台应报 unknown 并被拒: %+v\n%s", r, f.wrapperEnvLog())
		}
		if r := f.cdTool(map[string]string{"platform": cdListed}, cdListed, "apa_dismiss", map[string]any{"id": f.apaProposal("cd-order-tool3"), "evidence": cdOwnerWord}); r.ExitCode != 0 ||
			!strings.Contains(f.wrapperEnvLog(), "APA_SESSION_KIND=direct") {
			t.Errorf("工具侧：名单里的平台应报 direct 并成功: %+v\n%s", r, f.wrapperEnvLog())
		}
	})

	f.step("ACT-CHT-03 私聊里按推荐批量处理与按清单处理", func(t *testing.T) {
		// 其余步骤留下的建议先处理掉，让"按推荐处理"的范围只剩本步的几条。
		for _, e := range f.cdProposedItems() {
			f.webOK("POST", "/items/"+e+"/dismiss", `{"reason":"批量前清场"}`, 200)
		}
		prAcc = f.cdProposal(queue, seq+1, "cd-acc", map[string]any{"title": "起草：cd-acc", "kind": "task", "recommend": "accept", "reason": "需要跟进"})
		prDis = f.cdProposal(queue, seq+2, "cd-dis", map[string]any{"title": "起草：cd-dis", "kind": "task", "recommend": "dismiss", "reason": "广告"})
		prAtt = f.cdProposal(queue, seq+3, "cd-att", map[string]any{"title": "起草：cd-att", "kind": "task", "recommend": "attach", "attach_to": target, "reason": "属于该事项"})
		prNone = f.apaProposal("cd-none") // 没有推荐的建议
		seq += 3

		s0 := f.cdMaxSeq()
		r := f.cdTool(dm, "", "apa_proposals_apply", map[string]any{"recommended": true, "evidence": cdOwnerWord})
		if r.ExitCode != 0 {
			t.Fatalf("私聊里按推荐批量处理应成功: %+v", r)
		}
		if got := f.cdPhase(prAcc); got != "open" {
			t.Errorf("推荐 accept 的建议应成为 open，实际 %s", got)
		}
		for _, id := range []string{prDis, prAtt} {
			if got := f.cdPhase(id); got != "dropped" {
				t.Errorf("%s 应按推荐处置为 dropped，实际 %s", id, got)
			}
		}
		if got := f.cdPhase(prNone); got != "proposed" {
			t.Errorf("没有推荐的建议不应被处理，实际 %s", got)
		}
		if n := len(f.cdAttached(target, prAtt)); n != 1 {
			t.Errorf("推荐 attach 的建议应补到目标事项一次，实际 %d 条 proposal.attached", n)
		}
		handled := map[string]bool{}
		for _, e := range f.cdEvents("", "") {
			if e.Seq <= s0 || (e.Type != "item.phase_changed" && e.Type != "proposal.attached") {
				continue
			}
			obj := e.Object
			if e.Type == "proposal.attached" {
				obj, _ = e.Payload["proposal"].(string)
			}
			handled[obj] = true
			if e.Actor.Type != "agent" || e.Actor.OnBehalfOf != "owner" || e.Actor.SessionKind != "direct" || e.Ev != cdOwnerWord {
				t.Errorf("处置事件应由 Agent 代本人、带原话与 direct: %+v", e)
			}
		}
		for _, id := range []string{prAcc, prDis, prAtt} {
			if !handled[id] {
				t.Errorf("%s 没有带原话的处置事件", id)
			}
		}
		if handled[prNone] {
			t.Errorf("没有推荐的建议不应有处置事件")
		}

		// 清单模式：三条建议只动点到的那一条。
		prList1 = f.apaProposal("cd-list-1")
		prList2 = f.apaProposal("cd-list-2")
		prList3 = f.apaProposal("cd-list-3")
		r = f.cdTool(dm, "", "apa_proposals_apply", map[string]any{"actions": []map[string]any{{"id": prList2, "do": "dismiss", "reason": "不需要"}}, "evidence": cdOwnerWord})
		if r.ExitCode != 0 {
			t.Fatalf("私聊里按清单处理应成功: %+v", r)
		}
		for id, want := range map[string]string{prList1: "proposed", prList2: "dropped", prList3: "proposed"} {
			if got := f.cdPhase(id); got != want {
				t.Errorf("清单模式：%s 应为 %s，实际 %s", id, want, got)
			}
		}
		ev := f.cdEvents(prList2, "item.phase_changed")
		if len(ev) == 0 || ev[len(ev)-1].Actor.SessionKind != "direct" || ev[len(ev)-1].Ev != cdOwnerWord {
			t.Errorf("清单里那条的事件应带原话与 direct: %+v", ev)
		}
	})

	var progressBefore int
	f.step("ACT-CHT-04 私聊里补到已有事项与记进展（原话与授权单各成功一次）", func(t *testing.T) {
		// 补到已有事项（带原话）。
		r := f.cdTool(dm, "", "apa_attach", map[string]any{"id": prList1, "to": target, "evidence": "把它补到那一件"})
		if r.ExitCode != 0 {
			t.Fatalf("私聊里凭原话补到已有事项应成功: %+v", r)
		}
		if got := f.cdPhase(prList1); got != "dropped" {
			t.Errorf("被补到已有事项的建议应成为 dropped，实际 %s", got)
		}
		att := f.cdAttached(target, prList1)
		if len(att) != 1 || att[0].Ev != "把它补到那一件" || att[0].Actor.SessionKind != "direct" {
			t.Errorf("proposal.attached 应带原话与 direct: %+v", att)
		}

		// 给本人的事项记进展：凭原话。
		progressBefore = len(f.cdEvents(target, "item.progress_added"))
		r = f.cdTool(dm, "", "apa_item_progress", map[string]any{"id": target, "text": "cd 进展一", "evidence": "记一下进展"})
		if r.ExitCode != 0 {
			t.Fatalf("私聊里凭原话记进展应成功: %+v", r)
		}
		prog := f.cdEvents(target, "item.progress_added")
		if len(prog) != progressBefore+1 || prog[len(prog)-1].Ev != "记一下进展" || prog[len(prog)-1].Actor.SessionKind != "direct" {
			t.Errorf("进展事件应带原话与 direct: %+v", prog)
		}

		// 不带原话，凭 item.progress 授权单再记一条。
		grant := f.cdGrant([]string{"item.progress"}, "acc-open")
		r = f.cdTool(dm, "", "apa_item_progress", map[string]any{"id": target, "text": "cd 进展二", "grant": grant})
		if r.ExitCode != 0 {
			t.Fatalf("凭 item.progress 授权单记进展应成功: %+v", r)
		}
		prog = f.cdEvents(target, "item.progress_added")
		if len(prog) != progressBefore+2 {
			t.Fatalf("两次成功后进展事件应多两条，实际 %d → %d", progressBefore, len(prog))
		}
		last := prog[len(prog)-1]
		if last.Ev != "" || last.Grant != grant {
			t.Errorf("授权单那条进展不带原话，应带授权单 id %s: %+v", grant, last)
		}
	})

	f.step("ACT-CHT-05 多人会话里同样的调用被拒", func(t *testing.T) {
		// 复用 ACT-CHT-03、04 的凭原话调用，换成多人会话（聊天类型 group，平台在 APA_DIRECT_PLATFORMS 里）。
		p1, p2 := f.apaProposal("cd-grp-1"), f.apaProposal("cd-grp-2")
		before, _ := f.cdLedger()
		progress := len(f.cdEvents(target, "item.progress_added"))

		type call struct {
			tool string
			args map[string]any
			cli  []string
		}
		for _, c := range []call{
			{"apa_proposals_apply", map[string]any{"recommended": true, "evidence": cdOwnerWord}, []string{"proposals", "apply", "--recommended", "--evidence", cdOwnerWord}},
			{"apa_proposals_apply", map[string]any{"actions": []map[string]any{{"id": p1, "do": "dismiss"}}, "evidence": cdOwnerWord}, nil},
			{"apa_attach", map[string]any{"id": p2, "to": target, "evidence": cdOwnerWord}, []string{"attach", p2, "--to", target, "--evidence", cdOwnerWord}},
			{"apa_item_progress", map[string]any{"id": target, "text": "cd 多人", "evidence": cdOwnerWord}, []string{"item", "progress", target, "--text", "cd-多人", "--evidence", cdOwnerWord}},
		} {
			r := f.cdTool(group, cdListed, c.tool, c.args)
			if r.ExitCode != cdExitBasis || !strings.Contains(string(r.Stderr), "session_not_direct") {
				t.Errorf("%s 在多人会话里应退出码 23、code 为 session_not_direct: %+v", c.tool, r)
			}
			if !strings.Contains(f.wrapperEnvLog(), "APA_SESSION_KIND=group") {
				t.Errorf("%s 应以 group 上报会话类型: %s", c.tool, f.wrapperEnvLog())
			}
			if c.cli != nil { // STUB_HARNESS 的多人会话入口（名单里的平台加多人聊天类型）执行同一条命令
				if code := f.cdStub("listed", []string{"STUB_HARNESS_CHAT_TYPE=group", "APA_DIRECT_PLATFORMS=" + cdListed}, c.cli...); code != 3 {
					t.Errorf("STUB_HARNESS 多人入口下 apa %s 应被拒（替身退出码 3），实际 %d", strings.Join(c.cli, " "), code)
				}
			}
		}
		if after, _ := f.cdLedger(); after != before {
			t.Errorf("被拒的调用不应改账本（%d → %d）", before, after)
		}
		for id, want := range map[string]string{p1: "proposed", p2: "proposed", prNone: "proposed", prList3: "proposed"} {
			if got := f.cdPhase(id); got != want {
				t.Errorf("%s 应仍是 %s，实际 %s", id, want, got)
			}
		}
		if n := len(f.cdEvents(target, "item.progress_added")); n != progress {
			t.Errorf("多人会话里不应记进展（%d → %d）", progress, n)
		}
		if len(f.cdEvents("", "command.rejected")) == 0 {
			t.Error("被拒的调用应留下 command.rejected")
		}
		// §14.5：被拒的进展落在 open 的事项上，内核代写一条只有本人在 Web 能答的待决，说明原因，不带代理的自述。
		ask := f.cdEvents(target, "decision.requested")
		if len(ask) == 0 || !strings.Contains(fmt.Sprint(ask[len(ask)-1].Payload["item"]), "被拒绝") || strings.Contains(fmt.Sprint(ask[len(ask)-1].Payload["item"]), "cd 多人") {
			t.Errorf("事项上应有内核代写的拒绝说明（不含代理的自述）: %+v", ask)
		}
	})
}

// apaProposal 用 OBS 新建一条没有推荐的建议（项目 acc-open）。
func (f *fx) apaProposal(title string) string {
	f.t.Helper()
	return idOf(f.t, f.apa(f.OBS, 0, "propose", "--kind", "task", "--title", title, "--project", f.Projects["acc-open"]).Stdout)
}

func (f *fx) cdMaxSeq() int64 { _, s := f.cdLedger(); return s }

// cdProposedItems 是建议流里现有建议的 id。
func (f *fx) cdProposedItems() []string {
	f.t.Helper()
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/items?phase=proposed&limit=500", "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// cdAttached 是以 target 为对象、补入了 proposal 的 proposal.attached 事件。
func (f *fx) cdAttached(target, proposal string) (out []cdEvent) {
	for _, e := range f.cdEvents(target, "proposal.attached") {
		if e.Payload["proposal"] == proposal {
			out = append(out, e)
		}
	}
	return
}

// cdGrant 由本人在 Web 为 DEL 签发授权单：actions、范围（项目名）、有效期 1 天。
func (f *fx) cdGrant(actions []string, projects ...string) string {
	f.t.Helper()
	var ids []string
	for _, p := range projects {
		ids = append(ids, f.Projects[p])
	}
	var creds struct {
		Credentials []struct{ ID, Label string } `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/tokens", "", 200)), &creds); err != nil {
		f.t.Fatal(err)
	}
	del := ""
	for _, c := range creds.Credentials {
		if c.Label == "DEL" {
			del = c.ID
		}
	}
	if del == "" {
		f.t.Fatalf("找不到 DEL 凭据: %+v", creds)
	}
	body, _ := json.Marshal(map[string]any{"grantee": map[string]any{"credential": del}, "scope": map[string]any{"projects": ids},
		"actions": actions, "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "reason": "e2e"})
	var g struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/grants", string(body), 201)), &g); err != nil || g.ID == "" {
		f.t.Fatalf("签发授权单: %v", err)
	}
	return g.ID
}
