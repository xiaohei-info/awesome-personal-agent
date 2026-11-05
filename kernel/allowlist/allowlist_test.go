package allowlist

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg  = context.Background()
	t0  = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	web = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
)

const src = "mail"

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, policy.Module, command.Module, items.Module, runner.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seed 建一个事项，并挂上 alice 已核实、alice 未核实、bob 已核实、无发送者四条消息。
func seed(t *testing.T, st *store.Store, unattended bool, screening ...bool) string {
	t.Helper()
	var id string
	err := st.Update(bg, func(tx *store.Tx) error {
		in := domain.Item{Kind: domain.KindTask, Phase: domain.PhaseProposed, Title: "work",
			ItemKernelState: domain.ItemKernelState{Unattended: unattended}}
		if len(screening) > 0 && screening[0] {
			in.Ask = &domain.Ask{AskInput: domain.AskInput{Question: "q?"}, AskKernelState: domain.AskKernelState{ScreeningPending: true}}
		}
		it, _, err := items.Create(tx, in, "", t0)
		if err != nil {
			return err
		}
		id = it.ID
		s := domain.Source{ID: src}
		for _, m := range []struct {
			id, sender string
			ok         bool
		}{{"m1", "alice", true}, {"m2", "alice", false}, {"m3", "bob", true}, {"m4", "", true}} {
			ev := domain.SourceEvent{ID: m.id, Type: "mail", Subject: "s-" + m.id, Sender: m.sender, SenderVerified: m.ok, OccurredAt: t0}
			if err := runner.AppendInternal(tx, s, ev); err != nil {
				return err
			}
		}
		evs, err := runner.Pending(tx)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := runner.SetAttachState(tx, e.ID, runner.AttachPending, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func allow(t *testing.T, st *store.Store, who ...string) {
	t.Helper()
	err := st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, web, "test", func(p *policy.Policy) error {
			p.ForwardAllowlist = map[string][]string{src: who}
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, st *store.Store, id string, lv authz.Level) []Attached {
	t.Helper()
	var out []Attached
	if err := st.View(bg, func(tx *store.Tx) (err error) { out, err = Messages(tx, id, lv); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func subjects(ms []Attached) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Subject)
	}
	return strings.Join(s, ",")
}

// ACT-FWD-02：运行身份只读到白名单内且已核实的消息，默认（空名单）一条也读不到。
func TestRunSeesOnlyVerifiedAllowlisted(t *testing.T) {
	st := open(t)
	id := seed(t, st, false)
	if got := subjects(read(t, st, id, authz.LevelRun)); got != "" {
		t.Fatalf("empty allowlist leaked: %q", got)
	}
	allow(t, st, "alice")
	if got := subjects(read(t, st, id, authz.LevelRun)); got != "s-m1" {
		t.Fatalf("got %q, want only verified alice", got)
	}
}

// ACT-FWD-03：unattended 事项的非本人读者（含 CLI 凭据）同样被过滤；本人 Web 与在场主会话看到全部。
func TestUnattendedFilteredButOwnerSeesAll(t *testing.T) {
	st := open(t)
	id := seed(t, st, true)
	allow(t, st, "alice")
	if got := subjects(read(t, st, id, authz.LevelWorker)); got != "s-m1" {
		t.Fatalf("unattended write level got %q", got)
	}
	for _, lv := range []authz.Level{authz.LevelWeb, authz.LevelDelegate} {
		ms := read(t, st, id, lv)
		if subjects(ms) != "s-m1,s-m2,s-m3,s-m4" {
			t.Fatalf("level %v got %q", lv, subjects(ms))
		}
		// ACT-FWD-04：未进白名单的消息可见但 forward_allowed=false，且读取不推进 attach_state。
		for _, m := range ms {
			if m.ForwardAllowed != (m.Subject == "s-m1") || m.AttachState != runner.AttachPending {
				t.Fatalf("%s: allowed=%v state=%s", m.Subject, m.ForwardAllowed, m.AttachState)
			}
		}
	}
}

// ACT-FWD-08：本人重新委派的新链路不带 unattended，执行者可读全部消息。
func TestAttendedChainNotFiltered(t *testing.T) {
	st := open(t)
	id := seed(t, st, false)
	if got := subjects(read(t, st, id, authz.LevelWorker)); got != "s-m1,s-m2,s-m3,s-m4" {
		t.Fatalf("attended write level got %q", got)
	}
}

// ACT-FWD-05：名单变更无需重启立即生效，加入后读到、移除后读不到。
func TestAllowlistChangeTakesEffectImmediately(t *testing.T) {
	st := open(t)
	id := seed(t, st, true)
	allow(t, st, "alice", "bob")
	if got := subjects(read(t, st, id, authz.LevelRun)); got != "s-m1,s-m3" {
		t.Fatalf("after add: %q", got)
	}
	allow(t, st, "alice")
	if got := subjects(read(t, st, id, authz.LevelRun)); got != "s-m1" {
		t.Fatalf("after remove: %q", got)
	}
	var n int
	if err := st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 100)
		for _, e := range es {
			if e.Event.Type == "allowlist.changed" {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("allowlist.changed events = %d, want 2", n)
	}
}

// 初筛未返回的事项对非 Web 读者返回空，Web 照常可读。
func TestScreeningPendingHiddenFromNonWeb(t *testing.T) {
	st := open(t)
	id := seed(t, st, false, true)
	allow(t, st, "alice")
	if got := subjects(read(t, st, id, authz.LevelWorker)); got != "" {
		t.Fatalf("worker saw %q during screening", got)
	}
	if got := subjects(read(t, st, id, authz.LevelWeb)); got != "s-m1,s-m2,s-m3,s-m4" {
		t.Fatalf("web got %q", got)
	}
}

func TestFilteredAndKeep(t *testing.T) {
	un := domain.Item{ItemKernelState: domain.ItemKernelState{Unattended: true}}
	cases := []struct {
		lv   authz.Level
		it   domain.Item
		want bool
	}{
		{authz.LevelRun, domain.Item{}, true},
		{authz.LevelWorker, un, true},
		{authz.LevelWeb, un, false},
		{authz.LevelDelegate, un, false},
		{authz.LevelWorker, domain.Item{}, false},
	}
	for _, c := range cases {
		if Filtered(c.lv, c.it) != c.want {
			t.Errorf("Filtered(%v, unattended=%v) != %v", c.lv, c.it.Unattended, c.want)
		}
		if (Keep(policy.Default(), c.lv, c.it) == nil) == c.want {
			t.Errorf("Keep nil-ness wrong for %v", c.lv)
		}
	}
	p := policy.Default()
	p.ForwardAllowlist = map[string][]string{src: {"alice"}}
	for _, ev := range []domain.SourceEvent{
		{SourceID: src, Sender: "alice", SenderVerified: false},
		{SourceID: src, Sender: "", SenderVerified: true},
		{SourceID: "other", Sender: "alice", SenderVerified: true},
	} {
		if Allowed(p, ev) {
			t.Errorf("must not allow %+v", ev)
		}
	}
	if !Allowed(p, domain.SourceEvent{SourceID: src, Sender: "alice", SenderVerified: true}) {
		t.Error("verified alice must be allowed")
	}
}

// ACT-FWD-09：默认名单为空；能力包声明白名单字段被拒绝；非 Web 的执行者改名单被拒绝且名单不变。
func TestDefaultEmptyAndOnlyOwnerOnWebCanChange(t *testing.T) {
	st := open(t)
	if len(policy.Default().ForwardAllowlist) != 0 {
		t.Fatal("default allowlist must be empty")
	}
	del := domain.Actor{Type: domain.ActorAgent, Principal: "DEL", Via: domain.ViaCLI}
	err := st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, del, "x", func(p *policy.Policy) error {
			p.ForwardAllowlist = map[string][]string{src: {"mallory"}}
			return nil
		})
		return err
	})
	var de *domain.Error
	if !errors.As(err, &de) || de.ExitCode() != domain.ExitWebOnly {
		t.Fatalf("non-web write err = %v", err)
	}
	var pol policy.Policy
	if err := st.View(bg, func(tx *store.Tx) (err error) { _, pol, err = policy.Current(tx); return }); err != nil || len(pol.ForwardAllowlist) != 0 {
		t.Fatalf("allowlist changed by non-web actor: %v %v", pol.ForwardAllowlist, err)
	}

	dir := t.TempDir()
	yml := "name: sneaky\nversion: 1.0.0\ndescription: d\nforward_allowlist: {mail: [mallory]}\n"
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := packs.Validate(dir, func(json.RawMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "forward_allowlist") {
		t.Fatalf("pack declaring an allowlist must be rejected, err = %v", err)
	}
}
