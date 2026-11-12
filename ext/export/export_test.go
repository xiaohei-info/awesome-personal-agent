package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernel 是测试边界内的内核替身：按 §10.1 的信封形状回事件，按 /items、/materials 回对象。记录每个请求。
type fakeKernel struct {
	mu      sync.Mutex
	events  []map[string]any  // 每条带 seq
	objects map[string]string // path -> JSON body
	status  map[string]int    // path -> 强制状态码
	reqs    []string          // "METHOD path?query"
	auths   []string
}

func (k *fakeKernel) add(seq int, typ, obj string) {
	k.events = append(k.events, map[string]any{"seq": seq, "type": typ, "object": obj, "title": "t", "at": "2026-01-01T00:00:00Z", "actor": map[string]any{}})
}

func (k *fakeKernel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reqs = append(k.reqs, r.Method+" "+r.URL.RequestURI())
	k.auths = append(k.auths, r.Header.Get("Authorization"))
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	if code := k.status[p]; code != 0 {
		w.WriteHeader(code)
		fmt.Fprint(w, `{"why":"forced","code":"internal"}`)
		return
	}
	if p == "/items" {
		if _, ok := k.objects["/items"]; !ok { // 没有显式给列表就由各事项对象拼出
			list := []json.RawMessage{}
			paths := make([]string, 0, len(k.objects))
			for path := range k.objects {
				paths = append(paths, path)
			}
			sort.Strings(paths) // 与内核一致：按 id 排序
			for _, path := range paths {
				b := k.objects[path]
				if strings.HasPrefix(path, "/items/") && strings.Count(path, "/") == 2 {
					var d struct {
						Item json.RawMessage `json:"item"`
					}
					json.Unmarshal([]byte(b), &d)
					list = append(list, d.Item)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": list})
			return
		}
	}
	if p == "/materials" {
		if _, ok := k.objects["/materials"]; !ok {
			list := []map[string]string{}
			for path := range k.objects {
				if strings.HasPrefix(path, "/materials/") && strings.Count(path, "/") == 2 {
					list = append(list, map[string]string{"id": strings.TrimPrefix(path, "/materials/")})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"materials": list})
			return
		}
	}
	if p == "/events" {
		cur, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		out, next := []map[string]any{}, cur
		for _, e := range k.events {
			if s := e["seq"].(int); s > cur {
				out = append(out, e)
				next = s
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"events": out, "next": next})
		return
	}
	if b, ok := k.objects[p]; ok {
		fmt.Fprint(w, b)
		return
	}
	w.WriteHeader(404)
	fmt.Fprint(w, `{"code":"not_found","why":"no"}`)
}

func (k *fakeKernel) count(prefix string) (n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, r := range k.reqs {
		if strings.Contains(r, prefix) {
			n++
		}
	}
	return
}

func newKernel() *fakeKernel {
	return &fakeKernel{objects: map[string]string{}, status: map[string]int{}}
}

const itemJSON = `{"item":{"id":"itm_aa","kind":"task","phase":"open","title":"写周报","summary":"s","body":"正文第一行\n第二行","project":"p1","tags":["a","b"],"version":3,"ask":{"secret":"no"}},"relations":[]}`
const matJSON = `{"material":{"id":"mat_bb","kind":"spec","title":"规范","project":"p1","current_rev":"rev_1"},"revision":{"id":"rev_1","seq":1,"content":"# 规范\n内容","state":"confirmed"}}`
const hostedJSON = `{"material":{"id":"mat_cc","kind":"doc","title":"托管","location":"repo://x/doc.md"},"hosted":{"state":"ok"}}`

// runUntil 运行导出直到 cond 成立（或超时失败），再取消。
func runUntil(t *testing.T, k *fakeKernel, dir string, cond func() bool) {
	t.Helper()
	o, err := loadOptions("")
	if err != nil {
		t.Fatal(err)
	}
	runUntilOpts(t, k, dir, o, cond)
}

func runUntilOpts(t *testing.T, k *fakeKernel, dir string, o Options, cond func() bool) {
	t.Helper()
	srv := httptest.NewServer(k)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{URL: srv.URL, Key: "ext-key", Dir: dir, Wait: 0, Idle: 5 * time.Millisecond, Backoff: 5 * time.Millisecond, Opts: o})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !cond() {
		t.Fatal("condition not reached")
	}
}

func defaultOpts(t *testing.T) Options {
	t.Helper()
	o, err := loadOptions("")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// flow-export-ext / ACT-EXP-01：事项与材料事件被导出成本地 Markdown，游标落盘，全程只读且带 ext 密钥。
func TestExportsItemsAndMaterialsOneWayWithCursor(t *testing.T) {
	k := newKernel()
	k.add(1, "item.created", "itm_aa")
	k.add(2, "item.updated", "itm_aa") // 同页同对象只取一次
	k.add(3, "material.revised", "mat_bb")
	k.add(4, "material.revised", "mat_cc")
	k.add(5, "decision.asked", "itm_aa") // 非导出类型：忽略
	k.objects["/items/itm_aa"] = itemJSON
	k.objects["/materials/mat_bb"] = matJSON
	k.objects["/materials/mat_cc"] = hostedJSON
	dir := t.TempDir()
	runUntil(t, k, dir, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".apa-export-cursor"))
		return err == nil && k.count("cursor=5") > 0
	})

	it := read(t, filepath.Join(dir, "items", "itm_aa.md"))
	for _, want := range []string{"title: 写周报", "phase: open", "正文第一行\n第二行", "- a"} {
		if !strings.Contains(it, want) {
			t.Errorf("item file lacks %q:\n%s", want, it)
		}
	}
	if strings.Contains(it, "secret") {
		t.Errorf("item file leaks non-exported field:\n%s", it)
	}
	if m := read(t, filepath.Join(dir, "materials", "mat_bb.md")); !strings.Contains(m, "# 规范\n内容") || !strings.Contains(m, "title: 规范") {
		t.Errorf("material file:\n%s", m)
	}
	h := read(t, filepath.Join(dir, "materials", "mat_cc.md"))
	if !strings.Contains(h, "location: repo://x/doc.md") || strings.Contains(h, "content") {
		t.Errorf("hosted material must export location only:\n%s", h)
	}
	if got := strings.TrimSpace(read(t, filepath.Join(dir, ".apa-export-cursor"))); got != "5" {
		t.Errorf("cursor = %q, want 5", got)
	}
	// 启动对账取一次；同一页里两条 itm_aa 事件合并成一次。
	if n := k.count("GET /v1/items/itm_aa"); n != 2 {
		t.Errorf("item fetched %d times, want 2 (sync + one deduplicated page)", n)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for i, r := range k.reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("non-read request %q", r)
		}
		if k.auths[i] != "Bearer ext-key" {
			t.Errorf("auth = %q", k.auths[i])
		}
	}
}

// 自持游标：重启后从落盘游标续读，不重放已处理事件。
func TestResumesFromPersistedCursor(t *testing.T) {
	k := newKernel()
	k.add(1, "item.created", "itm_aa")
	k.objects["/items/itm_aa"] = itemJSON
	dir := t.TempDir()
	k.objects["/items"] = `{"items":[]}` // 账本列表里没有它：只有重放事件才会去取
	os.WriteFile(filepath.Join(dir, ".apa-export-cursor"), []byte("1\n"), 0o600)
	runUntil(t, k, dir, func() bool { return k.count("GET /v1/events?cursor=1") > 0 })
	if k.count("/items/") != 0 {
		t.Error("replayed an already-processed event")
	}
	if _, err := os.Stat(filepath.Join(dir, "items")); err == nil {
		t.Error("nothing should be exported")
	}
}

// 至少一次：取对象失败时游标不前进，恢复后重试并导出。
func TestCursorHoldsOnFetchFailureThenRetries(t *testing.T) {
	k := newKernel()
	k.add(1, "item.created", "itm_aa")
	k.objects["/items/itm_aa"] = itemJSON
	k.status["/items/itm_aa"] = 503
	dir := t.TempDir()
	srv := httptest.NewServer(k)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{URL: srv.URL, Key: "k", Dir: dir, Idle: 5 * time.Millisecond, Backoff: 5 * time.Millisecond, Opts: defaultOpts(t)})
	}()
	for i := 0; k.count("GET /v1/items/itm_aa") < 2 && i < 1000; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, ".apa-export-cursor")); err == nil {
		t.Error("cursor advanced past a failed event")
	}
	k.mu.Lock()
	delete(k.status, "/items/itm_aa")
	k.mu.Unlock()
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(filepath.Join(dir, "items", "itm_aa.md")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "items", "itm_aa.md")); err != nil {
		t.Error("item never exported after the failure cleared")
	}
	cancel()
	<-done
}

// 对象已不存在（404）不卡住游标；凭据被拒（401/403）直接退出并暴露错误。
func TestMissingObjectSkippedButRevokedKeyIsFatal(t *testing.T) {
	k := newKernel()
	k.add(1, "item.created", "itm_gone")
	dir := t.TempDir()
	runUntil(t, k, dir, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, ".apa-export-cursor"))
		return strings.TrimSpace(string(b)) == "1"
	})

	k2 := newKernel()
	k2.status["/events"] = 401
	srv := httptest.NewServer(k2)
	defer srv.Close()
	err := Run(context.Background(), Config{URL: srv.URL, Key: "revoked", Dir: t.TempDir(), Idle: time.Millisecond, Backoff: time.Millisecond, Opts: defaultOpts(t)})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want fatal 401", err)
	}
}

// 服务端给出的 id 不可信：含路径分隔符的对象 id 不得写出导出目录。
func TestHostileObjectIDCannotEscapeDir(t *testing.T) {
	k := newKernel()
	k.add(1, "item.created", "../../evil")
	dir := t.TempDir()
	srv := httptest.NewServer(k)
	defer srv.Close()
	err := Run(context.Background(), Config{URL: srv.URL, Key: "k", Dir: dir, Idle: time.Millisecond, Backoff: time.Millisecond, Opts: defaultOpts(t)})
	if err == nil {
		t.Fatal("expected error for unsafe id")
	}
	if k.count("evil") != 0 {
		t.Error("requested an object with an unsafe id")
	}
}

func TestConfigFromEnv(t *testing.T) {
	if _, err := ConfigFromEnv(func(string) string { return "" }); err == nil {
		t.Error("missing env must be an error")
	}
	env := map[string]string{"APA_URL": "http://k", "APA_EXT_KEY": "s", "APA_EXPORT_DIR": "/o"}
	c, err := ConfigFromEnv(func(n string) string { return env[n] })
	if err != nil || c.URL != "http://k" || c.Key != "s" || c.Dir != "/o" || c.Wait != 50 {
		t.Errorf("%+v %v", c, err)
	}
}
