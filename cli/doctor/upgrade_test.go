package doctor

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

// v2Home 建一个账本结构版本 2 的数据目录（升级夹具）：来源登记还没有外发范围列，用户包带已移除字段。
func v2Home(t *testing.T) (home string, newer *store.Registry) {
	t.Helper()
	oldSrc := store.Module{Name: registry.Module.Name, Migrations: registry.Module.Migrations[:1]}
	old := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, policy.Module, oldSrc} {
		must(t, old.Register(m))
	}
	home = filepath.Join(t.TempDir(), "apa")
	st, err := store.Open(home, old)
	must(t, err)
	must(t, st.Update(bg, func(tx *store.Tx) error {
		for _, id := range []string{"src_a", "src_b"} {
			if _, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
				dedupe_domain, config, cursor, enabled, stats) VALUES (?, 'script', '', 'x', '1m', 30, '', '', '', '', 1, '')`, id); err != nil {
				return err
			}
		}
		return nil
	}))
	must(t, st.Close())
	must(t, os.Chmod(home, 0o700))
	must(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 127.0.0.1:7777\n"), 0o644))
	src, dst := "inbound-triage", filepath.Join(home, "packs", "inbound-triage")
	must(t, fs.WalkDir(rootpacks.FS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dst, strings.TrimPrefix(p, src))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, _ := fs.ReadFile(rootpacks.FS, p)
		if strings.HasSuffix(p, "triage-screen.md") {
			b = bytes.Replace(b, []byte("on_undecided: yes\n"), []byte("confidence_threshold: 0.7\n"), 1)
		}
		return os.WriteFile(out, b, 0o644)
	}))
	return home, fullReg(t)
}

// ACT-UPG-02：预检只读；报告结构版本差异、会被拒的包（逐字段）、未选外发范围的来源与升级后事项。
func TestUpgradeCheckIsReadOnlyAndReportsTheDifferences(t *testing.T) {
	home, reg := v2Home(t)
	before := tree(t, home)
	rep := UpgradeCheck(bg, Input{Home: home, Reg: reg, Now: now})
	if after := tree(t, home); after != before {
		t.Fatalf("the data directory changed:\n%s\n---\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(home, "backups")); err == nil {
		t.Fatal("backups/ must not be created")
	}
	schema := find(t, rep, "schema_version")
	if schema.Data["current"] != 2 || schema.Data["target"] != reg.SchemaLedger() || !strings.Contains(schema.Why, "pre-upgrade snapshot") {
		t.Fatalf("schema: %+v", schema)
	}
	pack := find(t, rep, "pack_compat")
	d, ok := pack.Find("inbound-triage")
	if !ok || !strings.Contains(d.Why, "confidence_threshold") || !strings.Contains(d.Why, "on_undecided") {
		t.Fatalf("packs: %+v", pack)
	}
	out := find(t, rep, "source_outbound")
	if len(out.Details) != 2 || out.Status != kdoctor.Warning {
		t.Fatalf("outbound: %+v", out)
	}
	fu := find(t, rep, "after_upgrade")
	ids := map[string]bool{}
	for _, x := range fu.Details {
		ids[x.Subject] = true
	}
	for _, want := range []string{"web-login", "plugins", "outbound", "packs"} {
		if !ids[want] {
			t.Errorf("follow-up %s missing: %+v", want, fu)
		}
	}
	if !rep.OK {
		t.Fatalf("warnings must not fail the check: %+v", rep)
	}
	if find(t, rep, "public_url").Status != kdoctor.OK {
		t.Fatal("loopback listen needs no public_url")
	}
}

// ACT-UPG-03：监听非本机地址而没有 public_url：错误，目录不变。
func TestUpgradeCheckReportsMissingPublicURL(t *testing.T) {
	home, reg := v2Home(t)
	must(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 0.0.0.0:7777\n"), 0o644))
	before := tree(t, home)
	rep := UpgradeCheck(bg, Input{Home: home, Reg: reg, Now: now})
	c := find(t, rep, "public_url")
	if c.Status != kdoctor.Error || !strings.Contains(c.Why, "public_url") || rep.OK {
		t.Fatalf("%+v", c)
	}
	if tree(t, home) != before {
		t.Fatal("the data directory changed")
	}
}

// 预检的其余反例：账本已是目标版本、账本比二进制新、账本不存在。
func TestUpgradeCheckLedgerEdgeCases(t *testing.T) {
	e := newEnv(t)
	e.seal()
	if c := find(t, UpgradeCheck(bg, e.input()), "schema_version"); c.Status != kdoctor.OK || !strings.Contains(c.Why, "nothing to migrate") {
		t.Fatalf("current: %+v", c)
	}
	older := store.NewRegistry()
	must(t, older.Register(eventlog.Module))
	in := e.input()
	in.Reg = older
	if rep := UpgradeCheck(bg, in); rep.OK || !strings.Contains(find(t, rep, "schema_version").Why, "newer than this binary") {
		t.Fatalf("newer ledger: %+v", rep)
	}
	must(t, os.Remove(filepath.Join(e.home, "ledger.db")))
	if rep := UpgradeCheck(bg, e.input()); rep.OK || find(t, rep, "schema_version").Status != kdoctor.Error {
		t.Fatalf("missing ledger: %+v", rep)
	}
}
