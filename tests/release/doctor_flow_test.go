//go:build e2e

package release

// flow-doctor 的全流程验收（验收文档 ACT-DOC-00..13）：测试构建的真实 apa 二进制 + 真实内核进程，
// 只替身模型端点（CLS_STUB、MODEL_STUB）、无头运行、来源工具与渠道机器人。
// 宿主插件的真实自检命令由本机升级验证 L1–L12 承接；这里用按 DOCTOR_CASE 输出附录 A.1 JSON 的脚本替身。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

const privateTitle = "ACC-DOCTOR-PRIVATE-TITLE"

type docDetail struct {
	Subject string         `json:"subject"`
	Status  string         `json:"status"`
	Why     string         `json:"why"`
	Next    string         `json:"next"`
	Data    map[string]any `json:"data"`
}

type docCheck struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Why     string         `json:"why"`
	Next    string         `json:"next"`
	Details []docDetail    `json:"details"`
	Data    map[string]any `json:"data"`
}

type docReport struct {
	OK     bool       `json:"ok"`
	Checks []docCheck `json:"checks"`
	raw    string
	code   int
}

func (r docReport) check(t *testing.T, name string) docCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("自检报告里没有 %s 一项: %s", name, r.raw)
	return docCheck{}
}

func (c docCheck) detail(t *testing.T, subject string) docDetail {
	t.Helper()
	for _, d := range c.Details {
		if d.Subject == subject {
			return d
		}
	}
	t.Fatalf("%s 里没有对象 %s: %+v", c.Name, subject, c.Details)
	return docDetail{}
}

var docChecks = []string{"data_dir", "config", "tier", "model_endpoint", "sources", "packs", "host_plugin", "overlaps", "channels"}

type docFx struct {
	*baseline.Env
	t          *testing.T
	script     string // 宿主插件自检脚本
	caseFile   string // 脚本读取的 DOCTOR_CASE
	dir        string
	declare    func(selfcheck bool) // 重新提交并确认 acc-stub 声明
	adapterBin string
}

func (f *docFx) doctor() docReport {
	f.t.Helper()
	r := f.Apa("", "", "doctor", "--json")
	rep := docReport{raw: r.Stdout + r.Stderr, code: r.Code}
	if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil {
		f.t.Fatalf("apa doctor --json 的输出不是 JSON（退出码 %d）: %q %s", r.Code, r.Stdout, r.Stderr)
	}
	return rep
}

func (f *docFx) setCase(name string) {
	f.t.Helper()
	if err := os.WriteFile(f.caseFile, []byte(name), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *docFx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, path, body)
	if r.Code != want {
		f.t.Fatalf("%s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// snapshot 是数据目录的文件清单与哈希（排除 logs/ 下的诊断日志），等同 `find $APA_HOME -type f -exec sha256sum {} +`。
func snapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		if d.IsDir() {
			if rel == "logs" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		m[rel] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func diffSnapshots(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			d = append(d, "removed "+k)
		} else if w != v {
			d = append(d, "changed "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "added "+k)
		}
	}
	sort.Strings(d)
	return d
}

// waitStat 等来源 id 至少跑过一次（自检要报告"最近一次运行结果"）。
func (f *docFx) waitSourceRan(id string) {
	f.t.Helper()
	if !f.WaitFor(30*time.Second, func() bool {
		r := f.Web("GET", "/v1/sources/"+id, "")
		return r.Code == http.StatusOK && strings.Contains(r.Body, "last_success_at") && !strings.Contains(r.Body, `"last_success_at":null`)
	}) {
		f.t.Fatalf("来源 %s 没有在 30 秒内运行过: %s", id, f.Web("GET", "/v1/sources/"+id, "").Body)
	}
}

const defaultSelfcheck = `{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":true,"overlaps":[]}`

func newDocFx(t *testing.T) *docFx {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	e.InstallToolStub()
	// 自检在本机探测工具适配来源：工具替身目录放在 PATH 最前（与内核看到的一致）。
	t.Setenv("PATH", e.ToolBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &docFx{Env: e, t: t, dir: filepath.Join(e.Root, "doctor-flow")}
	f.answerProbe()
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.caseFile = filepath.Join(f.dir, "case")
	f.script = filepath.Join(f.dir, "selfcheck.sh")
	f.setCase("")
	sh := `#!/bin/sh
case "$(cat ` + f.caseFile + ` 2>/dev/null)" in
  no-session-kind) echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":false,"overlaps":[]}' ;;
  overlaps) echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":true,"overlaps":[{"kind":"injection","name":"other-todo-hook","detail":"injects todos"},{"kind":"schedule","name":"daily-standup","detail":"daily reminder"}]}' ;;
  *) echo '` + defaultSelfcheck + `' ;;
esac
`
	if err := os.WriteFile(f.script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	// 来源适配器：真实的邮件适配器，工具由 TOOL_STUB_BIN 里的替身顶替。
	f.adapterBin = filepath.Join(f.dir, "mail-himalaya")
	if out, code := run(t, repoRoot(t), os.Environ(), "go", "build", "-o", f.adapterBin, "./sources/mail/cmd/mail-himalaya"); code != 0 {
		t.Fatalf("构建邮件适配器: %s", out)
	}
	f.declare = func(selfcheck bool) {
		t.Helper()
		bin := e.Bin["stub-harness"]
		for _, h := range []struct {
			name   string
			resume bool
		}{{"acc-stub", true}, {"acc-stub-noresume", false}} {
			hl := map[string]any{
				"command": []string{bin, "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
				"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
			}
			if h.resume {
				hl["resume"] = []string{bin, "--resume", "{prompt}"}
			}
			d := map[string]any{"harness": h.name, "headless": hl}
			if selfcheck && h.resume {
				d["selfcheck"] = map[string]any{"command": []string{f.script}}
			}
			f.confirmDeclaration(h.name, d)
		}
	}
	return f
}

// confirmDeclaration 提交声明并在 Web 确认（取回执里的修订号）。
func (f *docFx) confirmDeclaration(harness string, decl map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(decl)
	r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "ACC-DOC 声明")
	var out struct {
		Rev int `json:"rev"`
	}
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &out) != nil || out.Rev == 0 {
		f.t.Fatalf("apa declare %s: %d %s %s", harness, r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", fmt.Sprintf("/v1/settings/declarations/%s/%d/confirm", harness, out.Rev), "", 200)
}

func TestFlowDoctor(t *testing.T) {
	f := newDocFx(t)
	step := func(name string, fn func(t *testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow() // 后一步依赖前一步恢复的环境（验收文档 ACT-DOC-13）
		}
	}
	var before map[string]string

	step("ACT-DOC-00 准备自检用实例", func(t *testing.T) {
		f.declare(true)
		// 事先建好的私有事项：自检的输出里不得出现它的标题或正文。
		r := f.Apa(f.DEL, "", "item", "add", "--kind", "task", "--title", privateTitle, "--summary", privateTitle+" summary", "--body", privateTitle+" body",
			"--phase", "open", "--project", f.Projects["acc-open"], "--evidence", "ACC-DOC 私有事项")
		if r.Code != 0 {
			t.Fatalf("item add: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
		ok := filepath.Join(f.dir, "doc-ok.sh")
		if err := os.WriteFile(ok, []byte("#!/bin/sh\necho '{\"cursor\":\"c1\"}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.RegisterScriptSource("src_doc_ok", ok)
		f.setOutbound("src_doc_ok", "meta")
		f.waitSourceRan("src_doc_ok")
		if r := f.Apa("", "", "backup", "create"); r.Code != 0 {
			t.Fatalf("apa backup create: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
	})

	step("ACT-DOC-01 健康实例上所有检查为正常，退出码 0", func(t *testing.T) {
		human := f.Apa("", "", "doctor")
		if human.Code != 0 || !strings.Contains(human.Stdout, "no error-level findings") {
			t.Fatalf("apa doctor 退出码 %d: %s%s", human.Code, human.Stdout, human.Stderr)
		}
		rep := f.doctor()
		if rep.code != 0 || !rep.OK {
			t.Fatalf("健康实例应无错误级结论: code=%d %s", rep.code, rep.raw)
		}
		names := make([]string, 0, len(rep.Checks))
		for _, c := range rep.Checks {
			names = append(names, c.Name)
			if c.Status != "ok" && c.Status != "warning" {
				t.Errorf("%s 的结论是 %q（%s）", c.Name, c.Status, c.Why)
			}
			if c.Why == "" || c.Next == "" {
				t.Errorf("%s 缺原因或下一步: %+v", c.Name, c)
			}
		}
		if strings.Join(names, ",") != strings.Join(docChecks, ",") {
			t.Errorf("检查类应为 %v，实际 %v", docChecks, names)
		}
		dd := rep.check(t, "data_dir")
		if !strings.Contains(dd.Why, "matches the binary") || dd.Data["schema_ledger"] == nil || dd.Data["last_backup_at"] == nil {
			t.Errorf("数据目录与版本应报告结构版本与二进制匹配、最近一次备份时间: %+v", dd)
		}
		if tier := rep.check(t, "tier"); tier.Data["tier"] != float64(3) {
			t.Errorf("健康实例（两个端点加 acc-stub 声明）应在第三档: %+v", tier)
		}
	})

	step("ACT-DOC-02 自检只读且不含账本里的用户内容", func(t *testing.T) {
		// 停掉内核再取清单：内核自己会周期性写账本，只有它停着，前后一致才说明是 apa doctor 没有动数据目录。
		f.StopKernel()
		before = snapshot(t, f.Home)
		rep := f.doctor()
		after := snapshot(t, f.Home)
		if d := diffSnapshots(before, after); len(d) > 0 {
			t.Errorf("apa doctor 改动了数据目录: %v", d)
		}
		if strings.Contains(rep.raw, privateTitle) {
			t.Errorf("自检输出含账本里的事项标题或正文")
		}
		if human := f.Apa("", "", "doctor"); strings.Contains(human.Stdout+human.Stderr, privateTitle) {
			t.Errorf("人读输出含账本里的事项标题")
		}
		f.StartKernelAgain()
	})

	step("ACT-DOC-03 数据目录与版本：异常时给出错误", func(t *testing.T) {
		st, err := os.Stat(f.Home)
		if err != nil {
			t.Fatal(err)
		}
		orig := st.Mode().Perm()
		if err := os.Chmod(f.Home, 0o777); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chmod(f.Home, orig); err != nil {
				t.Fatal(err)
			}
		}()
		rep := f.doctor()
		dd := rep.check(t, "data_dir")
		if dd.Status == "ok" || !strings.Contains(dd.Why, "writable by other users") || dd.Next == "" {
			t.Errorf("数据目录权限异常应被指出并给出下一步: %+v", dd)
		}
		if dd.Status == "error" && rep.code != 1 {
			t.Errorf("有错误级结论时退出码应为 1，实际 %d", rep.code)
		}
	})

	step("ACT-DOC-04 运行配置：监听非本机地址而没有 https 的 public_url 时报错误", func(t *testing.T) {
		cfgPath := filepath.Join(f.Home, "config.yaml")
		orig, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		f.StopKernel()
		defer func() {
			if err := os.WriteFile(cfgPath, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			f.StartKernelAgain()
		}()
		if err := os.WriteFile(cfgPath, []byte("listen: 0.0.0.0:18999\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rep := f.doctor()
		c := rep.check(t, "config")
		if c.Status != "error" || !strings.Contains(c.Why, "public_url") || rep.code != 1 {
			t.Errorf("监听非本机地址且无 https public_url 应为错误并退出码 1（实际 %d）: %+v", rep.code, c)
		}
		if err := os.WriteFile(cfgPath, []byte("listen: [unclosed\n  : : :\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rep = f.doctor()
		c = rep.check(t, "config")
		if c.Status != "error" || !strings.Contains(c.Why, "config.yaml cannot be parsed") || !regexp.MustCompile(`config\.yaml:\d+`).MatchString(c.Why) || rep.code != 1 {
			t.Errorf("无法解析的 config.yaml 应指出文件与位置并退出码 1（实际 %d）: %+v", rep.code, c)
		}
	})

	step("ACT-DOC-05 运行形态：报告当前处于哪一档、缺什么到下一档", func(t *testing.T) {
		tier := f.doctor().check(t, "tier")
		if tier.Data["tier"] != float64(3) || tier.Status == "error" {
			t.Fatalf("默认环境应为第三档: %+v", tier)
		}
		f.webOK("PUT", "/v1/settings/model-endpoints", `{"judge_endpoint":null,"generate_endpoint":null}`, 200)
		f.confirmHostlessDeclarations()
		rep := f.doctor()
		tier = rep.check(t, "tier")
		if tier.Data["tier"] != float64(1) || tier.Status != "ok" || !strings.Contains(tier.Next, "generate endpoint") {
			t.Errorf("清空端点与声明后应为第一档并指出需配置生成端点: %+v", tier)
		}
		if me := rep.check(t, "model_endpoint"); me.Status == "error" {
			t.Errorf("未配置端点不算错误: %+v", me)
		}
		f.WireEndpoints()
		f.declare(true)
		if tier := f.doctor().check(t, "tier"); tier.Data["tier"] != float64(3) {
			t.Errorf("恢复后应回到第三档: %+v", tier)
		}
	})

	step("ACT-DOC-06 模型端点：发内置示例题核对协议，不取账本内容", func(t *testing.T) {
		f.Cls.Reset()
		f.ModelStub.Reset()
		f.answerProbe()
		rep := f.doctor()
		me := rep.check(t, "model_endpoint")
		if me.Status != "ok" || len(me.Details) != 2 {
			t.Fatalf("正常时两项为正常: %+v", me)
		}
		for _, d := range me.Details {
			if d.Status != "ok" || !strings.Contains(d.Why, " ms") {
				t.Errorf("端点 %s 应正常且带耗时: %+v", d.Subject, d)
			}
		}
		batches := f.Cls.Batches()
		reqs := f.ModelStub.Requests()
		if len(batches) != 1 || batches[0].Questions != 1 {
			t.Fatalf("CLS_STUB 应恰好收到一道内置示例题，实际 %d 批", len(batches))
		}
		if len(reqs) != 1 {
			t.Fatalf("MODEL_STUB 应恰好收到一次结构化输出示例请求，实际 %d 次", len(reqs))
		}
		for _, body := range []string{batches[0].BodyText, reqs[0].BodyText} {
			if strings.Contains(body, privateTitle) {
				t.Errorf("示例请求含账本内容: %s", body)
			}
			if !strings.Contains(body, "7") {
				t.Errorf("示例请求应是内置的固定文本: %s", body)
			}
		}
		for name, fault := range map[string]modelstub.ClsFault{
			"self_reported": {Source: "self_reported"},
			"协议标识不符":        {Protocol: "probability-judgment/0"},
		} {
			f.Cls.SetFault(fault)
			rep := f.doctor()
			var judge docDetail
			for _, d := range rep.check(t, "model_endpoint").Details {
				if strings.Contains(d.Subject, "judge") {
					judge = d
				}
			}
			if judge.Status != "error" || judge.Why == "" || rep.code != 1 {
				t.Errorf("%s：判断端点应为错误并指明原因、退出码 1（实际 %d）: %+v", name, rep.code, judge)
			}
		}
		f.Cls.Reset()
		if rep := f.doctor(); rep.code != 0 {
			t.Errorf("恢复默认脚本后应回到退出码 0: %s", rep.raw)
		}
	})

	step("ACT-DOC-07 来源：命令缺失、工具版本不符、外发范围未选与外部全文外发都被指出", func(t *testing.T) {
		f.SetToolCase("mail-version-0.1")
		f.webOK("POST", "/v1/sources", `{"id":"src_doc_missing","type":"script","command":"/no/such/command","schedule":"1h","timeout":10}`, 201)
		f.webOK("POST", "/v1/sources", mustJSON(map[string]any{"id": "src_doc_mail", "type": "script", "source_type": "mail", "command": f.adapterBin,
			"schedule": "1h", "timeout": 10, "config": map[string]any{"account": "doc"}}), 201)
		noop := filepath.Join(f.dir, "noop.sh")
		if err := os.WriteFile(noop, []byte("#!/bin/sh\necho '{\"cursor\":\"c1\"}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.webOK("POST", "/v1/sources", mustJSON(map[string]any{"id": "src_doc_unset", "type": "script", "command": noop, "schedule": "1h", "timeout": 10}), 201)
		gen := f.Web("GET", "/v1/settings/model-endpoints", "")
		_ = gen
		f.setOutbound("src_doc_missing", "meta")
		f.setOutbound("src_doc_mail", "meta")
		f.setGenerateLocality("external")
		f.setOutbound("src_doc_ok", "full")
		defer func() {
			f.dropSources("src_doc_missing", "src_doc_mail", "src_doc_unset")
			f.setOutbound("src_doc_ok", "meta")
			f.setGenerateLocality("local")
		}()
		rep := f.doctor()
		src := rep.check(t, "sources")
		want := map[string]string{
			"src_doc_missing": "command does not exist", "src_doc_mail": "version is not in the range",
			"src_doc_unset": "outbound scope is not chosen", "src_doc_ok": "full text",
		}
		for id, sub := range want {
			d := src.detail(t, id)
			if d.Status == "ok" || !strings.Contains(d.Why, sub) || d.Data["last_run"] == nil {
				t.Errorf("%s 应指出 %q 并带最近一次运行结果: %+v", id, sub, d)
			}
		}
	})

	step("ACT-DOC-08 能力包：覆盖内置包的用户包使用已移除字段时逐字段指出", func(t *testing.T) {
		packDir := filepath.Join(f.Home, "packs", "inbound-triage")
		defer func() {
			if err := os.RemoveAll(filepath.Join(f.Home, "packs")); err != nil {
				t.Fatal(err)
			}
			f.StopKernel()
			f.StartKernelAgain()
		}()
		f.writeBadTriagePack(packDir)
		f.StopKernel()
		f.StartKernelAgain()
		c := f.doctor().check(t, "packs")
		d := c.detail(t, "inbound-triage")
		if c.Status == "ok" || d.Status == "ok" {
			t.Fatalf("覆盖内置包的坏包应被指出: %+v", c)
		}
		for _, sub := range []string{"confidence_threshold", "escalate_to", "on_undecided", "builtin"} {
			if !strings.Contains(d.Why, sub) {
				t.Errorf("能力包一项应逐字段指出 %q: %s", sub, d.Why)
			}
		}
	})

	step("ACT-DOC-09 宿主插件：声明不可执行或不上报会话类型时指出", func(t *testing.T) {
		f.setCase("no-session-kind")
		hp := f.doctor().check(t, "host_plugin").detail(t, "acc-stub")
		if hp.Status != "warning" || !strings.Contains(hp.Why, "does not report the session type") || !strings.Contains(hp.Why, "exit 23") ||
			hp.Data["plugin_version"] != "0.1.0" || hp.Data["last_call_at"] == nil {
			t.Errorf("不上报会话类型应被指出并带插件版本与最近调用时间: %+v", hp)
		}
		f.setCase("")
		// 把无头命令改指不存在的路径，按 Web 确认流程。
		f.confirmDeclaration("acc-stub", map[string]any{"harness": "acc-stub", "headless": map[string]any{
			"command": []string{"/no/such/harness", "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
			"access": map[string]any{"read": []string{"--access=read"}}},
			"selfcheck": map[string]any{"command": []string{f.script}}})
		rep := f.doctor()
		hp = rep.check(t, "host_plugin").detail(t, "acc-stub")
		if hp.Status != "error" || !strings.Contains(hp.Why, "not executable") || hp.Data["plugin_version"] != "0.1.0" || hp.Data["last_call_at"] == nil || rep.code != 1 {
			t.Errorf("无头运行声明不可执行应被指出（退出码 %d）: %+v", rep.code, hp)
		}
		f.declare(true)
	})

	step("ACT-DOC-10 重复提醒：只盘点插件自检报告的重叠，不改宿主配置", func(t *testing.T) {
		f.setCase("overlaps")
		hostBefore := hashFiles(t, f.script, f.caseFile)
		ov := f.doctor().check(t, "overlaps")
		if ov.Status != "warning" || len(ov.Details) != 2 {
			t.Fatalf("应列出两项重叠并提示: %+v", ov)
		}
		subjects := ov.Details[0].Subject + "|" + ov.Details[1].Subject
		for _, sub := range []string{"injection:other-todo-hook", "schedule:daily-standup"} {
			if !strings.Contains(subjects, sub) {
				t.Errorf("缺少重叠项 %s: %s", sub, subjects)
			}
		}
		if after := hashFiles(t, f.script, f.caseFile); after != hostBefore {
			t.Errorf("宿主一侧文件被改动")
		}
		f.setCase("")
		// 撤销 selfcheck.command：这一项被跳过，不是错误。
		f.declare(false)
		rep := f.doctor()
		ov = rep.check(t, "overlaps")
		if ov.Status == "error" || !strings.Contains(ov.Why, "skipped") || len(ov.Details) != 0 {
			t.Errorf("未声明自检命令时应跳过且不报错: %+v", ov)
		}
		f.declare(true)
	})

	step("ACT-DOC-11 渠道与导出：报告 ext 最近领取时间与投递积压", func(t *testing.T) {
		bot, err := fakebot.New()
		if err != nil {
			t.Fatal(err)
		}
		defer bot.Close()
		f.webOK("PUT", "/v1/settings/policy", `{"disturb":{"routes":{"decision":"acc-chan","reminder":"web","brief":"web","health":"web","wake":"web"},"merge_window_sec":2}}`, 200)
		stop := f.runChannelExt(bot)
		// 先领取一次，使"最近一次领取"有值，再停掉 CHAN_EXT。
		f.AskDecision("ACC-DOC 渠道预热")
		if !f.WaitFor(30*time.Second, func() bool { return len(bot.Sent()) > 0 }) {
			t.Fatalf("渠道 ext 没有领取并发出预热投递")
		}
		stop()
		for i := 0; i < 3; i++ {
			f.AskDecision(fmt.Sprintf("ACC-DOC 渠道积压 %d", i))
		}
		var ch docDetail
		f.WaitFor(20*time.Second, func() bool { // 合并窗口（2 秒）过后投递才入队
			ch = f.doctor().check(t, "channels").detail(t, "channel.acc-chan")
			return ch.Data["backlog"] == float64(3)
		})
		if ch.Status != "warning" || ch.Data["backlog"] != float64(3) || ch.Data["last_seen_at"] == nil {
			t.Fatalf("停掉渠道 ext 后应报告最近一次领取的时间与 3 条积压: %+v", ch)
		}
		first := ch.Data["last_seen_at"].(string)
		time.Sleep(6 * time.Second) // 领取时间的记录每 5 秒至多写一次账本；等过这段，重启后的第一次领取才会被记下
		stop = f.runChannelExt(bot)
		defer stop()
		if !f.WaitFor(60*time.Second, func() bool {
			d := f.doctor().check(t, "channels").detail(t, "channel.acc-chan")
			return d.Data["backlog"] == float64(0)
		}) {
			t.Fatalf("渠道 ext 启动后积压没有清零")
		}
		if !f.WaitFor(30*time.Second, func() bool {
			ch = f.doctor().check(t, "channels").detail(t, "channel.acc-chan")
			return ch.Data["last_seen_at"].(string) > first
		}) || ch.Status != "ok" || ch.Data["backlog"] != float64(0) {
			t.Errorf("领取完毕后积压应为 0 且领取时间更新（之前 %s）: %+v", first, ch)
		}
	})

	step("ACT-DOC-12 GET /doctor 提供内核一侧检查，需 observer", func(t *testing.T) {
		if r := f.APIRaw("GET", "/v1/doctor", "", ""); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Errorf("不带凭据应被拒，实际 HTTP %d", r.Code)
		}
		r := f.APIRaw("GET", "/v1/doctor", "", f.OBS)
		if r.Code != http.StatusOK {
			t.Fatalf("用 OBS 请求 GET /v1/doctor: HTTP %d %s", r.Code, r.Body)
		}
		var kernel docReport
		if err := json.Unmarshal([]byte(r.Body), &kernel); err != nil {
			t.Fatal(err)
		}
		cli := f.doctor()
		var names []string
		for _, c := range kernel.Checks {
			names = append(names, c.Name)
			if c.Name == "host_plugin" || c.Name == "overlaps" || c.Name == "data_dir" || c.Name == "config" {
				t.Errorf("GET /doctor 不应含只在 CLI 里做的检查 %s", c.Name)
			}
			if want := cli.check(t, c.Name); want.Status != c.Status {
				t.Errorf("%s 在 GET /doctor 的结论 %s 与 CLI 的 %s 不一致", c.Name, c.Status, want.Status)
			}
		}
		for _, n := range []string{"tier", "model_endpoint", "sources", "packs", "channels"} {
			if !strings.Contains(strings.Join(names, ","), n) {
				t.Errorf("GET /doctor 缺内核一侧的检查 %s: %v", n, names)
			}
		}
		// Web 健康页读的就是这个接口（本人会话）。
		if w := f.Web("GET", "/v1/doctor", ""); w.Code != http.StatusOK {
			t.Errorf("本人会话读 GET /v1/doctor: HTTP %d %s", w.Code, w.Body)
		}
	})

	step("ACT-DOC-13 恢复环境", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Join(f.Home, "packs")); err != nil {
			t.Fatal(err)
		}
		f.StopKernel()
		f.StartKernelAgain()
		rep := f.doctor()
		if rep.code != 0 || !rep.OK {
			t.Fatalf("环境恢复后 apa doctor 应退出码 0: %s", rep.raw)
		}
		for _, c := range rep.Checks {
			if c.Status == "error" {
				t.Errorf("%s 仍为错误: %s", c.Name, c.Why)
			}
		}
		if r := f.Apa("", "", "doctor"); r.Code != 0 {
			t.Errorf("apa doctor 退出码 %d", r.Code)
		}
	})
}

// answerProbe 让 MODEL_STUB 按 JSON Schema 回答自检的结构化输出示例请求（示例请求的系统提示是固定文本）。
func (f *docFx) answerProbe() {
	f.ModelStub.When("configuration probe", modelstub.Reply{Content: `{"answer":"yes","sufficient":true}`})
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func hashFiles(t *testing.T, paths ...string) string {
	t.Helper()
	h := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// confirmHostlessDeclarations 用只含执行者清单的声明取代两份无头运行声明（撤销 acc-stub 的无头运行）。
func (f *docFx) confirmHostlessDeclarations() {
	f.t.Helper()
	for _, h := range []string{"acc-stub", "acc-stub-noresume"} {
		f.confirmDeclaration(h, map[string]any{"harness": h, "executors": []map[string]any{{"id": h + ".cli", "kind": "external-cli"}}})
	}
}

// dropSources 删除来源登记。API 没有删除来源的入口（来源只能停用），所以验收环境在内核停着时直接清账本里的登记行。
func (f *docFx) dropSources(ids ...string) {
	f.t.Helper()
	f.StopKernel()
	db, err := sql.Open("sqlite", filepath.Join(f.Home, "ledger.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	for _, id := range ids {
		if _, err := db.Exec(`DELETE FROM sources_registry_source WHERE id = ?`, id); err != nil {
			f.t.Fatal(err)
		}
	}
	f.StartKernelAgain()
}

func (f *docFx) setOutbound(id, external string) {
	f.t.Helper()
	f.webOK("PUT", "/v1/settings/sources/"+id+"/outbound", `{"local":"full","external":"`+external+`"}`, 200)
}

func (f *docFx) setGenerateLocality(loc string) {
	f.t.Helper()
	f.webOK("PUT", "/v1/settings/model-endpoints", mustJSON(map[string]any{
		"generate_endpoint": map[string]any{"url": f.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": loc}}), 200)
}

// writeBadTriagePack 写一个覆盖内置 inbound-triage 的用户包：判断模板带已移除的 confidence_threshold 与 escalate_to，缺 on_undecided。
func (f *docFx) writeBadTriagePack(dir string) {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	// 以内置包为底稿：复制后只改写判断模板，其余保持合法。
	src := filepath.Join(repoRoot(f.t), "packs", "inbound-triage")
	if err := exec.Command("cp", "-r", src+"/.", dir).Run(); err != nil {
		f.t.Fatal(err)
	}
	tpl := filepath.Join(dir, "prompts", "triage-screen.md")
	b, err := os.ReadFile(tpl)
	if err != nil {
		f.t.Fatal(err)
	}
	bad := strings.Replace(string(b), "on_undecided: yes\n", "confidence_threshold: 0.7\nescalate_to: x\n", 1)
	if bad == string(b) {
		f.t.Fatal("内置 triage-screen.md 里没有 on_undecided: yes，无法构造坏包")
	}
	if err := os.WriteFile(tpl, []byte(bad), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// runChannelExt 启动随包的 Telegram 渠道 ext 连接机器人替身，返回停止函数。
func (f *docFx) runChannelExt(bot *fakebot.Bot) (stop func()) {
	f.t.Helper()
	cmd := exec.Command(f.Bin["telegram-ext"])
	cmd.Env = append(os.Environ(), "APA_URL="+f.URL, "APA_EXT_KEY="+f.CHAN, "TG_BOT=1:acc", "TG_CHAT=1", "TG_API="+bot.URL,
		"APA_EXT_STATE="+filepath.Join(f.Root, "ext-telegram-doc.json"))
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	f.t.Cleanup(stop)
	return stop
}
