//go:build e2e

// Package sourcepull 按验收文档 flow-source-contract、flow-source-rss、flow-source-http-json、flow-source-dir-watch
// 用真实 apa 二进制和随发行附带的拉取程序（rss-source、http-json、dir-watch）走完整流程。
// 来源按 1 秒周期登记以缩短等待（登记允许的最小周期，其余机制与分钟级周期相同）；
// 外部世界用本地替身：httptest 服务提供 RSS/JSON，ACC_SOURCE 是受测试控制的脚本。
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
package sourcepull

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

// 步骤等待上限：周期 1 秒，退避最多隔几个节拍。
const waitMax = 30 * time.Second

type fx struct {
	*baseline.Env
	t *testing.T
}

var (
	progOnce sync.Once
	progDir  string
	progErr  error
)

// programs 每个测试进程只构建一次随发行附带的拉取程序。
func programs(t *testing.T) map[string]string {
	t.Helper()
	progOnce.Do(func() {
		progDir, progErr = os.MkdirTemp("", "apa-e2e-sources-")
		if progErr != nil {
			return
		}
		root := filepath.Join("..", "..", "..")
		for name, pkg := range map[string]string{
			"rss-source": "./sources/rss/cmd/rss-source",
			"http-json":  "./sources/httpjson/cmd/http-json",
			"dir-watch":  "./sources/dirwatch/cmd/dir-watch",
		} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(progDir, name), pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				progErr = &buildError{pkg, string(out), err}
				return
			}
		}
	})
	if progErr != nil {
		t.Fatal(progErr)
	}
	out := map[string]string{}
	for _, n := range []string{"rss-source", "http-json", "dir-watch"} {
		out[n] = filepath.Join(progDir, n)
	}
	return out
}

type buildError struct {
	pkg, out string
	err      error
}

func (e *buildError) Error() string { return "go build " + e.pkg + ": " + e.err.Error() + "\n" + e.out }

// setup 建环境：init、serve、OBS/WRK/DEL 等凭据。
func setup(t *testing.T) *fx {
	t.Helper()
	programs(t)
	// 登记的拉取程序只认裸名（设计 §9.1），apa serve 继承测试进程的 PATH 来找到它们。
	t.Setenv("PATH", progDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	return &fx{Env: e, t: t}
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// srcState 是 `apa source show` 的输出里本测试关心的部分。
type srcState struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Cursor  string `json:"cursor"`
	Stats   struct {
		Runs                int    `json:"runs"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		Timeouts            int    `json:"timeouts"`
		LastSuccessAt       string `json:"last_success_at"`
		LastError           string `json:"last_error"`
		NeedsInputItem      string `json:"needs_input_item"`
	} `json:"stats"`
}

func (f *fx) show(id string) srcState {
	f.t.Helper()
	var s srcState
	r := f.apa(f.OBS, 0, "source", "show", id)
	if err := json.Unmarshal([]byte(r.Stdout), &s); err != nil {
		f.t.Fatalf("source show 输出不是 JSON: %q", r.Stdout)
	}
	return s
}

type event struct {
	Key struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Type        string `json:"type"`
	Subject     string `json:"subject"`
	OccurredAt  string `json:"occurred_at"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
}

// events 经来源页同一接口读某来源已落盘的事件。
func (f *fx) events(id string) []event {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/sources/"+id+"/events?limit=500", "", f.OBS)
	if r.Code != 200 {
		f.t.Fatalf("读来源事件 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

func ids(evs []event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Key.EventID)
	}
	return out
}

// until 轮询到条件成立，否则带上说明失败。
func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitMax)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("%v 内没有等到：%s", waitMax, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// settle 等来源至少再跑 n 次（无论成败），用于断言"某件事没有发生"之前给调度足够的节拍。
func (f *fx) settle(id string, n int) srcState {
	f.t.Helper()
	start := f.show(id).Stats.Runs
	f.until(id+" 再运行 "+string(rune('0'+n))+" 次", func() bool { return f.show(id).Stats.Runs >= start+n })
	return f.show(id)
}

// needsInputItems 返回 needs_input 阶段、起源是该来源的事项 id。
func (f *fx) needsInputItem(id string) string {
	f.t.Helper()
	return f.show(id).Stats.NeedsInputItem
}

func (f *fx) settle0(id string) int { return f.show(id).Stats.Runs }

type reject struct {
	Reason, Raw string
}

// rejects 读某来源没有入账的输出行及原因。内核目前没有经 HTTP 暴露这张表（见交付说明），
// 这里只读打开账本，证明原因已留存。
func (f *fx) rejects(id string) []reject {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?mode=ro")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT reason, raw FROM sources_runner_reject WHERE source_id = ? ORDER BY seq`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []reject
	for rows.Next() {
		var r reject
		if err := rows.Scan(&r.Reason, &r.Raw); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (f *fx) get(path string) string {
	f.t.Helper()
	r := f.APIRaw("GET", path, "", f.OBS)
	if r.Code != 200 {
		f.t.Fatalf("GET %s: HTTP %d %s", path, r.Code, r.Body)
	}
	return r.Body
}

func (f *fx) needsInputOf(item string) bool {
	f.t.Helper()
	var it struct {
		Item struct {
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.get("/v1/items/"+item)), &it); err != nil {
		f.t.Fatal(err)
	}
	return it.Item.Phase == "needs_input"
}

func (f *fx) hasFeedEvent(typ string) bool {
	f.t.Helper()
	return strings.Contains(f.get("/v1/events?limit=500"), `"`+typ+`"`)
}

func (f *fx) hasHealthDelivery(src string) bool {
	f.t.Helper()
	return strings.Contains(f.Web("GET", "/v1/deliveries/unread", "").Body, src)
}
