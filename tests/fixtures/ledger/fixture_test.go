package ledgerfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- ACT-ENV-08：夹具内容按 OMAC-NOTES §5.1，清单给出全部期望基线 ----

func TestManifestHasEveryAcceptanceKey(t *testing.T) {
	dir, _ := gen(t, 2)
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_ledger", "tables", "event_max_seq", "event_payload_sha256", "payload_hash_method", "backlog",
		"structured_rule_hits", "attachable", "score_markers", "old_cascade_ignored", "rule_ignored", "user_ignored", "outside_window",
		"policy_versions", "rule_modes", "tokens", "web_password", "session_cookie", "frozen_commit", "migration_files", "binary"} {
		if v, ok := m[k]; !ok || string(v) == "null" || string(v) == "{}" || string(v) == "[]" || string(v) == `""` || string(v) == "0" {
			t.Errorf("manifest.json 缺键或为空: %s = %s", k, v)
		}
	}
	var oc struct {
		Window struct{ From, To string }
		Count  int
		By     map[string]int `json:"by_source"`
	}
	if err := json.Unmarshal(m["old_cascade_ignored"], &oc); err != nil || oc.Window.From == "" || oc.Window.To == "" || oc.Count == 0 || len(oc.By) == 0 {
		t.Errorf("old_cascade_ignored 必须含 window.from/to、count、by_source: %s", m["old_cascade_ignored"])
	}
}

// §5.1 逐条核对：夹具里确有这些数据，且清单的数与账本里数出来的一致。
func TestFixtureContentFollowsNotes51(t *testing.T) {
	dir, m := gen(t, 2)
	st := open(t, dir, 2)
	defer st.Close()

	// 已应用 v1、v2 迁移的结构；账本里没有任何更新的迁移。
	if got := count(t, st, "SELECT COUNT(*) FROM store_migrations WHERE module = 'sources_runner' AND version = 2"); got != 1 {
		t.Errorf("sources_runner v2 应已应用")
	}
	if st.SchemaLedger() != 2 || m.SchemaLedger != 2 {
		t.Errorf("结构版本 = %d/%d, 想要 2", st.SchemaLedger(), m.SchemaLedger)
	}

	// 来源：邮件、日历、代码托管、自定义脚本；outbound 与 default_project 不存在；拉取停用。
	kinds := map[string]bool{}
	for _, k := range strCol(t, st, "SELECT source_type FROM sources_registry_source") {
		kinds[k] = true
	}
	for _, k := range []string{"mail", "calendar", "github", "custom"} {
		if !kinds[k] {
			t.Errorf("缺少 %s 来源", k)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM sources_registry_source"); n != 7 {
		t.Errorf("来源数 = %d, 想要 7", n)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM sources_registry_source WHERE type != 'webhook' AND enabled != 0"); n != 0 {
		t.Errorf("拉取类来源应全部停用, 有 %d 个启用", n)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM sources_registry_source WHERE config LIKE '%outbound%' OR config LIKE '%default_project%'"); n != 0 {
		t.Errorf("v0.1 的来源没有 outbound 与 default_project")
	}
	cols := strings.Join(strCol(t, st, "SELECT name FROM pragma_table_info('sources_registry_source')"), ",")
	if strings.Contains(cols, "outbound") || strings.Contains(cols, "default_project") {
		t.Errorf("v0.1 结构的来源表不应有 outbound/default_project 列: %s", cols)
	}

	// 数百条积压，覆盖四种形态；约一成是规则可直接处置的结构化事件。
	b := m.Backlog
	if b.Total < 300 || b.Total > 900 {
		t.Errorf("积压 %d 条, 想要数百条", b.Total)
	}
	for _, c := range []struct {
		disp, reason string
		want         int
	}{{"unjudged", "model_unavailable", b.Unjudged["model_unavailable"]}, {"unjudged", "undecided", b.Unjudged["undecided"]}, {"unjudged", "", b.Unjudged["none"]}} {
		got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = ? AND unjudged_reason = ?", c.disp, c.reason)
		if got != c.want || got == 0 {
			t.Errorf("%s/%q: 账本 %d 条, 清单 %d 条（都应 > 0 且相等）", c.disp, c.reason, got, c.want)
		}
	}
	if got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'expired'"); got != b.Expired || got == 0 {
		t.Errorf("expired: 账本 %d, 清单 %d", got, b.Expired)
	}
	if sum := b.Unjudged["model_unavailable"] + b.Unjudged["undecided"] + b.Unjudged["none"] + b.Expired; sum != b.Total {
		t.Errorf("积压分布之和 %d != total %d", sum, b.Total)
	}
	if got := m.StructuredHits + m.Attachable; got < b.Total/12 || got > b.Total/8 {
		t.Errorf("结构化事件 %d 条, 想要积压 %d 的约一成", got, b.Total)
	}
	// 可挂靠：与一个已建议事件（已有事项）共享关联键；不可挂靠的没有。
	for _, id := range m.AttachableIDs {
		keys := strCol(t, st, "SELECT correlation_keys FROM sources_runner_event WHERE id = ?", id)
		if n := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'proposed' AND item != '' AND correlation_keys = ? AND source_id = 'src_calendar'", keys[0]); n == 0 {
			t.Errorf("可挂靠事件 %s 没有同关联键的已建议事件", id)
		}
	}
	for _, id := range m.StructuredIDs {
		keys := strCol(t, st, "SELECT correlation_keys FROM sources_runner_event WHERE id = ?", id)
		if n := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'proposed' AND correlation_keys = ?", keys[0]); n != 0 {
			t.Errorf("规则直接处置的事件 %s 不应能挂靠", id)
		}
	}

	// 分数标记：三种都有；有一个来源的积压里只有 [FX-MID]。
	for _, mk := range []string{"[FX-HIGH]", "[FX-MID]", "[FX-LOW]"} {
		got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition IN ('unjudged','expired') AND subject LIKE ?", "%"+mk+"%")
		if mc := m.ScoreMarkers[mk]; mc == nil || mc.Count != got || got == 0 {
			t.Errorf("%s: 账本 %d 条, 清单 %+v", mk, got, mc)
		}
	}
	midOnly := ""
	for src, n := range m.ScoreMarkers["[FX-MID]"].BySource {
		if m.ScoreMarkers["[FX-HIGH]"].BySource[src] == 0 && m.ScoreMarkers["[FX-LOW]"].BySource[src] == 0 {
			midOnly = src
			if n == 0 {
				t.Errorf("只含 MID 的来源 %s 没有事件", src)
			}
		}
	}
	if midOnly == "" {
		t.Errorf("应有一个来源的积压里只含 [FX-MID] 事件")
	}

	// v0.1 的 run.finished（category: judge）。
	if n := count(t, st, "SELECT COUNT(*) FROM eventlog_event WHERE type = 'run.finished' AND payload LIKE '%\"category\":\"judge\"%'"); n < 50 {
		t.Errorf("run.finished(judge) 只有 %d 条", n)
	}

	// 策略版本：带 model_endpoint；开与未开 threshold_judge 各一份；api_key 只在账本里，不进 Event。
	var withTJ, withoutTJ, withEP int
	for _, p := range m.PolicyDetails {
		if p.ModelEndpoint {
			withEP++
			if p.ThresholdJudge {
				withTJ++
			} else {
				withoutTJ++
			}
		}
	}
	if withEP < 2 || withTJ < 1 || withoutTJ < 1 {
		t.Errorf("带 model_endpoint 的策略版本应同时有开/关 threshold_judge: %+v", m.PolicyDetails)
	}
	for _, d := range strCol(t, st, "SELECT doc FROM policy_version WHERE doc LIKE '%model_endpoint%url%'") {
		if !strings.Contains(d, `"url":"`+fixtureModelAt+`"`) {
			t.Errorf("策略里的端点必须是丢弃地址 %s", fixtureModelAt)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM eventlog_event WHERE payload LIKE '%'||?||'%'", fixtureAPIKey); n != 0 {
		t.Errorf("api_key 不应进入 Event")
	}

	// 规则模式：未动过（observe）与本人切换过的都有，切换留有 rule.mode_changed。
	rm := m.RuleModes
	if len(rm.Untouched) == 0 || len(rm.OwnerSwitched) == 0 || len(rm.UserPackExplicitObserve) == 0 {
		t.Errorf("规则模式应覆盖未动过、本人切换过、覆盖包显式 observe: %+v", rm)
	}
	for k, mode := range rm.Current {
		got := strCol(t, st, "SELECT mode FROM rules_state WHERE key = ?", k)
		if len(got) != 1 || got[0] != mode {
			t.Errorf("规则 %s 模式 账本 %v 清单 %s", k, got, mode)
		}
	}
	for _, k := range rm.Untouched {
		if rm.Current[k] != "observe" {
			t.Errorf("未动过的内置规则 %s 应为 observe", k)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM eventlog_event WHERE type = 'rule.mode_changed'"); n != len(rm.OwnerSwitched) {
		t.Errorf("rule.mode_changed %d 条, 本人切换 %d 条", n, len(rm.OwnerSwitched))
	}

	// 覆盖内置包、带 confidence_threshold 的用户包，随数据目录落盘。
	judge, err := os.ReadFile(filepath.Join(dir, "packs", "follow-up", "prompts", "follow-up-judge.md"))
	if err != nil || !bytes.Contains(judge, []byte("confidence_threshold: 0.85")) {
		t.Errorf("用户包的判断提示词应带 confidence_threshold: 0.85 (%v)", err)
	}

	// waiting 且已排跟进定时的事项；登录会话（有效与过期）。
	for _, id := range m.WaitingItems {
		if count(t, st, "SELECT COUNT(*) FROM items WHERE id = ? AND phase = 'waiting'", id) != 1 ||
			count(t, st, "SELECT COUNT(*) FROM clock_timer WHERE id = ? AND kind = 'follow_up'", "followup_"+id) != 1 ||
			count(t, st, "SELECT COUNT(*) FROM followup_pending WHERE item = ?", id) != 1 {
			t.Errorf("waiting 事项 %s 缺定时或待跟进行", id)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM apicore_session"); n < 2 || count(t, st, "SELECT COUNT(*) FROM apicore_session WHERE expires_at < ?", T0.UnixNano()) < 1 {
		t.Errorf("应有有效与已过期的登录会话, 共 %d", n)
	}

	// 对照组：旧级联忽略（有 judge 运行、无规则、非本人）、规则 ignore、本人忽略、窗口外。
	win := m.OldCascadeIgnored
	inWindow := count(t, st, `SELECT COUNT(*) FROM sources_runner_event e WHERE disposition = 'ignored' AND occurred_at >= ? AND occurred_at < ?
		AND EXISTS (SELECT 1 FROM eventlog_event r WHERE r.type = 'run.finished' AND r.object = e.id AND r.payload LIKE '%"category":"judge"%')`, mustTime(win.Window.From), mustTime(win.Window.To))
	if inWindow != win.Count || inWindow == 0 {
		t.Errorf("窗口内旧级联忽略: 账本 %d, 清单 %d", inWindow, win.Count)
	}
	sum := 0
	for src, n := range win.BySource {
		sum += n
		got := count(t, st, `SELECT COUNT(*) FROM sources_runner_event e WHERE source_id = ? AND disposition = 'ignored' AND occurred_at >= ? AND occurred_at < ?
			AND EXISTS (SELECT 1 FROM eventlog_event r WHERE r.type = 'run.finished' AND r.object = e.id)`, src, mustTime(win.Window.From), mustTime(win.Window.To))
		if got != n {
			t.Errorf("by_source[%s]: 账本 %d, 清单 %d", src, got, n)
		}
	}
	if sum != win.Count || len(win.BySource) < 3 {
		t.Errorf("by_source 之和 %d != count %d, 或来源太少 %v", sum, win.Count, win.BySource)
	}
	for _, id := range m.OutsideWindow {
		if count(t, st, `SELECT COUNT(*) FROM sources_runner_event WHERE id = ? AND disposition = 'ignored' AND (occurred_at < ? OR occurred_at >= ?)`, id, mustTime(win.Window.From), mustTime(win.Window.To)) != 1 ||
			count(t, st, `SELECT COUNT(*) FROM eventlog_event WHERE type = 'run.finished' AND object = ?`, id) != 1 {
			t.Errorf("窗口外旧忽略 %s 应落在窗口外且带 judge 运行", id)
		}
	}
	for _, id := range m.RuleIgnored {
		if count(t, st, `SELECT COUNT(*) FROM eventlog_event WHERE type = 'inbound.disposed' AND object = ? AND actor LIKE '%"via":"rule"%'`, id) != 1 ||
			count(t, st, `SELECT COUNT(*) FROM eventlog_event WHERE type = 'run.finished' AND object = ?`, id) != 0 {
			t.Errorf("规则忽略 %s 应由规则处置且没有 judge 运行", id)
		}
	}
	for _, id := range m.UserIgnored {
		if count(t, st, `SELECT COUNT(*) FROM eventlog_event WHERE type = 'inbound.disposed' AND object = ? AND actor LIKE '%"type":"human"%'`, id) != 1 ||
			count(t, st, `SELECT COUNT(*) FROM eventlog_event WHERE type = 'run.finished' AND object = ?`, id) != 0 {
			t.Errorf("本人忽略 %s 应由本人处置且没有 judge 运行", id)
		}
	}

	// 清单的行数与最大序号就是账本里数出来的。
	if got := tableCounts(t, st); !reflect.DeepEqual(got, m.Tables) {
		t.Errorf("tables 与账本不一致:\n清单 %v\n账本 %v", m.Tables, got)
	}
	if hs, seqs := eventHashes(t, st); !reflect.DeepEqual(hs, m.EventPayloadSHA) || int64(len(seqs)) != m.EventMaxSeq || seqs[len(seqs)-1] != m.EventMaxSeq {
		t.Errorf("event_payload_sha256 或 event_max_seq 与账本不一致")
	}
}

// 夹具里不含任何真实数据：地址一律是保留域名/丢弃端口，秘密是合成串。
func TestFixtureHoldsNoRealData(t *testing.T) {
	dir, m := gen(t, 2)
	email := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	for _, f := range []string{"ledger.db", "manifest.json", "config.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range email.FindAll(b, -1) {
			if dom := string(e[bytes.IndexByte(e, '@')+1:]); !strings.HasPrefix(dom, "example.invalid") && !strings.HasPrefix(dom, "example.com") {
				t.Errorf("%s 里出现疑似真实邮箱 %s", f, e)
			}
		}
	}
	for _, s := range m.Sources {
		if strings.Contains(s, "@") {
			t.Errorf("来源 id 含邮箱: %s", s)
		}
	}
}

// ---- 生成器：同内容、可重复、逐结构版本派生 ----

func TestGenerateIsByteForByteRepeatable(t *testing.T) {
	a, _ := gen(t, 2)
	b, _ := gen(t, 2)
	if ha, hb := treeHash(t, a), treeHash(t, b); ha != hb {
		t.Fatalf("两次生成的数据目录不一致:\n%s\n--\n%s", ha, hb)
	}
}

func TestEveryStructureVersionDerivesTheSameContent(t *testing.T) {
	latest := mustLatest(t)
	_, base := gen(t, 2)
	for v := 3; v <= latest; v++ {
		_, m := gen(t, v)
		if m.SchemaLedger != v {
			t.Errorf("V%d 清单 schema_ledger = %d", v, m.SchemaLedger)
		}
		if !reflect.DeepEqual(m.EventPayloadSHA, base.EventPayloadSHA) || !reflect.DeepEqual(m.Backlog, base.Backlog) ||
			!reflect.DeepEqual(m.PolicyVersions, base.PolicyVersions) || !reflect.DeepEqual(m.OldCascadeIgnored, base.OldCascadeIgnored) ||
			!reflect.DeepEqual(m.RuleModes, base.RuleModes) || !reflect.DeepEqual(m.ScoreMarkers, base.ScoreMarkers) {
			t.Errorf("V%d 与 V2 内容不同", v)
		}
		for tbl, n := range base.Tables {
			if tbl == "store_migrations" { // 每个版本多出各自的迁移记录
				continue
			}
			if m.Tables[tbl] != n {
				t.Errorf("V%d 表 %s 行数 %d != V2 的 %d", v, tbl, m.Tables[tbl], n)
			}
		}
		if len(m.Tables) <= len(base.Tables) {
			t.Errorf("V%d 应比 V2 多出新表", v)
		}
	}
}

func TestGenerateRejectsUnknownVersionAndDirtyDir(t *testing.T) {
	latest := mustLatest(t)
	for _, v := range []int{0, 1, latest + 1} {
		if _, err := Generate(filepath.Join(t.TempDir(), "h"), v); err == nil {
			t.Errorf("版本 %d 不应能生成", v)
		}
	}
	dirty := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirty, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dirty, 2); err == nil {
		t.Errorf("非空目录不应被覆盖")
	}
}

// 冻结的迁移不可改：版本 ≤ n 的迁移的校验和摘要与冻结时一致（字面值取自冻结提交的二进制写出的账本）。
func TestFrozenMigrationsAreUnchanged(t *testing.T) {
	want := map[int]string{2: "31ac079af2c9a4b100c52c40cffcfd5bd90a3fd23a5c94bf20ad62c9102e2a24"}
	for v := range Frozen {
		reg, err := RegistryAt(v)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, m := range reg.Modules() {
			for _, mg := range m.Migrations {
				lines = append(lines, fmt.Sprintf("%s/%d/%s", m.Name, mg.Version, mg.Checksum()))
			}
		}
		sort.Strings(lines)
		h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		if got := hex.EncodeToString(h[:]); got != want[v] {
			t.Errorf("V%d 的迁移校验和摘要变了: %s（冻结时 %s）——已被真实账本应用过的迁移不得修改，结构变更另起新迁移", v, got, want[v])
		}
	}
}

// ---- flow-upgrade-rollback / ACT-UPG-18：迁移链 ----

type pre struct {
	tables  map[string]int
	applied []string
	policy  []string
}

func snapshot(t *testing.T, st *store.Store) pre {
	return pre{tableCounts(t, st),
		strCol(t, st, "SELECT module||'/'||version||'/'||checksum FROM store_migrations ORDER BY 1"),
		strCol(t, st, "SELECT version||':'||doc FROM policy_version ORDER BY version")}
}

// 从版本 2 与每个结构版本的夹具升到主干最新，各自满足 §5.2 里账本层面的要求。
func TestMigrationChainFromEveryFixtureToLatest(t *testing.T) {
	latest := mustLatest(t)
	for v := 2; v <= latest; v++ {
		t.Run(fmt.Sprintf("V%d", v), func(t *testing.T) {
			dir, m := gen(t, v)
			home := copyTree(t, dir)

			before := open(t, home, v)
			p := snapshot(t, before)
			before.Close()

			st := open(t, home, latest)
			defer st.Close()
			if st.SchemaLedger() != latest {
				t.Fatalf("升级后 schema.ledger = %d, 想要 %d", st.SchemaLedger(), latest)
			}
			after := snapshot(t, st)

			// 不丢数据：每张表行数不减（升级本身不写内容，所以应相等）。
			for tbl, n := range p.tables {
				if after.tables[tbl] < n {
					t.Errorf("表 %s 行数减少: %d → %d", tbl, n, after.tables[tbl])
				}
				if after.tables[tbl] != m.Tables[tbl] && tbl != "store_migrations" {
					t.Errorf("表 %s 行数 %d != 清单 %d", tbl, after.tables[tbl], m.Tables[tbl])
				}
			}
			if v < latest && len(after.tables) <= len(p.tables) {
				t.Errorf("V%d → V%d 应新增表", v, latest)
			}
			// 不改写历史：每条 Event 的载荷哈希不变，序号从 1 起连续，末尾没有被追加。
			hs, seqs := eventHashes(t, st)
			if !reflect.DeepEqual(hs, m.EventPayloadSHA) {
				t.Errorf("Event 载荷哈希有变化")
			}
			for i, s := range seqs {
				if s != int64(i+1) {
					t.Fatalf("序号不连续: 第 %d 条是 %d", i+1, s)
				}
			}
			if seqs[len(seqs)-1] != m.EventMaxSeq {
				t.Errorf("升级追加了 Event: 最大序号 %d, 清单 %d", seqs[len(seqs)-1], m.EventMaxSeq)
			}
			// 已存策略版本逐字节不变；已应用迁移的校验和不变。
			if !reflect.DeepEqual(after.policy, p.policy) {
				t.Errorf("策略版本被改动")
			}
			for _, a := range p.applied {
				found := false
				for _, b := range after.applied {
					found = found || a == b
				}
				if !found {
					t.Errorf("已应用的迁移记录变了或丢了: %s", a)
				}
			}
			// 升级本身不触发重判、不放回：积压的处置分布与旧级联忽略组原样。
			dist := strCol(t, st, "SELECT disposition||'/'||unjudged_reason||'/'||COUNT(*) FROM sources_runner_event GROUP BY disposition, unjudged_reason ORDER BY 1")
			if v2 := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'unjudged' AND unjudged_reason IN ('', 'model_unavailable')"); v2 != m.Backlog.Health["model_unavailable"] {
				t.Errorf("可自动重判的积压 %d != 清单 %d（分布 %v）", v2, m.Backlog.Health["model_unavailable"], dist)
			}
			if got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'ignored'"); got != m.OldCascadeIgnored.Count+len(m.OutsideWindow)+len(m.RuleIgnored)+len(m.UserIgnored) {
				t.Errorf("升级改动了被忽略的事件: %d", got)
			}
			if got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'pending'"); got != 0 {
				t.Errorf("升级后出现了 %d 条 pending（重判被触发）", got)
			}
			// 规则模式、登录口令与会话原样（登录的失效是 apicore 的运行时行为，不在迁移里）。
			for k, mode := range m.RuleModes.Current {
				if got := strCol(t, st, "SELECT mode FROM rules_state WHERE key = ?", k); len(got) != 1 || got[0] != mode {
					t.Errorf("规则 %s 模式变了: %v", k, got)
				}
			}
		})
	}
}

// 失败可退：迁移中途失败，整体回滚，数据目录逐字节与升级前一致，旧结构版本仍能打开，之后正常升级成功。
func TestFailedMigrationRollsBackByteForByte(t *testing.T) {
	latest := mustLatest(t)
	for v := 2; v <= latest; v++ {
		t.Run(fmt.Sprintf("V%d", v), func(t *testing.T) {
			dir, m := gen(t, v)
			home := copyTree(t, dir)
			want := treeHash(t, home)

			failUpgrade(t, home, latest)
			if got := treeHash(t, home); got != want {
				t.Fatalf("回滚后数据目录与升级前不一致:\n前 %s\n后 %s", want, got)
			}
			old := open(t, home, v)
			if got := tableCounts(t, old); !reflect.DeepEqual(got, m.Tables) {
				t.Errorf("回滚后旧结构版本看到的行数不对")
			}
			old.Close()
			good := open(t, home, latest)
			good.Close()
		})
	}
}

// 版本闸：旧结构版本的程序打开已升级的账本时拒绝，不带着看不懂的数据运行，且不改一个字节。
func TestOldBuildRefusesUpgradedLedger(t *testing.T) {
	latest := mustLatest(t)
	dir, _ := gen(t, 2)
	home := copyTree(t, dir)
	up := open(t, home, latest)
	up.Close()
	want := treeHash(t, home)
	for v := 2; v < latest; v++ {
		reg, err := RegistryAt(v)
		if err != nil {
			t.Fatal(err)
		}
		if st, err := store.Open(home, reg); !errors.Is(err, store.ErrSchemaTooNew) {
			if st != nil {
				st.Close()
			}
			t.Errorf("V%d 的程序应被版本闸拒绝, 得到 %v", v, err)
		}
		if got := treeHash(t, home); got != want {
			t.Fatalf("被拒绝的打开改动了账本")
		}
	}
}

func mustTime(s string) int64 {
	t, err := timeParse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func timeParse(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339, s)
	return t.UnixNano(), err
}

// failUpgrade 对 home 做一次注定在中途失败的升级，并断言它失败了。
// 失败步骤是排在最后的一个模块：第一句成功、第二句失败；它之前所有待办迁移都已在同一事务里应用，回滚必须一并撤销。
func failUpgrade(t *testing.T, home string, latest int) {
	t.Helper()
	reg, err := RegistryAt(latest)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(store.Module{Name: "zz_fault", Migrations: []store.Migration{{
		Version: 1, Ledger: latest, Name: "fails-midway",
		SQL: "CREATE TABLE zz_fault_ok (x INTEGER);\nINSERT INTO zz_fault_missing VALUES (1);",
	}}}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(home, reg)
	if err == nil {
		st.Close()
		t.Fatal("带失败步骤的迁移不该成功")
	}
	if !strings.Contains(err.Error(), "zz_fault") {
		t.Errorf("错误应指出失败的迁移: %v", err)
	}
}
