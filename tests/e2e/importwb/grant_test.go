//go:build e2e

package importwb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// apaAs 以 token 运行 apa，会话类型与会话引用由调用方给定（baseline.Apa 在带 --evidence 时固定按私聊上报，
// 这里要在"私聊"与"多人会话"下各验证一次，所以自己拼环境）。stderr 的错误 JSON 里取 code。
func (f *fx) apaAs(token, kind string, args ...string) (baseline.Result, string) {
	f.t.Helper()
	cmd := exec.Command(f.Bin["apa"], args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+f.Home, "APA_URL="+f.URL, "APA_TOKEN="+token, "APA_SESSION_KIND="+kind, "APA_SESSION=e2e-session")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := baseline.Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatalf("运行 apa %v: %v", args, err)
	}
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(r.Stderr)), &e)
	return r, e.Code
}

func (f *fx) delID() string {
	f.t.Helper()
	for _, c := range decode[struct {
		Credentials []struct{ ID, Label string } `json:"credentials"`
	}](f.t, f.web("GET", "/v1/settings/tokens", "", 200)).Credentials {
		if c.Label == "DEL" {
			return c.ID
		}
	}
	f.t.Fatal("找不到 DEL 凭据")
	return ""
}

// grantBody 是签给 DEL 的单；import 为空表示不带 import 段，expires 为 0 表示不带 expires_at。
func grantBody(del string, actions []string, imp string, expires time.Duration, extra string) string {
	grantee := map[string]any{"credential": del}
	if extra == "background" { // 后台运行可用：导入单不允许（背景标志在 grantee 上）
		grantee["background"], extra = true, ""
	}
	b := map[string]any{"grantee": grantee, "scope": map[string]any{"all": true}, "actions": actions, "reason": "e2e"}
	if expires > 0 {
		b["expires_at"] = time.Now().Add(expires).UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(b)
	s := string(raw)
	if imp != "" {
		s = strings.TrimSuffix(s, "}") + `,"import":` + imp + "}"
	}
	if extra != "" {
		s = strings.TrimSuffix(s, "}") + "," + extra + "}"
	}
	return s
}

func (f *fx) issue(body string) string {
	f.t.Helper()
	return decode[struct {
		ID string `json:"id"`
	}](f.t, f.web("POST", "/v1/settings/grants", body, 201)).ID
}

// TestFlowImportGrant 是 flow-import-grant（验收文档 ACT-IGR-01..08）：Agent 只能凭本人在 Web 签发的导入专用授权单导入，
// 原话不够，来源名与条数受单约束。授权单经 Web 登录的 API 签发。
func TestFlowImportGrant(t *testing.T) {
	f := setup(t)
	del := f.delID()
	const day = 24 * time.Hour
	imp3 := func(max int, src string) string { return fmt.Sprintf(`{"source":%q,"max_records":%d}`, src, max) }
	dir := t.TempDir()
	plan := func(name, body string) string { p := filepath.Join(dir, name); writeFile(t, p, body); return p }
	two := plan("imp3.jsonl", f.simpleItems("i3", 2))
	var a string

	f.step("ACT-IGR-01 导入专用授权单的签发规则", func(t *testing.T) {
		a = f.issue(grantBody(del, []string{"import.apply"}, imp3(3, "acc-imp3"), 6*day, ""))
		bad := map[string]string{
			"B 有效期 8 天":            grantBody(del, []string{"import.apply"}, imp3(3, "acc-imp3"), 8*day, ""),
			"C 不带 import 段":        grantBody(del, []string{"import.apply"}, "", 6*day, ""),
			"D 带 background":       grantBody(del, []string{"import.apply"}, imp3(3, "acc-imp3"), 6*day, "background"),
			"E 与 item.progress 混签": grantBody(del, []string{"import.apply", "item.progress"}, imp3(3, "acc-imp3"), 6*day, ""),
			"F 不带 expires_at":      grantBody(del, []string{"import.apply"}, imp3(3, "acc-imp3"), 0, ""),
		}
		for name, b := range bad {
			r := f.Web("POST", "/v1/settings/grants", b)
			if r.Code != 400 || !strings.Contains(r.Body, `"why"`) {
				t.Errorf("%s: HTTP %d %s，期望 400 且 why 指出规则", name, r.Code, r.Body)
			}
		}
		if r := f.APIRaw("POST", "/v1/settings/grants", grantBody(del, []string{"import.apply"}, imp3(3, "acc-imp3"), 6*day, ""), f.DEL); r.Code != 403 {
			t.Errorf("DEL 自己签发: HTTP %d %s，期望 403（签发只在 Web）", r.Code, r.Body)
		}
		if n := strings.Count(f.web("GET", "/v1/settings/grants", "", 200), `"id":"grt_`); n != 1 {
			t.Errorf("只应存在 A 一张单，得到 %d", n)
		}
	})

	f.step("ACT-IGR-02 不带单与只带原话都是 23 grant_required，会话类型不改变结果", func(t *testing.T) {
		before, seq0 := f.tableCounts(), f.maxSeq()
		for _, kind := range []string{"direct", "group"} {
			for _, extra := range [][]string{nil, {"--evidence", "我说导入"}} {
				r, code := f.apaAs(f.DEL, kind, append([]string{"import", "--file", two, "--source", "acc-imp3"}, extra...)...)
				if r.Code != 23 || code != "grant_required" {
					t.Errorf("会话 %s 附加 %v: 退出码 %d code=%q，期望 23 grant_required\n%s", kind, extra, r.Code, code, r.Stderr)
				}
			}
		}
		f.onlyRejections(before, seq0)
	})

	f.step("ACT-IGR-03 凭单导入成功，事件带授权单 id", func(t *testing.T) {
		r, _ := f.apaAs(f.DEL, "group", "import", "--file", two, "--source", "acc-imp3", "--grant", a)
		if r.Code != 0 || strings.Count(r.Stdout, `"outcome":"created"`) != 2 {
			t.Fatalf("退出码 %d，stdout: %s stderr: %s", r.Code, r.Stdout, r.Stderr)
		}
		bs := f.batches()
		if len(bs) != 1 || bs[0].Grant != a || bs[0].Created != 2 || bs[0].Source != "acc-imp3" {
			t.Fatalf("GET /imports = %+v", bs)
		}
		evs := f.audit("imported=" + bs[0].ID)
		if len(evs) != 2 {
			t.Fatalf("批次事件 %d 条，期望 2", len(evs))
		}
		for _, e := range evs {
			if e.Grant != a || e.Imported != bs[0].ID || e.RecordedAt == "" {
				t.Errorf("事件缺授权单、导入标记或写入时刻: %+v", e)
			}
		}
	})

	f.step("ACT-IGR-04 来源名不符与累计超上限被拒", func(t *testing.T) {
		before, seq0 := f.tableCounts(), f.maxSeq()
		one := plan("other.jsonl", f.simpleItems("o", 1))
		if r, code := f.apaAs(f.DEL, "direct", "import", "--file", one, "--source", "acc-other", "--grant", a); r.Code != 23 || code != "import_source_mismatch" {
			t.Errorf("来源名不符: 退出码 %d code=%q\n%s", r.Code, code, r.Stderr)
		}
		two2 := plan("two2.jsonl", f.simpleItems("t2", 2))
		if r, code := f.apaAs(f.DEL, "direct", "import", "--file", two2, "--source", "acc-imp3", "--grant", a); r.Code != 23 || code != "import_limit_exceeded" {
			t.Errorf("累计超上限: 退出码 %d code=%q\n%s", r.Code, code, r.Stderr)
		}
		f.onlyRejections(before, seq0)
	})

	f.step("ACT-IGR-05 预检不占授权单条数", func(t *testing.T) {
		one := plan("one.jsonl", f.simpleItems("p", 1))
		before := f.tableCounts()
		for i := 0; i < 3; i++ {
			r, _ := f.apaAs(f.DEL, "direct", "import", "--file", one, "--source", "acc-imp3", "--grant", a, "--dry-run")
			if r.Code != 0 || !strings.Contains(r.Stdout, `"outcome":"created"`) {
				t.Fatalf("第 %d 次预检: 退出码 %d %s%s", i+1, r.Code, r.Stdout, r.Stderr)
			}
		}
		if after := f.tableCounts(); !reflect.DeepEqual(before, after) {
			t.Errorf("预检写了账本: %v", diffCounts(before, after))
		}
		if r, _ := f.apaAs(f.DEL, "direct", "import", "--file", one, "--source", "acc-imp3", "--grant", a); r.Code != 0 {
			t.Errorf("预检之后真实导入 1 条被拒（预检占了条数）: %d %s", r.Code, r.Stderr)
		}
		if got := f.batches(); len(got) != 2 || got[0].Created+got[1].Created != 3 {
			t.Errorf("累计应为 3: %+v", got)
		}
	})

	f.step("ACT-IGR-06 运行凭据不能导入", func(t *testing.T) {
		f.EnableStubRuns() // 重启内核热加载运行能力包：登录会话随之重取
		f.relogin()
		_, _, env := f.StubRun("hang") // 运行期间凭据有效：在超时（4 秒）之前用完
		tok := env["APA_TOKEN"]
		if tok == "" {
			t.Fatal("没有取到运行凭据")
		}
		before := f.tableCounts()
		r := f.importHTTP("acc-imp3", "", f.simpleItems("run", 1), tok, map[string]string{"X-APA-Grant": a})
		if r.Code != 403 {
			t.Errorf("运行凭据导入: HTTP %d %s，期望 403", r.Code, r.Body)
		}
		if res, _ := f.apaAs(tok, "direct", "import", "--file", two, "--source", "acc-imp3", "--grant", a); res.Code != 4 {
			t.Errorf("运行凭据的 CLI 退出码 %d，期望 4\n%s", res.Code, res.Stderr)
		}
		after := f.tableCounts()
		for _, k := range []string{"items", "item_import_map", "item_import_batch"} {
			if before[k] != after[k] {
				t.Errorf("运行凭据导入写了 %s: %d -> %d", k, before[k], after[k])
			}
		}
	})

	f.step("ACT-IGR-07 CLI 按 500 条分批并沿用批次号", func(t *testing.T) {
		g := f.issue(grantBody(del, []string{"import.apply"}, imp3(1200, "acc-imp4"), day, ""))
		big := plan("imp4.jsonl", f.simpleItems("b4", 1100))
		r, _ := f.apaAs(f.DEL, "direct", "import", "--file", big, "--source", "acc-imp4", "--grant", g)
		if r.Code != 0 || strings.Count(r.Stdout, `"outcome":"created"`) != 1100 {
			t.Fatalf("退出码 %d，created 行数 %d\n%.300s", r.Code, strings.Count(r.Stdout, `"outcome":"created"`), r.Stderr)
		}
		var got []batchView
		for _, b := range f.batches() {
			if b.Source == "acc-imp4" {
				got = append(got, b)
			}
		}
		if len(got) != 1 || got[0].Created != 1100 || got[0].Grant != g {
			t.Errorf("acc-imp4 的批次 = %+v，期望只有一个且 created 为 1100", got)
		}
		if n := f.eventCount(`type = 'import.completed' AND payload LIKE '%"acc-imp4"%'`); n != 3 {
			t.Errorf("CLI 应分 3 次请求（每次一条 import.completed），得到 %d", n)
		}
	})

	f.step("ACT-IGR-08 item.progress 可列入授权单并凭单记进展", func(t *testing.T) {
		home, work := f.Projects["acc-home"], f.Projects["acc-work"]
		body, _ := json.Marshal(map[string]any{"grantee": map[string]any{"credential": del}, "scope": map[string]any{"projects": []string{home}},
			"actions": []string{"item.progress"}, "expires_at": time.Now().Add(day).UTC().Format(time.RFC3339), "reason": "e2e"})
		g := f.issue(string(body))
		add := func(project string) string {
			return decode[struct {
				ID string `json:"id"`
			}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "进展对照", "--phase", "open", "--project", project, "--evidence", "夹具").Stdout).ID
		}
		inHome, inWork := add(home), add(work)
		f.apa(f.DEL, 0, "item", "progress", inHome, "--text", "凭单进展", "--grant", g)
		evs := f.audit("type=item.progress_added&object=" + inHome)
		if len(evs) != 1 || evs[0].Grant != g || evs[0].Actor.OnBehalfOf == "" {
			t.Errorf("acc-home 的进展事件 = %+v，期望带 grant 与 on_behalf_of", evs)
		}
		if r, code := f.apaAs(f.DEL, "direct", "item", "progress", inWork, "--text", "越界", "--grant", g); r.Code != 23 {
			t.Errorf("acc-work 的进展: 退出码 %d code=%q，期望 23（不在 scope）\n%s", r.Code, code, r.Stderr)
		}
		if n := len(f.audit("type=item.progress_added&object=" + inWork)); n != 0 {
			t.Errorf("越界的进展被写入了")
		}
	})
}

// onlyRejections 断言被拒的导入没有写任何东西：除事件日志外各表行数不变，事件日志里新增的只能是拒绝本身的审计（command.rejected）。
func (f *fx) onlyRejections(before map[string]int, seq0 int64) {
	f.t.Helper()
	d := diffCounts(before, f.tableCounts())
	delete(d, "eventlog_event")
	if len(d) != 0 {
		f.t.Errorf("被拒的导入写了账本: %v", d)
	}
	for _, typ := range f.queryStrings(`SELECT type FROM eventlog_event WHERE seq > ? AND type <> 'command.rejected'`, seq0) {
		f.t.Errorf("被拒的导入留下了 %s 事件", typ)
	}
}
