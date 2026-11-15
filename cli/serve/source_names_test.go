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

// 启动补名（§9.1、§16.5）：升级前登记、没有显示名的来源在 apa serve 启动时先按仓库自带来源程序的接入声明起名，
// 同一程序的多个实例按 config 得到不同的名字；没有声明的才按类型兜底，并记一条健康说明。每个记一条 source.renamed；
// 不产生新的判断、投递或重判，也不碰来源消息、事项与运行。形状与本人实例的 7 个来源相同，另加一个没有声明的自定义脚本。
func TestServeNamesLegacySourcesWithoutTriggeringAnything(t *testing.T) {
	home := initOnFreePort(t)
	type src = map[string]any
	script := func(id, kind, cmd string, cfg src) src {
		return src{"id": id, "type": "script", "source_type": kind, "command": cmd, "config": cfg, "schedule": "1h", "timeout": 10, "enabled": false}
	}
	srcs := []any{
		script("src_cal_apple", "calendar", "apple-pim-source", src{"kind": "calendar"}),
		script("src_calendar", "calendar", "calendar", src{"account": "me@example.com"}),
		src{"id": "src_github", "type": "pull", "source_type": "github", "command": "github-source", "config": src{"repos": []string{"example/app"}}, "schedule": "1h", "timeout": 10, "enabled": false},
		script("src_mail", "mail", "mail-himalaya", src{"account": "gmail"}),
		script("src_mail_icloud", "mail", "/usr/local/bin/apple-pim-source", src{"kind": "mail", "account": "iCloud"}),
		script("src_mail_qq", "mail", "apple-pim-source", src{"kind": "mail", "account": "QQ"}),
		script("src_reminders", "reminder", "apple-pim-source", src{"kind": "reminders"}),
		script("src_custom", "inspect", "/bin/sh", src{}), // 第三方程序：没有声明，登记时必须给名字
	}
	srcs[len(srcs)-1].(src)["name"] = "巡检"
	file, _ := json.Marshal(map[string]any{"sources": srcs})
	if err := os.WriteFile(filepath.Join(home, "sources.json"), file, 0o644); err != nil {
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
	// 升级前的 sources.json 没有 name 字段（第一次登记时必须给，这里只为让登记成功）。
	delete(srcs[len(srcs)-1].(src), "name")
	file, _ = json.Marshal(map[string]any{"sources": srcs})
	if err := os.WriteFile(filepath.Join(home, "sources.json"), file, 0o644); err != nil {
		t.Fatal(err)
	}
	s := open()
	// 模拟升级前的来源：名字为空；再放一条等模型的来源消息，它是最可能被"顺手"重判的东西。
	seedExec(t, s, `UPDATE sources_registry_source SET name = ''`)
	seedExec(t, s, `INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
		correlation_keys, sender, sender_verified, disposition, unjudged_reason) VALUES ('mail', 'e1', 'se1', 'src_mail', 'r', 'mail.received', 's', '', '', '', ?,
		'[]', '', 0, 'unjudged', 'model_unavailable')`, time.Now().UnixNano()) // occurred_at 是纳秒
	watched := []string{"outcall_judgment", "outcall_run", "delivery_delivery WHERE class != 'health'", "items", "sources_runner_event"}
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
	if names != "src_cal_apple=Apple 日历,src_calendar=Google 日历,src_custom=自定义脚本,src_github=GitHub,src_mail=gmail 邮件,"+
		"src_mail_icloud=iCloud 邮件,src_mail_qq=QQ 邮件,src_reminders=提醒事项" {
		t.Fatalf("names: %v", names)
	}
	if after := state(s); after != before {
		t.Fatalf("backfill touched judgments, deliveries or rejudges:\nbefore %s\nafter  %s", before, after)
	}
	// 唯一的投递是那条健康说明按「系统健康」去向（缺省只进网页未读）出的一条，不经渠道推送。
	if got := fmt.Sprint(scalar(t, s, "SELECT group_concat(class || ':' || route) FROM delivery_delivery")); got != "health:web" {
		t.Fatalf("deliveries: %s", got)
	}
	types := fmt.Sprint(scalar(t, s, fmt.Sprintf("SELECT group_concat(type, ',') FROM eventlog_event WHERE seq > %v", seq)))
	if n := strings.Count(types, "source.renamed"); n != 8 || strings.Count(types, "health.alert") != 1 {
		t.Fatalf("one source.renamed per legacy source and one health note, got %s", types)
	}
	if note := fmt.Sprint(scalar(t, s, "SELECT object || ' ' || payload FROM eventlog_event WHERE type = 'health.alert'")); note != `sources:names {"kind":"source_names","sources":["src_custom"]}` {
		t.Fatalf("the health note lists only the type-named source: %s", note)
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
