//go:build e2e

package extractalign

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 60 * time.Second

// 退出码（附录 B.1）。
const exitOK = 0

type fx struct {
	*baseline.Env
	t *testing.T
}

func (f *fx) step(name string, fn func()) {
	f.t.Helper()
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn()
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (f *fx) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(waitMax, cond) {
		log := f.ServiceLog()
		if len(log) > 3000 {
			log = log[len(log)-3000:]
		}
		f.t.Fatalf("%v 内没有等到：%s\n内核日志尾部：%s\n来源列表：%s", waitMax, what, log, f.Web("GET", "/v1/sources", "").Body)
	}
}

func jsonOf(t *testing.T, s string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), into); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// ---- SRC_STUB：测试构建，每个来源一份副本，case 文件与可执行文件同目录 ----

var (
	srcOnce sync.Once
	srcPath string
	srcErr  error
)

func buildSrcStub() (string, error) {
	srcOnce.Do(func() {
		dir, err := os.MkdirTemp("", "apa-e2e-srcstub-")
		if err != nil {
			srcErr = err
			return
		}
		srcPath = filepath.Join(dir, "src-stub")
		cmd := exec.Command("go", "build", "-o", srcPath, "./tests/doubles/srcstub/cmd/src-stub")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			srcErr = fmt.Errorf("go build src-stub: %v\n%s", err, out)
		}
	})
	return srcPath, srcErr
}

func repoRoot() string {
	wd, _ := os.Getwd() // tests/e2e/extractalign
	return filepath.Join(wd, "..", "..", "..")
}

type source struct {
	f   *fx
	id  string
	dir string
}

// register 登记一个 SRC_STUB 来源（type=script，周期 1 秒），外发范围 local=full、external=full。
func (f *fx) register(id, sourceType string) *source {
	f.t.Helper()
	bin, err := buildSrcStub()
	f.must(err)
	dir := filepath.Join(f.Root, "src-"+id)
	f.must(os.MkdirAll(dir, 0o755))
	dst := filepath.Join(dir, "src-stub")
	in, err := os.Open(bin)
	f.must(err)
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	f.must(err)
	_, err = io.Copy(out, in)
	f.must(err)
	f.must(out.Close())
	s := &source{f: f, id: id, dir: dir}
	s.setCase("")
	body, _ := json.Marshal(map[string]any{"id": id, "name": id, "type": "script", "source_type": sourceType, "command": dst,
		"schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
	f.webOK("PUT", "/settings/sources/"+id+"/outbound", `{"local":"full","external":"full"}`, 200)
	return s
}

// setCase 写来源的 case 文件：内置 case 名，或以 `{` 开头的原样 JSONL；空串让来源什么都不输出。
func (s *source) setCase(c string) {
	s.f.t.Helper()
	if c == "" {
		c = `{"cursor":"idle"}`
	}
	// case 文件内容是 JSONL 文件的路径（SRC_STUB：case 指向文件时原样输出文件内容）。
	out := filepath.Join(s.dir, "out.jsonl")
	tmp := out + ".tmp"
	s.f.must(os.WriteFile(tmp, []byte(c), 0o644))
	s.f.must(os.Rename(tmp, out))
	s.f.must(os.WriteFile(filepath.Join(s.dir, "case"), []byte(out), 0o644))
}

// put 让来源下一个周期输出这些标准事件（SRC_STUB 的自定义 JSONL 模式）。
func (s *source) put(evs ...map[string]any) []string {
	s.f.t.Helper()
	var lines, ids []string
	for _, e := range evs {
		b, err := json.Marshal(e)
		s.f.must(err)
		lines = append(lines, string(b))
		ids = append(ids, e["id"].(string))
	}
	s.setCase(strings.Join(lines, "\n"))
	return ids
}

type event struct {
	ID        string `json:"id"`
	DedupeKey struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Type        string   `json:"type"`
	Subject     string   `json:"subject"`
	Disposition string   `json:"disposition"`
	Item        string   `json:"item"`
	Items       []string `json:"items"`
	Judgment    string   `json:"judgment"`
}

func (s *source) events() []event {
	s.f.t.Helper()
	r := s.f.APIRaw("GET", "/v1/sources/"+s.id+"/events?limit=500", "", s.f.OBS)
	if r.Code != 200 {
		s.f.t.Fatalf("读来源事件 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Events []event `json:"events"`
	}
	jsonOf(s.f.t, r.Body, &out)
	return out.Events
}

// wait 等 ids 都落盘且处置完毕（不再 pending）。
func (s *source) wait(ids ...string) map[string]event {
	s.f.t.Helper()
	got := map[string]event{}
	s.f.until(s.id+" 的事件 "+strings.Join(ids, ",")+" 处置完毕", func() bool {
		all := s.events()
		for _, id := range ids {
			ok := false
			for _, e := range all {
				if e.DedupeKey.EventID == id && e.Disposition != "" && e.Disposition != "pending" {
					got[id], ok = e, true
				}
			}
			if !ok {
				return false
			}
		}
		return true
	})
	return got
}

// emit = put + wait。
func (s *source) emit(evs ...map[string]any) map[string]event { return s.wait(s.put(evs...)...) }

// ---- 事项 ----

func (f *fx) webItem(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	jsonOf(f.t, f.webOK("GET", "/items/"+id, "", 200), &out)
	return out.Item
}

func (f *fx) proposal(id string) map[string]any {
	f.t.Helper()
	p, _ := f.webItem(id)["proposal"].(map[string]any)
	if p == nil {
		f.t.Fatalf("事项 %s 没有 proposal", id)
	}
	return p
}

type audit struct {
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (f *fx) audit(object, typ string) []audit {
	f.t.Helper()
	path := "/audit/events?limit=500&type=" + typ
	if object != "" {
		path += "&object=" + object
	}
	var out struct {
		Events []audit `json:"events"`
	}
	jsonOf(f.t, f.webOK("GET", path, "", 200), &out)
	return out.Events
}

// ---- 两份模型替身日志 ----

func (f *fx) clsMentions(sub string) int {
	n := 0
	for _, b := range f.Cls.Batches() {
		if strings.Contains(b.BodyText, sub) {
			n++
		}
	}
	return n
}

func (f *fx) modelMentions(sub string) int {
	n := 0
	for _, r := range f.ModelStub.Requests() {
		if strings.Contains(r.BodyText, sub) {
			n++
		}
	}
	return n
}

// webItemID 以本人身份在 acc-work 建一件 open 事项，返回 id。
func (f *fx) webItemID(title string) string {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"kind": "task", "title": title, "phase": "open", "project": f.Projects["acc-work"]})
	var out struct {
		ID string `json:"id"`
	}
	jsonOf(f.t, f.webOK("POST", "/items", string(b), 201), &out)
	return out.ID
}

type packsChk struct {
	Status string `json:"status"`
	Data   struct {
		Loaded   int `json:"loaded"`
		Rejected int `json:"rejected"`
	} `json:"data"`
	Why     string `json:"why"`
	Details any    `json:"details"`
}

// packsCheck 取 `apa doctor --json` 的 packs 一项（doctor 整体因 MODEL_STUB 的默认回答不合起草 schema 而退出码 1，与本流程无关）。
func (f *fx) packsCheck() packsChk {
	f.t.Helper()
	r := f.Apa(f.OBS, "", "doctor", "--json")
	var out struct {
		Checks []struct {
			Name string `json:"name"`
			packsChk
		} `json:"checks"`
	}
	jsonOf(f.t, r.Stdout, &out)
	for _, c := range out.Checks {
		if c.Name == "packs" {
			return c.packsChk
		}
	}
	f.t.Fatalf("doctor 没有 packs 一项: %s", r.Stdout)
	return packsChk{}
}

// draftWB 让生成端点对未被规则命中的请求返回工作台脚本样例 name。
func (f *fx) draftWB(name string, p modelstub.WBParams) {
	f.t.Helper()
	f.must(f.ModelStub.UseWBCase(name, p))
}

// startExt 启动随包的 Telegram 渠道 ext 并连接机器人替身；提醒与决策都走渠道 acc-chan。返回机器人与 ext 日志。
// root 是顶层 *testing.T：ext 要活到流程结束，不能挂在某个步骤的子测试上。
func (f *fx) startExt(root *testing.T) (*fakebot.Bot, func() string) {
	f.t.Helper()
	f.disturb(20, nil)
	bot, err := fakebot.New()
	f.must(err)
	cmd := exec.Command(f.Bin["telegram-ext"])
	cmd.Env = append(os.Environ(), "APA_URL="+f.URL, "APA_EXT_KEY="+f.CHAN, "TG_BOT=1:acc", "TG_CHAT=1", "TG_API="+bot.URL,
		"APA_EXT_STATE="+filepath.Join(f.Root, "ext-telegram.json"))
	var mu sync.Mutex
	var buf strings.Builder
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) })
	cmd.Stdout, cmd.Stderr = w, w
	f.must(cmd.Start())
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(w, "[ext exited: %v]", err)
		close(done)
	}()
	root.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		bot.Close()
	})
	return bot, func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }
