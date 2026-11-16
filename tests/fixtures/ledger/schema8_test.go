package ledgerfixture

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 账本结构版本 7 夹具（ACT-WBENV-03）：清单带全部键，邮件事件正文恰好 16384 字节且与清单哈希一致。
func TestV7FixtureCarriesTheUpgradeRehearsalData(t *testing.T) {
	if Frozen[7] == "" {
		t.Fatal("结构版本 7 必须登记为冻结版本")
	}
	dir, m := gen(t, 7)
	st := open(t, dir, 7)
	defer st.Close()

	if len(m.Mail16K) < 3 {
		t.Fatalf("正文恰好 16 KiB 的邮件事件至少 3 条, 清单里 %d 条", len(m.Mail16K))
	}
	for _, e := range m.Mail16K {
		if n := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE id = ? AND length(CAST(detail AS BLOB)) = 16384", e.ID); n != 1 {
			t.Errorf("事件 %s 的正文不是恰好 16384 字节", e.ID)
		}
		if typ := strCol(t, st, "SELECT source_id FROM sources_runner_event WHERE id = ?", e.ID); len(typ) != 1 || !strings.HasPrefix(typ[0], "src_mail") {
			t.Errorf("事件 %s 不是邮件来源的事件: %v", e.ID, typ)
		}
		if got := strCol(t, st, "SELECT detail FROM sources_runner_event WHERE id = ?", e.ID); len(got) != 1 || sha(got[0]) != e.BodySHA256 {
			t.Errorf("事件 %s 的正文哈希与清单不同", e.ID)
		}
	}
	if m.HostedMaterial == "" || m.OwnedMaterial == "" {
		t.Fatalf("清单缺托管型或自有型材料 id: %q %q", m.HostedMaterial, m.OwnedMaterial)
	}
	if got := strCol(t, st, "SELECT location FROM materials WHERE id = ?", m.HostedMaterial); len(got) != 1 || got[0] == "" {
		t.Errorf("托管型材料应有仓库位置: %v", got)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM hosted_material WHERE material = ?", m.HostedMaterial); n != 1 {
		t.Errorf("托管型材料缺 hosted_material 行")
	}
	if got := strCol(t, st, "SELECT location FROM materials WHERE id = ?", m.OwnedMaterial); len(got) != 1 || got[0] != "" {
		t.Errorf("自有型材料不应有仓库位置: %v", got)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM revisions WHERE material = ?", m.OwnedMaterial); n < 1 {
		t.Errorf("自有型材料应有修订")
	}
	if len(m.GrantEvents) == 0 {
		t.Fatal("清单缺带授权单的 Event")
	}
	for _, g := range m.GrantEvents {
		if got := strCol(t, st, "SELECT grant_id FROM eventlog_event WHERE seq = ? AND object = ?", g.Seq, g.Object); len(got) != 1 || got[0] != g.Grant || g.Grant == "" {
			t.Errorf("Event %d 应带授权单 %q: %v", g.Seq, g.Grant, got)
		}
		if n := count(t, st, "SELECT COUNT(*) FROM policy_grant WHERE id = ?", g.Grant); n != 1 {
			t.Errorf("授权单 %s 未登记", g.Grant)
		}
	}
	if m.ExternalUnselectedSource == "" {
		t.Fatal("清单缺外发范围（外部）未选择的来源")
	}
	if got := strCol(t, st, "SELECT outbound_external FROM sources_registry_source WHERE id = ?", m.ExternalUnselectedSource); len(got) != 1 || got[0] != "" {
		t.Errorf("来源 %s 的外部外发范围应为未选择: %v", m.ExternalUnselectedSource, got)
	}
}

// 7 → 8（WB-01..WB-08）：逐表逐列核对，旧行不改写、新列按“没有”读，旧二进制被版本闸拒绝。
func TestUpgradeV7ToV8AddsEverySchemaPieceWithoutRewritingRows(t *testing.T) {
	latest := mustLatest(t)
	if latest != 8 {
		t.Fatalf("schema.ledger = %d, 想要 8", latest)
	}
	dir, m := gen(t, 7)
	home := copyTree(t, dir)
	st := open(t, home, 8)
	defer st.Close()

	has := func(tbl, col string) bool {
		return count(t, st, "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", tbl, col) == 1
	}
	for _, c := range [][2]string{
		{"sources_runner_event", "content_bytes"}, {"sources_runner_event", "content_state"}, // WB-02
		{"eventlog_event", "import_batch"}, {"eventlog_event", "recorded_at"}, // WB-03
		{"items", "imported"},                                 // WB-04
		{"materials", "form"}, {"materials", "link_location"}, // WB-06
		{"sources_registry_source", "content_retention_days"}, // WB-07
	} {
		if !has(c[0], c[1]) {
			t.Errorf("缺列 %s.%s", c[0], c[1])
		}
	}
	for tbl, cols := range map[string][]string{
		"sources_runner_event_part": {"event_id", "part", "media_type", "bytes", "sha256", "content"}, // WB-01
		"item_import_batch":         {"id", "source", "actor", "grant_id", "count", "at"},             // WB-05
		"item_import_map":           {"source", "record_type", "external_id", "object_id"},            // WB-05
	} {
		for _, c := range cols {
			if !has(tbl, c) {
				t.Errorf("缺表或列 %s.%s", tbl, c)
			}
		}
		if n := count(t, st, "SELECT COUNT(*) FROM "+tbl); n != 0 {
			t.Errorf("新表 %s 升级后应为空: %d", tbl, n)
		}
	}

	// 旧行不改写、新列按“没有”读。
	n := m.Tables["sources_runner_event"]
	if got := count(t, st, "SELECT COUNT(*) FROM sources_runner_event WHERE content_state = 'legacy' AND content_bytes IS NULL"); got != n {
		t.Errorf("已有来源事件应全部是 legacy 且字节数为空: %d / %d", got, n)
	}
	if got := count(t, st, "SELECT COUNT(*) FROM eventlog_event WHERE import_batch = '' AND recorded_at = 0"); got != m.Tables["eventlog_event"] {
		t.Errorf("已有 Event 的导入列应为空: %d / %d", got, m.Tables["eventlog_event"])
	}
	if got := count(t, st, "SELECT COUNT(*) FROM items WHERE imported = ''"); got != m.Tables["items"] {
		t.Errorf("已有事项的导入信息应为空: %d / %d", got, m.Tables["items"])
	}
	if got := count(t, st, "SELECT COUNT(*) FROM materials WHERE form = '' AND link_location = ''"); got != m.Tables["materials"] || got == 0 {
		t.Errorf("已有材料的形态与位置应为空（未改写）: %d / %d", got, m.Tables["materials"])
	}
	if got := count(t, st, "SELECT COUNT(*) FROM sources_registry_source WHERE content_retention_days IS NULL"); got != m.Tables["sources_registry_source"] {
		t.Errorf("已有来源的保留天数应为空: %d / %d", got, m.Tables["sources_registry_source"])
	}
	hs, _ := eventHashes(t, st)
	if !reflect.DeepEqual(hs, m.EventPayloadSHA) {
		t.Errorf("已有 Event 载荷变了")
	}
	for _, e := range m.Mail16K {
		if got := strCol(t, st, "SELECT detail FROM sources_runner_event WHERE id = ?", e.ID); len(got) != 1 || sha(got[0]) != e.BodySHA256 {
			t.Errorf("升级后 16 KiB 邮件事件 %s 正文变了", e.ID)
		}
	}
	st.Close()

	// 旧（结构版本 7）二进制拒绝打开升级后的账本。
	reg7, err := RegistryAt(7)
	if err != nil {
		t.Fatal(err)
	}
	if old, err := store.Open(home, reg7); err == nil {
		old.Close()
		t.Fatal("结构版本 7 的程序不应能打开结构版本 8 的账本")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("应报账本比程序新: %v", err)
	}
}

// 表达式索引、唯一性与约束：索引确被按 origin 的查询用到且不唯一；导入对照与链接型位置唯一；枚举列拒绝非法值。
func TestV8IndexesAndConstraints(t *testing.T) {
	dir, _ := gen(t, 7)
	st := open(t, copyTree(t, dir), 8)
	defer st.Close()
	exec := func(q string, a ...any) error {
		return st.Update(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(q, a...); return err })
	}

	var plan []string
	if err := st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`EXPLAIN QUERY PLAN SELECT id FROM items WHERE json_extract(data, '$.origin.source') = 'x' AND json_extract(data, '$.origin.ref') = 'y'`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id, parent, unused int64
			var detail string
			if err := r.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "items_origin") {
		t.Errorf("按 origin 的查询没有用到 items_origin 索引: %v", plan)
	}
	for i := 0; i < 2; i++ { // 索引不唯一：同一 origin 可以有多个事项
		if err := exec(`INSERT INTO items (id, phase, kind, project, assignee, data) VALUES (?, 'open', 'task', 'p', '', '{"origin":{"source":"s","ref":"r"}}')`, "itm_dup_"+string(rune('a'+i))); err != nil {
			t.Fatalf("origin 索引不应唯一: %v", err)
		}
	}

	ins := func(id, project, form, loc string) error {
		return exec(`INSERT INTO materials (id, project, kind, title, tags, form, link_location) VALUES (?, ?, 'reference', 't', '[]', ?, ?)`, id, project, form, loc)
	}
	if err := ins("mat_l1", "prj_a", "link", "vault:v/a.md"); err != nil {
		t.Fatal(err)
	}
	if err := ins("mat_l2", "prj_a", "link", "vault:v/a.md"); err == nil {
		t.Error("同一项目同一位置的链接型材料应被唯一索引拒绝")
	}
	if err := ins("mat_l3", "prj_b", "link", "vault:v/a.md"); err != nil {
		t.Errorf("不同项目可以登记同一位置: %v", err)
	}
	if err := ins("mat_o1", "prj_a", "owned", ""); err != nil {
		t.Fatal(err)
	}
	if err := ins("mat_o2", "prj_a", "owned", ""); err != nil {
		t.Errorf("唯一性只约束链接型: %v", err)
	}
	if err := ins("mat_bad", "prj_a", "bogus", ""); err == nil {
		t.Error("形态只允许 hosted、owned、link 或空")
	}

	if err := exec(`INSERT INTO item_import_map (source, record_type, external_id, object_id) VALUES ('s', 'item', 'e1', 'itm_1')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO item_import_map (source, record_type, external_id, object_id) VALUES ('s', 'item', 'e1', 'itm_2')`); err == nil {
		t.Error("（来源名，记录类型，外部 id）应唯一")
	}
	if err := exec(`INSERT INTO item_import_map (source, record_type, external_id, object_id) VALUES ('s', 'progress', 'e1', 'itm_2')`); err != nil {
		t.Errorf("记录类型不同可以同一外部 id: %v", err)
	}

	part := func(id, p string) error {
		return exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES (?, ?, 'text/plain', 1, 'h', x'41')`, id, p)
	}
	if err := part("sev_x", "body"); err != nil {
		t.Fatal(err)
	}
	if err := part("sev_x", "body"); err == nil {
		t.Error("同一事件同一部分只能有一行")
	}
	if err := part("sev_x", "bogus"); err == nil {
		t.Error("部分只允许 body、original、headers、attachments、messages")
	}
	if err := exec(`UPDATE sources_runner_event SET content_state = 'bogus' WHERE id = (SELECT id FROM sources_runner_event LIMIT 1)`); err == nil {
		t.Error("完整程度只允许 full、partial、legacy、purged")
	}
	for _, s := range []string{"full", "partial", "purged", "legacy"} {
		if err := exec(`UPDATE sources_runner_event SET content_state = ? WHERE id = (SELECT id FROM sources_runner_event LIMIT 1)`, s); err != nil {
			t.Errorf("完整程度 %s 应可写: %v", s, err)
		}
	}
}
