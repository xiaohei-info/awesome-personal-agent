//go:build e2e

// Package upgrade 是全流程验收 flow-upgrade-rollback 与 flow-backup-restore 的唯一 owner（门槛 2）：
// 测试构建的真实 apa 二进制 + 合成账本夹具（每个已冻结结构版本一份）+ 旧版本二进制，
// 只替身模型端点；不使用真实模型、宿主、渠道账号。离不开真实依赖的断言由本机升级验证 L1–L12 承接。
// Web 步骤以夹具口令登录后请求 Web 页面调用的同一组 /v1 接口（与其他 tests/e2e 流程一致），页面渲染由 web/ 组件测试覆盖。
package upgrade

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

type lab struct {
	*baseline.Env
	t   *testing.T // 当前步骤的 t
	top *testing.T // 整个测试的 t：长期存在的进程在它结束时才清理
}

func newLab(t *testing.T) *lab { t.Helper(); return &lab{baseline.New(t), t, t} }

func (l *lab) apaBin() string { return l.Bin["apa"] }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func cleanEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "APA_") {
			env = append(env, kv)
		}
	}
	env = append(env, "APA_HOME="+home)
	return append(env, extra...)
}

// run 以 bin 在数据目录 home 上运行 apa 子命令；extra 为附加环境（APA_URL、APA_TOKEN 等）。
func (l *lab) run(bin, home string, extra []string, args ...string) baseline.Result {
	l.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = cleanEnv(home, extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("运行 %s %v: %v", bin, args, err)
	}
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		l.t.Fatalf("%s %v 90 秒未结束", filepath.Base(bin), args)
	}
	r := baseline.Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		l.t.Fatalf("运行 %v: %v", args, err)
	}
	return r
}

// apa 运行待测二进制并断言退出码。
func (l *lab) apa(home string, extra []string, want int, args ...string) baseline.Result {
	l.t.Helper()
	r := l.run(l.apaBin(), home, extra, args...)
	if r.Code != want {
		l.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// ---- 服务进程 ----

type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuf
	done chan struct{}
	URL  string
	Home string
}

var listenRe = regexp.MustCompile(`(?m)^listen:.*$`)

func freePort() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// setListen 把 config.yaml 的 listen 改为 addr，其余行（含夹具的 unjudged_rejudge_days: 0）保持。
func setListen(t *testing.T, home, addr string) {
	t.Helper()
	p := filepath.Join(home, "config.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	nb := listenRe.ReplaceAllString(string(b), "listen: "+addr)
	if !strings.Contains(nb, "listen: "+addr) {
		nb = "listen: " + addr + "\n" + nb
	}
	if err := os.WriteFile(p, []byte(nb), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listenOf(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^listen:\s*(\S+)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("config.yaml 没有 listen")
	}
	return m[1]
}

// tryServe 用 bin 在 home 上启动 `apa serve`（监听改为本机空闲端口）。就绪（stdout 出现 "ok":true）返回 server；
// 在就绪前退出则返回 nil 与退出结果（退出码、全部输出）。
func (l *lab) tryServe(bin, home string, extra ...string) (*server, baseline.Result) {
	l.t.Helper()
	setListen(l.t, home, "127.0.0.1:"+freePort())
	return l.tryServeAsIs(bin, home, extra...)
}

// tryServeAsIs 同 tryServe，但沿用 config.yaml 里已有的 listen（不改任何文件）。
func (l *lab) tryServeAsIs(bin, home string, extra ...string) (*server, baseline.Result) {
	l.t.Helper()
	_, port, _ := net.SplitHostPort(listenOf(l.t, home))
	cmd := exec.Command(bin, "serve")
	cmd.Env = cleanEnv(home, extra...)
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("apa serve: %v", err)
	}
	s := &server{t: l.t, cmd: cmd, out: out, done: make(chan struct{}), URL: "http://127.0.0.1:" + port, Home: home}
	go func() { _ = cmd.Wait(); close(s.done) }()
	l.top.Cleanup(s.stop)
	deadline := time.After(40 * time.Second)
	for {
		select {
		case <-s.done:
			return nil, baseline.Result{Code: cmd.ProcessState.ExitCode(), Stdout: out.String()}
		case <-deadline:
			l.t.Fatalf("apa serve 40 秒内未就绪也未退出: %s", out.String())
		case <-time.After(50 * time.Millisecond):
			if strings.Contains(out.String(), `"ok":true`) {
				return s, baseline.Result{}
			}
		}
	}
}

func (l *lab) serve(bin, home string, extra ...string) *server {
	l.t.Helper()
	s, r := l.tryServe(bin, home, extra...)
	if s == nil {
		l.t.Fatalf("%s serve 提前退出（%d）: %s", filepath.Base(bin), r.Code, r.Stdout)
	}
	return s
}

func (s *server) Log() string { return s.out.String() }

func (s *server) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// ---- HTTP ----

type resp struct {
	Code int
	Body string
	Hdr  http.Header
}

func httpDo(t *testing.T, method, url, body string, hdr map[string]string) resp {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, string(b), r.Header}
}

func (s *server) get(token, path string) resp {
	s.t.Helper()
	return httpDo(s.t, "GET", s.URL+path, "", map[string]string{"Authorization": "Bearer " + token})
}

// webSession 以口令登录，返回会话 Cookie 与 CSRF 令牌。
func (s *server) webLogin(secret string) (code int, cookie, csrf string) {
	s.t.Helper()
	r := httpDo(s.t, "POST", s.URL+"/v1/auth/login", `{"password":`+quote(secret)+`}`, nil)
	for _, c := range (&http.Response{Header: r.Hdr}).Cookies() {
		if c.Name == "apa_session" {
			cookie = "apa_session=" + c.Value
		}
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal([]byte(r.Body), &out)
	return r.Code, cookie, out.CSRF
}

func (s *server) web(cookie, csrf, method, path, body string) resp {
	s.t.Helper()
	return httpDo(s.t, method, s.URL+path, body, map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// ---- 账本（只读 sqlite，等价验收文档的 `sqlite3 -readonly`） ----

func openRO(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openRWPath(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openRW(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// tableCounts 与夹具清单同一口径：全部表（不含 sqlite_ 内部表与全文索引影子表）的行数。
func tableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'material_search_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + n).Scan(&c); err != nil { // 表名来自 sqlite_master
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func one[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

type dbEvent struct {
	Seq              int64
	Type, Object     string
	Actor, Payload   string
	ActorType, Via   string
	ActorPrincipal   string
	PayloadHashLower string
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func events(t *testing.T, db *sql.DB, where string, args ...any) []dbEvent {
	t.Helper()
	rows, err := db.Query(`SELECT seq, type, object, actor, payload FROM eventlog_event `+where+` ORDER BY seq`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []dbEvent
	for rows.Next() {
		var e dbEvent
		if err := rows.Scan(&e.Seq, &e.Type, &e.Object, &e.Actor, &e.Payload); err != nil {
			t.Fatal(err)
		}
		var a struct{ Type, Principal, Via string }
		_ = json.Unmarshal([]byte(e.Actor), &a)
		e.ActorType, e.ActorPrincipal, e.Via = a.Type, a.Principal, a.Via
		e.PayloadHashLower = sha(e.Payload)
		out = append(out, e)
	}
	return out
}

// ---- 数据目录的文件清单与哈希（等价 `find . -type f | sort | xargs sha256sum`，另含目录） ----

func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		switch {
		case d.IsDir():
			out[rel] = "dir"
		case d.Type().IsRegular():
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = sha(string(b))
		default:
			out[rel] = "other"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for k, v := range before {
		if a, ok := after[k]; !ok {
			diffs = append(diffs, "缺少 "+k)
		} else if a != v {
			diffs = append(diffs, "改动 "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			diffs = append(diffs, "新增 "+k)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Errorf("%s：数据目录前后不一致：%s", what, strings.Join(diffs, "; "))
	}
}

func without(m map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if k != prefix && !strings.HasPrefix(k, prefix+"/") {
			out[k] = v
		}
	}
	return out
}

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha(string(b))
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
	return v
}

func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s（等待 %s 仍未出现）", what, d)
		}
	}
}

func contains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s 缺少 %q：\n%s", what, sub, s)
		}
	}
}

func itoa(n int) string { return fmt.Sprint(n) }
