//go:build live

// Package calendar 按验收文档 flow-source-calendar（ACT-CAL-01..05）用真实 APA 二进制、真实 gog 和用户授权的
// 真实 Google 日历走完整流程。红线（见 APA_LIVE_README）：
//   - 只创建/更新/删除标题以 [APA-LIVE-TEST] 开头、落在 2027-01 的测试日程，测完删除并回读确认；
//   - 断言、日志、证据只引用带前缀的日程，不读取、不输出真实日程的内容或数量；
//   - gog 文件 keyring 的口令只经 APA_LIVE_CALENDAR_KEYRING_FILE 读取，不打印；APA_HOME 是临时目录。
//
// 偏差：gog 0.9.0 的 create 无法关闭日历默认提醒（--reminder "" 仍为 useDefault）。测试日程在 2027-01、存活几分钟
// 即被删除，期间不可能触发提醒。ACT-CAL-04 的"撤销授权"不真的撤销用户令牌，而是用 gog 自己的两种真实授权失败
// 代替：账号未授权、keyring 口令不对。
package calendar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	waitMax = 120 * time.Second
	src     = "src_cal"
	prefix  = "[APA-LIVE-TEST] "
)

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("缺少环境变量 %s（先 source APA_LIVE_ENV_FILE）", k)
	}
	return v
}

// run 运行外部命令，返回合并前的 stdout、stderr 和退出码。
func run(t *testing.T, dir string, env []string, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return o.String(), e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("运行 %s: %v", name, err)
	}
	return o.String(), e.String(), 0
}

// parseKV 读 KEY=VALUE 文件（兼容 gog 的 keyring.env：注释、export、成对引号）。
func parseKV(b []byte) map[string]string {
	m := map[string]string{}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "export "))
		k, v, ok := strings.Cut(ln, "=")
		if !ok || strings.HasPrefix(ln, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[strings.TrimSpace(k)] = v
	}
	return m
}

type live struct {
	*baseline.Env
	t        *testing.T
	account  string
	pw       string   // gog keyring 口令，仅用于 grep 检查，不输出
	gogEnv   []string // 直接运行 gog 的环境（含 keyring 变量）
	ctl      string   // 控制目录：path 文件决定适配器的 PATH
	origPath string
	adapter  string
	created  []string // 已创建的测试日程 id（含重复日程的主日程）
}

func (l *live) gog(args ...string) (string, string, int) {
	l.t.Helper()
	a := append([]string{"--no-input", "--json", "--account=" + l.account}, args...)
	return run(l.t, "", l.gogEnv, "gog", a...)
}

func (l *live) setPath(p string) {
	l.t.Helper()
	if err := os.WriteFile(filepath.Join(l.ctl, "path"), []byte(p), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

type calEvent struct {
	ID         string `json:"id"`
	ICalUID    string `json:"iCalUID"`
	Summary    string `json:"summary"`
	Recurrence any    `json:"recurrence"`
}

func decodeEvents(b string) []calEvent {
	var w struct {
		Events []calEvent `json:"events"`
	}
	if json.Unmarshal([]byte(b), &w) == nil && w.Events != nil {
		return w.Events
	}
	var arr []calEvent
	_ = json.Unmarshal([]byte(b), &arr)
	return arr
}

// create 建一个测试日程（无参与人、不发通知）并登记清理。返回 id 和 iCalUID。
func (l *live) create(summary, from, to string, extra ...string) (string, string) {
	l.t.Helper()
	if !strings.HasPrefix(summary, prefix) || !strings.HasPrefix(from, "2027-01-") {
		l.t.Fatalf("红线：只能建 %q 前缀、2027-01 的测试日程", prefix)
	}
	args := append([]string{"calendar", "create", "primary", "--summary", summary, "--from", from, "--to", to, "--send-updates", "none"}, extra...)
	out, errs, code := l.gog(args...)
	var w struct {
		Event calEvent `json:"event"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &w) != nil || w.Event.ID == "" {
		l.t.Fatalf("gog calendar create: %d %s", code, errs)
	}
	l.created = append(l.created, w.Event.ID)
	return w.Event.ID, w.Event.ICalUID
}

// prefixedInJanuary 回读 2027-01 里带前缀的日程（只统计带前缀的，真实日程不触碰）。
func (l *live) prefixedInJanuary() []calEvent {
	l.t.Helper()
	out, errs, code := l.gog("calendar", "events", "primary", "--from", "2027-01-01T00:00:00Z", "--to", "2027-02-01T00:00:00Z", "--max", "2500")
	if code != 0 {
		l.t.Fatalf("回读测试日程: %d %s", code, errs)
	}
	var mine []calEvent
	for _, e := range decodeEvents(out) {
		if strings.HasPrefix(e.Summary, prefix) {
			mine = append(mine, e)
		}
	}
	return mine
}

type srcState struct {
	Enabled         bool   `json:"enabled"`
	Cursor          string `json:"cursor"`
	CredentialsPath string `json:"credentials_path"`
	Stats           struct {
		Runs           int    `json:"runs"`
		LastError      string `json:"last_error"`
		NeedsInputItem string `json:"needs_input_item"`
	} `json:"stats"`
}

func (l *live) show() srcState {
	l.t.Helper()
	r := l.Apa(l.OBS, "", "source", "show", src)
	var s srcState
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &s) != nil {
		l.t.Fatalf("source show: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	return s
}

func (l *live) until(what string, cond func() bool) {
	l.t.Helper()
	for deadline := time.Now().Add(waitMax); !cond(); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			st := l.show()
			l.t.Fatalf("%v 内没有等到：%s（runs=%d last_error=%q needs_input=%q）", waitMax, what, st.Stats.Runs, st.Stats.LastError, st.Stats.NeedsInputItem)
		}
	}
}

// settle 等来源至少再运行 n 次（无论成败）。
func (l *live) settle(n int) {
	l.t.Helper()
	start := l.show().Stats.Runs
	l.until(fmt.Sprintf("来源再运行 %d 次", n), func() bool { return l.show().Stats.Runs >= start+n })
}

type event struct {
	Subject     string `json:"subject"`
	Type        string `json:"type"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	Key         struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

// myEvents 只返回主题带前缀且含本次 tag 的来源事件；其余（真实日程）不进入任何断言和输出。
func (l *live) myEvents(tag string) []event {
	l.t.Helper()
	r := l.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500", "", l.OBS)
	if r.Code != 200 {
		l.t.Fatalf("读来源事件 HTTP %d", r.Code)
	}
	var out struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		l.t.Fatal(err)
	}
	var mine []event
	for _, e := range out.Events {
		if strings.HasPrefix(e.Subject, prefix) && strings.Contains(e.Subject, tag) {
			mine = append(mine, e)
		}
	}
	return mine
}

func (l *live) item(id string) map[string]any {
	l.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	r := l.Apa(l.OBS, "", "item", "show", id)
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &out) != nil {
		l.t.Fatalf("item show %s: %d", id, r.Code)
	}
	return out.Item
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// adapterRefuses 直接运行适配器：退出码 2、stdout 无任何输出（不输出新游标）。stderr 原样返回供断言。
func (l *live) adapterRefuses(path, input string) string {
	l.t.Helper()
	cmd := exec.Command(l.adapter)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = []string{"PATH=" + path, "HOME=" + os.Getenv("HOME")}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || o.Len() != 0 {
		l.t.Errorf("适配器应退出码 2 且无输出: err=%v stdout-bytes=%d", err, o.Len())
	}
	return e.String()
}

func TestFlowSourceCalendar(t *testing.T) {
	account := need(t, "APA_LIVE_CALENDAR_ACCOUNT")
	if need(t, "APA_LIVE_CALENDAR_ID") != "primary" {
		t.Fatal("本测试按 primary 日历编写")
	}
	if need(t, "APA_LIVE_CALENDAR_EVENT_PREFIX")+" " != prefix {
		t.Fatalf("测试日程前缀应为 %q", prefix)
	}
	keyringFile := need(t, "APA_LIVE_CALENDAR_KEYRING_FILE")
	kb, err := os.ReadFile(keyringFile)
	if err != nil {
		t.Fatal("读不到 gog keyring 口令文件")
	}
	kv := parseKV(kb)
	if kv["GOG_KEYRING_PASSWORD"] == "" {
		t.Fatal("keyring 文件缺少 GOG_KEYRING_PASSWORD")
	}
	if _, err := exec.LookPath("gog"); err != nil {
		t.Fatal("本机没有 gog")
	}

	root := t.TempDir()
	l := &live{t: t, account: account, pw: kv["GOG_KEYRING_PASSWORD"], origPath: os.Getenv("PATH"), ctl: filepath.Join(root, "ctl")}
	l.gogEnv = append(os.Environ(), "GOG_KEYRING_PASSWORD="+kv["GOG_KEYRING_PASSWORD"])
	if b := kv["GOG_KEYRING_BACKEND"]; b != "" {
		l.gogEnv = append(l.gogEnv, "GOG_KEYRING_BACKEND="+b)
	}
	if err := os.MkdirAll(l.ctl, 0o755); err != nil {
		t.Fatal(err)
	}
	l.setPath(l.origPath)

	// 无论测试怎样结束都删除测试日程，并回读确认：2027-01 里不再有带前缀的日程。
	t.Cleanup(func() {
		for _, id := range l.created {
			l.gog("--force", "calendar", "delete", "primary", id)
		}
		if left := l.prefixedInJanuary(); len(left) != 0 {
			t.Errorf("清理后仍有 %d 个测试日程残留", len(left))
		}
	})

	repo, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	l.adapter = filepath.Join(root, "calendar-gog")
	if out, errs, code := run(t, repo, nil, "go", "build", "-o", l.adapter, "./sources/calendar/cmd/calendar"); code != 0 {
		t.Fatalf("构建适配器: %s%s", out, errs)
	}
	wrapper := filepath.Join(root, "run.sh")
	writeExec(t, wrapper, fmt.Sprintf("#!/bin/sh\nPATH=\"$(cat %s/path)\" exec %s\n", l.ctl, l.adapter))

	l.Env = baseline.New(t)
	l.StartKernel()
	l.IssueCredentials()
	l.CreateProjects()
	l.StartModelStub()
	// 日历规则的项目归属随用户而定：数据目录放同名包，把 project_by 改成 acc-home（设计 §9.7 / 包注释）。
	builtin, err := os.ReadFile(filepath.Join(repo, "packs", "inbound-triage", "pack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	over := strings.Replace(string(builtin), "project_by: none\n    then: proposed", "project_by: \"fixed:"+l.Projects["acc-home"]+"\"\n    then: proposed", 1)
	if over == string(builtin) {
		t.Fatal("内置包里找不到日历规则的 project_by: none")
	}
	packDir := filepath.Join(l.Home, "packs", "inbound-triage")
	if out, errs, code := run(t, "", nil, "cp", "-R", filepath.Join(repo, "packs", "inbound-triage"), packDir+".tmp"); code != 0 {
		t.Fatalf("复制内置包: %s%s", out, errs)
	}
	if err := os.Rename(packDir+".tmp", packDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.yaml"), []byte(over), 0o644); err != nil {
		t.Fatal(err)
	}
	l.StopKernel()
	l.StartKernelAgain()

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	subj := prefix + "cal " + tag
	horizon := int(time.Until(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)).Hours()/24) + 2
	credDir := filepath.Join(l.Home, "sources", src)
	credLink := filepath.Join(credDir, "keyring.env")
	var uid string

	step("ACT-CAL-01 用工具自己的认证登记适配器，凭据不进数据目录", func(t *testing.T) {
		// gog 的认证与令牌刷新是工具自己的事：先证明 gog 用这个账号能读日历。
		if _, errs, code := l.gog("calendar", "events", "primary", "--from", "2027-01-01T00:00:00Z", "--to", "2027-01-01T00:00:01Z"); code != 0 {
			t.Fatalf("gog 对该账号不可用: %d %s", code, errs)
		}
		if err := os.MkdirAll(credDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(keyringFile, credLink); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"id": src, "type": "script", "source_type": "calendar", "command": "/bin/sh " + wrapper,
			"schedule": "5s", "timeout": 100, "credentials_path": credLink, "dedupe_domain": "calendar",
			"config": map[string]any{"account": account, "calendar_id": "primary", "horizon_days": horizon}})
		if r := l.Web("POST", "/v1/sources", string(body)); r.Code != 201 {
			t.Fatalf("登记来源: HTTP %d %s", r.Code, r.Body)
		}
		l.until("首次拉取成功", func() bool { st := l.show(); return st.Cursor != "" && st.Stats.LastError == "" })
		st := l.show()
		if !st.Enabled {
			t.Fatal("来源应为 enabled")
		}
		// keyring 口令和 gog 令牌库都不在 APA_HOME 内（符号链接本身不算内容；grep -r 不跟随符号链接）。
		if out, _, _ := run(t, "", nil, "grep", "-rlF", l.pw, l.Home); strings.TrimSpace(out) != "" {
			t.Errorf("keyring 口令出现在 APA_HOME 的文件: %s", strings.ReplaceAll(out, l.Home, "$APA_HOME"))
		}
		if out, _, _ := run(t, "", nil, "grep", "-rlF", l.pw, filepath.Join(l.Home, "ledger.db")); strings.TrimSpace(out) != "" {
			t.Error("keyring 口令写进了 ledger.db")
		}
		if target, _ := os.Readlink(credLink); target != keyringFile {
			t.Errorf("凭据应是指向 gog 自己 keyring.env 的符号链接: %q", target)
		}
	})

	var firstEv event
	step("ACT-CAL-02 新日程经确定性规则变成事项，不调用判断型", func(t *testing.T) {
		l.ModelStub.Reset()
		_, uid = l.create(subj, "2027-01-16T19:10:00Z", "2027-01-16T19:20:00Z", "--description", "APA live e2e 测试日程，会被自动删除")
		l.until("来源产生该日程事件", func() bool { return len(l.myEvents(tag)) > 0 })
		l.until("事件被处理", func() bool { return l.myEvents(tag)[0].Disposition != "" })
		firstEv = l.myEvents(tag)[0]
		if firstEv.Type != "event.created" || firstEv.Disposition != "proposed" || firstEv.Item == "" {
			t.Fatalf("事件应为 event.created/proposed 且带事项: type=%s disposition=%s item=%q", firstEv.Type, firstEv.Disposition, firstEv.Item)
		}
		it := l.item(firstEv.Item)
		if str(it, "phase") != "proposed" || str(it, "kind") != "task" || str(it, "title") != subj {
			t.Errorf("事项应为 proposed/task 且标题为日程标题: phase=%v kind=%v title=%v", it["phase"], it["kind"], it["title"])
		}
		if str(it, "project") != l.Projects["acc-home"] {
			t.Errorf("事项应按规则归入 acc-home(%s): project=%v", l.Projects["acc-home"], it["project"])
		}
		if n := len(l.ModelStub.Requests()); n != 0 {
			t.Errorf("日历规则是确定性映射，不应调用判断型：模型端点收到 %d 次请求", n)
		}
	})

	step("ACT-CAL-03 日程更新和重复日程能区分", func(t *testing.T) {
		l.ModelStub.Reset()
		updated := subj + " updated"
		id := l.created[0]
		if _, errs, code := l.gog("calendar", "update", "primary", id, "--summary", updated); code != 0 {
			t.Fatalf("gog calendar update: %d %s", code, errs)
		}
		l.until("更新产生新事件", func() bool {
			for _, e := range l.myEvents(tag) {
				if e.Subject == updated {
					return true
				}
			}
			return false
		})
		l.settle(2) // 再多拉两个周期：同一版本不应重复成新事件
		var oldV, newV []event
		for _, e := range l.myEvents(tag) {
			switch e.Subject {
			case subj:
				oldV = append(oldV, e)
			case updated:
				newV = append(newV, e)
			}
		}
		if len(oldV) != 1 || len(newV) != 1 {
			t.Fatalf("旧版本和新版本应各有 1 个事件（同一版本不重复）: old=%d new=%d", len(oldV), len(newV))
		}
		if newV[0].Key.EventID == oldV[0].Key.EventID || !strings.HasPrefix(newV[0].Key.EventID, uid+"|") {
			t.Errorf("更新应产生同一 UID 的新事件 id: %q → %q", oldV[0].Key.EventID, newV[0].Key.EventID)
		}
		if newV[0].Type != "event.updated" {
			t.Errorf("更新后的事件类型应为 event.updated: %s", newV[0].Type)
		}
		// 更新不产生重复事项：新旧事件指向同一个事项，或新事件不另建事项。
		if newV[0].Item != oldV[0].Item {
			t.Errorf("更新应写回原事项而不是新建: %s vs %q", oldV[0].Item, newV[0].Item)
		}
		if got := str(l.item(oldV[0].Item), "title"); got != updated {
			t.Errorf("原事项的标题应刷新为更新后的标题: %q", got)
		}
		if n := len(l.ModelStub.Requests()); n != 0 {
			t.Errorf("更新不应调用判断型: %d", n)
		}

		// 重复日程：每个实例按 UID + RECURRENCE-ID 区分。
		rsubj := prefix + "rec " + tag
		_, ruid := l.create(rsubj, "2027-01-17T19:20:00Z", "2027-01-17T19:30:00Z", "--rrule", "RRULE:FREQ=DAILY;COUNT=3")
		l.until("重复日程的三个实例都进入账本", func() bool { return len(l.myEvents("rec "+tag)) >= 3 })
		l.settle(2)
		evs := l.myEvents("rec " + tag)
		seen := map[string]bool{}
		for _, e := range evs {
			parts := strings.Split(e.Key.EventID, "|")
			if len(parts) != 3 || parts[0] != ruid || parts[1] == "" {
				t.Errorf("重复日程实例的事件 id 应为 UID|RECURRENCE-ID|更新标识: %q", e.Key.EventID)
				continue
			}
			if seen[parts[1]] {
				t.Errorf("同一实例 %s 出现了重复事件", parts[1])
			}
			seen[parts[1]] = true
		}
		if len(evs) != 3 || len(seen) != 3 {
			t.Errorf("COUNT=3 的重复日程应展开为 3 个互不相同的实例: events=%d distinct=%d", len(evs), len(seen))
		}
		// 实测结论写回设计 §9.7（见 docs/solution-design.md 的实测段）：gog calendar events 默认展开重复日程，
		// 每个实例带自己的 id、同一个 iCalUID 和 originalStartTime。
	})

	cursorUnchanged := func(t *testing.T, before string) {
		t.Helper()
		if c := l.show().Cursor; c != before {
			t.Errorf("游标不应前进: %q → %q", before, c)
		}
	}
	// 每种失败：等两个周期，来源以退出码 2 失败、生成 needs_input、游标不动；之后恢复并等一次成功运行。
	failsWithNeedsInput := func(t *testing.T, apply, restore func(), wantErr string, adapterIn, adapterPath string) {
		t.Helper()
		before := l.show().Cursor
		apply()
		l.settle(2)
		s := l.show()
		cursorUnchanged(t, before)
		if s.Stats.NeedsInputItem == "" || !strings.Contains(s.Stats.LastError, "exit status 2") {
			t.Fatalf("应以退出码 2 生成 needs_input: item=%q last_error=%q", s.Stats.NeedsInputItem, s.Stats.LastError)
		}
		if se := l.adapterRefuses(adapterPath, adapterIn); !strings.Contains(strings.ToLower(se), strings.ToLower(wantErr)) {
			t.Errorf("适配器 stderr 应说明原因 %q: %q", wantErr, se)
		}
		restore()
		l.until("恢复后成功运行", func() bool { return l.show().Stats.LastError == "" })
	}

	step("ACT-CAL-04 工具失败时生成 needs_input，游标不动", func(t *testing.T) {
		in := func(acct, cred string) string {
			b, _ := json.Marshal(map[string]any{"source_id": src, "cursor": "", "credentials_path": cred,
				"config": map[string]any{"account": acct, "calendar_id": "primary", "horizon_days": 1}})
			return string(b)
		}
		fake := func(name, body string) string {
			d := filepath.Join(root, "fake-"+name)
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExec(t, filepath.Join(d, "gog"), body)
			return d
		}
		// 1) gog 不在 PATH
		bare := "/usr/bin:/bin"
		failsWithNeedsInput(t, func() { l.setPath(bare) }, func() { l.setPath(l.origPath) }, "gog", in(account, credLink), bare)
		// 2) 版本不在声明范围：只伪造 --version 的输出，其余不应被解析。
		d := fake("version", "#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 'v9.9.9 (fake)'; exit 0; }\nexit 1\n")
		vp := d + ":" + l.origPath
		failsWithNeedsInput(t, func() { l.setPath(vp) }, func() { l.setPath(l.origPath) }, "9.9.9", in(account, credLink), vp)
		// 3a) 授权失效之一：账号未授权（gog 自己的 "No auth for calendar"）。不动用户真实令牌。
		other := "apa-live-nobody@example.invalid"
		patch := func(acct string) func() {
			return func() {
				b, _ := json.Marshal(map[string]any{"config": map[string]any{"account": acct, "calendar_id": "primary", "horizon_days": horizon}})
				if r := l.Web("PATCH", "/v1/sources/"+src, string(b)); r.Code != 200 {
					t.Fatalf("改来源配置: HTTP %d %s", r.Code, r.Body)
				}
			}
		}
		failsWithNeedsInput(t, patch(other), patch(account), "auth", in(other, credLink), l.origPath)
		// 3b) 授权失效之二：keyring 口令不对，gog 解不开令牌库。凭据文件放在来源目录内，指向错口令。
		bad := filepath.Join(credDir, "bad-keyring.env")
		if err := os.WriteFile(bad, []byte("GOG_KEYRING_BACKEND=file\nGOG_KEYRING_PASSWORD=wrong-password\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		setCred := func(p string) func() {
			return func() {
				b, _ := json.Marshal(map[string]any{"credentials_path": p})
				if r := l.Web("PATCH", "/v1/sources/"+src, string(b)); r.Code != 200 {
					t.Fatalf("改凭据路径: HTTP %d %s", r.Code, r.Body)
				}
			}
		}
		failsWithNeedsInput(t, setCred(bad), setCred(credLink), "token", in(account, bad), l.origPath)
	})

	step("ACT-CAL-05 适配器契约测试不连真实账号", func(t *testing.T) {
		// 契约测试在不含 gog 凭据、不含 HOME 里 gog 配置的环境里跑：PATH 无 gog，HOME 指向空目录，
		// 但 go 的模块缓存要用真实 HOME，故用 GOMODCACHE/GOCACHE 显式固定。
		goenv := func(k string) string { o, _, _ := run(t, "", nil, "go", "env", k); return strings.TrimSpace(o) }
		env := []string{"PATH=" + filepath.Dir(mustLook(t, "go")) + ":/usr/bin:/bin", "HOME=" + t.TempDir(),
			"GOMODCACHE=" + goenv("GOMODCACHE"), "GOCACHE=" + goenv("GOCACHE")}
		out, errs, code := run(t, repo, env, "go", "test", "-count=1", "./sources/calendar/...")
		if code != 0 {
			t.Fatalf("go test ./sources/calendar/... 失败:\n%s%s", out, errs)
		}
		if strings.Contains(out+errs, account) {
			t.Error("契约测试输出不应含真实账号")
		}
		if _, err := os.Stat(filepath.Join(repo, "sources", "calendar", "testdata", "0.9.0", "events.json")); err != nil {
			t.Errorf("声明版本 0.9.0 缺录制样本: %v", err)
		}
	})
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
