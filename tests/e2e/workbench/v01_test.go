//go:build e2e

package workbench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// v01Instance 是旧版本二进制在全新空目录里起的实例（ACT-WBN-14）。
type v01Instance struct {
	bin, home, url, session, csrf string
}

func (v *v01Instance) do(t *testing.T, method, path, body, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, v.url+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if v.session != "" {
		req.Header.Set("Cookie", "apa_session="+v.session)
		req.Header.Set("X-CSRF-Token", v.csrf)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("v0.1 %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == "apa_session" {
			v.session = c.Value
		}
	}
	return resp.StatusCode, string(b)
}

func startV01(t *testing.T, bin string) *v01Instance {
	t.Helper()
	home, err := os.MkdirTemp("", "apa-e2e-v01-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cmd := exec.Command(bin, "init", "--password-stdin")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	cmd.Stdin = strings.NewReader(baseline.Password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("v0.1 apa init: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfgPath := filepath.Join(home, "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(cfg), "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "listen:") {
			lines[i] = "listen: " + addr
		}
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(bin, "serve")
	srv.Env = append(os.Environ(), "APA_HOME="+home)
	out, _ := srv.StdoutPipe()
	srv.Stderr = io.Discard
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		var r struct{ OK bool }
		if json.Unmarshal([]byte(line), &r) != nil || !r.OK {
			t.Fatalf("v0.1 实例没有起来: %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("v0.1 实例 30 秒内没有就绪")
	}
	v := &v01Instance{bin: bin, home: home, url: "http://" + addr}
	code, body := v.do(t, "POST", "/v1/auth/login", `{"password":"`+baseline.Password+`"}`, "")
	var lg struct {
		CSRF string `json:"csrf_token"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &lg) != nil {
		t.Fatalf("v0.1 登录失败 %d %s", code, body)
	}
	v.csrf = lg.CSRF
	return v
}

// apa 以 token 运行该实例对应版本的 apa。
func (v *v01Instance) apa(t *testing.T, token string, args ...string) (int, string) {
	return runApa(t, v.bin, v.home, v.url, token, args...)
}

func runApa(t *testing.T, bin, home, url, token string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+home, "APA_URL="+url, "APA_TOKEN="+token)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("apa %v: %v", args, err)
	}
	return code, out.String()
}

func keysOfFirst(t *testing.T, out string) []string {
	t.Helper()
	var l struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil || len(l.Items) == 0 {
		t.Fatalf("list 输出不含事项: %v\n%s", err, out)
	}
	var ks []string
	for k := range l.Items[0] {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// v01CompatList 对 v0.1 实例与当前实例各建一条带 t1 标签的 open 事项，比较 `apa item list` 的旧参数与输出键。
func (k *kit) v01CompatList(t *testing.T) {
	v := startV01(t, k.V01Bin())
	// v0.1：同名项目、observer token、带 t1 标签的 open 事项
	code, body := v.do(t, "POST", "/v1/projects", `{"name":"acc-work"}`, "")
	var p struct {
		ID string `json:"id"`
	}
	if code != 201 || json.Unmarshal([]byte(body), &p) != nil {
		t.Fatalf("v0.1 新建项目: %d %s", code, body)
	}
	code, body = v.do(t, "POST", "/v1/settings/tokens", `{"kind":"observer","label":"OBS"}`, "")
	var tok struct {
		Secret string `json:"secret"`
	}
	if code != 201 || json.Unmarshal([]byte(body), &tok) != nil {
		t.Fatalf("v0.1 签发 observer: %d %s", code, body)
	}
	item := `{"kind":"task","title":"V01-T1","phase":"open","project":"` + p.ID + `","tags":["t1"]}`
	if code, body = v.do(t, "POST", "/v1/items", item, ""); code != 201 {
		t.Fatalf("v0.1 新建事项: %d %s", code, body)
	}
	k.webOK("POST", "/items", `{"kind":"task","title":"V02-T1","phase":"open","project":"`+k.Projects["acc-work"]+`","tags":["t1"]}`, 201)

	// --project 取项目 id：两个实例各自的 acc-work。
	c1, o1 := v.apa(t, tok.Secret, "item", "list", "--phase", "open", "--kind", "task", "--project", p.ID)
	r2 := k.Apa(k.OBS, "", "item", "list", "--phase", "open", "--kind", "task", "--project", k.Projects["acc-work"])
	if c1 != 0 || r2.Code != 0 {
		t.Fatalf("旧参数被拒: v0.1 退出码 %d，v0.2.0 退出码 %d\n%s\n%s", c1, r2.Code, o1, r2.Stderr)
	}
	old, cur := keysOfFirst(t, o1), keysOfFirst(t, r2.Stdout)
	for _, key := range old {
		if !contains(cur, key) {
			t.Errorf("v0.1 的键 %q 在 v0.2.0 输出里缺失或改名", key)
		}
	}
	var added []string
	for _, key := range cur {
		if !contains(old, key) {
			added = append(added, key)
		}
	}
	for _, key := range added {
		if !contains([]string{"planned_for", "progress", "proposal", "version"}, key) {
			t.Logf("v0.2.0 新增键: %s", key)
		}
	}
	// v0.1 的 CLI 没有 --tag（只有 HTTP 的 tag 参数），标签筛选经 HTTP 比较。
	_, h1 := v.do(t, "GET", "/v1/items?phase=open&tag=t1", "", tok.Secret)
	if !strings.Contains(h1, "V01-T1") || !strings.Contains(k.APIRaw("GET", "/v1/items?phase=open&tag=t1", "", k.OBS).Body, "V02-T1") {
		t.Errorf("tag 筛选语义不同: v0.1=%s", h1)
	}
	// 筛选语义相同：两边各只返回自己那条
	if !strings.Contains(o1, "V01-T1") || !strings.Contains(r2.Stdout, "V02-T1") {
		t.Errorf("筛选语义不同: v0.1=%s v0.2.0=%s", o1, r2.Stdout)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
