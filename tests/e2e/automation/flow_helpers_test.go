//go:build e2e

package automation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// kit 是场景十、十一共用的验收环境：flow-env-baseline 的隔离 APA_HOME、真实 apa 二进制、两个模型端点替身。
// 子测试期间 kit 与环境脚本的致命错误记在该子测试上，前一步失败后续不得开始。
type kit struct {
	*baseline.Env
	t *testing.T

	clsBase, modelBase int // ACT-*-01 记下的 $CLS_LOG、$MODEL_LOG 行数基线
}

func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	return &kit{Env: e, t: t}
}

func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (k *kit) must(err error) {
	k.t.Helper()
	if err != nil {
		k.t.Fatal(err)
	}
}

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) apa(token string, want int, args ...string) baseline.Result {
	k.t.Helper()
	r := k.Apa(token, "", args...)
	if r.Code != want {
		k.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (k *kit) restart() { k.t.Helper(); k.StopKernel(); k.StartKernelAgain() }

// markModelBaseline 是验收文档的 `wc -l $CLS_LOG $MODEL_LOG`：之后断言"行数与基线相同"。
func (k *kit) markModelBaseline() {
	k.clsBase, k.modelBase = len(k.Cls.Batches()), len(k.ModelStub.Requests())
}

func (k *kit) noModelCalls(when string) {
	k.t.Helper()
	if n := len(k.Cls.Batches()) - k.clsBase; n != 0 {
		k.t.Errorf("%s：CLS_LOG 比基线多 %d 条请求", when, n)
	}
	if n := len(k.ModelStub.Requests()) - k.modelBase; n != 0 {
		k.t.Errorf("%s：MODEL_LOG 比基线多 %d 条请求: %s", when, n, k.ModelStub.Requests()[k.modelBase].BodyText)
	}
}

// ---- 脚本来源：队列目录里放"下一次拉取要输出的事件"，输出后移走，游标随已输出文件数递增 ----

const queueScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

type queueSource struct {
	k    *kit
	id   string
	dir  string
	seq  int
	stub bool
}

// registerSource 在 Web 来源页登记一个 type=script 来源，周期缩成 1 秒（验收文档为每分钟）。
func (k *kit) registerSource(id, sourceType string) *queueSource {
	k.t.Helper()
	dir := filepath.Join(k.Root, "src-"+id)
	for _, d := range []string{"in", "done"} {
		k.must(os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	k.must(os.WriteFile(filepath.Join(dir, "run.sh"), []byte(queueScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": sourceType,
		"command": filepath.Join(dir, "run.sh") + " " + dir, "schedule": "1s", "timeout": 30})
	k.webOK("POST", "/v1/sources", string(body), 201)
	return &queueSource{k: k, id: id, dir: dir}
}

// emit 把若干行 JSON 事件放进队列，下一个周期被脚本输出。
func (s *queueSource) emit(events ...map[string]any) {
	s.k.t.Helper()
	var sb strings.Builder
	for _, e := range events {
		b, _ := json.Marshal(e)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	s.seq++
	tmp := filepath.Join(s.dir, fmt.Sprintf("tmp-%d", s.seq))
	s.k.must(os.WriteFile(tmp, []byte(sb.String()), 0o644))
	s.k.must(os.Rename(tmp, filepath.Join(s.dir, "in", fmt.Sprintf("%04d.jsonl", s.seq))))
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

type srcEvent struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Summary     string `json:"summary"`
	Link        string `json:"link"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (k *kit) events(src string) []srcEvent {
	k.t.Helper()
	r := k.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500", "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("来源事件: %d %s", r.Code, r.Body)
	}
	var out struct {
		Events []srcEvent `json:"events"`
	}
	jsonOf(k.t, r.Body, &out)
	return out.Events
}

// waitEvent 等来源事件（去重键末段为 id）落盘并有处置（不再 pending），返回它。
func (k *kit) waitEvent(src, id string) srcEvent {
	k.t.Helper()
	var found srcEvent
	if !k.WaitFor(45*time.Second, func() bool {
		for _, e := range k.events(src) {
			if e.DedupeKey.EventID == id && e.Disposition != "" && e.Disposition != "pending" {
				found = e
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("来源 %s 的事件 %s 在 45 秒内没有被处置: %+v\n%s", src, id, k.events(src), k.ServiceLog())
	}
	return found
}

// ---- 投递 ----

type delivery struct {
	ID     string `json:"id"`
	Item   string `json:"item"`
	Class  string `json:"class"`
	Route  string `json:"route"`
	State  string `json:"state"`
	Origin struct {
		Event string `json:"source_event"`
		Rule  string `json:"rule"`
	} `json:"origin"`
	Content struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		WebURL  string   `json:"web_url"`
		Link    string   `json:"link"`
		Options []string `json:"options"`
	} `json:"content"`
	Raw string `json:"-"`
}

// claim 用 CHAN 长轮询领取一次；没有投递返回 nil。
func (k *kit) claim(waitSec int) *delivery {
	k.t.Helper()
	r := k.APIRaw("GET", fmt.Sprintf("/v1/deliveries/next?wait=%d", waitSec), "", k.CHAN)
	switch r.Code {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
		var d delivery
		jsonOf(k.t, r.Body, &d)
		d.Raw = r.Body
		return &d
	}
	k.t.Fatalf("领取投递: HTTP %d %s", r.Code, r.Body)
	return nil
}

func (k *kit) unread() []delivery {
	k.t.Helper()
	var out struct {
		Deliveries []delivery `json:"deliveries"`
	}
	jsonOf(k.t, k.webOK("GET", "/v1/deliveries/unread", "", 200), &out)
	return out.Deliveries
}

// setDisturb 以本人身份在 Web 设置页保存打扰策略（只改 disturb 一段）。
func (k *kit) setDisturb(d map[string]any) {
	k.t.Helper()
	b, _ := json.Marshal(map[string]any{"disturb": d})
	k.webOK("PUT", "/v1/settings/policy", string(b), 200)
}

// ---- 事项 ----

type ledgerItem struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Title      string          `json:"title"`
	Phase      string          `json:"phase"`
	Project    string          `json:"project"`
	PlannedFor string          `json:"planned_for"`
	Summary    string          `json:"summary"`
	Spec       json.RawMessage `json:"spec"`
	Reason     string          `json:"reason"`
}

func (k *kit) items(kind string) []ledgerItem {
	k.t.Helper()
	args := []string{"item", "list"}
	if kind != "" {
		args = append(args, "--kind", kind)
	}
	r := k.apa(k.OBS, 0, args...)
	var out struct {
		Items []ledgerItem `json:"items"`
	}
	jsonOf(k.t, r.Stdout, &out)
	return out.Items
}

func (k *kit) show(id string) (it ledgerItem, raw string) {
	k.t.Helper()
	raw = k.apa(k.OBS, 0, "item", "show", id).Stdout
	var out struct {
		Item ledgerItem `json:"item"`
	}
	jsonOf(k.t, raw, &out)
	return out.Item, raw
}

func (k *kit) why(id string) string {
	k.t.Helper()
	return k.apa(k.OBS, 0, "why", id).Stdout
}

// runDirs 是 $APA_HOME/runs 下的全部运行目录名。
func (k *kit) runDirs() []string {
	ents, _ := os.ReadDir(filepath.Join(k.Home, "runs"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// startExt 启动随包的 Telegram 渠道 ext（不改其配置与协议）连接机器人替身，提醒与决策都路由到渠道 acc-chan。
// 返回停止函数、机器人替身与 ext 日志读取函数。
func (k *kit) startExt() (stop func(), bot *fakebot.Bot, logs func() string) {
	k.t.Helper()
	k.setDisturb(map[string]any{"routes": map[string]string{"decision": "acc-chan", "reminder": "acc-chan", "brief": "web", "health": "web", "wake": "web"},
		"merge_window_sec": 0, "daily_cap": 20})
	bot, err := fakebot.New()
	k.must(err)
	cmd := exec.Command(k.Bin["telegram-ext"])
	cmd.Env = append(os.Environ(), "APA_URL="+k.URL, "APA_EXT_KEY="+k.CHAN, "TG_BOT=1:acc", "TG_CHAT=1", "TG_API="+bot.URL,
		"APA_EXT_STATE="+filepath.Join(k.Root, "ext-telegram.json"))
	buf := &lockedBuf{}
	cmd.Stdout, cmd.Stderr = buf, buf
	k.must(cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			bot.Close()
		})
	}
	k.t.Cleanup(stop)
	return stop, bot, buf.String
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// accHarness 是 STUB_HARNESS 的"写简报""例行任务"行为扩展（accharness），经 `apa declare` 声明并在 Web 确认为 acc-stub 的同名替身。
type accHarness struct {
	k        *kit
	modeFile string
	Rec      string // $BRIEF_REC / $RTN_REC：每次运行的资料、提示词、参数与任务说明副本在 <Rec>/<run-id>/
}

var buildHarnessOnce sync.Once
var harnessBin string
var harnessErr error

// declareHarness 声明并确认名为 name 的无头运行替身（read/edit/full 三档），设为默认 harness，初始模式 mode。
func (k *kit) declareHarness(name, mode string) *accHarness {
	k.t.Helper()
	buildHarnessOnce.Do(func() {
		dir, err := os.MkdirTemp("", "apa-e2e-accharness-")
		if err != nil {
			harnessErr = err
			return
		}
		harnessBin = filepath.Join(dir, "accharness")
		cmd := exec.Command("go", "build", "-o", harnessBin, "github.com/xiaohei-info/awesome-personal-agent/tests/e2e/automation/accharness")
		if out, err := cmd.CombinedOutput(); err != nil {
			harnessErr = fmt.Errorf("go build accharness: %v\n%s", err, out)
		}
	})
	k.must(harnessErr)
	h := &accHarness{k: k, modeFile: filepath.Join(k.Root, name+".mode"), Rec: filepath.Join(k.Root, name+"-rec")}
	k.must(os.MkdirAll(h.Rec, 0o755))
	h.Mode(mode)
	decl, _ := json.Marshal(map[string]any{"harness": name, "headless": map[string]any{
		"command": []string{harnessBin, h.modeFile, k.Bin["apa"], h.Rec, "{run_dir}", "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := k.Apa(k.DEL, string(decl), "declare", "-", "--evidence", "验收声明"+name); r.Code != 0 {
		k.t.Fatalf("apa declare %s: %d %s%s", name, r.Code, r.Stdout, r.Stderr)
	}
	k.webOK("POST", "/v1/settings/declarations/"+name+"/1/confirm", "", 200)
	return h
}

// Mode 切换替身的行为（brief、routine、exit1、noop），对之后拉起的运行生效。
func (h *accHarness) Mode(m string) { h.k.must(os.WriteFile(h.modeFile, []byte(m), 0o644)) }

// RunRecords 是已记录的运行 id（按开始时间先后）。
func (h *accHarness) RunRecords() []string {
	ents, _ := os.ReadDir(h.Rec)
	sort.Slice(ents, func(i, j int) bool {
		a, _ := ents[i].Info()
		b, _ := ents[j].Info()
		return a != nil && b != nil && a.ModTime().Before(b.ModTime())
	})
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (h *accHarness) File(run, name string) string {
	b, _ := os.ReadFile(filepath.Join(h.Rec, run, name))
	return string(b)
}

// materialBody 取运行资料文件（materials.json）里名为 name 的一段资料的正文；没有则为空。
func materialBody(t *testing.T, materials, name string) string {
	t.Helper()
	var m struct {
		Materials []struct{ Name, Body string }
	}
	jsonOf(t, materials, &m)
	for _, x := range m.Materials {
		if x.Name == name {
			return x.Body
		}
	}
	return ""
}

// noModelCallsBeyondItemSummaries 是 `$CLS_LOG`、`$MODEL_LOG` 行数与基线相同的断言，唯一的放宽是 kernel/summaries 的一句话摘要
// （设计 §6.3：任何 open 事项缺 summary 且配了生成端点时由内核起草，与来源规则、routine 的执行路径无关）。
// 新增的 MODEL_LOG 请求必须全部是这类草稿且只针对 kind 的事项，CLS_LOG 与判断题一条都不许有。
func (k *kit) noModelCallsBeyondItemSummaries(when, kind string) {
	k.t.Helper()
	if n := len(k.Cls.Batches()) - k.clsBase; n != 0 {
		k.t.Errorf("%s：CLS_LOG 比基线多 %d 条请求", when, n)
	}
	for _, r := range k.ModelStub.Requests()[k.modelBase:] {
		if baseline.IsJudgeRequest(r) || !strings.Contains(r.BodyText, "写一句话摘要") || !strings.Contains(r.BodyText, `\"kind\":\"`+kind+`\"`) {
			k.t.Errorf("%s：MODEL_LOG 出现 %s 执行路径上的模型请求: %s", when, kind, r.BodyText)
		}
	}
}

// alignClock 等到内核时钟（真实时间加测试时钟偏移）落在某分钟的前 20 秒内，使随后约 40 秒的计数断言
// 不会被真实时间自然走过的分钟边界多触发一次 "* * * * *"。
func (k *kit) alignClock() {
	k.t.Helper()
	var off int64
	if b, err := os.ReadFile(filepath.Join(k.Home, "test-clock-offset")); err == nil {
		off, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if sec := time.Now().Add(time.Duration(off) * time.Second).Second(); sec > 20 {
		time.Sleep(time.Duration(61-sec) * time.Second)
	}
}
