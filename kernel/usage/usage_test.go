package usage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 运行记账与汇总：用量取声明的运行目录文件；没回报的只计次数；唤醒运行与非 Agent 运行不记；同一运行只记一次。
func TestRunsAndSummary(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{executors.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	decl := `{"harness":"h","name":{"zh":"甲","en":"A"},"headless":{"command":["x"],"session":{"extract":"$.s"},"access":{"read":[]},
		"usage":{"file":"u.json","input":"$.input_tokens","output":"$.output_tokens","cache_read":"$.cache_read_tokens"}}}`
	if err := st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES ('h', 1, ?, ?)", decl, executors.StateActive)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	runs := t.TempDir()
	run := func(id string, c domain.RunCategory, launch, file string) {
		t.Helper()
		if file != "" {
			if err := os.MkdirAll(filepath.Join(runs, id), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(runs, id, "u.json"), []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r := domain.Run{ID: id, Category: c, Harness: "h", Launch: launch, FinishedAt: &now}
		if err := AddRun(ctx, st, runs, proc.Result{Run: r}, now); err != nil {
			t.Fatal(err)
		}
	}
	usage := func() string {
		return `{"input_tokens":100,"output_tokens":7,"cache_read_tokens":60}`
	}
	run("r1", domain.RunTask, "", usage())
	run("r1", domain.RunTask, "", usage())
	run("r2", domain.RunTask, "", `{"input_tokens":0,"output_tokens":0}`) // 一次请求都没成：未回报
	run("r3", domain.RunTask, "", "")
	run("r4", domain.RunBackground, policy.LaunchWake, usage())
	run("r5", domain.RunSource, "", usage())
	if err := st.Update(ctx, func(tx *store.Tx) error {
		return Add(tx, now.AddDate(0, 0, -1), Record{Key: Key{Part: PartModel, Purpose: "p"}, Counts: Counts{Requests: 1}})
	}); err != nil {
		t.Fatal(err)
	}

	var got, today Summary
	if err := st.View(ctx, func(tx *store.Tx) (err error) {
		if got, err = Summarize(tx, now, 7); err != nil {
			return err
		}
		today, err = Summarize(tx, now, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := Row{Key{Part: PartAgent, Purpose: "task", Agent: "甲"}, Counts{Requests: 3, Reported: 1, InputTokens: 100, OutputTokens: 7, CacheReadTokens: 60}}
	if len(got.Rows) != 2 || got.Rows[0] != want {
		t.Fatalf("rows = %+v", got.Rows)
	}
	// 按天：7 个连续日期补齐，旧的在前；只列有记录的点。
	if len(got.Slots) != 7 || got.Slots[0] != "2026-02-24" || got.Slots[6] != "2026-03-02" ||
		len(got.Series) != 2 || got.Series[0].Slot != "2026-03-01" || got.Series[1].Slot != "2026-03-02" || got.Series[1].Requests != 3 {
		t.Fatalf("slots = %v, series = %+v", got.Slots, got.Series)
	}
	// 今天：24 个钟点槽。
	if len(today.Slots) != 24 || today.Slots[0] != "00" || len(today.Series) != 1 || today.Series[0].Slot != "12" {
		t.Fatalf("today slots = %v, series = %+v", today.Slots, today.Series)
	}
	if len(today.Rows) != 1 || today.Since == nil || !today.Since.Equal(now.AddDate(0, 0, -1)) {
		t.Fatalf("today = %+v", today)
	}
}

// 通用用量契约（附录 A.2）：任意 harness 只凭声明回报。假 harness 从 stdout 事件回报，输入不含缓存、自带模型名；
// 声明经内核的解析器；没声明用量的另一个 harness 记为未回报，即使它写了同样的输出。
func TestDeclaredUsageContractAnyHarness(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{executors.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	base := `"headless":{"command":["x"],"session":{"extract":"$.id"},"access":{"read":[]}`
	for harness, decl := range map[string]string{
		"fake-stream": `{"harness":"fake-stream",` + base + `,"usage":{"event":"done","input":"$.u.in","output":"$.u.out",
			"cache_read":"$.u.hit","cache_write":"$.u.put","model":"$.u.model","input_excludes_cache":true}}}`,
		"fake-silent": `{"harness":"fake-silent",` + base + `}}`,
	} {
		if _, err := executors.ParseDeclaration([]byte(decl)); err != nil {
			t.Fatal(err)
		}
		if err := st.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES (?, 1, ?, ?)", harness, decl, executors.StateActive)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{`{"file":"../x","input":"$.i","output":"$.o"}`, `{"file":"u","event":"done","input":"$.i","output":"$.o"}`, `{"input":"$.i"}`} {
		if _, err := executors.ParseDeclaration([]byte(`{"harness":"b",` + base + `,"usage":` + bad + `}}`)); err == nil {
			t.Errorf("declaration with usage %s accepted", bad)
		}
	}
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	stdout := []byte("noise\n" + `{"type":"tool","u":{"in":999,"out":999}}` + "\n" +
		`{"type":"done","u":{"in":10,"out":4,"hit":30,"put":5,"model":"m-x"}}` + "\n" + `{"type":"tool","u":{"in":999,"out":999}}`)
	for i, harness := range []string{"fake-stream", "fake-silent"} {
		r := domain.Run{ID: harness, Category: domain.RunTask, Harness: harness, AgentSnapshot: &domain.AgentSnapshot{AgentName: harness, ConfiguredModel: "configured"}}
		if err := AddRun(ctx, st, t.TempDir(), proc.Result{Run: r, Stdout: stdout}, now); err != nil {
			t.Fatal(i, err)
		}
	}
	var got Summary
	if err := st.View(ctx, func(tx *store.Tx) (err error) { got, err = Summarize(tx, now, 1); return }); err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Key{PartAgent, "task", "fake-stream", "m-x"}, Counts{Requests: 1, Reported: 1, InputTokens: 45, OutputTokens: 4, CacheReadTokens: 30, CacheWriteTokens: 5}},
		{Key{PartAgent, "task", "fake-silent", "configured"}, Counts{Requests: 1}},
	}
	if !slices.Equal(got.Rows, want) {
		t.Fatalf("rows = %+v", got.Rows)
	}
}

// 日界与钟点按调用方时区，不按 UTC：东八区的 00:30 算今天 00 时，前一分钟算昨天。空账本也给出完整时间槽。
func TestSummaryLocalDayBoundary(t *testing.T) {
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	zone := time.FixedZone("x", 8*3600)
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, zone)
	read := func(days int) (s Summary) {
		t.Helper()
		if err := st.View(ctx, func(tx *store.Tx) (err error) { s, err = Summarize(tx, now, days); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	if s := read(30); len(s.Slots) != 30 || s.Slots[0] != "2026-02-01" || s.Since != nil || s.Series == nil || len(s.Series) != 0 {
		t.Fatalf("empty = %+v", s)
	}
	if err := st.Update(ctx, func(tx *store.Tx) error {
		for _, at := range []time.Time{time.Date(2026, 3, 2, 0, 30, 0, 0, zone), time.Date(2026, 3, 1, 23, 59, 0, 0, zone)} {
			if err := Add(tx, at.UTC(), Record{Key: Key{Part: PartModel, Purpose: "p"}, Counts: Counts{Requests: 1}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s := read(1); len(s.Series) != 1 || s.Series[0].Slot != "00" || s.Series[0].Requests != 1 {
		t.Fatalf("today = %+v", s.Series)
	}
	if s := read(7); len(s.Series) != 2 || s.Series[0].Slot != "2026-03-01" {
		t.Fatalf("week = %+v", s.Series)
	}
}
