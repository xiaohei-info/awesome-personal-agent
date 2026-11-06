//go:build e2e

// Package ops 是全流程验收 flow-health、flow-purge、flow-retention 的唯一 owner
// （验收文档 ACT-HLT-01..02、ACT-PRG-01..05、ACT-RET-01..02）：真实 apa 二进制 + 真实内核进程，
// 只替身模型端点与无头运行。Web 步骤以本人会话请求 Web 页面调用的同一组 /v1 接口（与其他 tests/e2e 流程一致），
// 页面渲染由 web/ 组件测试覆盖。
package ops

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

func newFx(t *testing.T) *fx { t.Helper(); return &fx{baseline.Setup(t), t} }

// apa 运行命令并断言退出码。
func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// webOK 以本人 Web 会话请求并断言状态码，返回响应体。
func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) file(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
	return v
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// waitFor 轮询直到 cond 为真。
func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(end) {
			f.t.Fatalf("%s（等待 %s 仍未出现）", what, d)
		}
	}
}

// step 顺序执行流程步骤；前一步未通过，后续不得开始。
func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

type event struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Title  string `json:"title"`
	Actor  struct {
		Principal string `json:"principal"`
	} `json:"actor"`
	Tombstone bool `json:"tombstone"`
}

// feed 读出事件流里 cursor 之后的全部信封（`apa events --cursor`）；每行一个 JSON。
func (f *fx) feed(cursor int64) []event {
	f.t.Helper()
	r := f.apa(f.OBS, 0, "events", "--cursor", itoa(cursor))
	var out []event
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if line == "" {
			continue
		}
		out = append(out, decode[event](f.t, line))
	}
	return out
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// grepHome 返回 APA_HOME 下内容含 needle 的文件（等价 `grep -rl needle $APA_HOME`）。
func (f *fx) grepHome(needle string) []string {
	f.t.Helper()
	var hits []string
	err := filepath.Walk(f.Home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return hits
}

// itemIDs 返回全部阶段的事项 id 集合。
func (f *fx) itemIDs() map[string]bool {
	f.t.Helper()
	out := decode[struct {
		Items []struct{ ID string } `json:"items"`
	}](f.t, f.apa(f.OBS, 0, "item", "list").Stdout)
	m := map[string]bool{}
	for _, it := range out.Items {
		m[it.ID] = true
	}
	return m
}

// capture 以 WRK 提交一段对话增量；session 区分同一内容的不同会话。
func (f *fx) capture(project, text, session string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text, "project": f.Projects[project]})
	req, _ := http.NewRequest("POST", f.URL+"/v1/capture", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.WRK)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APA-Session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"captured":true`) {
		f.t.Fatalf("capture: %d %s", resp.StatusCode, b)
	}
}

// vacuumLedger 对已停止的内核的账本做 VACUUM，清掉 SQLite 空闲页里残留的已删除内容。
func (f *fx) vacuumLedger() {
	f.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.Home, "ledger.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM"); err != nil {
		f.t.Fatalf("VACUUM: %v", err)
	}
}
