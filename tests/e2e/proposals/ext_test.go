//go:build e2e

package proposals

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func judgeYes(key string) modelstub.ChatJudge {
	return modelstub.ChatJudge{Key: modelstub.Key{Contains: key}, Answer: "yes"}
}

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// hookSink 是接收随包 webhook 转发 ext 外发请求的本地服务（替身）。
type hookSink struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newHookSink() *hookSink {
	h := &hookSink{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(b))
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return h
}

func (h *hookSink) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

// forwardOnlyCreated 运行随包的 webhook 转发 ext（只订阅 item.created）：
// 它不转发 proposal.attached 与 item.progress_added，但游标越过它们（progressSeq 是已有的 item.progress_added 序号），
// 之后的 item.created 照常被转发。
func (k *kit) forwardOnlyCreated(progressSeq int64) {
	k.t.Helper()
	var key struct {
		Secret string `json:"secret"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("POST", "/settings/ext-keys", `{"label":"FWD","scope":"forward"}`, 201)), &key))
	sink := newHookSink()
	k.t.Cleanup(sink.Close)
	state := filepath.Join(k.Root, "forward.cursor")
	cmd := exec.Command(k.Bin["apa"], "ext", "run", "webhookforward")
	cmd.Env = append(os.Environ(), "APA_HOME="+k.Home, "APA_URL="+k.URL, "APA_EXT_DIR="+filepath.Join(repoRoot(), "ext"),
		"APA_EXT_KEY="+key.Secret, "APA_WEBHOOK_URL="+sink.URL, "APA_WEBHOOK_SECRET=acc-hook-secret",
		"APA_WEBHOOK_STATE="+state, "APA_WEBHOOK_TYPES=item.created")
	cmd.Dir = repoRoot()
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	k.must(cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	k.t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	cursor := func() int64 {
		b, _ := os.ReadFile(state)
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n
	}
	if !k.WaitFor(120*time.Second, func() bool { return cursor() >= k.head() }) {
		k.t.Fatalf("转发 ext 的游标 %d 没有越过事件 %d\n%s", cursor(), k.head(), logs.String())
	}
	if cursor() < progressSeq {
		k.t.Errorf("游标 %d 应越过 item.progress_added（%d）", cursor(), progressSeq)
	}
	sent := sink.snapshot()
	if len(sent) == 0 {
		k.t.Fatal("转发 ext 应转发过历史的 item.created")
	}
	for _, body := range sent {
		var e envelope
		if json.Unmarshal([]byte(body), &e) != nil || e.Type != "item.created" {
			k.t.Errorf("转发 ext 只订阅 item.created，却转发了: %.200s", body)
		}
	}
	n := len(sent)
	id := k.openItem("prh 转发之后的新事项")
	if !k.WaitFor(60*time.Second, func() bool {
		for _, body := range sink.snapshot()[n:] {
			if strings.Contains(body, id) {
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("越过不处理的类型之后，新的 item.created %s 没有被转发\n%s", id, logs.String())
	}
}
