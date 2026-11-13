package registry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func str(s string) *string { return &s }

// 验收 ACT-OBD-03/04/05：同一条事件按范围裁剪，被裁掉的内容在任何字段里都不出现。
func TestTrimByScope(t *testing.T) {
	detail := "HEAD-MARK" + strings.Repeat("甲", 590) + "MID-MARK" + strings.Repeat("乙", 1390) + "TAIL-MARK"
	ev := domain.SourceEvent{Type: "doc.updated", Subject: "OBD-1 外发测试", Sender: "alice@example.org", SenderVerified: true,
		Link: "https://docs.example.org/path/secret-123?tok=abc", Summary: "SUM-MARK", Detail: detail,
		OccurredAt: time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC)}

	all := func(tr Trimmed) string {
		return strings.Join([]string{tr.Type, tr.Subject, tr.Sender, tr.OccurredAt, tr.Link, tr.Summary, tr.Detail}, "\x00")
	}
	cases := []struct {
		scope   string
		has     []string
		missing []string
	}{
		{ScopeFull, []string{"SUM-MARK", "HEAD-MARK", "MID-MARK", "TAIL-MARK", "/path/secret-123"}, nil},
		{ScopeExcerpt, []string{"SUM-MARK", "HEAD-MARK", "docs.example.org", "alice@example.org"}, []string{"MID-MARK", "TAIL-MARK", "/path/secret-123", "tok=abc"}},
		{ScopeMeta, []string{"doc.updated", "OBD-1 外发测试", "alice@example.org", "2026-03-01T08:30:00Z", "docs.example.org"},
			[]string{"/path/secret-123", "SUM-MARK", "HEAD-MARK", "MID-MARK", "TAIL-MARK"}},
	}
	for _, c := range cases {
		tr, ok := Trim(ev, c.scope)
		if !ok {
			t.Fatalf("%s: not sendable", c.scope)
		}
		got := all(tr)
		for _, w := range c.has {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing from %q", c.scope, w, got)
			}
		}
		for _, w := range c.missing {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q must not be sent", c.scope, w)
			}
		}
	}
	// excerpt 按字符截 500，不按字节、不截断多字节字符
	tr, _ := Trim(ev, ScopeExcerpt)
	if n := len([]rune(tr.Detail)); n != ExcerptChars || !strings.HasPrefix(tr.Detail, "HEAD-MARK") {
		t.Fatalf("excerpt detail has %d chars", n)
	}
	if short, _ := Trim(domain.SourceEvent{Detail: "短"}, ScopeExcerpt); short.Detail != "短" {
		t.Fatalf("short detail changed: %q", short.Detail)
	}
	// 链接解析不出主机时整个丢弃
	if tr, _ := Trim(domain.SourceEvent{Link: "not a url ::"}, ScopeMeta); tr.Link != "" {
		t.Fatalf("link = %q", tr.Link)
	}
	for _, s := range []string{ScopeNone, "", "bogus"} {
		if _, ok := Trim(ev, s); ok {
			t.Errorf("scope %q must not be sendable", s)
		}
	}
}

// 验收 ACT-OBD-06/07/08/12：按端点位置取范围；某端点为 none 或外部未选时该端点不可用；都不可用时按原因拦下。
func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		o      domain.Outbound
		locs   []string
		scopes []string
		block  string
	}{
		{"external full", domain.Outbound{Local: "full", External: str("full")}, []string{"external"}, []string{"full"}, ""},
		{"undeclared locality is external", domain.Outbound{Local: "full", External: str("meta")}, []string{""}, []string{"meta"}, ""},
		{"local takes the local scope", domain.Outbound{Local: "full", External: str("meta")}, []string{"local", "external"}, []string{"full", "meta"}, ""},
		{"local scope defaults to full", domain.Outbound{External: str("none")}, []string{"local"}, []string{"full"}, ""},
		{"external none, local usable", domain.Outbound{Local: "full", External: str("none")}, []string{"external", "local"}, []string{"", "full"}, ""},
		{"all none → digest", domain.Outbound{Local: "none", External: str("none")}, []string{"local", "external"}, []string{"", ""}, BlockNone},
		{"only external endpoints, unset", domain.Outbound{Local: "full"}, []string{"external", "external"}, []string{"", ""}, BlockUnset},
		{"unset beats none when nothing is usable", domain.Outbound{Local: "none"}, []string{"local", "external"}, []string{"", ""}, BlockUnset},
		{"unset external, usable local endpoint", domain.Outbound{Local: "full"}, []string{"external", "local"}, []string{"", "full"}, ""},
		{"no endpoint configured is not an outbound matter", domain.Outbound{}, nil, []string{}, ""},
	}
	for _, c := range cases {
		d := Decide(c.o, c.locs...)
		if d.Block != c.block || strings.Join(d.Scopes, ",") != strings.Join(c.scopes, ",") {
			t.Errorf("%s: got %+v, want scopes %v block %q", c.name, d, c.scopes, c.block)
		}
	}
}

// setupWithProjects 在登记模块之外补上项目表：默认项目要核对项目存在。
func setupWithProjects(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil), x: command.New(st, nil), dir: t.TempDir()}
	if err := Register(e.x, Options{Bundled: []string{"pull-feed"}, Layout: config.LayoutOf(e.dir)}); err != nil {
		t.Fatal(err)
	}
	return e
}

// 验收 ACT-OBD-02/11：登记时本机范围缺省 full、外部范围未选择；CLI 路径可带默认项目。
func TestRegisterDefaultsOutboundAndDefaultProject(t *testing.T) {
	e := setupWithProjects(t)
	del := e.bearer(t, auth.Delegate)
	if _, err := e.do(del, CmdRegister, m{"id": "src_rss", "type": "pull", "command": "pull-feed", "schedule": "10m", "timeout": 60, "default_project": "acc-home"}); err == nil {
		t.Fatal("default project that does not exist must be rejected")
	} else if de, ok := err.(*domain.Error); !ok || de.Code != domain.CodeNotFound {
		t.Fatalf("err = %v", err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (id, name, intro, tags, status, repos, allowed_executors, capture) VALUES ('acc-home', 'home', '', '[]', 'active', '[]', 'null', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(del, CmdRegister, m{"id": "src_rss", "type": "pull", "command": "pull-feed", "schedule": "10m", "timeout": 60, "default_project": "acc-home"}); err != nil {
		t.Fatal(err)
	}
	s := e.get(t, "src_rss")
	if s.DefaultProject != "acc-home" || s.Outbound.Local != ScopeFull || s.Outbound.External != nil {
		t.Fatalf("source = %+v", s)
	}
	// 登记请求里不能带 outbound（只经设置命令）
	if _, err := e.do(del, CmdRegister, m{"id": "src_x", "type": "pull", "command": "pull-feed", "schedule": "10m", "timeout": 60,
		"outbound": m{"external": "full"}}); err == nil {
		t.Fatal("outbound in register must be rejected")
	}
	// 更新默认项目；空串清除；更新不动外发范围
	if _, err := e.do(owner, CmdSetOutbound, m{"id": "src_rss", "external": "excerpt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(del, CmdUpdate, m{"id": "src_rss", "default_project": ""}); err != nil {
		t.Fatal(err)
	}
	s = e.get(t, "src_rss")
	if s.DefaultProject != "" || s.Outbound.External == nil || *s.Outbound.External != ScopeExcerpt {
		t.Fatalf("source = %+v", s)
	}
}

// 验收 ACT-OBD-10：外发范围只有 Web 能写；delegate 得 web_only（20），其余凭据被拒，范围不变也不记事件。
func TestSetOutboundWebOnly(t *testing.T) {
	e := setupWithProjects(t)
	if _, err := e.do(owner, CmdRegisterExec, m{"id": "src_obd", "command": "echo", "schedule": "1m", "timeout": 30}); err != nil {
		t.Fatal(err)
	}
	before := len(e.types(t))
	for _, k := range []auth.Kind{auth.Delegate, auth.Worker, auth.Observer} {
		_, err := e.do(e.bearer(t, k), CmdSetOutbound, m{"id": "src_obd", "local": "full", "external": "full"})
		de, ok := err.(*domain.Error)
		if !ok {
			t.Fatalf("%s: err = %v", k, err)
		}
		if k == auth.Delegate && de.Code != domain.CodeWebOnly {
			t.Errorf("delegate: %v", de)
		}
		if k != auth.Delegate && de.Code == domain.CodeWebOnly {
			t.Errorf("%s: %v", k, de)
		}
	}
	if s := e.get(t, "src_obd"); s.Outbound.External != nil || s.Outbound.Local != ScopeFull {
		t.Fatalf("outbound changed by a rejected call: %+v", s.Outbound)
	}
	for _, ty := range e.types(t)[before:] {
		if ty == EvUpdated {
			t.Fatalf("a rejected call recorded %s", ty)
		}
	}
}

// Web 设置：选定、改回未选择、取值非法被拒；变更记 source.updated，未变不记。
func TestSetOutboundByOwner(t *testing.T) {
	e := setupWithProjects(t)
	if _, err := e.do(owner, CmdRegisterExec, m{"id": "src_obd", "command": "echo", "schedule": "1m", "timeout": 30}); err != nil {
		t.Fatal(err)
	}
	set := func(in m) error { _, err := e.do(owner, CmdSetOutbound, in); return err }
	if err := set(m{"id": "src_obd", "local": "none", "external": "excerpt"}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_obd"); s.Outbound.Local != "none" || *s.Outbound.External != "excerpt" {
		t.Fatalf("outbound = %+v", s.Outbound)
	}
	n := len(e.types(t))
	if err := set(m{"id": "src_obd", "local": "none", "external": "excerpt"}); err != nil || len(e.types(t)) != n {
		t.Fatalf("unchanged setting must be a no-op: err=%v events %d→%d", err, n, len(e.types(t)))
	}
	if err := set(m{"id": "src_obd", "external": nil}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_obd"); s.Outbound.External != nil || s.Outbound.Local != "none" {
		t.Fatalf("null must restore 'not chosen' and keep local: %+v", s.Outbound)
	}
	for _, bad := range []m{{"id": "src_obd", "external": "all"}, {"id": "src_obd", "local": ""}, {"id": "src_obd", "extra": 1}} {
		if err := set(bad); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
	if err := set(m{"id": "src_none", "external": "full"}); err == nil {
		t.Error("unknown source must be rejected")
	}
	if got := e.types(t); got[len(got)-1] != EvUpdated {
		t.Fatalf("events = %v", got)
	}
}

// 验收 ACT-OBD-18：v0.1 结构的行（只有旧列）升级后读出来：默认项目为空、本机范围 full、外部范围未选择。
func TestRowsFromBeforeV3ReadAsNotChosen(t *testing.T) {
	e := setupWithProjects(t)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, config, cursor, enabled, stats)
			VALUES ('src_old', 'script', 'mail', 'x', '1m', 30, '', '', '', '', 1, '')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s := e.get(t, "src_old")
	if s.DefaultProject != "" || s.Outbound.Local != ScopeFull || s.Outbound.External != nil {
		t.Fatalf("old row = %+v", s)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"default_project":""`) || !strings.Contains(string(b), `"outbound":{"local":"full","external":null}`) {
		t.Fatalf("json = %s", b)
	}
}
