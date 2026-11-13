//go:build e2e

package judge

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// fixtureKernel 是待测二进制在夹具副本上运行的 `apa serve`，以夹具的 Web 口令登录（与共用环境的口令不同）。
type fixtureKernel struct {
	t       *testing.T
	base    string
	home    string
	csrf    string
	client  *http.Client
	cmd     *exec.Cmd
	cleanup func()
}

func startOnFixture(t *testing.T, e *baseline.Env, fx *baseline.Fixture) *fixtureKernel {
	t.Helper()
	listen := freeAddr(t)
	cfgPath := filepath.Join(fx.Home, "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(cfg), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "listen:") {
			lines[i] = "listen: " + listen
		}
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e.Bin["apa"], "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+fx.Home)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	k := &fixtureKernel{t: t, base: "http://" + listen, home: fx.Home, cmd: cmd}
	t.Cleanup(k.stop)
	ready := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(out).ReadString('\n'); ready <- l }()
	select {
	case l := <-ready:
		if !strings.Contains(l, `"ok":true`) {
			t.Fatalf("待测二进制没有在夹具副本上起来: %q", l)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("待测二进制 60 秒内没有就绪")
	}
	jar, _ := cookiejar.New(nil)
	k.client = &http.Client{Jar: jar, Timeout: 30 * time.Second}
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	code, body := k.do("POST", "/v1/auth/login", `{"password":"`+ledgerfixture.WebSecret+`"}`)
	if code != 200 || json.Unmarshal([]byte(body), &login) != nil || login.CSRF == "" {
		t.Fatalf("夹具口令登录失败: %d %s", code, body)
	}
	k.csrf = login.CSRF
	return k
}

func (k *fixtureKernel) stop() {
	if k.cmd != nil && k.cmd.Process != nil {
		_ = k.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = k.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = k.cmd.Process.Kill()
		}
		k.cmd = nil
	}
}

func (k *fixtureKernel) do(method, path, body string) (int, string) {
	req, _ := http.NewRequest(method, k.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if k.csrf != "" {
		req.Header.Set("X-CSRF-Token", k.csrf)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		k.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// policyVersions 读账本里全部策略版本的内容哈希（升级前后逐一比较）。
func policyVersions(t *testing.T, home string) map[int]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT version, doc FROM policy_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var doc string
		if err := rows.Scan(&v, &doc); err != nil {
			t.Fatal(err)
		}
		out[v] = hashOf(doc)
	}
	return out
}

func latestPolicyDoc(t *testing.T, home string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var doc string
	if err := db.QueryRow(`SELECT doc FROM policy_version ORDER BY version DESC LIMIT 1`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// legacyFixture 是 ACT-EPC-04、05、06：夹具副本先用旧版本二进制打开并计算策略版本哈希，再用待测版本打开另一份相同的副本。
func legacyFixture(t *testing.T, e *baseline.Env) {
	t.Helper()
	pre := e.LedgerFixture(2)
	old := pre.ServeV01(e.V01Bin())
	if code, body := old.Get("/v1/health", pre.M.Tokens["observer"]); code != 200 {
		t.Fatalf("旧版本二进制应能打开夹具副本: %d %s", code, body)
	}
	want := map[int]string{}
	var endpoint struct {
		URL                string `json:"url"`
		ScreeningTimeoutMs int    `json:"screening_timeout_ms"`
	}
	for _, d := range pre.M.PolicyDetails {
		want[d.ID] = d.SHA256
	}
	var doc struct {
		ModelEndpoint json.RawMessage `json:"model_endpoint"`
	}
	if err := json.Unmarshal([]byte(latestPolicyDoc(t, pre.Home)), &doc); err != nil || json.Unmarshal(doc.ModelEndpoint, &endpoint) != nil || endpoint.URL == "" {
		t.Fatalf("夹具最新策略版本应带 model_endpoint: %v %s", err, latestPolicyDoc(t, pre.Home))
	}

	fx := e.LedgerFixture(2)
	k := startOnFixture(t, e, fx)

	t.Run("ACT-EPC-04 旧账本里的 model_endpoint 读作生成端点", func(t *testing.T) {
		code, body := k.do("GET", "/v1/settings/model-endpoints", "")
		if code != 200 {
			t.Fatalf("GET model-endpoints: %d %s", code, body)
		}
		got := jsonMap(t, body)
		g := sub(got, "generate_endpoint")
		eq(t, "generate_endpoint.url", g["url"], endpoint.URL)
		eq(t, "generate_endpoint.locality", g["locality"], "external")
		if got["judge_endpoint"] != nil {
			t.Errorf("judge_endpoint 应为 null: %v", got["judge_endpoint"])
		}
		if endpoint.ScreeningTimeoutMs != 0 {
			eq(t, "screening_timeout_ms", got["screening_timeout_ms"], float64(endpoint.ScreeningTimeoutMs))
		}
		after := policyVersions(t, fx.Home)
		for id, sum := range want {
			if after[id] != sum {
				t.Errorf("策略版本 %d 的内容哈希改变: %s → %s", id, sum, after[id])
			}
		}
		if len(after) != len(want) {
			t.Errorf("升级不应新增策略版本: %d → %d", len(want), len(after))
		}
	})

	t.Run("ACT-EPC-06 本人在 Web 保存后写成新字段，旧版本不变", func(t *testing.T) {
		_, body := k.do("GET", "/v1/settings/model-endpoints", "")
		got := jsonMap(t, body)
		save := map[string]any{"generate_endpoint": stripResponseOnly(sub(got, "generate_endpoint")), "screening_timeout_ms": got["screening_timeout_ms"]}
		if j := sub(got, "judge_endpoint"); j != nil {
			save["judge_endpoint"] = stripResponseOnly(j)
		}
		if code, b := k.do("PUT", "/v1/settings/model-endpoints", mustJSON(save)); code != 200 {
			t.Fatalf("保存: %d %s", code, b)
		}
		after := policyVersions(t, fx.Home)
		if len(after) != len(want)+1 {
			t.Fatalf("应新增一个策略版本: %d → %d", len(want), len(after))
		}
		for id, sum := range want {
			if after[id] != sum {
				t.Errorf("策略版本 %d 的内容哈希改变", id)
			}
		}
		latest := latestPolicyDoc(t, fx.Home)
		for _, must := range []string{"generate_endpoint", "screening_timeout_ms"} {
			if !strings.Contains(latest, must) {
				t.Errorf("新版本缺少 %s: %s", must, latest)
			}
		}
		if strings.Contains(latest, `"model_endpoint"`) {
			t.Errorf("新版本不应再有 model_endpoint: %s", latest)
		}
	})

	t.Run("ACT-EPC-05 旧形状的 PUT 请求仍被接受", func(t *testing.T) {
		j := map[string]any{"url": "http://127.0.0.1:1/judge", "protocol": "probability-judgment/1", "locality": "local"}
		if code, b := k.do("PUT", "/v1/settings/model-endpoints", mustJSON(map[string]any{"judge_endpoint": j})); code != 200 {
			t.Fatalf("设置判断端点 J: %d %s", code, b)
		}
		legacy := `{"model_endpoint": {"url": "http://127.0.0.1:2/v1", "model": "acc-model", "api_key": "k1", "screening_timeout_ms": 45000}}`
		code, b := k.do("PUT", "/v1/settings/model-endpoints", legacy)
		if code != 200 {
			t.Fatalf("旧形状 PUT: %d %s", code, b)
		}
		got := jsonMap(t, b)
		g := sub(got, "generate_endpoint")
		eq(t, "generate.url", g["url"], "http://127.0.0.1:2/v1")
		eq(t, "generate.model", g["model"], "acc-model")
		eq(t, "generate.locality", g["locality"], "external")
		eq(t, "screening_timeout_ms（顶层）", got["screening_timeout_ms"], 45000.0)
		eq(t, "judge_endpoint 保持 J", sub(got, "judge_endpoint")["url"], "http://127.0.0.1:1/judge")
	})
}

// stripResponseOnly 去掉只在响应里出现的字段（api_key_set），得到 Web 页面保存时提交的形状。
func stripResponseOnly(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != "api_key_set" {
			out[k] = v
		}
	}
	return out
}
