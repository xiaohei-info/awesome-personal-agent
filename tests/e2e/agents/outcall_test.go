//go:build e2e

package agents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// script 在 Root/scripts 下写一个可执行脚本，返回路径。
func (f *fx) script(name, body string) string {
	f.t.Helper()
	dir := filepath.Join(f.Root, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// addScriptSource 在 Web 登记周期脚本来源（脚本来源只能在 Web 登记）。
func (f *fx) addScriptSource(id, command, schedule string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": "acc", "command": command, "schedule": schedule, "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
}

func count(rs []run, pred func(run) bool) (n int) {
	for _, r := range rs {
		if pred(r) {
			n++
		}
	}
	return
}

func state(s string) func(run) bool { return func(r run) bool { return r.State == s } }

// sampler 在后台轮询运行记录，记下各类别同时运行数的峰值（只读 HTTP，不依赖 *testing.T）。
type sampler struct {
	f    *fx
	mu   sync.Mutex
	max  map[string]int
	stop chan struct{}
	done chan struct{}
}

func (f *fx) sample() *sampler {
	s := &sampler{f: f, max: map[string]int{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			r := f.APIRaw("GET", "/v1/runs?limit=500", "", f.OBS)
			var out struct {
				Runs []run `json:"runs"`
			}
			if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil {
				continue
			}
			running := map[string]int{}
			for _, x := range out.Runs {
				if x.State == "running" {
					running[x.Category]++
					if x.Category == "source" || x.Category == "transform" || x.Category == "reader" {
						running["short"]++ // 短任务队列：来源拉取、转换脚本、托管文档读取
					}
				}
			}
			s.mu.Lock()
			for c, n := range running {
				s.max[c] = max(s.max[c], n)
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *sampler) finish() map[string]int {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

// longQueue 读健康视图里长时运行队列的排队信息。
func (f *fx) outcallHealth() (running, queued int, longest float64) {
	f.t.Helper()
	var h struct {
		Signals []struct {
			ID     string          `json:"id"`
			Detail json.RawMessage `json:"detail"`
		} `json:"signals"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "health").Stdout), &h); err != nil {
		f.t.Fatal(err)
	}
	for _, s := range h.Signals {
		if s.ID != "outcall" {
			continue
		}
		var d struct {
			Long struct {
				Running int     `json:"running"`
				Queued  int     `json:"queued"`
				Longest float64 `json:"longest_wait_seconds"`
			} `json:"long"`
		}
		if err := json.Unmarshal(s.Detail, &d); err != nil {
			f.t.Fatal(err)
		}
		return d.Long.Running, d.Long.Queued, d.Long.Longest
	}
	f.t.Fatal("健康视图没有 outcall 信号")
	return
}

// TestFlowOutcallQueue 对应 flow-outcall-limits 的 ACT-OUT-01、ACT-OUT-02：长时运行额度为 1（max_subprocesses=1）。
// 挂起时长按 10 秒缩放（验收文档为 2 分钟），排队、不丢、互不挤占的判据不变。
func TestFlowOutcallQueue(t *testing.T) {
	t.Parallel()
	f := setup(t, "max_subprocesses: 1\nmax_short_subprocesses: 1\n")
	f.auto("run-task")
	f.webOK("PATCH", "/projects/"+f.Projects["acc-open"], `{"capture":true}`, 200)
	smp := f.sample()
	var order []string

	t.Run("ACT-OUT-01 长时运行超限时排队", func(t *testing.T) {
		f.mode("hang", "HANG_FOR=10s")
		for _, n := range []string{"A", "B", "C"} {
			f.task("OUT01-"+n, "acc-task")
			order = append(order, "OUT01-"+n)
			time.Sleep(200 * time.Millisecond) // 入队顺序确定
		}
		f.waitFor("一个 running、两个 queued", 20*time.Second, func() bool {
			rs := f.taskRuns()
			return count(rs, state("running")) == 1 && count(rs, state("queued")) == 2
		})
		running, queued, _ := f.outcallHealth()
		eq(t, "健康视图 running", running, 1)
		eq(t, "健康视图 queued", queued, 2)
		time.Sleep(1200 * time.Millisecond)
		if _, _, longest := f.outcallHealth(); longest < 1 {
			t.Errorf("健康视图的最长等待 = %v 秒，期望 ≥ 1", longest)
		}
	})

	t.Run("ACT-OUT-02 长时运行占满时来源拉取与模型调用照常", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.Judge(modelstub.ChatJudge{Answer: "yes"}) // 对话捕获先预筛（是非题），再抽取
		f.ModelStub.When("抽取可以成为建议的候选事项", reply(`{"candidates":[{"title":"OUT02 候选","kind":"task"}]}`))
		f.addScriptSource("src_acc_tick", f.script("tick.sh", "cat >/dev/null\necho '{\"cursor\":\"c\"}'\n"), "1s")
		f.capture("acc-open", "OUT02 长时运行占满时的判断", "s1")
		f.waitFor("来源拉取在短任务队列执行", 15*time.Second, func() bool {
			return count(f.runs("--category", "source"), state("succeeded")) >= 1
		})
		f.waitFor("模型调用不受影响", 15*time.Second, func() bool { return len(f.proposals()) == 1 })
		// 这两件事发生时长时运行仍占满额度，否则证明不了互不挤占。
		rs := f.taskRuns()
		eq(t, "此时仍在运行的长时运行", count(rs, state("running")), 1)
		eq(t, "此时仍在排队的长时运行", count(rs, state("queued")), 2)
	})

	t.Run("ACT-OUT-01 前一个结束后依次执行，没有 Run 被丢弃", func(t *testing.T) {
		f.waitFor("三个长时运行依次结束", 60*time.Second, func() bool {
			return count(f.taskRuns(), state("succeeded")) == 3
		})
		rs := f.taskRuns()
		eq(t, "任务型 Run 总数", len(rs), 3)
		for i := 1; i < len(rs); i++ {
			if !(rs[i-1].Target != rs[i].Target) {
				t.Error("Run 目标重复")
			}
		}
		started := f.runsStartedAt()
		for i := 1; i < len(started); i++ {
			if !started[i-1].Before(started[i]) {
				t.Errorf("没有按入队顺序依次执行: %v", started)
			}
		}
		peak := smp.finish()
		eq(t, "长时运行同时运行的峰值", peak["task"], 1)
		_ = order
	})
}

// runsStartedAt 返回任务型 Run 的开始时间（按入队顺序）。
func (f *fx) runsStartedAt() []time.Time {
	f.t.Helper()
	var out struct {
		Runs []struct {
			StartedAt time.Time `json:"started_at"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "run", "list", "--category", "task").Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	var ts []time.Time
	for _, r := range out.Runs {
		ts = append(ts, r.StartedAt)
	}
	return ts
}

var _ = fmt.Sprintf
var _ = strings.Contains

// ingestRecord 向 webhook 来源投递一条已签名的请求，返回入站记录 id。
func (f *fx) ingestRecord(src, secret, body, delivery string) string {
	f.t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req, _ := http.NewRequest("POST", f.URL+"/ingest/"+src, strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		f.t.Fatalf("ingest: HTTP %d", resp.StatusCode)
	}
	var list struct {
		Ingests []struct{ ID string } `json:"ingests"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/sources/"+src+"/ingests", "", 200)), &list); err != nil || len(list.Ingests) == 0 {
		f.t.Fatalf("没有入站记录: %v", err)
	}
	return list.Ingests[len(list.Ingests)-1].ID
}

// runDir 返回运行目录 $APA_HOME/runs/<run-id>。
func (f *fx) runDir(id string) string { return filepath.Join(f.Home, "runs", id) }

// procsIn 返回当前目录在 dir 之下的存活进程（运行目录是任务型子进程及其子孙的工作目录）。
// 用 lsof 取各进程 cwd，Linux 与 macOS 通用；lsof 报告真实路径，所以 dir 先解符号链接。
func procsIn(dir string) []int {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	out, _ := exec.Command("lsof", "-a", "-d", "cwd", "-Fpn").Output() // 部分进程无权访问时 lsof 退出码非 0，输出仍有效
	var pids []int
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0 && strings.HasPrefix(line[1:], dir):
			pids = append(pids, pid)
		}
	}
	return pids
}

func alive(pid int) bool {
	// 僵尸进程不算存活：父进程已被杀，由 init 收养后很快回收。
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	return err == nil && st != "" && !strings.HasPrefix(st, "Z")
}

// TestFlowOutcallShort 对应 ACT-OUT-03、ACT-OUT-04：短任务额度为 1（max_short_subprocesses=1）。
// 周期与耗时按秒缩放（验收文档为每分钟 / 3 分钟），额度、优先级和不重复入队的判据不变。
func TestFlowOutcallShort(t *testing.T) {
	t.Parallel()
	f := setup(t, "max_short_subprocesses: 1\n")
	f.ModelStub.When(judgeMark, reply(`{"choice":"ignore","confidence":0.9}`))
	smp := f.sample()

	t.Run("ACT-OUT-03 短任务额度和优先级", func(t *testing.T) {
		slow := f.script("slow.sh", "cat >/dev/null\nsleep 4\necho '{\"cursor\":\"c\"}'\n")
		for _, n := range []string{"a", "b", "c"} {
			f.addScriptSource("src_slow_"+n, slow, "1s")
		}
		tr := filepath.Join(f.Home, "sources", "transform.sh") // 转换脚本必须在主机脚本目录之下
		if err := os.WriteFile(tr, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"id\":\"w1\",\"type\":\"acc.hook\",\"subject\":\"hook\",\"occurred_at\":\"2026-01-01T00:00:00Z\"}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, _ := json.Marshal(map[string]any{"scheme": "hmac-sha256-header", "transform": map[string]any{"command": tr}})
		body, _ := json.Marshal(map[string]any{"id": "src_hook", "type": "webhook", "source_type": "acc", "config": json.RawMessage(cfg)})
		f.webOK("POST", "/sources", string(body), 201)
		var key struct{ Secret string }
		if err := json.Unmarshal([]byte(f.webOK("POST", "/sources/src_hook/webhook", "", 201)), &key); err != nil || key.Secret == "" {
			t.Fatalf("webhook 密钥: %v", err)
		}
		ing := f.ingestRecord("src_hook", key.Secret, `{"a":1}`, "d1")
		f.waitFor("入站记录首次处理完", 40*time.Second, func() bool { return count(f.runs("--category", "transform"), state("succeeded")) == 1 })
		f.waitFor("定时拉取在短任务队列里排队", 20*time.Second, func() bool { return count(f.runs("--category", "source"), state("queued")) >= 1 })

		f.webOK("POST", "/ingests/"+ing+"/replay", "", 200) // 本人在 Web 重放
		var replay run
		f.waitFor("重放的转换 Run 入队", 10*time.Second, func() bool {
			if rs := f.runs("--category", "transform"); len(rs) == 2 {
				replay = rs[1]
				return true
			}
			return false
		})
		// 重放入队时已经排队的定时拉取，都应排在重放之后。
		ahead := map[string]bool{}
		for _, r := range f.runs("--category", "source", "--state", "queued") {
			ahead[r.ID] = true
		}
		f.waitFor("重放运行结束", 40*time.Second, func() bool { return f.runByID(replay.ID).State == "succeeded" })
		replayStart := f.startedAt(replay.ID)
		if len(ahead) == 0 && replay.State != "queued" {
			t.Skip("重放入队时没有排队的定时拉取，无法比较优先级")
		}
		for id := range ahead {
			if st := f.startedAt(id); !st.IsZero() && st.Before(replayStart) {
				t.Errorf("本人触发的重放(开始 %v)排在定时拉取 %s(开始 %v)之后", replayStart, id, st)
			}
		}
		if peak := smp.finish()["short"]; peak > 1 {
			t.Errorf("短任务同时运行的峰值 = %d，超过额度 1", peak)
		}
	})

}

// TestFlowOutcallSourceDedupe 对应 ACT-OUT-04（单独的环境：不与上面三个慢来源争短任务额度）。
func TestFlowOutcallSourceDedupe(t *testing.T) {
	t.Parallel()
	f := setup(t, "")
	f.ModelStub.When(judgeMark, reply(`{"choice":"ignore","confidence":0.9}`))
	t.Run("ACT-OUT-04 同一来源不重复入队", func(t *testing.T) {
		script := f.script("long.sh", `in=$(cat)
n=$(cat "$0.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0.n"
printf '%s\n' "$in" >> "$0.stdin"
sleep 3
echo "{\"id\":\"e$n\",\"type\":\"acc.long\",\"subject\":\"long $n\",\"occurred_at\":\"2026-01-01T00:00:00Z\"}"
echo "{\"cursor\":\"$n\"}"
`)
		f.addScriptSource("src_long", script, "1s")
		deadline := time.Now().Add(14 * time.Second)
		peak := 0
		for time.Now().Before(deadline) {
			pending := count(f.runs("--target", "src_long"), func(r run) bool { return r.State == "queued" || r.State == "running" })
			peak = max(peak, pending)
			time.Sleep(200 * time.Millisecond)
		}
		eq(t, "同一来源同时排队或运行的 Run 数峰值", peak, 1)
		done := count(f.runs("--target", "src_long"), state("succeeded"))
		if done < 3 {
			t.Fatalf("14 秒内应完成至少 3 次拉取，实际 %d", done)
		}
		// 游标不漏事件：来源事件 e1..eN 连续，没有缺口和重复；每次 stdin 的 cursor 是上一次末行的游标。
		var evs struct {
			Events []struct{ DedupeKey, ID string } `json:"events"`
		}
		_ = evs
		raw := f.webOK("GET", "/sources/src_long/events?limit=500", "", 200)
		var out struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range out.Events {
			dk, _ := e["dedupe_key"].(map[string]any)
			key := fmt.Sprint(dk["event_id"])
			if seen[key] {
				t.Errorf("来源事件重复: %s", key)
			}
			seen[key] = true
		}
		eq(t, "来源事件数 = 已完成拉取数", len(seen), done)
		for i := 1; i <= done; i++ {
			found := false
			for k := range seen {
				found = found || k == fmt.Sprintf("e%d", i)
			}
			if !found {
				t.Errorf("缺少事件 e%d: %v", i, seen)
			}
		}
		stdin, _ := os.ReadFile(script + ".stdin")
		lines := strings.Split(strings.TrimSpace(string(stdin)), "\n")
		for i, l := range lines {
			var doc struct {
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal([]byte(l), &doc); err != nil {
				t.Fatalf("stdin 不是 JSON: %q", l)
			}
			want := ""
			if i > 0 {
				want = fmt.Sprint(i)
			}
			eq(t, fmt.Sprintf("第 %d 次拉取收到的 cursor", i+1), doc.Cursor, want)
		}
	})
}

func (f *fx) runByID(id string) run {
	f.t.Helper()
	var r run
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "run", "show", id).Stdout), &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fx) startedAt(id string) time.Time {
	f.t.Helper()
	var r struct {
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "run", "show", id).Stdout), &r); err != nil {
		f.t.Fatal(err)
	}
	return r.StartedAt
}

// TestFlowOutcallProc 对应 ACT-OUT-05、ACT-OUT-06、ACT-OUT-08：子进程环境、超时杀进程组、运行目录。
func TestFlowOutcallProc(t *testing.T) {
	t.Parallel()
	f := setup(t, "")
	f.auto("run-task", "run-timeout")

	t.Run("ACT-OUT-05 子进程不继承内核环境", func(t *testing.T) {
		f.mode("receipt")
		f.task("OUT05 环境", "acc-task")
		f.waitFor("任务型运行结束", 20*time.Second, func() bool { return count(f.taskRuns(), state("succeeded")) == 1 })
		r := f.taskRuns()[0]
		raw, err := os.ReadFile(filepath.Join(f.runDir(r.ID), "stub-harness-env.json"))
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]string
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if _, has := env["ACC_SECRET"]; has || strings.Contains(string(raw), "leak") {
			t.Error("内核环境里的 ACC_SECRET 泄漏给了子进程")
		}
		if strings.Contains(string(raw), "stub-key") {
			t.Error("模型 api_key 泄漏给了子进程")
		}
		for _, k := range []string{"PATH", "HOME", "XDG_CONFIG_HOME", "APA_URL", "APA_TOKEN", "APA_RUN", "APA_PROJECT"} {
			if env[k] == "" {
				t.Errorf("子进程缺少 %s", k)
			}
		}
		eq(t, "APA_RUN", env["APA_RUN"], r.ID)
		eq(t, "APA_PROJECT", env["APA_PROJECT"], f.Projects["acc-open"])
		if _, has := env["APA_HOME"]; has {
			t.Error("APA_HOME 不在白名单内，不应继承")
		}
	})

	t.Run("ACT-OUT-08 运行目录保存资料和输出", func(t *testing.T) {
		f.mode("receipt")
		const marker = "OUT08-EXTERNAL-BODY-MARKER"
		f.task(marker, "acc-task")
		f.waitFor("第二个任务型运行结束", 20*time.Second, func() bool { return count(f.taskRuns(), state("succeeded")) == 2 })
		r := f.taskRuns()[1]
		dir := f.runDir(r.ID)
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("运行目录缺少 %s: %v", name, err)
			}
			return string(b)
		}
		materials, prompt, output, sent := read("materials.json"), read("prompt.md"), read("output.txt"), read("args.txt")
		if !strings.Contains(materials, marker) {
			t.Errorf("资料文件应含事项快照: %s", materials)
		}
		for name, text := range map[string]string{"prompt.md": prompt, "实际送给 harness 的提示词": sent} {
			if strings.Contains(text, marker) {
				t.Errorf("%s 含外部正文: %s", name, text)
			}
			if !strings.Contains(text, r.Target) || !strings.Contains(text, "ACCTASK") {
				t.Errorf("%s 应只含固定模板和事项 id: %s", name, text)
			}
		}
		if !strings.Contains(output, `"outcome":"done"`) {
			t.Errorf("输出文件应含 stdout 末段摘要: %s", output)
		}
	})

	t.Run("ACT-OUT-06 超时杀进程组", func(t *testing.T) {
		f.mode("hang", "SPAWN=1")
		trig := f.task("OUT06 超时", "acc-timeout")
		var r run
		f.waitFor("超时运行拉起", 20*time.Second, func() bool {
			for _, x := range f.runs("--category", "task", "--state", "running") {
				r = x
				return true
			}
			return false
		})
		dir := f.runDir(r.ID)
		f.waitFor("子进程已派生", 10*time.Second, func() bool { _, err := os.Stat(filepath.Join(dir, "stub-harness-child.pid")); return err == nil })
		raw, _ := os.ReadFile(filepath.Join(dir, "stub-harness-child.pid"))
		child, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		before := procsIn(dir)
		if len(before) < 2 || !alive(child) {
			t.Fatalf("超时前应有父子两个进程在运行目录内: %v child=%d", before, child)
		}
		start := time.Now()
		f.waitFor("Run 超时", 90*time.Second, func() bool { return f.runByID(r.ID).State == "timeout" })
		if d := time.Since(start); d < 20*time.Second {
			t.Errorf("超时过早: 距拉起不足 20 秒（%v）", d)
		}
		f.waitFor("残留进程清理", 5*time.Second, func() bool { return len(procsIn(dir)) == 0 && !alive(child) })
		f.waitFor("事项转 needs_input", 10*time.Second, func() bool { return f.phase(r.Target) == "needs_input" })
		_ = trig
	})
}

// TestFlowOutcallRestart 对应 ACT-OUT-07：有一个 running、一个 queued 的 Run 时 kill -9 内核和 harness 再重启。
func TestFlowOutcallRestart(t *testing.T) {
	t.Parallel()
	f := setup(t, "max_subprocesses: 1\n")
	f.auto("run-task")
	f.mode("hang", "HANG_FOR=120s")
	f.task("OUT07-A", "acc-task")
	time.Sleep(300 * time.Millisecond)
	f.task("OUT07-B", "acc-task")
	var a, b run
	f.waitFor("一个 running、一个 queued", 20*time.Second, func() bool {
		rs := f.taskRuns()
		if len(rs) != 2 || rs[0].State != "running" || rs[1].State != "queued" {
			return false
		}
		a, b = rs[0], rs[1]
		return true
	})
	dirA := f.runDir(a.ID)
	pids := procsIn(dirA)
	if len(pids) == 0 {
		t.Fatal("找不到 harness 进程")
	}
	if err := syscall.Kill(f.ServePID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	f.waitFor("harness 进程消失", 5*time.Second, func() bool { return len(procsIn(dirA)) == 0 })
	f.mode("receipt") // 重启后重新执行的排队运行正常回执
	f.StartKernelAgain()

	f.waitFor("running 的按失败处理", 30*time.Second, func() bool { return f.runByID(a.ID).State == "failed" })
	f.waitFor("running 的事项转 needs_input", 15*time.Second, func() bool { return f.phase(a.Target) == "needs_input" })
	f.waitFor("queued 的重新执行", 30*time.Second, func() bool { return f.runByID(b.ID).State == "succeeded" })
	f.waitFor("queued 的事项完成", 15*time.Second, func() bool { return f.phase(b.Target) == "done" })
	eq(t, "没有 Run 卡在 running 或 queued", count(f.taskRuns(), func(r run) bool { return r.State == "running" || r.State == "queued" }), 0)
	eq(t, "Run 总数（queued 的是同一个 Run 重新执行，不是新建）", len(f.taskRuns()), 2)
}
