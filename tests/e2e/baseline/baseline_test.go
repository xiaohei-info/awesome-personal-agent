//go:build e2e

package baseline

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// TestFlowEnvBaseline 按验收文档 flow-env-baseline 的顺序，用真实 apa 二进制逐步建立环境并核对每个动作的"通过"判据。
// 任一步失败即停止，后续步骤不得开始（ACT-ENV-01 的失败语义）。
func TestFlowEnvBaseline(t *testing.T) {
	e := New(t)
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-ENV-01 隔离数据目录并启动内核", func(t *testing.T) {
		e.StartKernel()
		for _, f := range []string{"config.yaml", "ledger.db", "blobs", "packs", "sources", "runs", "logs"} {
			if _, err := os.Stat(filepath.Join(e.Home, f)); err != nil {
				t.Errorf("APA_HOME 缺少 %s: %v", f, err)
			}
		}
		if !strings.HasPrefix(e.Home, e.Root) {
			t.Errorf("APA_HOME 必须在测试专用目录内: %s", e.Home)
		}
		resp, err := http.Get(e.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(page), "<div id=\"root\">") {
			t.Fatalf("Web 登录页打不开: %d %s", resp.StatusCode, page)
		}
		// 登录是真的：错误口令被拒绝且读不到账本；正确口令能读。
		if r := e.WebRaw("POST", "/v1/auth/login", `{"password":"wrong-password-x"}`, false); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Errorf("错误口令应被拒绝，实际 %d", r.Code)
		}
		if r := e.Web("GET", "/v1/settings/executors", ""); r.Code != 200 {
			t.Errorf("Web 登录后应能读设置: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-ENV-02 签发五个凭据并维护执行者清单", func(t *testing.T) {
		e.IssueCredentials()
		for name, tok := range map[string]string{"OBS": e.OBS, "WRK": e.WRK, "WRK2": e.WRK2, "DEL": e.DEL, "CHAN": e.CHAN} {
			if tok == "" {
				t.Errorf("%s 没有值", name)
			}
		}
		if r := e.Apa(e.OBS, "", "inbox"); r.Code != 0 {
			t.Errorf("OBS apa inbox 退出码 %d: %s", r.Code, r.Stderr)
		}
		for name, tok := range map[string]string{"WRK": e.WRK, "WRK2": e.WRK2, "DEL": e.DEL} {
			if r := e.Apa(tok, "", "inbox"); r.Code != 0 {
				t.Errorf("%s 不能认证: 退出码 %d %s", name, r.Code, r.Stderr)
			}
		}
		if r := e.APIRaw("GET", "/v1/deliveries/next?wait=0", "", e.CHAN); r.Code != http.StatusNoContent {
			t.Errorf("CHAN 应能认证并领到空队列(204)，实际 %d %s", r.Code, r.Body)
		}
		if r := e.Apa("invalid-token", "", "inbox"); r.Code != 3 {
			t.Errorf("无效 token 应是退出码 3，实际 %d", r.Code)
		}
		r := e.Apa(e.OBS, "", "executors")
		for _, id := range []string{"work-host.cli", "other-host.cli"} {
			if r.Code != 0 || !strings.Contains(r.Stdout, `"`+id+`"`) {
				t.Errorf("执行者清单缺少 %s: %s", id, r.Stdout)
			}
		}
		if !strings.Contains(r.Stdout, `"external-cli"`) {
			t.Errorf("执行者 kind 应是 external-cli: %s", r.Stdout)
		}
	})

	step("ACT-ENV-03 建立四个验收项目", func(t *testing.T) {
		e.CreateProjects()
		show := func(name string) map[string]json.RawMessage {
			r := e.Apa(e.OBS, "", "project", "show", e.Projects[name])
			if r.Code != 0 {
				t.Fatalf("project show %s: 退出码 %d %s", name, r.Code, r.Stderr)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
		work := show("acc-work")
		if got := string(work["allowed_executors"]); got != `["work-host.cli"]` {
			t.Errorf("acc-work allowed_executors = %s", got)
		}
		if !strings.Contains(string(work["repos"]), e.TestRepo) {
			t.Errorf("acc-work repos 缺少 TEST_REPO: %s", work["repos"])
		}
		if got := string(show("acc-open")["allowed_executors"]); got != "null" {
			t.Errorf("acc-open 应为未设置(null)，实际 %s", got)
		}
		if got := string(show("acc-empty")["allowed_executors"]); got != "[]" {
			t.Errorf("acc-empty 应为空列表，实际 %s", got)
		}
		if home := show("acc-home"); len(home["repos"]) > 0 && string(home["repos"]) != "null" {
			t.Errorf("acc-home 不应有 repos: %s", home["repos"])
		}
	})

	step("ACT-ENV-04 模型端点替身", func(t *testing.T) {
		e.StartModelStub()
		e.ModelStub.Enqueue(modelstub.Reply{Content: "scripted-reply-1"})
		resp := e.chatCompletion(t, "hello-env-04")
		if !strings.Contains(resp, "scripted-reply-1") {
			t.Errorf("没有返回脚本内容: %s", resp)
		}
		reqs := e.ModelStub.Requests()
		if len(reqs) != 1 || !strings.Contains(reqs[0].BodyText, "hello-env-04") {
			t.Errorf("没有留下完整请求记录: %+v", reqs)
		}
		e.ModelStub.Enqueue(modelstub.Reply{Status: 500})
		if !strings.Contains(e.chatCompletion(t, "boom"), "error") {
			t.Error("HTTP 500 不可脚本化")
		}
		// Web 设置里的 base_url 确实指向它。
		r := e.Web("GET", "/v1/settings/model-endpoints", "")
		if r.Code != 200 || !strings.Contains(r.Body, e.ModelStub.URL()) {
			t.Errorf("模型端点没有指向替身: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-ENV-05 无头运行替身及其声明", func(t *testing.T) {
		e.DeclareStubHarness()
		r := e.Web("GET", "/v1/settings/commands", "")
		if r.Code != 200 {
			t.Fatalf("读取已生效声明: %d %s", r.Code, r.Body)
		}
		for _, h := range []string{"acc-stub", "acc-stub-noresume"} {
			if !strings.Contains(r.Body, `"`+h+`"`) {
				t.Errorf("声明 %s 未生效: %s", h, r.Body)
			}
		}
		if p := e.Web("GET", "/v1/settings/declarations", ""); strings.Contains(p.Body, "acc-stub") {
			t.Errorf("声明仍在待确认: %s", p.Body)
		}
		// 声明的命令真能跑：no-receipt 模式退出 0；exit1 模式退出 1。
		for mode, want := range map[string]int{"no-receipt": 0, "exit1": 1} {
			cmd := exec.Command(e.Bin["stub-harness"], "STUB_MODE="+mode)
			cmd.Dir = t.TempDir()
			err := cmd.Run()
			got := 0
			if ee, ok := err.(*exec.ExitError); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("STUB_HARNESS %s 退出码 %d，期望 %d", mode, got, want)
			}
		}
	})

	step("ACT-ENV-06 记录真实外部依赖状态（本人确认项）", func(t *testing.T) {
		got := e.RecordRealDependencies()
		if len(got) != 5 {
			t.Fatalf("应记录 hermes/himalaya/gog/gh/telegram 五项，实际 %v", got)
		}
		for _, d := range got {
			t.Logf("ACT-ENV-06 %-9s ready=%v %s", d.Name, d.Ready, d.Detail)
		}
	})
}

// 两个环境互相隔离：不同 APA_HOME、不同端口、不同凭据；一个停止不影响另一个。
func TestEnvironmentsAreIsolated(t *testing.T) {
	a, b := Setup(t), Setup(t)
	if a.Home == b.Home || a.URL == b.URL || a.OBS == b.OBS {
		t.Fatal("两个环境共用了目录、地址或凭据")
	}
	if r := b.Apa(a.OBS, "", "inbox"); r.Code != 3 {
		t.Errorf("A 的凭据不应在 B 的内核上有效，实际退出码 %d", r.Code)
	}
	a.StopKernel()
	if r := b.Apa(b.OBS, "", "inbox"); r.Code != 0 {
		t.Errorf("停掉 A 不应影响 B: %d %s", r.Code, r.Stderr)
	}
	if r := a.Apa(a.OBS, "", "inbox"); r.Code != 7 {
		t.Errorf("内核不可达应是退出码 7，实际 %d", r.Code)
	}
}

// 重新启动内核后，已建立的环境数据仍在（后续流程可以停启内核）。
func TestSetupSurvivesKernelRestart(t *testing.T) {
	e := Setup(t)
	e.StopKernel()
	e.StartKernelAgain()
	if r := e.Apa(e.WRK, "", "executors"); r.Code != 0 || !strings.Contains(r.Stdout, "work-host.cli") {
		t.Fatalf("重启后环境数据丢失: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
}
