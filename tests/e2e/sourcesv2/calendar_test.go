//go:build e2e

package sourcesv2

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const calArgPrefix = "--no-input --json --enable-commands=calendar --account=acc@example.com calendar events"

// TestFlowSourceCalendar 按验收文档 flow-source-calendar 的顺序执行 ACT-CAL-01..11。
func TestFlowSourceCalendar(t *testing.T) {
	k := setup(t)
	const src = "src_cal"
	home := k.Projects["acc-home"]
	cred := filepath.Join(k.Home, "sources", src, "keyring.env")
	const password = "acc-keyring-pw"
	var a1Item, c1First, c1Second string
	defer func() { _, _, _ = a1Item, c1First, c1Second }()

	// bySubject 取标题为 subject 的来源事件（同一日程的不同版本按 id 区分，这里取各版本里的第一个或指定版本）。
	bySubject := func(subject string) []SrcEvent {
		var out []SrcEvent
		for _, e := range k.events(src) {
			if e.Subject == subject {
				out = append(out, e)
			}
		}
		return out
	}
	// settledSubject 等标题为 subject 的事件出现并完成处置。
	settledSubject := func(subject string) SrcEvent {
		var ev SrcEvent
		k.until("日程事件 "+subject+" 完成处置", func() bool {
			for _, e := range bySubject(subject) {
				if e.Disposition != "pending" {
					ev = e
					return true
				}
			}
			return false
		})
		return ev
	}

	k.step("ACT-CAL-01 登记日历适配器（录制样本）", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cred, []byte("GOG_KEYRING_PASSWORD="+password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		k.SetToolCase("cal-base")
		// 登记时 calendar-event 之外的事件（d1 已取消）走默认判断，给它一个明确的"不值得"。
		k.clsYes("取消的聚餐", 0.05)
		k.webSource(map[string]any{"id": src, "type": "script", "command": filepath.Join(progDir, "calendar"), "schedule": "1s", "timeout": 20,
			"source_type": "calendar", "dedupe_domain": "acc-cal", "default_project": home, "credentials_path": cred,
			"config": map[string]any{"account": "acc@example.com"}})
		k.clearToolLog()
		s := k.show(src)
		if !s.Enabled || s.DefaultProject != home {
			t.Errorf("来源应 enabled、default_project=acc-home: %+v", s)
		}
		// 口令只在凭据文件里：账本、配置、数据目录里的其他文件都不含它。
		var hits []string
		_ = filepath.Walk(k.Home, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(password)) {
				hits = append(hits, p)
			}
			return nil
		})
		if len(hits) != 1 || hits[0] != cred {
			t.Errorf("口令只应出现在凭据文件里: %v", hits)
		}
	})

	k.step("ACT-CAL-02 日程经确定性规则变成建议，归入来源默认项目，不调模型", func(t *testing.T) {
		for _, subject := range []string{"保险到期日", "牙医复诊", "周会"} {
			k.until(subject+" 的事件入账", func() bool { return len(bySubject(subject)) > 0 })
		}
		k.until("周会的两个实例都入账并处置", func() bool {
			evs := bySubject("周会")
			if len(evs) != 2 {
				return false
			}
			for _, e := range evs {
				if e.Disposition == "pending" {
					return false
				}
			}
			return true
		})
		a1 := settledSubject("保险到期日")
		b1 := settledSubject("牙医复诊")
		c1 := bySubject("周会")
		eq(t, "a1 类型", a1.Type, "event.created")
		eq(t, "b1 类型（更新时间按秒晚于创建时间）", b1.Type, "event.updated")
		for _, ev := range append([]SrcEvent{a1, b1}, c1...) {
			if ev.Disposition != "proposed" || ev.Item == "" {
				t.Fatalf("%s 应成为 proposed 事项: %+v", ev.Subject, ev)
			}
			it := k.item(ev.Item)
			if it.Kind != "task" || it.Phase != "proposed" || it.Project != home {
				t.Errorf("%s 应是 kind=task、proposed、项目取来源默认项目 acc-home: %+v", ev.Subject, it)
			}
			if !k.eventRule(ev, "calendar-event") {
				t.Errorf("%s 应命中 calendar-event", ev.Subject)
			}
		}
		if len(c1[0].CorrelationKeys) != 1 || len(c1[1].CorrelationKeys) != 1 || c1[0].CorrelationKeys[0] == c1[1].CorrelationKeys[0] {
			t.Errorf("周会两个实例的关联键应互不相同: %v %v", c1[0].CorrelationKeys, c1[1].CorrelationKeys)
		}
		if c1[0].Item == c1[1].Item {
			t.Errorf("周会的两个实例应各成一条 proposed 事项，实际合并成一条 %s（merge_key=%v）", c1[0].Item, c1[0].Raw["merge_key"])
		}
		for _, ck := range []string{c1[0].CorrelationKeys[0], c1[1].CorrelationKeys[0]} {
			if !strings.Contains(ck, "|") || strings.HasSuffix(ck, "|") {
				t.Errorf("实例的关联键应是 <UID>|<RECURRENCE-ID>: %q", ck)
			}
		}
		a1Item = a1.Item
		for _, ev := range c1 {
			if strings.Contains(ev.CorrelationKeys[0], "2026-03-10") {
				c1First = ev.Item
			} else {
				c1Second = ev.Item
			}
		}
		// 工具只被读调用。
		for _, argv := range k.toolCalls("gog") {
			joined := strings.Join(argv, " ")
			if joined != "--version" && !strings.HasPrefix(joined, calArgPrefix) {
				t.Errorf("gog 只应有版本查询和 calendar events 读调用: %v", argv)
			}
		}
		for _, subject := range []string{"保险到期日", "牙医复诊", "周会"} {
			eq(t, "两份模型替身日志里提到「"+subject+"」的请求", k.modelAbout(subject), 0)
		}
	})

	k.step("ACT-CAL-03 同一日程的更新经关联键刷新原事项", func(t *testing.T) {
		k.webOK("POST", "/items/"+a1Item+"/accept", `{}`, 200)
		eq(t, "接受后阶段", k.item(a1Item).Phase, "open")
		events, lines := len(k.events(src)), k.modelLines()
		k.SetToolCase("cal-updated")
		var newA1, newC1 SrcEvent
		k.until("a1 与 c1 第一个实例的新版本入账并处置", func() bool {
			for _, e := range k.events(src) {
				if e.Subject == "保险到期日（改到周五）" && e.Disposition != "pending" {
					newA1 = e
				}
				if e.Subject == "周会（改线上）" && e.Disposition != "pending" {
					newC1 = e
				}
			}
			return newA1.ID != "" && newC1.ID != ""
		})
		eq(t, "新增来源事件数", len(k.events(src))-events, 2)
		if newA1.Type != "event.updated" || newA1.DedupeKey.EventID == bySubject("保险到期日")[0].DedupeKey.EventID {
			t.Errorf("a1 的更新应是新的 event.updated 事件（id 与旧版本不同）: %+v", newA1)
		}
		for _, ev := range []SrcEvent{newA1, newC1} {
			if ev.Disposition != "attached" || ev.AttachState != "processed" {
				t.Errorf("%s 的新事件应 attached、attach_state=processed: %+v", ev.Subject, ev)
			}
		}
		a1 := k.item(a1Item)
		if a1.Title != "保险到期日（改到周五）" || a1.Phase != "open" || newA1.Item != a1Item {
			t.Errorf("a1 的事项应刷新标题并保持 open: %+v（事件指向 %s）", a1, newA1.Item)
		}
		if len(k.audit("item.updated", a1Item)) == 0 {
			t.Error("a1 的事项应记 item.updated")
		}
		if newC1.Item != c1First {
			t.Errorf("c1 第一个实例的新事件应刷新它自己的事项 %s: %+v", c1First, newC1)
		}
		if got := k.item(c1First).Title; got != "周会（改线上）" {
			t.Errorf("c1 第一个实例的 proposed 事项标题应刷新: %q", got)
		}
		if c1Second == c1First {
			t.Errorf("c1 的两个实例是同一条事项 %s，无法证明\"第二个实例不变\"", c1Second)
		} else if got := k.item(c1Second).Title; got != "周会" {
			t.Errorf("c1 第二个实例的事项不应变: %q", got)
		}
		for _, subject := range []string{"保险到期日（改到周五）", "周会（改线上）"} {
			eq(t, "两份模型替身日志里提到「"+subject+"」的请求", k.modelAbout(subject), 0)
		}
		_ = lines
		eq(t, "建议流之外没有新建事项：事项总数", len(k.itemIDs()), 4-boolInt(c1First == c1Second))
	})

	// failsAs 把工具替身切到 c，等日历来源再跑一次并确认：退出码 2、生成 needs_input、游标不动。
	failsAs := func(t *testing.T, c string) SrcState {
		before := k.show(src)
		k.SetToolCase(c)
		var s SrcState
		k.until("case "+c+" 的拉取失败并生成 needs_input", func() bool {
			s = k.show(src)
			return s.Stats.Runs > before.Stats.Runs && strings.HasPrefix(s.Stats.LastError, exitConfig)
		})
		if s.Cursor != before.Cursor {
			t.Errorf("游标不应前进: %q → %q", before.Cursor, s.Cursor)
		}
		if s.Stats.NeedsInputItem == "" || k.item(s.Stats.NeedsInputItem).Phase != "needs_input" {
			t.Errorf("应生成 needs_input 事项: %+v", s.Stats)
		}
		return s
	}

	k.step("ACT-CAL-04 工具未安装时生成 needs_input", func(t *testing.T) {
		events := len(k.events(src))
		bin, off := filepath.Join(k.ToolBin, "gog"), filepath.Join(k.ToolBin, "gog.off")
		if err := os.Rename(bin, off); err != nil {
			t.Fatal(err)
		}
		s := failsAs(t, "cal-updated")
		if err := os.Rename(off, bin); err != nil {
			t.Fatal(err)
		}
		k.until("放回后下一周期照常拉取", func() bool { n := k.show(src); return n.Stats.ConsecutiveFailures == 0 && n.Stats.Runs > s.Stats.Runs })
		eq(t, "来源事件数（不产生重复）", len(k.events(src)), events)
	})

	k.step("ACT-CAL-05 适配器契约测试", func(t *testing.T) {
		cmd := exec.Command("go", "test", "-count=1", "./sources/calendar/...", "./sources/applepim/...")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go test ./sources/calendar/... ./sources/applepim/...: %v\n%s", err, out)
		}
		for _, f := range []string{"sources/calendar/testdata/0.9.0/version.txt", "sources/calendar/testdata/0.9.0/events.json", "sources/applepim/testdata/events.json"} {
			if _, err := os.Stat(filepath.Join(repoRoot(), f)); err != nil {
				t.Errorf("缺录制样本 %s", f)
			}
		}
		b, err := os.ReadFile(filepath.Join(repoRoot(), "sources/calendar/calendar_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"event.created", "event.updated", "event.cancelled", "CorrelationKeys", "ExitAuth", "ExitTemporary"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("日历契约测试缺少对 %s 的断言", want)
			}
		}
	})

	k.step("ACT-CAL-06 版本不在声明范围时生成 needs_input", func(t *testing.T) {
		k.clearToolLog()
		s := failsAs(t, "cal-version-0.1")
		if !strings.Contains(s.Stats.LastError, "exit status 2") {
			t.Errorf("退出码应为 2: %+v", s.Stats)
		}
		for _, argv := range k.toolCalls("gog") {
			if len(argv) != 1 || argv[0] != "--version" {
				t.Errorf("版本查询之后不应再调用取数命令: %v", argv)
			}
		}
	})

	k.step("ACT-CAL-07 认证失败时生成 needs_input", func(t *testing.T) {
		events := len(k.events(src))
		failsAs(t, "cal-auth-fail")
		k.SetToolCase("cal-updated")
		k.until("切回后恢复拉取", func() bool { return k.show(src).Stats.ConsecutiveFailures == 0 })
		k.cycle(src, 1)
		eq(t, "来源事件数（恢复后无重复）", len(k.events(src)), events)
	})

	k.step("ACT-CAL-08 凭据经 credentials_path 只注入给工具子进程", func(t *testing.T) {
		k.clearToolLog()
		k.cycle(src, 2)
		gog := 0
		for _, le := range k.ToolLog() {
			if le.Tool != "gog" {
				continue
			}
			gog++
			if !contains(le.Env, "GOG_KEYRING_PASSWORD") || !contains(le.Env, "GOG_KEYRING_BACKEND") {
				t.Errorf("工具子进程环境里应有 GOG_KEYRING_PASSWORD 与 GOG_KEYRING_BACKEND: %v", le.Env)
			}
			if strings.Contains(strings.Join(le.Argv, " "), password) {
				t.Errorf("口令不应出现在 argv: %v", le.Argv)
			}
		}
		if gog == 0 {
			t.Fatal("TOOL_LOG 里没有 gog 调用")
		}
		page := k.apa(k.OBS, 0, "source", "show", src).Stdout + k.webOK("GET", "/sources/"+src, "", 200) + k.webOK("GET", "/sources", "", 200)
		if strings.Contains(page, password) || strings.Contains(k.ServiceLog(), password) {
			t.Error("口令不应出现在来源页或内核日志里")
		}
		// 缺必填键：文件里只有一个无关的键，值是不能出现在报错里的内容。
		if err := os.WriteFile(cred, []byte("OTHER_SECRET=do-not-echo-me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := failsAs(t, "cal-updated")
		if strings.Contains(s.Stats.LastError, "do-not-echo-me") || strings.Contains(k.ServiceLog(), "do-not-echo-me") ||
			strings.Contains(k.item(s.Stats.NeedsInputItem).Raw["title"].(string), "do-not-echo-me") {
			t.Error("报错不应带文件内容")
		}
		if err := os.WriteFile(cred, []byte("GOG_KEYRING_PASSWORD="+password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		k.until("恢复凭据后拉取成功", func() bool { return k.show(src).Stats.ConsecutiveFailures == 0 })
	})

	k.step("ACT-CAL-09 已关闭事项不再被刷新，新版本按普通事件处理", func(t *testing.T) {
		k.apa(k.DEL, 0, "item", "transition", a1Item, "--to", "done", "--reason", "保险已处理", "--evidence", "本人：保险这事办完了")
		sug := len(k.suggestions())
		k.SetToolCase("cal-updated-2")
		var ev SrcEvent
		k.until("a1 的再次更新入账并处置", func() bool {
			for _, e := range bySubject("保险到期日（改到下周一）") {
				if e.Disposition != "pending" {
					ev = e
					return true
				}
			}
			return false
		})
		if got := k.item(a1Item).Title; got != "保险到期日（改到周五）" {
			t.Errorf("已 done 的事项标题不应变: %q", got)
		}
		if ev.Disposition != "proposed" || ev.Item == "" || ev.Item == a1Item || !k.eventRule(ev, "calendar-event") {
			t.Fatalf("新事件应走 calendar-event 规则成为新的 proposed 建议: %+v", ev)
		}
		if it := k.item(ev.Item); it.Phase != "proposed" || it.Project != home {
			t.Errorf("新建议应是 proposed、项目 acc-home: %+v", it)
		}
		eq(t, "建议流条数", len(k.suggestions()), sug+1)
	})

	k.step("ACT-CAL-10 默认项目只影响之后的事件，项目取法按优先级", func(t *testing.T) {
		k.patchSource(src, map[string]any{"default_project": ""})
		k.SetToolCase("cal-new")
		ev := settledSubject("新增的体检")
		if ev.Type != "event.created" || ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("e1 应成为建议: %+v", ev)
		}
		if p := k.item(ev.Item).Project; p != "" {
			t.Errorf("清空默认项目后 e1 的建议不应归任何项目: %q", p)
		}
		if p := k.item(c1First).Project; p != home {
			t.Errorf("已建事项的项目应仍是 acc-home: %q", p)
		}
	})

	k.step("ACT-CAL-11 到期的提醒事项经内置规则成为建议", func(t *testing.T) {
		k.patchSource(src, map[string]any{"enabled": false})
		const rem = "src_pimrem"
		k.webSource(map[string]any{"id": rem, "type": "script", "command": filepath.Join(progDir, "apple-pim-source"), "schedule": "1s", "timeout": 20,
			"source_type": "reminder", "dedupe_domain": "acc-pim-rem", "default_project": home,
			"config": map[string]any{"kind": "reminders", "bin_dir": k.ToolBin}})
		lines := k.modelLines()
		k.SetToolCase("pim-reminders")
		k.until("四条提醒事项入账并处置", func() bool {
			evs := k.events(rem)
			if len(evs) != 4 {
				return false
			}
			for _, e := range evs {
				if e.Disposition == "pending" {
					return false
				}
			}
			return true
		})
		for _, ev := range k.events(rem) {
			it := k.item(ev.Item)
			if ev.Type != "reminder.due" || ev.Disposition != "proposed" || it.Kind != "task" || it.Phase != "proposed" || it.Project != home {
				t.Errorf("提醒应成为 kind=task、project=acc-home 的建议: %+v %+v", ev, it)
			}
			if !k.eventRule(ev, "reminder-due") {
				t.Errorf("%s 应命中 reminder-due", ev.Subject)
			}
		}
		eq(t, "两份模型替身日志新增行数", k.modelLines()-lines, 0)
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
