package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// v01Migrations 是 v0.1.1 发布提交上每个模块每一步迁移的校验和（另加 sources_runner/2，见下）（名字加 SQL）。
// 这些迁移已被真实账本应用过，不得修改、合并或删除（设计 §16.5，ACT-UPG-19）。
var v01Migrations = map[string]string{
	"agentsmd/1":         "678e12aba4210c7551f52b75eab24be0ed8dbd93fd5caa09b8e4beb8b3dcc7e9",
	"agenttask/1":        "8e962ebfde13b57e1e8bb652a73615086976a8f393c1faaa858542e6d3760a59",
	"agenttask/2":        "9a3d3b63d4de5ac8b8b8e585e7a67e237d314fbdd2e02c2886bfd6664b6dc78b",
	"apicore/1":          "544b538e9c9e18f45b8160c28bc6b7f048cfe8db90d87d17b223f4b284de6554",
	"attention/1":        "c59917dcb4e72f3dfb339b9a4e40f114b828dea7a2393e8f4f67e35fa5ea948f",
	"attention/2":        "fb7941a1967c325bc7bc623110dbf418cf7765cecd73ea127ebefa739016104f",
	"auth/1":             "90b5def6c3b5e8bb0dfe3e0ab65d24470c87494fd1ff7387ca6872a844a94f36",
	"background/1":       "c53ed774ea399e71dcc58714b4e1209fbd4a11a37183f907b80d398a5e7dd2e1",
	"chain/1":            "20f57e01053197fb23910d0c738bc46e8fd1c8a9a30423b109ce76366c4ee7a5",
	"channel/1":          "2e25f02e5097d02e8918be063b15ccb1be57959b42e2521d90fe765c91ac5f9c",
	"channel/2":          "13503367c5ddf0dc2f46098909ad5717e433648d0af081ccb622da923d33932b",
	"clock/1":            "a9001d3eb6ada9ce54d6b08f6f15280cc878e45085325c1da8f9d38fe8ca8e66",
	"command/1":          "b213c8db4c37ca8c4f038e779022ac520c321d70e5b08a2e4532ef4a637dcc88",
	"context/1":          "9a4bb5bad0bf62b775d78bc84f520057012a72c938e48bf28f75787c5e0bcadc",
	"delegation/1":       "0fd23d4849a621891c63b7a106ac2444517ecca2bdc1313679bc37a925609863",
	"delivery/1":         "19cd7a263b13fc69ee97fa03c4cf1e6aae35cb915eaeb9f94eef6f6629ff3987",
	"delivery/2":         "85e11d43bd3c080df6b83656b2833be57c6d7d6067dce1ac9f106bf769476cfc",
	"direct/1":           "d482812bb3aea39bad6406f83d1557ce1dc79c8f775100a4f019fc253e1c0c56",
	"eventlog/1":         "bf97f30d17a1d25ade98c5bae61e9d29728fd36a02358c63218e6c9b186515d4",
	"eventlog/2":         "34b82bdbaf0e21e47e5d988fa0ad9291412349f7bfe6b2e3a9695ec6772c3d74",
	"executors/1":        "e9bb83897f6f243937182062ed0640a9943475aaa4a329efc22916c967a3343a",
	"followup/1":         "f7f11b031e520c363bf77a8503bd451c2ea372b0aa26a5a04ee1ad86f55896aa",
	"guideline/1":        "7372e5e3fb3d0d5d3e30fa3ce97d707f197ce39a592920b9fd57a7e56a0e67d9",
	"health/1":           "4763259a5bf236ba7e3bf35e78b290c7e63744a1a85e3ba0a5d62f2438d685c2",
	"hosted/1":           "8cb93c6658ab0172fbf272c43460cb9a11cc97c76b69f93f60934e116e3f2d95",
	"instances/1":        "86d29e64b632c8d4b2937142f3bc11bc6de877d512b06ec010c51684d8f82556",
	"items/1":            "bbe7f6c755d8f2d52e2f985abd88813525c6f724bd80374383cb7371e6c46f75",
	"items/2":            "5f012482571cd88e23c26e6addbba532caf1fb993370c6b1a4c0a1bc3af01e2c",
	"materials/1":        "12eafa59117845696756cb47c0a3e60c1c8ac9bc32157f7ff874363ce9e5e63a",
	"materials/2":        "9d524f587dd92373345acf61437895f15fe41f3a971b081e881677f65858843d",
	"outcall_proc/1":     "e4d3ef203216e324d06464d5e1c937c289acdcb17bb3fadef776809d57d1540e",
	"policy/1":           "25d219357c34b8025b4ff438a4781cd2695a39b5cc296dcb29300dbf98adba0e",
	"policy/2":           "f68b3d89832039e810b591de53710ec8361b58c44042281e36ff39dd08aea958",
	"projects/1":         "e4a824257c19432210cafd1caf3e5acea953d91a18a9c02734257efe47d41c63",
	"resume/1":           "61763762afcc8f648c40a2478be170c66d547323d6d9d72d76d3c6acc529045d",
	"rules/1":            "2d0722beb3bc64e9ac50371da0f1ddabb6613621c23c013c0310222087a34f81",
	"runidentity/1":      "127d306970061c62b5d6acdfde44f057f564bb08b761400c7b02fe749ae7bf30",
	"runidentity/2":      "1e31df4483473d9b6c40fa72439bd9d6ba0b00aa80518402421240e588861f79",
	"search/1":           "295a2d5de669722c3fafc61ff714ae2bd607a2ed4420f068dde2add300fdb668",
	"search/2":           "6e8cd79beaa4b0fd063e4b40bf64d2fb8038c35bc477547d0837ff09a9a932ef",
	"sources_registry/1": "45a58cad109fc571c5f5149863b557e69dd365415f7a95926a86335238703fcd",
	// sources_runner/2 在 v0.1.1 之后、结构版本 3 之前进入主干（bfafc2d03，#254），跟主干的已有部署已应用它，同样冻结。
	"sources_runner/2": "a967381100b10c987cadd8bbe6ffc544080a76073cceff541c0fbef953bfe7be",
	"sources_runner/1": "b8d3a2e865b73de1774fadf67f1532295353e059b213542f83679b9f14e98d88",
	"summaries/1":      "44254e1759c9ec0389c35ccb273b79bb381de3e969db58f00f5a8e6493b7e04a",
	"wake/1":           "ebee0dc948fff25b9a7fb4372e5b62a8da959881eca4f209aa76134b9aab994e",
	"webhook/1":        "9e416941c961cb567ecae41d288a8879f2e55cecff1773dc9c8eada83a7a40c4",
}

// ACT-UPG-19：账本结构版本 1、2 的迁移与 v0.1 发布提交逐字节一致；新增的迁移全部属于结构版本 3 及以后。
func TestFrozenMigrationsMatchV01Release(t *testing.T) {
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range reg.Modules() {
		for _, mg := range m.Migrations {
			key := fmt.Sprintf("%s/%d", m.Name, mg.Version)
			want, frozen := v01Migrations[key]
			switch {
			case mg.LedgerVersion() <= 2 && !frozen:
				t.Errorf("%s is ledger structure version %d but was not in v0.1", key, mg.LedgerVersion())
			case mg.LedgerVersion() > 2 && frozen:
				t.Errorf("%s was in v0.1 and must stay at its ledger version", key)
			case frozen && mg.Checksum() != want:
				t.Errorf("%s: frozen v0.1 migration changed", key)
			}
			seen[key] = true
		}
	}
	for key := range v01Migrations {
		if !seen[key] {
			t.Errorf("%s: frozen v0.1 migration removed", key)
		}
	}
	if reg.SchemaLedger() != 3 {
		t.Errorf("schema.ledger = %d, want 3", reg.SchemaLedger())
	}
}

// v01Registry 只登记结构版本 ≤ 2 的迁移，等价于 v0.1 程序认识的账本。
func v01Registry(t *testing.T) *store.Registry {
	t.Helper()
	full, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	old := store.NewRegistry()
	for _, m := range full.Modules() {
		var keep []store.Migration
		for _, mg := range m.Migrations {
			if mg.LedgerVersion() <= 2 {
				keep = append(keep, mg)
			}
		}
		if err := old.Register(store.Module{Name: m.Name, Migrations: keep}); err != nil {
			t.Fatal(err)
		}
	}
	return old
}

type snapshot struct {
	rows    map[string]int64
	payload map[int64]string // 事件序号 → 载荷哈希
}

func take(t *testing.T, s *store.Store) snapshot {
	t.Helper()
	snap := snapshot{rows: map[string]int64{}, payload: map[int64]string{}}
	err := s.View(context.Background(), func(tx *store.Tx) error {
		var tables []string
		r, err := tx.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'store_%'")
		if err != nil {
			return err
		}
		for r.Next() {
			var n string
			if err := r.Scan(&n); err != nil {
				return err
			}
			tables = append(tables, n)
		}
		if err := r.Close(); err != nil {
			return err
		}
		for _, n := range tables {
			r, err := tx.Query("SELECT count(*) FROM " + n)
			if err != nil {
				return err
			}
			r.Next()
			var c int64
			if err := r.Scan(&c); err != nil {
				return err
			}
			r.Close()
			snap.rows[n] = c
		}
		r, err = tx.Query("SELECT seq, payload FROM eventlog_event ORDER BY seq")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var seq int64
			var p string
			if err := r.Scan(&seq, &p); err != nil {
				return err
			}
			snap.payload[seq] = fmt.Sprintf("%x", sha256.Sum256([]byte(p)))
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func seedExec(t *testing.T, s *store.Store, sql string, args ...any) {
	t.Helper()
	if err := s.Update(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(sql, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func scalar(t *testing.T, s *store.Store, sql string) (v any) {
	t.Helper()
	err := s.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(sql)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&v)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// seedV2 往结构版本 2 的账本里写一批代表性旧数据：Event、事项、来源与来源事件（含各种关联键写法）、登录会话。
func seedV2(t *testing.T, s *store.Store) {
	t.Helper()
	for i := 1; i <= 5; i++ {
		seedExec(t, s, `INSERT INTO eventlog_event (type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, at)
			VALUES ('item.created', ?, 'owner', '', ?, 0, '', '', '', ?)`, fmt.Sprintf("i%d", i), fmt.Sprintf(`{"n":%d}`, i), 1000+i)
	}
	seedExec(t, s, `INSERT INTO items (id, phase, kind, project, assignee, data, created_at, updated_at) VALUES ('i1', 'open', 'task', '', '', '{"title":"keep"}', 1001, 1001)`)
	seedExec(t, s, `INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, config, cursor, enabled, stats)
		VALUES ('mail', 'pull', 'mail', 'x', '5m', 30, '', '', '{}', '', 1, '{}')`)
	for i, keys := range []string{`["t1","t2"]`, `[]`, ``, `null`} {
		seedExec(t, s, `INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at, correlation_keys, sender, sender_verified, disposition, unjudged_reason)
			VALUES ('mail', ?, ?, 'mail', 'r', 'mail', 's', '', '', '', 1, ?, '', 0, 'unjudged', 'undecided')`, fmt.Sprintf("e%d", i), fmt.Sprintf("se%d", i), keys)
	}
	seedExec(t, s, `INSERT INTO apicore_session (id_hash, csrf, created_at, expires_at) VALUES ('h1', 'c', 1, 9223372036854775807)`)
}

// ACT-UPG-05、ACT-UPG-06：结构版本 2 的账本升到 3，各表行数不减，旧 Event 载荷哈希不变、序号连续。
func TestUpgradeV2To3KeepsRowsAndHistory(t *testing.T) {
	home := t.TempDir()
	old, err := store.Open(home, v01Registry(t))
	if err != nil {
		t.Fatal(err)
	}
	if old.SchemaLedger() != 2 {
		t.Fatalf("v0.1 ledger = %d, want 2", old.SchemaLedger())
	}
	seedV2(t, old)
	before := take(t, old)
	old.Close()

	reg, _ := registry()
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.SchemaLedger() != 3 {
		t.Fatalf("ledger = %d, want 3", s.SchemaLedger())
	}
	after := take(t, s)

	var tables []string
	for n := range before.rows {
		tables = append(tables, n)
	}
	sort.Strings(tables)
	for _, n := range tables {
		if after.rows[n] < before.rows[n] {
			t.Errorf("table %s lost rows: %d -> %d", n, before.rows[n], after.rows[n])
		}
	}
	for _, n := range []string{"item_progress", "item_correlation", "outcall_judgment", "sources_runner_metric"} {
		if _, ok := after.rows[n]; !ok {
			t.Errorf("v3 table %s missing", n)
		}
	}
	for seq, h := range before.payload {
		if after.payload[seq] != h {
			t.Errorf("event %d payload changed", seq)
		}
	}
	for seq := int64(1); seq <= int64(len(after.payload)); seq++ {
		if _, ok := after.payload[seq]; !ok {
			t.Errorf("event sequence gap at %d", seq)
		}
	}

	// 新列对旧行取空值/未选择；升级前的会话整体失效但行保留；v0.1 登记的来源外发范围“未选择”。
	if got := scalar(t, s, "SELECT planned_for || proposal || merge_key FROM items WHERE id = 'i1'"); got != "" {
		t.Errorf("items new columns = %q", got)
	}
	if got := scalar(t, s, "SELECT outbound_external || '|' || outbound_local || '|' || default_project FROM sources_registry_source WHERE id = 'mail'"); got != "|full|" {
		t.Errorf("source scope = %q", got)
	}
	if got := scalar(t, s, "SELECT expires_at FROM apicore_session WHERE id_hash = 'h1'"); got != int64(0) {
		t.Errorf("pre-upgrade session still valid: expires_at = %v", got)
	}
	if got := scalar(t, s, "SELECT unjudged_reason FROM sources_runner_event WHERE id = 'se0'"); got != "undecided" {
		t.Errorf("unjudged_reason rewritten: %v", got)
	}
}

// ACT-UPG-14：只认识结构版本 2 的程序打开升级后的账本，拒绝启动。
func TestOldBuildRefusesUpgradedLedger(t *testing.T) {
	home := t.TempDir()
	reg, _ := registry()
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := store.Open(home, v01Registry(t)); !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("v0.1-shaped build opened a ledger structure version 3 ledger: %v", err)
	}
}
