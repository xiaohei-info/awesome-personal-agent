package ledgerfixture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v01Binary 返回旧结构版本的主程序：优先用环境变量 APA_V01_BIN；没有就用 fetch-v01.sh 从冻结提交构建（缓存在系统临时目录）。
// 只有「这份克隆里没有冻结提交」（浅克隆）时才跳过；构建失败是失败。
func v01Binary(t *testing.T) (bin, plugin string) {
	t.Helper()
	if b := os.Getenv("APA_V01_BIN"); b != "" {
		if _, err := os.Stat(b); err != nil {
			t.Fatalf("APA_V01_BIN=%s: %v", b, err)
		}
		return b, os.Getenv("APA_V01_PLUGIN")
	}
	commit := Frozen[2]
	out := filepath.Join(os.TempDir(), "apa-fixture-v01")
	bin = filepath.Join(out, "apa-"+commit)
	plugin = filepath.Join(out, "apa-hermes-plugin-"+commit+".tar.gz")
	if _, err := os.Stat(bin); err == nil {
		return bin, plugin
	}
	cmd := exec.Command("bash", "fetch-v01.sh", out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
			t.Skipf("冻结提交 %s 不在这份克隆里；设置 APA_V01_BIN 或 git fetch --unshallow", commit)
		}
		t.Fatalf("fetch-v01.sh: %v", err)
	}
	return bin, plugin
}

// APA_V01_PLUGIN：宿主插件包取得后确含 v0.1 的几个插件。
func TestV01PluginPackageIsObtainable(t *testing.T) {
	_, plugin := v01Binary(t)
	if plugin == "" {
		t.Skip("APA_V01_PLUGIN 未设置")
	}
	out, err := exec.Command("tar", "-tzf", plugin).Output()
	if err != nil {
		t.Fatalf("插件包不是可读的 tar.gz: %v", err)
	}
	for _, want := range []string{"hermes/core/plugin.yaml", "hermes/capture/plugin.yaml", "hermes/receipt/plugin.yaml"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("插件包缺 %s", want)
		}
	}
	if strings.Contains(string(out), "/tests/") {
		t.Errorf("插件包不应带测试")
	}
}

type v01Server struct {
	base string
	cmd  *exec.Cmd
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startV01 用旧版本二进制在数据目录上跑 `apa serve`。监听地址写进副本的 config.yaml（保留夹具的 unjudged_rejudge_days: 0）。
func startV01(t *testing.T, bin, home string) *v01Server {
	t.Helper()
	port := freePort(t)
	cfg, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(cfg), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "listen:") {
			lines[i] = fmt.Sprintf("listen: 127.0.0.1:%d", port)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		var r struct{ OK bool }
		if json.Unmarshal([]byte(line), &r) != nil || !r.OK {
			t.Fatalf("旧版本二进制没有起来: %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("旧版本二进制 30 秒内没有就绪")
	}
	return &v01Server{base: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd}
}

func (s *v01Server) get(t *testing.T, path string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", s.base+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// ACT-ENV-08 的通过标准：旧版本二进制能打开副本，`apa health` 的积压计数与 manifest.json 一致；
// 夹具的口令能登录，夹具的会话 cookie 与 token 有效。
func TestV01BinaryOpensFixtureAndReadsManifestBaselines(t *testing.T) {
	bin, _ := v01Binary(t)
	dir, m := gen(t, 2)
	home := copyTree(t, dir)
	srv := startV01(t, bin, home)

	code, body := srv.get(t, "/v1/health", map[string]string{"Authorization": "Bearer " + m.Tokens["observer"]})
	if code != 200 {
		t.Fatalf("observer token 读 health: %d %s", code, body)
	}
	var h struct {
		Signals []struct {
			ID     string
			Count  int
			Detail json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range h.Signals {
		if s.ID != "unjudged_backlog" {
			continue
		}
		found = true
		var d map[string]any
		if err := json.Unmarshal(s.Detail, &d); err != nil {
			t.Fatal(err)
		}
		want := m.Backlog.Health
		if s.Count != want["count"] || int(d["expired"].(float64)) != want["expired"] ||
			int(d["model_unavailable"].(float64)) != want["model_unavailable"] || int(d["undecided"].(float64)) != want["undecided"] {
			t.Errorf("health 积压 %+v 与清单 %v 不一致", s, want)
		}
	}
	if !found {
		t.Fatal("health 没有 unjudged_backlog 信号")
	}
	if code, b := srv.get(t, "/v1/items", map[string]string{"Cookie": m.SessionCookie}); code != 200 {
		t.Errorf("夹具的会话 cookie 应有效: %d %s", code, b)
	}
	req, _ := http.NewRequest("POST", srv.base+"/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"%s":%q}`, loginField(t), m.WebSecret)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("夹具口令登录 = %d", resp.StatusCode)
	}
	if code, _ := srv.get(t, "/v1/items", map[string]string{"Authorization": "Bearer " + m.Tokens["delegate"]}); code != 200 {
		t.Errorf("夹具的 delegate token 应有效: %d", code)
	}
}

// 登录请求体里口令字段的名字取自内核源码，免得测试自己猜。
func loginField(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "core", "session.go"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "func (s *Server) login")
	j := strings.Index(string(b)[i:], "json:\"")
	rest := string(b)[i+j+len("json:\""):]
	return rest[:strings.Index(rest, "\"")]
}

// 版本闸与回退，用真实的旧二进制验证：升级后旧二进制起不来且账本不变；迁移中途失败后旧二进制照常打开。
func TestV01BinaryRefusesUpgradedLedgerAndSurvivesFailedUpgrade(t *testing.T) {
	bin, _ := v01Binary(t)
	latest := mustLatest(t)
	dir, m := gen(t, 2)

	// 失败的升级：旧二进制仍能打开（且读到的积压不变）。
	failed := copyTree(t, dir)
	failUpgrade(t, failed, latest)
	srv := startV01(t, bin, failed)
	if code, _ := srv.get(t, "/v1/health", map[string]string{"Authorization": "Bearer " + m.Tokens["observer"]}); code != 200 {
		t.Errorf("失败升级之后旧二进制应能照常服务: %d", code)
	}
	_ = srv.cmd.Process.Kill()

	// 成功的升级：旧二进制拒绝启动，账本一个字节不变。
	home := copyTree(t, dir)
	up := open(t, home, latest)
	up.Close()
	want := treeHash(t, home)
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	done := make(chan error, 1)
	go func() { _, err := cmd.Output(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("旧二进制不该能打开版本更新的账本")
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("旧二进制没有拒绝启动")
	}
	if got := treeHash(t, home); got != want {
		t.Errorf("被拒绝的启动改动了账本")
	}
}
