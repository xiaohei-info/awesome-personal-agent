//go:build e2e

package judgment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// cases 配置 ACT-JRC-01 的替身用例：$CLS_CASES（triage-screen 的概率）与 $MODEL_CASES（对话端点的答案）。
func (f *fx) cases() {
	for _, c := range []struct {
		k string
		p float64
	}{{"JRC-HIGH", 0.90}, {"JRC-LOW", 0.05}, {"JRC-MID", 0.30}, {"JRC-UND", 0.30}} {
		f.cls(c.k, c.p)
	}
	sufficient, insufficient := true, false
	f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "JRC-MID"}, Answer: "yes", Sufficient: &sufficient})
	f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "JRC-UND"}, Answer: "no", Sufficient: &insufficient})
}

func (f *fx) lastSeq() int64 {
	s := f.audit("")
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1].Seq
}

func (f *fx) lows(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("JRC-LOW-%d", i))
	}
	return out
}

func (f *fx) eventID(key string) string {
	for _, e := range f.sourceEvents("") {
		if e.Key.EventID == "ev-"+key {
			return e.ID
		}
	}
	return ""
}

// openItem 用 DEL 代本人建一个 open 的任务事项。
func (f *fx) openItem(title, project string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects[project], "--evidence", "建个已有事项")
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v %s", err, r.Stdout)
	}
	return m.ID
}

// ---- ACT-JRC-05：目标事项上的运行 ----

const runPack = "acc-jrc"

// enableJudgmentRun 装上验收包 acc-jrc（来源 JRC_SRC 的事项进入 open 时在该事项上起一次后台运行）并声明包装脚本为 harness：
// 脚本以运行者身份（内核注入的 APA_TOKEN）读目标事项与无关事项的判断记录，结果写到 outDir。
func (f *fx) enableJudgmentRun(targetRec, otherRec string) {
	f.t.Helper()
	f.runOut = filepath.Join(f.Root, "jrc-out")
	must(f.t, os.MkdirAll(f.runOut, 0o755))
	wrapper := filepath.Join(f.Root, "acc-jrc-harness.sh")
	script := fmt.Sprintf(`#!/bin/sh
curl -s -o /dev/null -w '%%{http_code}' -H "Authorization: Bearer $APA_TOKEN" "$APA_URL/v1/judgments/%[1]s" > %[3]s/jrc-high.code
curl -s -o /dev/null -w '%%{http_code}' -H "Authorization: Bearer $APA_TOKEN" "$APA_URL/v1/judgments/%[2]s" > %[3]s/jrc-low.code
%[4]s judgment show %[2]s > %[3]s/jrc-cli.out 2>&1
echo $? > %[3]s/jrc-cli.code
STUB_HARNESS_MODE=summary-only exec %[5]s "$@"
`, targetRec, otherRec, f.runOut, f.Bin["apa"], f.Bin["stub-harness"])
	must(f.t, os.WriteFile(wrapper, []byte(script), 0o755))
	decl, _ := json.Marshal(map[string]any{"harness": "acc-jrc-run", "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "验收声明替身 harness"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/acc-jrc-run/1/confirm", "", 200)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"acc-jrc-run"}`, 200)
	pack := "name: " + runPack + "\nversion: 1.0.0\ndescription: 验收：来源事项进入 open 时在该事项上起后台运行\nevent_rules:\n" +
		"  - id: run-on-item\n    mode: observe\n    when: {event: item.phase_changed, to_phase: open, source: " + f.srcID + "}\n" +
		"    do: [{action: start_background_run, runner: main_session, prompt: handle-item, access: read, cwd: run_dir}]\n"
	dir := filepath.Join(f.Home, "packs", runPack)
	must(f.t, os.MkdirAll(filepath.Join(dir, "prompts"), 0o755))
	must(f.t, os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(pack), 0o644))
	must(f.t, os.WriteFile(filepath.Join(dir, "prompts", "handle-item.md"), []byte("---\nrun_as: main_session\ntimeout: 1m\n---\nJRC 你在处理事项 {item_id}。\n"), 0o644))
	f.StopKernel()
	f.StartKernelAgain()
	f.webOK("PATCH", "/rules/"+url.PathEscape(runPack+"/event_rules/run-on-item")+"/mode", `{"mode":"auto"}`, 200)
}

func (f *fx) waitRunFile(name string) string {
	f.t.Helper()
	f.until("运行进程写出 "+name, func() bool { _, err := os.Stat(filepath.Join(f.runOut, name)); return err == nil })
	return f.runOut
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- ACT-JRC-06：保留期 ----

func (f *fx) setConfig(key, value string) {
	f.t.Helper()
	p := filepath.Join(f.Home, "config.yaml")
	b, err := os.ReadFile(p)
	must(f.t, err)
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !strings.HasPrefix(l, key+":") {
			keep = append(keep, l)
		}
	}
	if value != "" {
		keep = append(keep, key+": "+value)
	}
	must(f.t, os.WriteFile(p, []byte(strings.Join(keep, "\n")+"\n"), 0o644))
}

func (f *fx) retention(t *testing.T, e sourceEvent, r record, item string) {
	f.StopKernel()
	f.setConfig("judgment_retention_days", "1")
	f.StartKernelAgain()
	f.TCAdvance(172800)
	// 后台维护每分钟一轮。
	if !f.WaitFor(150*time.Second, func() bool { return f.APIRaw("GET", "/v1/judgments/"+r.ID, "", f.OBS).Code == 404 }) {
		t.Fatalf("判断记录 %s 过了保留期仍可读", r.ID)
	}
	var tl struct {
		Steps []auditStep `json:"steps"`
	}
	check := func(id string) {
		if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "why", id).Stdout), &tl); err != nil {
			t.Fatalf("apa why %s: %v", id, err)
		}
		var j *struct {
			ID        string  `json:"id"`
			Answer    string  `json:"answer"`
			DecidedBy string  `json:"decided_by"`
			Expired   bool    `json:"expired"`
			Record    *record `json:"record"`
		}
		for _, s := range tl.Steps {
			if s.Judgment != nil {
				j = s.Judgment
			}
		}
		if j == nil || !j.Expired || j.Record != nil || j.Answer != "yes" || j.DecidedBy != "classifier" {
			t.Errorf("apa why %s 仍应有判断那一行，内容是命令 Event 里的结论（是、由分类端点定案），不再列逐级过程: %+v", id, j)
		}
	}
	check(item)
	check(e.ID) // 同一份判断在来源事件上的回放：保留期之后只剩命令 Event 里的结论
	if len(f.sourceEvents("")) == 0 || f.eventOf("JRC-HIGH").Disposition == "" {
		t.Errorf("事项与来源事件不应被清理")
	}
	if len(f.audit("inbound.disposed")) == 0 || len(f.audit("item.created")) == 0 {
		t.Errorf("Event 不应被清理")
	}
	f.StopKernel()
	f.setConfig("judgment_retention_days", "")
	f.StartKernelAgain() // 偏移量保留，时间不回拨
}

// ---- ACT-JRC-07：v0.1 夹具 ----

func (f *fx) legacyJudgeEvents(t *testing.T) {
	fx := f.LedgerFixture(2)
	srv := fx.ServeV01(f.Bin["apa"]) // 同一个测试构建二进制在夹具副本上启动，迁移账本
	obs, del := fx.M.Tokens["observer"], fx.M.Tokens["delegate"]
	var list struct {
		Events []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Source  string `json:"source_id"`
		} `json:"events"`
	}
	var evID, srcID string
	for _, s := range fx.M.Sources {
		code, body := srv.Get("/v1/sources/"+s+"/events?disposition=ignored&limit=500", obs)
		if code != 200 || json.Unmarshal(body, &list) != nil {
			continue
		}
		for _, e := range list.Events {
			if strings.HasPrefix(e.Subject, "Old cascade ignored") {
				evID, srcID = e.ID, s
				break
			}
		}
		if evID != "" {
			break
		}
	}
	if evID == "" {
		t.Fatalf("夹具里应有被旧级联忽略的来源事件（带 v0.1 run.finished category: judge）")
	}
	runApa := func(token string, args ...string) (string, int) {
		cmd := exec.Command(f.Bin["apa"], args...)
		cmd.Env = append(os.Environ(), "APA_HOME="+fx.Home, "APA_URL="+srv.Base, "APA_TOKEN="+token)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	// 旧判断来自 run.finished（category: judge），照常可读；载荷未被改写（逐条对夹具清单的哈希）。
	out, code := runApa(del, "why", evID)
	if code != 0 {
		t.Fatalf("apa why %s 退出码 %d: %s", evID, code, out)
	}
	var tl struct {
		Steps []auditStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &tl); err != nil {
		t.Fatalf("apa why 输出: %v %s", err, out)
	}
	legacy := 0
	for _, s := range tl.Steps {
		if s.Type == "run.finished" && payloadField(s.Payload, "category") == "judge" {
			legacy++
			sum := sha256.Sum256(s.Payload)
			if want := fx.M.EventPayloadSHA[fmt.Sprint(s.Seq)]; want != "" && want != hex.EncodeToString(sum[:]) {
				t.Errorf("seq %d 的旧 Event 载荷被改写", s.Seq)
			}
		}
		if s.Judgment != nil {
			t.Errorf("旧判断不应被搬成判断记录: %+v", s.Judgment)
		}
	}
	if legacy == 0 {
		t.Errorf("apa why 应能显示那条旧判断（来自 run.finished）: %s", out)
	}
	code2, body := srv.Get("/v1/judgments?subject="+evID, obs)
	var recs struct {
		Judgments []any `json:"judgments"`
	}
	if code2 != 200 || json.Unmarshal(body, &recs) != nil || len(recs.Judgments) != 0 {
		t.Errorf("/judgments 应返回空列表: %d %s", code2, body)
	}
	st, c := runApa(obs, "judgment", "stats", "--source", srcID)
	var s stats
	if c != 0 || json.Unmarshal([]byte(st), &s) != nil {
		t.Fatalf("apa judgment stats: %d %s", c, st)
	}
	calls := 0
	for _, r := range s.Rows {
		calls += r.Calls
	}
	if calls != 0 {
		t.Errorf("stats 里该来源的 calls 应为 0（旧判断不计入）: %s", st)
	}
}

// ---- ACT-JRC-17：抽检池 ----

type poolEntry struct {
	Judgment string   `json:"judgment"`
	Event    string   `json:"event"`
	Source   string   `json:"source"`
	WebPath  string   `json:"web_path"`
	Results  []string `json:"results"`
}

func (f *fx) spotPool(body string) []poolEntry {
	f.t.Helper()
	var out struct {
		SpotCheck []poolEntry `json:"spot_check"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		f.t.Fatalf("抽检池不是 JSON: %v %s", err, body)
	}
	return out.SpotCheck
}

// ---- ACT-JRC-20：简报 ----

func (f *fx) briefSpotCheck(t *testing.T) {
	f.emit(f.lows(7, 12)...)
	for _, k := range f.lows(7, 12) {
		if e := f.eventOf(k); e.Disposition != "ignored" {
			t.Fatalf("%s 应被忽略: %+v", k, e)
		}
	}
	f.webOK("PUT", "/settings/policy", `{"default_harness":"acc-stub"}`, 200)
	f.webOK("POST", "/instances", `{"pack":"brief","params":{"schedule":"5m"}}`, 201)
	f.TCAdvance(300)
	var mat string
	f.until("简报运行目录里有带 spot_check 的资料文件", func() bool {
		files, _ := filepath.Glob(filepath.Join(f.Home, "runs", "*", "*"))
		for _, p := range files {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), `"spot_check"`) {
				mat = string(b)
				return true
			}
		}
		return false
	})
	i := strings.Index(mat, `"spot_check"`)
	var doc struct {
		Materials []struct {
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"materials"`
	}
	var entries []poolEntry
	if json.Unmarshal([]byte(mat), &doc) == nil {
		for _, m := range doc.Materials {
			if m.Name == "spot_check" {
				entries = f.spotPool(m.Body)
			}
		}
	}
	if len(entries) != 5 {
		t.Fatalf("简报资料里应有 5 条抽检条目，实际 %d（%s）", len(entries), mat[i:min(len(mat), i+300)])
	}
	for _, e := range entries {
		r := f.record(e.Judgment)
		if r.Use != "inbound_screen" || r.Answer != "no" || e.WebPath == "" || strings.Contains(strings.Join(e.Results, ","), "submitted") {
			t.Errorf("条目应来自被初筛忽略的事件、带 Web 入口，且不带直接提交的结论: %+v %+v", e, r)
		}
	}
	// 结论仍只能在 Web 提交：简报的资料里只有入口，运行凭据与 observer 都不能写结论。
	if r := f.APIRaw("POST", "/v1/judgments/"+entries[0].Judgment+"/review", `{"result":"correct"}`, f.OBS); r.Code != 403 {
		t.Errorf("简报之外的凭据不能提交抽检结论，实际 %d", r.Code)
	}
}

func (f *fx) cleanup(t *testing.T) {
	var inst struct {
		Instances []struct {
			ID   string `json:"id"`
			Pack string `json:"pack"`
		} `json:"instances"`
	}
	_ = json.Unmarshal([]byte(f.webOK("GET", "/instances", "", 200)), &inst)
	for _, i := range inst.Instances {
		f.webOK("PATCH", "/instances/"+i.ID, `{"enabled":false}`, 200)
	}
	f.apa(f.DEL, 0, "source", "disable", f.srcID, "--evidence", "清理")
}
