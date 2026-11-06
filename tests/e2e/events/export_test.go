//go:build e2e

package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hit 是接收器收到的一次 POST。
type hit struct {
	at                     time.Time
	eventID, ts, sig, body string
}

// receiver 是本地接收器替身：前 fail 次请求回 500，之后回 200；记录每一次请求。
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	fail int
	hits []hit
}

func newReceiver(fail int) *receiver {
	r := &receiver{fail: fail}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.hits = append(r.hits, hit{time.Now(), q.Header.Get("X-APA-Event-Id"), q.Header.Get("X-APA-Timestamp"), q.Header.Get("X-APA-Signature"), string(b)})
		if len(r.hits) <= r.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *receiver) snapshot() []hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hit(nil), r.hits...)
}

func (r *receiver) port() string { _, p, _ := net.SplitHostPort(r.Listener.Addr().String()); return p }

func validSignature(secret string, h hit) bool {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(h.ts + "." + h.body))
	return h.sig == "sha256="+hex.EncodeToString(m.Sum(nil))
}

func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".tmp-") {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

// headSeq 是账本里最后一条登记事件对应的游标。
func (f *fx) headSeq() int64 { _, next := f.feed(f.OBS, 0); return next }

// TestFlowExportExt 按 flow-export-ext 的顺序执行 ACT-EXP-01..04：导出 ext 与 webhook 转发 ext 都是
// `apa ext run` 拉起的真实独立进程，只持只读 ext 密钥，经 HTTP 订阅内核。
func TestFlowExportExt(t *testing.T) {
	f := setup(t)
	extDir := filepath.Join(repoRoot(), "ext")
	exportDir := t.TempDir()
	exportKey := f.extKey("EXP", "export")
	exportEnv := []string{"APA_EXT_DIR=" + extDir, "APA_EXT_KEY=" + exportKey, "APA_EXPORT_DIR=" + exportDir}
	cursorPath := filepath.Join(exportDir, ".apa-export-cursor")
	waitCursor := func(t *testing.T) {
		t.Helper()
		head := f.headSeq()
		eventually(t, 60*time.Second, "导出 ext 的游标追上账本 "+itoa(head), func() bool {
			b, _ := os.ReadFile(cursorPath)
			n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			return n >= head
		})
	}
	waitFile := func(t *testing.T, name, want string) {
		t.Helper()
		eventually(t, 60*time.Second, name+" 含 "+want, func() bool {
			b, _ := os.ReadFile(filepath.Join(exportDir, name))
			return strings.Contains(string(b), want)
		})
	}

	var export *proc
	t.Run("ACT-EXP-01 导出 ext 单向写到目标", func(t *testing.T) {
		list := f.start([]string{"APA_EXT_DIR=" + extDir}, "ext", "list")
		<-list.done
		var exts struct {
			Exts []struct{ Name, Kind string } `json:"exts"`
		}
		if err := json.Unmarshal([]byte(list.out.String()), &exts); err != nil {
			t.Fatalf("apa ext list 输出不是 JSON: %q", list.out.String())
		}
		kinds := map[string]string{}
		for _, x := range exts.Exts {
			kinds[x.Name] = x.Kind
		}
		if kinds["export"] != "export" || kinds["webhookforward"] != "webhook" {
			t.Fatalf("apa ext list 没有找到仓库提供的导出与 webhook 转发 ext: %v", kinds)
		}

		export = f.start(exportEnv, "ext", "run", "export")
		a := f.task("exp-01 甲", "--body", "甲的正文", "--ask", `{"question":"内部提问-不得外流","options":[{"id":"A","label":"x"}]}`)
		waitFile(t, "items/"+a+".md", "甲的正文")
		doc := readDir(t, exportDir)["items/"+a+".md"]
		if !strings.HasPrefix(doc, "---\n") || !strings.Contains(doc, "title: exp-01 甲") || !strings.Contains(doc, "phase: open") {
			t.Errorf("导出文件没有标题与阶段的 front matter:\n%s", doc)
		}
		if strings.Contains(doc, "内部提问") {
			t.Errorf("账本内部结构（ask）外流到导出文件:\n%s", doc)
		}
		f.retitle(a, "exp-01 甲改")
		waitFile(t, "items/"+a+".md", "title: exp-01 甲改")
		f.transition(a, "done")
		waitFile(t, "items/"+a+".md", "phase: done")

		// 重启续接：停掉 ext，期间改 a、建 b；重启后补齐，不遗漏。
		waitCursor(t)
		export.stop()
		before, _ := os.ReadFile(cursorPath)
		b := f.task("exp-01 乙")
		f.apa(f.DEL, 0, "item", "update", a, "--summary", "停机期间的改动", "--reason", "改", "--evidence", "验收")
		if _, err := os.Stat(filepath.Join(exportDir, "items", b+".md")); err == nil {
			t.Fatal("ext 已停止，目标目录却出现了停机后事项的导出")
		}
		export = f.start(exportEnv, "ext", "run", "export")
		waitFile(t, "items/"+b+".md", "title: exp-01 乙")
		waitFile(t, "items/"+a+".md", "停机期间的改动")
		waitCursor(t)
		if after, _ := os.ReadFile(cursorPath); string(after) == string(before) {
			t.Errorf("重启并追上后游标没有前进: %s", after)
		}

		// 重放：游标回到 0，同一批事件再来一遍，目标目录内容必须逐字节不变。
		export.stop()
		snap := readDir(t, exportDir)
		if err := os.WriteFile(cursorPath, []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		export = f.start(exportEnv, "ext", "run", "export")
		waitCursor(t)
		if got := readDir(t, exportDir); len(got) != len(snap) {
			t.Errorf("重放后文件数 %d ≠ %d", len(got), len(snap))
		} else {
			for name, want := range snap {
				if got[name] != want {
					t.Errorf("重放写坏了 %s:\n--- 之前\n%s\n--- 之后\n%s", name, want, got[name])
				}
			}
		}
	})
	if t.Failed() {
		t.Fatal("ACT-EXP-01 未通过，后续步骤不得开始")
	}

	t.Run("ACT-EXP-02 只读密钥不能写", func(t *testing.T) {
		count := func() int { return strings.Count(f.apa(f.OBS, 0, "item", "list").Stdout, `"itm_`) }
		before := count()
		body := `{"kind":"task","title":"ext 越权写入","phase":"open"}`
		if r := f.APIRaw("POST", "/v1/items", body, exportKey); r.Code != 403 && r.Code != 401 {
			t.Errorf("导出 ext 密钥 POST /v1/items: HTTP %d（期望被拒）%s", r.Code, r.Body)
		}
		if n := count(); n != before {
			t.Errorf("被拒之后事项数从 %d 变成 %d：写入生效了", before, n)
		}
		// ack：同一个 id 用渠道密钥得到"没有这个投递"，导出密钥必须在鉴权层就被拒，而不是碰巧 404。
		ack := "/v1/deliveries/dlv_nonexistent/ack"
		if r := f.APIRaw("POST", ack, `{}`, exportKey); r.Code != 403 && r.Code != 401 {
			t.Errorf("导出 ext 密钥 ack: HTTP %d（期望 401/403）%s", r.Code, r.Body)
		}
		if r := f.APIRaw("POST", ack, `{}`, f.CHAN); r.Code == 403 || r.Code == 401 || r.Code == 200 {
			t.Errorf("对照：渠道密钥 ack 不存在的投递应是 404 类错误，实际 HTTP %d %s", r.Code, r.Body)
		}
	})
	if t.Failed() {
		t.Fatal("ACT-EXP-02 未通过，后续步骤不得开始")
	}

	rcv := newReceiver(3)
	t.Cleanup(rcv.Close)
	const hookSecret = "acc-hook-secret"
	t.Run("ACT-EXP-03 webhook 转发 ext 签名外发", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "forward.cursor")
		f.start([]string{"APA_EXT_DIR=" + extDir, "APA_EXT_KEY=" + f.extKey("FWD", "forward"), "APA_WEBHOOK_URL=" + rcv.URL,
			"APA_WEBHOOK_SECRET=" + hookSecret, "APA_WEBHOOK_STATE=" + state}, "ext", "run", "webhookforward")

		id := f.task("exp-03 转发")
		eventually(t, 90*time.Second, "接收器收到 item.created "+id+" 并确认", func() bool {
			hs := rcv.snapshot()
			return len(hs) > 4 && strings.Contains(strings.Join(bodies(hs), ""), id) && hs[len(hs)-1].eventID != hs[0].eventID
		})
		hs := rcv.snapshot()
		for i, h := range hs {
			if !validSignature(hookSecret, h) {
				t.Errorf("第 %d 次请求签名无效: %+v", i, h)
			}
			var e envelope
			if json.Unmarshal([]byte(h.body), &e) != nil || itoa(e.Seq) != h.eventID {
				t.Errorf("第 %d 次请求的事件 id %q 与信封 seq 对不上: %s", i, h.eventID, h.body)
			}
		}
		// 前三次 500：同一事件 id 原样重试，间隔指数退避，第四次起被确认。
		if len(hs) < 5 {
			t.Fatalf("接收器只收到 %d 次请求", len(hs))
		}
		for i := 1; i < 4; i++ {
			if hs[i].eventID != hs[0].eventID || hs[i].body != hs[0].body {
				t.Errorf("500 之后重试的不是同一事件: %q → %q", hs[0].eventID, hs[i].eventID)
			}
		}
		gap := func(i int) time.Duration { return hs[i].at.Sub(hs[i-1].at) }
		if gap(1) < 800*time.Millisecond || gap(2) < gap(1)*3/2 || gap(3) < gap(2)*3/2 {
			t.Errorf("没有按退避重试，间隔 %v %v %v", gap(1), gap(2), gap(3))
		}
		if hs[4].eventID == hs[0].eventID {
			t.Errorf("接收器恢复后仍在重发已确认的事件 %s", hs[0].eventID)
		}
		var found bool
		for _, h := range hs {
			if strings.Contains(h.body, `"object":"`+id+`"`) && strings.Contains(h.body, `"type":"item.created"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("接收器没有收到事项 %s 的 item.created", id)
		}
		seen := map[string]bool{}
		for _, h := range hs {
			seen[h.eventID] = true
		}
		for _, e := range feedAll(f) {
			if !seen[itoa(e.Seq)] {
				t.Errorf("事件 %d（%s）丢失", e.Seq, e.Type)
			}
		}
	})
	if t.Failed() {
		t.Fatal("ACT-EXP-03 未通过，后续步骤不得开始")
	}

	t.Run("ACT-EXP-04 内核不主动外发", func(t *testing.T) {
		pid := f.ServePID()
		if pid == 0 {
			t.Fatal("没有 apa serve 进程")
		}
		listen := strings.TrimPrefix(f.URL, "http://")
		_, kernelPort, _ := net.SplitHostPort(listen)
		var viaKernel int
		for i := 0; i < 3; i++ {
			id := f.task("exp-04 事项 " + strconv.Itoa(i))
			f.retitle(id, "exp-04 改 "+strconv.Itoa(i))
			out, _ := exec.Command("lsof", "-nP", "-a", "-i", "-p", strconv.Itoa(pid)).CombinedOutput()
			for _, l := range strings.Split(string(out), "\n") {
				if !strings.Contains(l, "->") {
					continue
				}
				name := strings.Fields(l)
				local, remote, _ := strings.Cut(name[8], "->")
				if !strings.HasSuffix(local, ":"+kernelPort) {
					t.Errorf("内核有对外发起的连接: %s", l)
				}
				if strings.Contains(remote, ":"+rcv.port()) {
					t.Errorf("内核直接连了接收器: %s", l)
				}
				viaKernel++
			}
			time.Sleep(300 * time.Millisecond)
		}
		if viaKernel == 0 {
			t.Error("lsof 没有观察到任何 ext→内核 的连接，观察手段本身不可信")
		}
		n := len(rcv.snapshot())
		f.task("exp-04 收尾")
		eventually(t, 30*time.Second, "转发 ext 把新事件送到接收器", func() bool { return len(rcv.snapshot()) > n })
	})
}

func bodies(hs []hit) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.body)
	}
	return out
}

func feedAll(f *fx) []envelope { es, _ := f.feed(f.OBS, 0); return es }
