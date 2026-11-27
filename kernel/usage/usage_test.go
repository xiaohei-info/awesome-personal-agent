package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 运行记账与汇总：按声明取最后一条结果事件的用量；没回报的只计次数；唤醒运行与非 Agent 运行不记；同一运行只记一次。
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
		"usage":{"input":"$.tokens.input","output":"$.tokens.output","cache_read":"$.tokens.cache_read"}}}`
	if _, err := executors.ParseDeclaration([]byte(decl)); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES ('h', 1, ?, ?)", decl, executors.StateActive)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	run := func(id string, c domain.RunCategory, launch, stdout string) {
		t.Helper()
		r := domain.Run{ID: id, Category: c, Harness: "h", Launch: launch, FinishedAt: &now}
		if err := AddRun(ctx, st, proc.Result{Run: r, Stdout: []byte(stdout)}, now); err != nil {
			t.Fatal(err)
		}
	}
	ok := "cut off line\n" + `{"type":"tool_result","tokens":{"input":999,"output":999}}` + "\n" +
		`{"type":"result","tokens":{"input":100,"output":7,"cache_read":60}}` + "\nnot json\n"
	run("r1", domain.RunTask, "", ok)
	run("r1", domain.RunTask, "", ok)
	run("r2", domain.RunTask, "", `{"type":"result","tokens":{"input":0,"output":0}}`)
	run("r3", domain.RunBackground, policy.LaunchWake, ok)
	run("r4", domain.RunSource, "", ok)
	if err := st.Update(ctx, func(tx *store.Tx) error {
		return Add(tx, now.AddDate(0, 0, -1), Record{Key: Key{Part: PartModel, Purpose: "p"}, Counts: Counts{Requests: 1}})
	}); err != nil {
		t.Fatal(err)
	}

	var got, today Summary
	if err := st.View(ctx, func(tx *store.Tx) (err error) {
		if got, err = Summarize(tx, now.AddDate(0, 0, -7)); err != nil {
			return err
		}
		today, err = Summarize(tx, now.Truncate(24*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := Row{Key{Part: PartAgent, Purpose: "task", Agent: "甲"}, Counts{Requests: 2, Reported: 1, InputTokens: 100, OutputTokens: 7, CacheReadTokens: 60}}
	if len(got.Rows) != 2 || got.Rows[0] != want {
		t.Fatalf("rows = %+v", got.Rows)
	}
	if len(got.Days) != 2 || got.Days[0].Date != "2026-03-02" || got.Days[1].Date != "2026-03-01" {
		t.Fatalf("days = %+v", got.Days)
	}
	if len(today.Rows) != 1 || today.Since == nil || !today.Since.Equal(now.AddDate(0, 0, -1)) {
		t.Fatalf("today = %+v", today)
	}
}

// 声明为会话累计的用量按同一会话上一条做差：续接不重复计；累计值变小（压缩或重置）按本次整条计；
// 续接后换了会话 id 按续接的那个 id 找上一条；别的会话互不影响。
func TestCumulativeSessionUsage(t *testing.T) {
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
	decl := `{"harness":"h","headless":{"command":["x"],"session":{"extract":"$.s"},"access":{"read":[]},
		"usage":{"input":"$.in","output":"$.out","cache_read":"$.hit","cumulative":true}}}`
	if err := st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES ('h', 1, ?, ?)", decl, executors.StateActive)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		resumed, session string
		in, out, hit     int
		want             [3]int64 // 到这一步为止的合计
	}{
		{"", "a", 100, 10, 50, [3]int64{100, 10, 50}},
		{"a", "a", 250, 25, 150, [3]int64{250, 25, 150}},
		{"a", "a", 400, 45, 300, [3]int64{400, 45, 300}},
		{"", "b", 70, 7, 0, [3]int64{470, 52, 300}},
		{"a", "a", 30, 3, 0, [3]int64{500, 55, 300}},   // 重置
		{"a", "a2", 90, 9, 20, [3]int64{560, 61, 320}}, // 换了 id
		{"a2", "a2", 100, 9, 20, [3]int64{570, 61, 320}},
	} {
		r := domain.Run{ID: fmt.Sprint("r", i), Category: domain.RunBackground, Harness: "h", SessionID: c.resumed}
		out := fmt.Sprintf(`{"type":"result","s":%q,"in":%d,"out":%d,"hit":%d}`, c.session, c.in, c.out, c.hit)
		if err := AddRun(ctx, st, proc.Result{Run: r, Stdout: []byte(out)}, now); err != nil {
			t.Fatal(err)
		}
		var got Summary
		if err := st.View(ctx, func(tx *store.Tx) (err error) { got, err = Summarize(tx, now); return }); err != nil {
			t.Fatal(err)
		}
		g := got.Rows[0].Counts
		if sum := [3]int64{g.InputTokens, g.OutputTokens, g.CacheReadTokens}; sum != c.want || g.Reported != int64(i+1) {
			t.Fatalf("step %d: sum %v reported %d, want %v", i, sum, g.Reported, c.want)
		}
	}
}
