//go:build e2e

// Package events 按验收文档 flow-event-subscribe（ACT-EVT-01..04）与 flow-export-ext（ACT-EXP-01..04）
// 用真实 apa 二进制、仓库提供的导出 ext 与 webhook 转发 ext 独立执行"订阅账本变化、至少一次、游标自持"。
// 环境来自 flow-env-baseline 的共用脚本；接收器是测试内的本地 HTTP 服务（替身），其余全是真实进程与真实 HTTP。
//
// 只改测试，不改产品代码。
package events

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

func setup(t *testing.T) *fx { t.Helper(); return &fx{baseline.Setup(t), t} }

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// task 用 DEL 代本人建一个 open 的任务事项，返回 id。
func (f *fx) task(title string, extra ...string) string {
	f.t.Helper()
	args := append([]string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务"}, extra...)
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, args...).Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v", err)
	}
	return m.ID
}

func (f *fx) retitle(id, title string) {
	f.t.Helper()
	f.apa(f.DEL, 0, "item", "update", id, "--title", title, "--reason", "改标题", "--evidence", "验收")
}

func (f *fx) transition(id, to string) {
	f.t.Helper()
	f.apa(f.DEL, 0, "item", "transition", id, "--to", to, "--reason", "推进", "--evidence", "验收")
}

// extKey 在 Web 设置里签发一把 ext 密钥，返回明文。
func (f *fx) extKey(label, scope string) string {
	f.t.Helper()
	var out struct {
		Secret string `json:"secret"`
	}
	body, _ := json.Marshal(map[string]string{"label": label, "scope": scope})
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/ext-keys", string(body), 201)), &out); err != nil || out.Secret == "" {
		f.t.Fatalf("签发 ext 密钥 %s 没有返回明文: %v", scope, err)
	}
	return out.Secret
}

// envelope 是 GET /v1/events 与 `apa events` 输出的信封。
type envelope struct {
	Seq    int64           `json:"seq"`
	Type   string          `json:"type"`
	Object string          `json:"object"`
	Title  string          `json:"title"`
	Actor  json.RawMessage `json:"actor"`
	At     string          `json:"at"`
}

// feed 读 `GET /v1/events?cursor=` 的一页（不等待）。
func (f *fx) feed(token string, cursor int64) (events []envelope, next int64) {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/events?cursor="+itoa(cursor)+"&limit=500", "", token)
	if r.Code != 200 {
		f.t.Fatalf("GET /v1/events: HTTP %d %s", r.Code, r.Body)
	}
	var page struct {
		Events []envelope `json:"events"`
		Next   int64      `json:"next"`
	}
	if err := json.Unmarshal([]byte(r.Body), &page); err != nil {
		f.t.Fatal(err)
	}
	return page.Events, page.Next
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// lines 把输出按行解析成信封；半行（还没写完）留在后面不解析。
func lines(out string) []envelope {
	var es []envelope
	for _, l := range strings.Split(out, "\n") {
		var e envelope
		if strings.TrimSpace(l) != "" && json.Unmarshal([]byte(l), &e) == nil {
			es = append(es, e)
		}
	}
	return es
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// proc 是后台运行的 apa 子进程（独立进程组，停止时整组收 SIGTERM，包括 `go run` 拉起的 ext）。
type proc struct {
	cmd  *exec.Cmd
	out  *syncBuf
	done chan struct{}
	t    *testing.T
}

func (f *fx) start(env []string, args ...string) *proc {
	f.t.Helper()
	p := &proc{out: &syncBuf{}, done: make(chan struct{}), t: f.t}
	p.cmd = exec.Command(f.Bin["apa"], args...)
	p.cmd.Env = append(os.Environ(), append([]string{"APA_HOME=" + f.Home, "APA_URL=" + f.URL}, env...)...)
	p.cmd.Dir = repoRoot()
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.cmd.Start(); err != nil {
		f.t.Fatalf("启动 apa %v: %v", args, err)
	}
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	f.t.Cleanup(p.stop)
	return p
}

func (p *proc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// eventually 每 100ms 检查一次条件，timeout 内不成立就失败。
func eventually(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); ; time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s 内没有等到：%s", timeout, what)
		}
	}
}
