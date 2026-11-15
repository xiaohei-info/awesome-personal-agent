package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 启动补名（§9.1、§16.5）：升级前登记、没有显示名的来源在 apa serve 启动时按兜底规则起名，每个记一条 source.renamed；
// 不产生新的判断、投递或重判，也不碰来源消息、事项与运行。
func TestServeNamesLegacySourcesWithoutTriggeringAnything(t *testing.T) {
	home := initOnFreePort(t)
	var srcs []any
	for _, s := range [][2]string{{"src_cal_apple", "calendar"}, {"src_mail", "mail"}, {"src_mail_qq", "mail"}, {"src_custom", "inspect"}} {
		srcs = append(srcs, map[string]any{"id": s[0], "type": "script", "source_type": s[1], "command": "/bin/sh", "schedule": "1h", "timeout": 10, "enabled": false})
	}
	src, _ := json.Marshal(map[string]any{"sources": srcs})
	if err := os.WriteFile(filepath.Join(home, "sources.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	_, stop := start(t, home) // 对照：第一次启动登记来源（此时就起了名）
	stop()

	reg, _ := registry()
	open := func() *store.Store {
		s, err := store.Open(home, reg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	// 模拟升级前的来源：名字为空；再放一条等模型的来源消息，它是最可能被"顺手"重判的东西。
	seedExec(t, s, `UPDATE sources_registry_source SET name = ''`)
	seedExec(t, s, `INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
		correlation_keys, sender, sender_verified, disposition, unjudged_reason) VALUES ('mail', 'e1', 'se1', 'src_mail', 'r', 'mail.received', 's', '', '', '', ?,
		'[]', '', 0, 'unjudged', 'model_unavailable')`, time.Now().UnixNano()) // occurred_at 是纳秒
	watched := []string{"outcall_judgment", "outcall_run", "delivery_delivery", "items", "sources_runner_event"}
	state := func(s *store.Store) string {
		var b strings.Builder
		for _, tb := range watched {
			fmt.Fprintf(&b, "%s=%v ", tb, scalar(t, s, "SELECT COUNT(*) FROM "+tb))
		}
		fmt.Fprintf(&b, "rejudges=%v disposition=%v", scalar(t, s, "SELECT COALESCE(SUM(rejudges), 0) FROM sources_runner_event"),
			scalar(t, s, "SELECT group_concat(disposition || ':' || unjudged_reason) FROM sources_runner_event"))
		return b.String()
	}
	before, seq := state(s), scalar(t, s, "SELECT MAX(seq) FROM eventlog_event")
	s.Close()

	_, stop = start(t, home)
	time.Sleep(2 * clockTicks) // 让时钟与维护循环各走一轮
	stop()

	s = open()
	defer s.Close()
	names := scalar(t, s, "SELECT group_concat(id || '=' || name, ',') FROM (SELECT id, name FROM sources_registry_source ORDER BY id)")
	if names != "src_cal_apple=日历,src_custom=自定义脚本,src_mail=邮件,src_mail_qq=邮件 2" {
		t.Fatalf("names: %v", names)
	}
	if after := state(s); after != before {
		t.Fatalf("backfill touched judgments, deliveries or rejudges:\nbefore %s\nafter  %s", before, after)
	}
	types := fmt.Sprint(scalar(t, s, fmt.Sprintf("SELECT group_concat(type, ',') FROM eventlog_event WHERE seq > %v", seq)))
	if n := strings.Count(types, "source.renamed"); n != 4 {
		t.Fatalf("one source.renamed per legacy source, got %d: %s", n, types)
	}
	for _, p := range []string{"inbound.", "decision.", "run.", "item.", "delivery", "followmsg."} {
		if strings.Contains(types, p) {
			t.Fatalf("backfill caused %s events: %s", p, types)
		}
	}
	actor := scalar(t, s, "SELECT DISTINCT actor FROM eventlog_event WHERE type = 'source.renamed'")
	if !strings.Contains(fmt.Sprint(actor), `"principal":"kernel:sources"`) || !strings.Contains(fmt.Sprint(actor), `"type":"system"`) {
		t.Fatalf("renamed by the system, not the owner: %v", actor)
	}
}

// 降级：账本里有本程序不认识的迁移（被更新的二进制迁移过）时，apa serve 拒绝启动并说明怎么回退，不报成"本地失败、检查权限"。
func TestServeRefusesLedgerMigratedByANewerBuild(t *testing.T) {
	home := initOnFreePort(t)
	reg, _ := registry()
	newer := store.Module{Name: "zz_newer", Migrations: []store.Migration{{Version: 1, Ledger: 3, Name: "from-the-future", SQL: `CREATE TABLE zz_newer_x (id TEXT)`}}}
	if err := reg.Register(newer); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	r := apa(t, home, "", "serve")
	if r.code != 2 || !strings.Contains(r.errOut, "migrated by a newer build") || !strings.Contains(r.errOut, "pre-upgrade snapshot") {
		t.Fatalf("want exit 2 explaining the downgrade: %+v", r)
	}
}
