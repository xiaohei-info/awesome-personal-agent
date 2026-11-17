package connections

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

var web = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

const devKey = "PLAIN-device-key-0123456789" // 只在测试里出现的明文，下面逐处检查它不外泄

var pushDecl = func() conn.Decl {
	t := conn.Text{Zh: "推送", En: "Push"}
	return conn.Decl{Type: "push", Direction: conn.DirectionChannel, Title: t, Tagline: t, Default: t, Via: t, Fields: []conn.Field{
		{Key: "server", Kind: conn.KindURL, Label: t, Default: "https://push.example", Required: true},
		{Key: "device_key", Kind: conn.KindSecret, Label: t, Required: true, Target: true},
		{Key: "password", Kind: conn.KindSecret, Label: t},
	}}
}()

type env struct {
	st  *store.Store
	x   *command.Executor
	au  *auth.Auth
	v   *secrets.Vault
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, secrets.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil), v: secrets.Generate(), now: time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)}
	e.x = command.New(st, func() time.Time { return e.now })
	s := New(e.v, func(typ string) (conn.Decl, bool) { return pushDecl, typ == "push" })
	if err := s.Register(e.x); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) bearer(t *testing.T, kind auth.Kind) command.Bearer {
	t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { _, secret, err = e.au.Issue(tx, auth.Spec{Kind: kind}); return }); err != nil {
		t.Fatal(err)
	}
	return command.Bearer{Auth: e.au, Secret: secret, Origin: auth.Origin{Session: "s1", SessionKind: "direct"}}
}

func (e *env) do(id command.Identity, cmd string, in any) (json.RawMessage, error) {
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b})
	return r.Value, err
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) json.RawMessage {
	t.Helper()
	v, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return v
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func (e *env) draft(t *testing.T, v json.RawMessage) Draft {
	t.Helper()
	var d Draft
	if err := json.Unmarshal(v, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) routes(t *testing.T) map[string]string {
	t.Helper()
	var p policy.Policy
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { _, p, err = policy.Current(tx); return }); err != nil {
		t.Fatal(err)
	}
	return p.Disturb.Routes
}

func (e *env) reveal(t *testing.T, owner string) map[string]string {
	t.Helper()
	var m map[string]string
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { m, err = e.v.Reveal(tx, owner, pushDecl.SecretKeys()); return }); err != nil {
		t.Fatal(err)
	}
	return m
}

// noPlaintext：凭据明文只以密文存在 secrets_value；连接表、草稿表、Event 里都没有。
func (e *env) noPlaintext(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"SELECT COUNT(*) FROM connections_draft WHERE instr(config || secrets || changes || name, ?) > 0",
		"SELECT COUNT(*) FROM connections_connection WHERE instr(config || name, ?) > 0",
		"SELECT COUNT(*) FROM eventlog_event WHERE instr(CAST(payload AS TEXT), ?) > 0",
		"SELECT COUNT(*) FROM secrets_value WHERE instr(ciphertext, ?) > 0",
	} {
		if n := e.count(t, q, devKey); n != 0 {
			t.Fatalf("plaintext found by %q", q)
		}
	}
}

// 新建草稿：确认前不建连接、不改路由；确认要在 Web，delegate 得 web_only；确认后启用、凭据换属主、路由生效；重复确认为确定的冲突。
func TestProposeThenConfirmNewChannel(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Worker)
	before := e.routes(t)
	v := e.must(t, worker, CmdPropose, map[string]any{"type": "push", "name": "我的手机", "secrets": map[string]string{"device_key": devKey}, "routes": []string{"decision", "health"}})
	d := e.draft(t, v)
	if strings.Contains(string(v), devKey) || d.Secrets["device_key"].Hint != "PLA…789" || d.Secrets["device_key"].Length != len(devKey) || d.Config["server"] != "https://push.example" {
		t.Fatalf("draft view: %s", v)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM connections_connection"); n != 0 {
		t.Fatalf("a draft must not create a connection before confirmation: %d", n)
	}
	if got := e.routes(t); got["decision"] != before["decision"] || got["health"] != before["health"] {
		t.Fatalf("a draft must not change routes before confirmation: %v", got)
	}
	if _, err := e.do(e.bearer(t, auth.Observer), CmdPropose, map[string]any{"type": "push", "name": "x", "secrets": map[string]string{"device_key": devKey}}); code(err) != domain.CodePermissionDenied {
		t.Fatalf("observer propose: %v", err)
	}
	if _, err := e.do(e.bearer(t, auth.Delegate), CmdConfirm, map[string]string{"id": d.ID}); code(err) != domain.CodeWebOnly {
		t.Fatalf("delegate confirm must be web_only: %v", err)
	}
	if _, err := e.do(worker, CmdConfirm, map[string]string{"id": d.ID}); code(err) != domain.CodePermissionDenied {
		t.Fatalf("worker confirm: %v", err)
	}
	var c Connection
	if err := json.Unmarshal(e.must(t, web, CmdConfirm, map[string]string{"id": d.ID}), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Name != "我的手机" || !c.Secrets["device_key"] {
		t.Fatalf("confirmed connection: %+v", c)
	}
	if got := e.routes(t); got["decision"] != "channel:"+c.ID || got["health"] != "channel:"+c.ID || got["brief"] == "channel:"+c.ID {
		t.Fatalf("routes after confirm: %v", got)
	}
	if got := e.reveal(t, c.ID); got["device_key"] != devKey {
		t.Fatal("credential was not moved to the connection")
	}
	if got := e.reveal(t, d.ID); len(got) != 0 {
		t.Fatal("the draft still owns credentials after confirmation")
	}
	if _, err := e.do(web, CmdConfirm, map[string]string{"id": d.ID}); code(err) != domain.CodeConflict || !strings.Contains(err.Error(), "draft_confirmed") {
		t.Fatalf("second confirm: %v", err)
	}
	e.noPlaintext(t)
}

// 同一目标再提交：旧草稿被取代、凭据删除，确认旧版本是确定的冲突，不会启用新目标；只有提交者与 Web 能看见、测试、撤回。
func TestNewProposalSupersedesAndOthersCannotTouch(t *testing.T) {
	e := setup(t)
	a, b := e.bearer(t, auth.Worker), e.bearer(t, auth.Delegate)
	old := e.draft(t, e.must(t, a, CmdPropose, map[string]any{"type": "push", "name": "手机", "secrets": map[string]string{"device_key": "first-device-key-0001"}}))
	nw := e.draft(t, e.must(t, a, CmdPropose, map[string]any{"type": "push", "name": "手机", "secrets": map[string]string{"device_key": devKey}}))
	if old.ID == nw.ID {
		t.Fatal("a new proposal must be a new draft id")
	}
	if _, err := e.do(web, CmdConfirm, map[string]string{"id": old.ID}); code(err) != domain.CodeConflict || !strings.Contains(err.Error(), "draft_superseded") {
		t.Fatalf("confirm superseded: %v", err)
	}
	if got := e.reveal(t, old.ID); len(got) != 0 {
		t.Fatal("superseded draft kept its credentials")
	}
	for _, cmd := range []string{CmdDraftTest, CmdDiscard} {
		if _, err := e.do(b, cmd, map[string]string{"id": nw.ID}); code(err) != domain.CodeNotFound {
			t.Fatalf("%s by another agent: %v", cmd, err)
		}
	}
	var list struct{ Drafts []Draft }
	json.Unmarshal(e.must(t, b, CmdDrafts, map[string]string{}), &list)
	if len(list.Drafts) != 0 {
		t.Fatalf("another agent sees drafts: %+v", list.Drafts)
	}
	json.Unmarshal(e.must(t, web, CmdDrafts, map[string]string{}), &list)
	if len(list.Drafts) != 2 || list.Drafts[0].ID != nw.ID || list.Drafts[0].State != DraftPending || list.Drafts[1].State != DraftSuperseded {
		t.Fatalf("web list: %+v", list.Drafts)
	}
	var tgt DraftTarget
	json.Unmarshal(e.must(t, a, CmdDraftTest, map[string]string{"id": nw.ID}), &tgt)
	if tgt.ID != nw.ID || tgt.Settings["server"] != "https://push.example" {
		t.Fatalf("test target: %+v", tgt)
	}
	e.must(t, a, CmdDiscard, map[string]string{"id": nw.ID})
	if got := e.reveal(t, nw.ID); len(got) != 0 {
		t.Fatal("discarded draft kept its credentials")
	}
	if _, err := e.do(web, CmdConfirm, map[string]string{"id": nw.ID}); code(err) != domain.CodeConflict || !strings.Contains(err.Error(), "draft_discarded") {
		t.Fatalf("confirm discarded: %v", err)
	}
}

// 过期：24 小时后确认为 draft_expired；清理只删草稿自己的凭据，已启用连接的凭据不动。
func TestExpiredDraftIsRefusedAndCleanedWithoutTouchingLiveSecrets(t *testing.T) {
	e := setup(t)
	live := e.must(t, web, CmdCreate, map[string]any{"type": "push", "name": "现用", "secrets": map[string]string{"device_key": "live-device-key-000001"}, "enabled": true})
	var lc Connection
	json.Unmarshal(live, &lc)
	d := e.draft(t, e.must(t, e.bearer(t, auth.Worker), CmdPropose, map[string]any{"connection": lc.ID, "config": map[string]string{"server": "https://other.example"}}))
	if d.Secrets["device_key"].Action != SecretKeep || len(e.reveal(t, d.ID)) != 1 {
		t.Fatalf("a kept credential must be copied to the draft: %+v", d.Secrets)
	}
	e.now = e.now.Add(DraftTTL)
	if _, err := e.do(web, CmdConfirm, map[string]string{"id": d.ID}); code(err) != domain.CodeConflict || !strings.Contains(err.Error(), "draft_expired") {
		t.Fatalf("confirm after expiry: %v", err)
	}
	if err := ExpireDrafts(bg, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if len(e.reveal(t, d.ID)) != 0 || e.reveal(t, lc.ID)["device_key"] != "live-device-key-000001" {
		t.Fatal("expiry must delete only the draft's credentials")
	}
	if n := e.count(t, "SELECT COUNT(*) FROM connections_draft WHERE state = 'expired'"); n != 1 {
		t.Fatalf("expired drafts = %d", n)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM eventlog_event WHERE type = ?", EvDraftExpired); n != 1 {
		t.Fatalf("expiry events = %d", n)
	}
}

// 改已有连接：确认前现用值照常生效；连接在草稿之后被本人改过时确认为 connection_changed；确认后沿用的凭据保留、启停不变。
func TestConfigureDraftKeepsLiveValuesUntilConfirmed(t *testing.T) {
	e := setup(t)
	var lc Connection
	json.Unmarshal(e.must(t, web, CmdCreate, map[string]any{"type": "push", "name": "现用", "secrets": map[string]string{"device_key": "live-device-key-000001", "password": "pw-1"}, "enabled": true}), &lc)
	w := e.bearer(t, auth.Worker)
	d := e.draft(t, e.must(t, w, CmdPropose, map[string]any{"connection": lc.ID, "config": map[string]string{"server": "https://other.example"}, "secrets": map[string]string{"password": ""}, "routes": []string{"brief"}}))
	if strings.Join(d.Changes, ",") != "password,routes,server" {
		t.Fatalf("changes = %v", d.Changes)
	}
	if _, err := e.do(w, CmdPropose, map[string]any{"connection": lc.ID, "name": "改名"}); code(err) != domain.CodeInvalidRequest {
		t.Fatalf("a draft cannot rename: %v", err)
	}
	var got Connection
	json.Unmarshal(e.must(t, web, "connection.rename", map[string]string{"id": lc.ID, "name": "现用2"}), &got)
	if _, err := e.do(web, CmdConfirm, map[string]string{"id": d.ID}); code(err) != domain.CodeConflict || !strings.Contains(err.Error(), "connection_changed") {
		t.Fatalf("confirm after the owner changed it: %v", err)
	}
	if e.reveal(t, lc.ID)["password"] != "pw-1" {
		t.Fatal("a refused confirmation must not change the live connection")
	}
	e.must(t, web, "connection.disable", map[string]string{"id": lc.ID})
	d = e.draft(t, e.must(t, w, CmdPropose, map[string]any{"connection": lc.ID, "config": map[string]string{"server": "https://other.example"}, "secrets": map[string]string{"password": ""}, "routes": []string{"brief"}}))
	json.Unmarshal(e.must(t, web, CmdConfirm, map[string]string{"id": d.ID}), &got)
	if got.Enabled || got.Config["server"] != "https://other.example" || got.Name != "现用2" {
		t.Fatalf("after confirm: %+v", got)
	}
	if s := e.reveal(t, lc.ID); s["device_key"] != "live-device-key-000001" || s["password"] != "" {
		t.Fatalf("secrets after confirm: kept=%v cleared=%v", s["device_key"] != "", s["password"] == "")
	}
	if r := e.routes(t); r["brief"] != "channel:"+lc.ID {
		t.Fatalf("routes: %v", r)
	}
}
