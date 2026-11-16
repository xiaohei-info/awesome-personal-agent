//go:build e2e

package rawingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/srcstub"
)

// TestFlowRawLimits 是验收 flow-raw-limits（ACT-RLIM-01…04）：超过单条上限的事件整条拒收并留痕，同批其余事件照常落盘，
// 游标推进；内核不截取一段入账。
func TestFlowRawLimits(t *testing.T) {
	k := setup(t)
	const src = "src_acc_raw"
	k.addStub(src, "mail", "full", "full")

	k.step("ACT-RLIM-01 调低单条上限便于验证", func(t *testing.T) {
		k.setConfig("max_source_event_mb: 1\n")
		r := k.Apa(k.OBS, "", "doctor", "--json")
		if r.Code != 0 || strings.Contains(r.Stdout, "max_source_event_mb") && strings.Contains(r.Stdout, `"error"`) {
			t.Fatalf("apa doctor --json 退出码 %d，应无配置错误\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
	})

	k.step("ACT-RLIM-02 超限事件被拒收，前后两条照常入账，游标推进", func(t *testing.T) {
		k.setCase(src, "oversize-mid")
		k.until("oversize-mid 的游标推进", func() bool { return k.show(src).Cursor == "oversize-mid" })
		var ids []string
		for _, e := range k.events(src) {
			if !strings.HasPrefix(e.DedupeKey.EventID, "oversize-") { // 登记时的初始 case（mail-like）不属于本步
				continue
			}
			ids = append(ids, e.DedupeKey.EventID)
			if e.DedupeKey.EventID == "oversize-1" || e.DedupeKey.EventID == "oversize-3" {
				eq(t, e.DedupeKey.EventID+" 的 content.state", e.Content.State, "full")
			}
		}
		if got := strings.Join(ids, ","); got != "oversize-1,oversize-3" {
			t.Fatalf("事件列表 = %s，期望第 1、3 条照常入账、第 2 条不在", got)
		}
		var hit *rejectRow
		for _, r := range k.rejects(src) {
			if r.Line == 2 && strings.HasPrefix(r.Reason, "event_too_large") { // 第 2 条；原始行落账时截到 512 字节，id 在截断之后
				hit = &r
			}
		}
		if hit == nil {
			t.Fatalf("被拒收的事件里没有第 2 条: %+v", k.rejects(src))
		}
		m := regexpBytes.FindStringSubmatch(hit.Reason)
		if m == nil {
			t.Fatalf("原因 = %q，期望 event_too_large 并带实际字节数", hit.Reason)
		}
		var n int
		fmt.Sscan(m[1], &n)
		if n < srcstub.OversizeBytes {
			t.Errorf("原因里的实际字节数 %d 小于第 2 条原文 %d", n, srcstub.OversizeBytes)
		}
		// 账本里没有第 2 条的任何截取片段：整个账本文件里找不到它正文的一小段。
		k.noFragment(srcstub.Body("oversize", 4096)[:200], "ACT-RLIM-02 第 2 条的正文开头")
		if s := k.show(src); s.Stats.ConsecutiveFailures != 0 {
			t.Errorf("整行拒收不应让这次拉取失败: %+v", s.Stats)
		}
	})

	k.step("ACT-RLIM-03 聚合消息超过 200 条的事件整行拒收", func(t *testing.T) {
		// 同一批里夹一条正常事件：证明其余行照常。
		many := k.stubOut("too-many-msgs")[0]
		normal := map[string]any{"id": "after-many", "type": "mail.received", "subject": "After many", "detail": "normal row after the rejected one",
			"occurred_at": "2026-11-10T10:00:00Z"}
		k.setJSONL(src, []map[string]any{many, normal}, "many-batch")
		k.eventByStubID(src, "after-many")
		k.until("游标推进到本批之后", func() bool { return k.show(src).Cursor == "many-batch" })
		var hit *rejectRow
		for _, r := range k.rejects(src) {
			if r.Line == 1 {
				hit = &r
			}
		}
		if hit != nil && !(strings.Contains(hit.Reason, "items has 201 entries") && strings.Contains(hit.Reason, "at most 200")) {
			t.Errorf("原因 = %q，应写明 201 条超过上限 200", hit.Reason)
		}
		if hit == nil {
			t.Fatalf("超过 200 条消息的行应进被拒收的事件并写明原因: %+v", k.rejects(src))
		}
		for _, e := range k.events(src) {
			if e.DedupeKey.EventID == "too-many-msgs-1" {
				t.Errorf("超过 200 条消息的事件不应入账（也不是只存前 200 条）: %+v", e)
			}
		}
	})

	k.step("ACT-RLIM-04 一次拉取的 stdout 超过 max_source_output_mb 时的处置", func(t *testing.T) {
		// 配置校验要求 max_source_event_mb 小于 max_source_output_mb（§16.2），且 event 最小为 1：output 取 2 MiB（验收文档写的 1 MiB 不可配）。
		k.setConfig("max_source_event_mb: 1\nmax_source_output_mb: 2\n")
		before := k.show(src)
		var lines []map[string]any
		for i := 0; i < 12; i++ { // 12 × 200 KiB ≈ 2.4 MiB
			lines = append(lines, map[string]any{"id": fmt.Sprintf("out-%02d", i), "type": "mail.received", "subject": fmt.Sprintf("Out %d", i),
				"detail": srcstub.Body(fmt.Sprintf("o%d", i), srcstub.BigBodyBytes), "occurred_at": "2026-11-10T11:00:00Z"})
		}
		k.setJSONL(src, lines, "out-batch")
		k.until("来源记录超限的临时失败", func() bool {
			s := k.show(src)
			return s.Stats.ConsecutiveFailures > 0 && strings.Contains(s.Stats.LastError, "max_source_output_mb")
		})
		var landed []string
		for _, e := range k.events(src) {
			if strings.HasPrefix(e.DedupeKey.EventID, "out-") {
				landed = append(landed, e.DedupeKey.EventID)
				eq(t, e.DedupeKey.EventID+" 的 content.state", e.Content.State, "full")
			}
		}
		if len(landed) == 0 || len(landed) >= 12 {
			t.Fatalf("上限内已解析的事件应落盘、其余丢弃，实际落盘 %v", landed)
		}
		for i, id := range landed { // 落盘的是前缀：末尾的半行不入账
			eq(t, "落盘事件顺序", id, fmt.Sprintf("out-%02d", i))
		}
		eq(t, "游标不动", k.show(src).Cursor, before.Cursor)
		if k.show(src).Cursor == "out-batch" {
			t.Error("游标被推进到了本批之后")
		}
		for _, r := range k.rejects(src) {
			if strings.Contains(r.Raw, "out-") && strings.Contains(r.Raw, "o11 line") {
				t.Errorf("半行被当作输出行入账或登记: %.80s", r.Raw)
			}
		}

		// 结束后改回缺省并重启：同一批输出放得下，其余事件落盘、游标推进。
		k.setConfig("")
		k.until("缺省上限下游标推进、全部事件落盘", func() bool { return k.show(src).Cursor == "out-batch" })
		n := 0
		for _, e := range k.events(src) {
			if strings.HasPrefix(e.DedupeKey.EventID, "out-") {
				n++
			}
		}
		eq(t, "恢复缺省后落盘的 out-* 事件数", n, 12)
	})
}

var regexpBytes = regexp.MustCompile(`^event_too_large: (\d+) bytes`)

// noFragment 在整个账本数据库里找不到 frag：被拒收的事件没有任何截取片段入账。
func (k *kit) noFragment(frag, what string) {
	k.t.Helper()
	k.StopKernel() // 关掉内核再读：WAL 已合并，读到的是账本全貌
	defer k.StartKernelAgain()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(k.Home, "ledger.db")+"?mode=ro")
	if err != nil {
		k.t.Fatal(err)
	}
	defer db.Close()
	tabs, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		k.t.Fatal(err)
	}
	var names []string
	for tabs.Next() {
		var n string
		_ = tabs.Scan(&n)
		names = append(names, n)
	}
	tabs.Close()
	for _, tbl := range names {
		cols, err := db.Query(`SELECT name FROM pragma_table_info('` + tbl + `')`)
		if err != nil {
			continue
		}
		var cs []string
		for cols.Next() {
			var c string
			_ = cols.Scan(&c)
			cs = append(cs, c)
		}
		cols.Close()
		for _, c := range cs {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM "%s" WHERE CAST("%s" AS TEXT) LIKE ? ESCAPE '\'`, tbl, c)
			like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(frag) + "%"
			if err := db.QueryRow(q, like).Scan(&n); err == nil && n > 0 {
				// 来源拒收记录里的原始行被截到 512 字节，且只在 rejects 表；其余表不得出现。
				if strings.Contains(tbl, "reject") {
					continue
				}
				k.t.Errorf("%s：账本表 %s 列 %s 里找到 %d 处片段", what, tbl, c, n)
			}
		}
	}
}
