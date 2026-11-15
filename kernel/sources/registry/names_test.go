package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func conflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeConflict
}

// 显示名规则只在 plan() 里：每个登记入口都覆盖带名字、不带名字（兜底名，同类加序号）、重名三种情况（§9.1）。
func TestEveryRegistrationEntryAppliesTheNameRule(t *testing.T) {
	webhook := func(id string) m { return m{"id": id, "type": "webhook", "source_type": "feed"} }
	scriptFeed := func(id string) m { s := script(id); s["source_type"] = "feed"; return s }
	entries := map[string]func(e *env, in m) error{
		"source.register pull（Web、CLI、delegate）": func(e *env, in m) error { _, err := e.do(owner, CmdRegister, in); return err },
		"source.register webhook":                func(e *env, in m) error { _, err := e.do(owner, CmdRegister, in); return err },
		"source.register_script（只 Web）":          func(e *env, in m) error { _, err := e.do(owner, CmdRegisterExec, in); return err },
		"主机配置文件 sources.json": func(e *env, in m) error {
			path := filepath.Join(t.TempDir(), "sources.json")
			if err := os.WriteFile(path, js(m{"sources": []m{in}}), 0o600); err != nil {
				t.Fatal(err)
			}
			return LoadFile(bg, e.x, owner, path)
		},
	}
	shape := map[string]func(string) m{"source.register pull（Web、CLI、delegate）": pull, "source.register webhook": webhook,
		"source.register_script（只 Web）": scriptFeed, "主机配置文件 sources.json": scriptFeed}
	for name, register := range entries {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			src := shape[name]
			named := src("src_named")
			named["name"] = "  我的订阅  "
			if err := register(e, named); err != nil {
				t.Fatal(err)
			}
			if got := e.get(t, "src_named").Name; got != "我的订阅" {
				t.Fatalf("named: %q", got)
			}
			for i, want := range []string{"订阅", "订阅 2", "订阅 3"} {
				id := fmt.Sprintf("src_auto%d", i)
				if err := register(e, src(id)); err != nil {
					t.Fatal(err)
				}
				if got := e.get(t, id).Name; got != want {
					t.Fatalf("fallback %d: %q, want %q", i, got, want)
				}
			}
			dup := src("src_dup")
			dup["name"] = "订阅 2" // 与内核起的名字重名同样被拒
			if err := register(e, dup); !conflict(err) {
				t.Fatalf("duplicate of a fallback name: %v", err)
			}
			dup["name"] = "我的订阅"
			if err := register(e, dup); !conflict(err) {
				t.Fatalf("duplicate name: %v", err)
			}
			if err := e.st.View(bg, func(tx *store.Tx) error { _, err := Get(tx, "src_dup"); return err }); !isNotFound(err) {
				t.Fatalf("a rejected registration must not be stored: %v", err)
			}
		})
	}
}

func TestNameValidationAndTypeFallback(t *testing.T) {
	e := setup(t)
	long := pull("src_long")
	long["name"] = strings.Repeat("长", MaxName+1)
	if _, err := e.do(owner, CmdRegister, long); exit(err) != domain.ExitUsage {
		t.Fatalf("too long: %v", err)
	}
	ctl := pull("src_ctl")
	ctl["name"] = "a\nb"
	if _, err := e.do(owner, CmdRegister, ctl); exit(err) != domain.ExitUsage {
		t.Fatalf("control character: %v", err)
	}
	ok := pull("src_max")
	ok["name"] = strings.Repeat("长", MaxName)
	if _, err := e.do(owner, CmdRegister, ok); err != nil {
		t.Fatal(err)
	}
	// 没登记显示名的 source_type 按来源类型起名；不区分大小写判重。
	if _, err := e.do(owner, CmdRegisterExec, script("src_inspect")); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_inspect").Name; got != "自定义脚本" {
		t.Fatalf("type fallback: %q", got)
	}
	up := pull("src_upper")
	up["name"] = "订阅源"
	if _, err := e.do(owner, CmdRegister, up); err != nil {
		t.Fatal(err)
	}
	up2 := pull("src_upper2")
	up2["name"] = strings.ToUpper("订阅源")
	if _, err := e.do(owner, CmdRegister, up2); !conflict(err) {
		t.Fatal("same name must conflict")
	}
	lat := pull("src_lat")
	lat["name"] = "Inbox"
	lat2 := pull("src_lat2")
	lat2["name"] = "inbox"
	if _, err := e.do(owner, CmdRegister, lat); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdRegister, lat2); !conflict(err) {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
}

func TestRenameIsRecordedIdempotentAndResettable(t *testing.T) {
	e := setup(t)
	for _, id := range []string{"src_a", "src_b"} {
		if _, err := e.do(owner, CmdRegister, pull(id)); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.types(t))
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": " 工作订阅 "}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_a"); s.Name != "工作订阅" {
		t.Fatalf("%+v", s)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": "工作订阅"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": "订阅 2"}); !conflict(err) {
		t.Fatalf("rename onto another source's name: %v", err)
	}
	// 空名改回内核起的名字：src_b 占着「订阅 2」，「订阅」空出来了。
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": ""}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_a"); s.Name != "订阅" {
		t.Fatalf("reset: %+v", s)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_none", "name": "x"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown source: %v", err)
	}
	var renamed []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == EvRenamed {
				renamed = append(renamed, x)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(e.types(t)) - before; got != 2 || len(renamed) != 2 {
		t.Fatalf("two renames recorded, the no-op and the rejected one not: %d events, %d renamed", got, len(renamed))
	}
	var p map[string]string
	_ = json.Unmarshal(renamed[0].Event.Payload, &p)
	if renamed[0].Event.Object != "src_a" || p["name"] != "工作订阅" || p["previous"] != "订阅" || renamed[0].Event.Actor.Principal != "owner" {
		t.Fatalf("payload: %+v %v", renamed[0].Event, p)
	}
}

// 升级前登记的来源没有名字：Backfill 按同一套规则起名并记 source.renamed，其他什么都不写；再跑一次什么也不做。
func TestBackfillNamesLegacySourcesOnly(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_has")); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, r := range [][2]string{{"src_m1", "mail"}, {"src_m2", "mail"}, {"src_x", "other"}} {
			if _, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
				dedupe_domain, config, cursor, enabled, stats) VALUES (?, 'pull', ?, 'pull-feed', '1h', 30, '', '', '', 'c9', 1, '{"runs":3}')`, r[0], r[1]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := e.types(t)
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	n, err := Backfill(bg, e.st, map[string]string{"feed": "订阅", "mail": "邮件"}, at)
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	for id, want := range map[string]string{"src_has": "订阅", "src_m1": "邮件", "src_m2": "邮件 2", "src_x": "定时拉取"} {
		if s := e.get(t, id); s.Name != want || (id != "src_has" && (s.Cursor != "c9" || string(s.Stats) != `{"runs":3}`)) {
			t.Errorf("%s: %+v", id, s)
		}
	}
	added := e.types(t)[len(before):]
	if len(added) != 3 || strings.Join(added, ",") != "source.renamed,source.renamed,source.renamed" {
		t.Fatalf("only renames recorded: %v", added)
	}
	if n, err := Backfill(bg, e.st, nil, at); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
}
