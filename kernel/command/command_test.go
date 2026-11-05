package command

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

// 测试用的投影表：模拟某个业务模块的数据，用来证明失败时投影与事件一并回滚。
var noteModule = store.Module{Name: "tnote", Migrations: []store.Migration{{
	Version: 1, Name: "create", SQL: `CREATE TABLE tnote (id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
}}}

type env struct {
	st      *store.Store
	x       *Executor
	au      *auth.Auth
	commits []Commit
	applied int
}

type noteIn struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Assign string `json:"assignee"`
	Fail   bool   `json:"fail"`
	Forge  int    `json:"forge_depth"`
}

func parse(in json.RawMessage) noteIn {
	var n noteIn
	_ = json.Unmarshal(in, &n)
	return n
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, Module, noteModule} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil)}
	e.x = New(st, nil)
	e.x.OnCommit(func(c Commit) { e.commits = append(e.commits, c) })

	object := func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
		n := parse(in)
		return authz.Object{ID: n.ID, Assignee: n.Assign}, nil
	}
	must := func(s Spec) {
		if err := e.x.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	must(Spec{Name: "note.write", Action: authz.ActionUpdateOwn, Object: object,
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			if parse(in).Body == "" {
				return domain.NewError(domain.CodeInvalidRequest, "body required", "add body")
			}
			return nil
		},
		Apply: func(c *Ctx) (any, error) {
			e.applied++
			n := parse(c.Input)
			if _, err := c.Tx.Exec(`INSERT INTO tnote (id, body) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET body = excluded.body`, n.ID, n.Body); err != nil {
				return nil, err
			}
			// 调用方即使在 Emit 里塞了深度和操作者，内核也会覆盖。
			if _, err := c.Emit(domain.Event{Type: domain.EvItemUpdated, Object: n.ID, Payload: json.RawMessage(`{}`),
				ChainDepth: n.Forge, Actor: domain.Actor{Type: domain.ActorHuman, Principal: "forged", Via: domain.ViaWeb}}); err != nil {
				return nil, err
			}
			if n.Fail {
				return nil, errors.New("boom")
			}
			return map[string]string{"id": n.ID}, nil
		}})
	must(Spec{Name: "decision.answer", Action: authz.ActionAnswerDecision,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			return authz.Object{ID: parse(in).ID, Category: policy.CategoryFunds}, nil
		},
		Apply: func(c *Ctx) (any, error) { e.applied++; return "ok", nil }})
	must(Spec{Name: "item.assign", Action: authz.ActionReassign,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			return authz.Object{ID: parse(in).ID, Executor: "stranger", ProjectInfo: &domain.Project{AllowedExecutors: []string{"a"}}}, nil
		},
		Apply: func(c *Ctx) (any, error) { e.applied++; return nil, nil }})
	must(Spec{Name: "ext.ping", Action: authz.ActionSubmitPlugin, Kinds: []auth.Kind{auth.Ext}, Object: object,
		Apply: func(c *Ctx) (any, error) { return "pong", nil }})
	must(Spec{Name: "ext.nokind", Action: authz.ActionSubmitPlugin, Object: object,
		Apply: func(c *Ctx) (any, error) { return nil, nil }})
	must(Spec{Name: "note.versioned", Action: authz.ActionUpdateOwn, Object: object, RequireVersion: true,
		Apply: func(c *Ctx) (any, error) { return nil, nil }})
	return e
}

func (e *env) token(t *testing.T, s auth.Spec) auth.Credential {
	t.Helper()
	var c auth.Credential
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { c, secret, err = e.au.Issue(tx, s); return }); err != nil {
		t.Fatal(err)
	}
	c.Label = secret // 借 Label 带回明文，仅限测试
	return c
}

func (e *env) bearer(t *testing.T, s auth.Spec) Bearer {
	c := e.token(t, s)
	return Bearer{Auth: e.au, Secret: c.Label}
}

var owner = Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

func in(v noteIn) json.RawMessage { b, _ := json.Marshal(v); return b }

func (e *env) events(t *testing.T) []eventlog.Entry {
	t.Helper()
	var es []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { es, err = eventlog.Read(tx, 0, 1000); return }); err != nil {
		t.Fatal(err)
	}
	return es
}

func (e *env) noteCount(t *testing.T) int {
	t.Helper()
	var n int64
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT COUNT(*) FROM tnote")
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func TestSuccessWritesProjectionEventAndNotifies(t *testing.T) {
	e := setup(t)
	res, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1", Body: "hi", Forge: 7})})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || string(res.Value) != `{"id":"n1"}` || len(res.Seqs) != 1 {
		t.Fatalf("result %+v", res)
	}
	ev := e.events(t)[0].Event
	if ev.Actor.Principal != "owner" || ev.ChainDepth != 0 {
		t.Fatalf("kernel must own actor and depth: %+v", ev)
	}
	if len(e.commits) != 1 || e.commits[0].Command != "note.write" || e.commits[0].Seqs[0] != ev.Seq {
		t.Fatalf("commits %+v", e.commits)
	}
	if res2, _ := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1", Body: "again"})}); res2.Version != 2 {
		t.Fatalf("version should increase per write: %+v", res2)
	}
}

func TestAuthentication(t *testing.T) {
	e := setup(t)
	if _, err := e.x.Do(bg, Request{Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"})}); exit(err) != domain.ExitUnauthenticated {
		t.Fatalf("no identity: %v", err)
	}
	if _, err := e.x.Do(bg, Request{Identity: Bearer{Auth: e.au, Secret: "bogus"}, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"})}); exit(err) != domain.ExitUnauthenticated {
		t.Fatalf("bad secret: %v", err)
	}
	if _, err := e.x.Do(bg, Request{Identity: Trusted{}, Command: "note.write"}); exit(err) != domain.ExitUnauthenticated {
		t.Fatalf("empty trusted: %v", err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "nope"}); exit(err) != domain.ExitUsage {
		t.Fatalf("unknown command: %v", err)
	}
	// 吊销后的下一次命令立即被拒。
	w := e.token(t, auth.Spec{Kind: auth.Worker, Executor: "me"})
	b := Bearer{Auth: e.au, Secret: w.Label}
	if _, err := e.x.Do(bg, Request{Identity: b, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b", Assign: "me"})}); err != nil {
		t.Fatal(err)
	}
	_ = e.st.Update(bg, func(tx *store.Tx) error { return e.au.Revoke(tx, w.ID) })
	if _, err := e.x.Do(bg, Request{Identity: b, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b", Assign: "me"})}); exit(err) != domain.ExitUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
}

func TestAuthorizationDenyAndRejectionTrail(t *testing.T) {
	e := setup(t)
	obs := e.bearer(t, auth.Spec{Kind: auth.Observer})
	before := len(e.events(t))
	if _, err := e.x.Do(bg, Request{Identity: obs, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"})}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer: %v", err)
	}
	if len(e.events(t)) != before || e.applied != 0 {
		t.Fatal("plain denial must not write")
	}

	// delegate 回答 web_only：退出码 20，不执行，并留一条 command.rejected。
	dlg := e.token(t, auth.Spec{Kind: auth.Delegate})
	d := Bearer{Auth: e.au, Secret: dlg.Label, Origin: auth.Origin{Session: "s1"}}
	if _, err := e.x.Do(bg, Request{Identity: d, Command: "decision.answer", Input: in(noteIn{ID: "d1"}), Evidence: "yes", WebURL: "https://w/d1"}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("web_only: %v", err)
	}
	// 运行身份回答决策：退出码 4，也留痕。
	run := Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "w", Via: domain.ViaCLI, Run: "r1"}, Level: authz.LevelRun}
	if _, err := e.x.Do(bg, Request{Identity: run, Command: "decision.answer", Input: in(noteIn{ID: "d2"})}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("run: %v", err)
	}
	// 执行者不在项目名单：退出码 21。
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "item.assign", Input: in(noteIn{ID: "i1"}), Owner: "me"}); exit(err) != domain.ExitExecutorNotAllowed {
		t.Fatalf("executor: %v", err)
	}
	rej := e.events(t)[before:]
	if len(rej) != 3 || e.applied != 0 {
		t.Fatalf("want 3 rejection events and no apply, got %d / applied %d", len(rej), e.applied)
	}
	for _, r := range rej {
		if r.Event.Type != domain.EvCommandRejected {
			t.Fatalf("type %s", r.Event.Type)
		}
	}
	if rej[0].Event.Actor.Session != "s1" || rej[0].Event.Object != "d1" {
		t.Fatalf("trail must name who tried what: %+v", rej[0].Event)
	}
	if len(e.commits) != 0 {
		t.Fatal("rejected commands must not notify consumers")
	}
}

func TestExtAndWebhookKindsNeedExplicitAllow(t *testing.T) {
	e := setup(t)
	ext := e.bearer(t, auth.Spec{Kind: auth.Ext, Scope: "chat"})
	if _, err := e.x.Do(bg, Request{Identity: ext, Command: "ext.ping", Input: in(noteIn{ID: "p"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Do(bg, Request{Identity: ext, Command: "ext.nokind", Input: in(noteIn{ID: "p"})}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("%v", err)
	}
	whk := e.bearer(t, auth.Spec{Kind: auth.Webhook, Scope: "src"})
	if _, err := e.x.Do(bg, Request{Identity: whk, Command: "ext.ping", Input: in(noteIn{ID: "p"})}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("webhook not in Kinds: %v", err)
	}
}

func TestValidationFailureAndApplyErrorRollBackEverything(t *testing.T) {
	e := setup(t)
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1"})}); exit(err) != domain.ExitUsage {
		t.Fatalf("validate: %v", err)
	}
	_, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n2", Body: "x", Fail: true}), IdemKey: "k"})
	if err == nil || exit(err) != -1 {
		t.Fatalf("real error must surface unchanged: %v", err)
	}
	if e.noteCount(t) != 0 || len(e.events(t)) != 0 || len(e.commits) != 0 {
		t.Fatal("projection, event, version and idempotency record must roll back together")
	}
	// 失败没有占用幂等键，也没有推进版本。
	res, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n2", Body: "x"}), IdemKey: "k"})
	if err != nil || res.Version != 1 || res.Replayed {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: json.RawMessage(`{bad`)}); exit(err) != domain.ExitUsage {
		t.Fatalf("bad json: %v", err)
	}
}

func TestOptimisticVersion(t *testing.T) {
	e := setup(t)
	v := func(n int64) *int64 { return &n }
	r1, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n", Body: "a"}), ExpectVersion: v(0)})
	if err != nil || r1.Version != 1 {
		t.Fatalf("%+v %v", r1, err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"}), ExpectVersion: v(0)}); exit(err) != domain.ExitConflict {
		t.Fatalf("stale: %v", err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"}), ExpectVersion: v(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.versioned", Input: in(noteIn{ID: "n"})}); exit(err) != domain.ExitUsage {
		t.Fatalf("required: %v", err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "ext.nokind", Input: in(noteIn{}), ExpectVersion: v(0)}); exit(err) != domain.ExitUsage {
		t.Fatalf("version without object: %v", err)
	}
}

func TestIdempotencyKey(t *testing.T) {
	e := setup(t)
	req := Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n", Body: "a"}), IdemKey: "k1"}
	first, err := e.x.Do(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.x.Do(bg, req)
	if err != nil || !again.Replayed || string(again.Value) != string(first.Value) || again.Version != first.Version {
		t.Fatalf("replay must return the first result: %+v %v", again, err)
	}
	if e.applied != 1 || len(e.events(t)) != 1 || len(e.commits) != 1 {
		t.Fatal("replay must not write or notify again")
	}
	req.Input = in(noteIn{ID: "n", Body: "different"})
	if _, err := e.x.Do(bg, req); exit(err) != domain.ExitConflict {
		t.Fatalf("same key different input: %v", err)
	}
	// 键按操作者隔离。
	other := Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner2", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	req.Identity = other
	if r, err := e.x.Do(bg, req); err != nil || r.Replayed {
		t.Fatalf("keys are per principal: %+v %v", r, err)
	}
}

func TestKernelFieldsCannotBeWritten(t *testing.T) {
	e := setup(t)
	for _, body := range []string{
		`{"id":"n","body":"b","chain_depth":0}`,
		`{"id":"n","body":"b","nested":{"list":[{"unattended":true}]}}`,
		`{"id":"n","body":"b","version":9}`,
		`{"id":"n","body":"b","ask":{"risk_class":"low","screening_pending":false}}`,
		`{"id":"n","body":"b","basis":{}}`,
	} {
		if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: json.RawMessage(body)}); exit(err) != domain.ExitUsage {
			t.Fatalf("%s: %v", body, err)
		}
	}
	if e.applied != 0 {
		t.Fatal("rejected before apply")
	}
	// ext 与 spec 是用户数据，里面同名的键不是内核字段。
	ok := `{"id":"n","body":"b","ext":{"version":3},"spec":{"chain_depth":1}}`
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: json.RawMessage(ok)}); err != nil {
		t.Fatal(err)
	}
}

func TestChainDepthInheritance(t *testing.T) {
	e := setup(t)
	rule := Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "rules", Via: domain.ViaRule}, Level: authz.LevelWeb}
	// rule 不是 Web 身份，授权会拒；规则动作按 run/系统级别由内部入口给出，这里用 delegate 级别模拟自动动作。
	rule.Level = authz.LevelDelegate
	write := func(id string, ch Chain, who Trusted, ruleID string) (Result, error) {
		return e.x.Do(bg, Request{Identity: who, Command: "note.write", Input: in(noteIn{ID: id, Body: "b"}), Chain: ch, RuleID: ruleID})
	}
	depthOf := func(r Result) int {
		for _, en := range e.events(t) {
			if en.Event.Seq == r.Seqs[0] {
				return en.Event.ChainDepth
			}
		}
		t.Fatal("event missing")
		return -1
	}
	r, err := write("root", Chain{}, owner, "")
	if err != nil || depthOf(r) != 0 {
		t.Fatalf("new chain starts at 0: %v", err)
	}
	r, err = write("c1", Chain{Mode: ChainInherit, From: "root"}, owner, "")
	if err != nil || depthOf(r) != 1 {
		t.Fatalf("inherit is +1: %v", err)
	}
	r, err = write("c2", Chain{Mode: ChainInherit, From: "c1"}, owner, "")
	if err != nil || depthOf(r) != 2 {
		t.Fatal("grandchild is 2")
	}
	r, err = write("c3", Chain{Mode: ChainKeep, From: "c2"}, owner, "")
	if err != nil || depthOf(r) != 2 {
		t.Fatalf("keep does not add: %v", err)
	}
	if _, err := write("x", Chain{Mode: ChainInherit}, owner, ""); exit(err) != domain.ExitUsage {
		t.Fatalf("source required: %v", err)
	}
	if _, err := write("x", Chain{Mode: 9, From: "c1"}, owner, ""); exit(err) != domain.ExitUsage {
		t.Fatalf("bad mode: %v", err)
	}
	// 自动动作必须带规则 id，事件记策略版本与规则 id。
	if _, err := write("a", Chain{}, rule, ""); exit(err) != domain.ExitUsage {
		t.Fatalf("rule id: %v", err)
	}
	r, err = write("a", Chain{Mode: ChainInherit, From: "c1"}, rule, "r-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range e.events(t) {
		if en.Event.Seq == r.Seqs[0] && (en.Event.Basis == nil || en.Event.Basis.RuleID != "r-1" || en.Event.ChainDepth != 2) {
			t.Fatalf("basis/depth %+v", en.Event)
		}
	}
}

func TestChainDepthOverThresholdOnlyRecords(t *testing.T) {
	e := setup(t)
	e.x.MaxChainDepth = 2
	prev := "o0"
	var last Result
	for i := 0; i < 4; i++ {
		id := string(rune('a' + i))
		ch := Chain{}
		if i > 0 {
			ch = Chain{Mode: ChainInherit, From: prev}
		}
		r, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: id, Body: "b"}), Chain: ch})
		if err != nil {
			t.Fatal(err)
		}
		prev, last = id, r
		if i == 3 && !r.Suppressed {
			t.Fatal("depth 3 > 2 must be suppressed")
		}
		if i < 3 && r.Suppressed {
			t.Fatalf("depth %d must run", i)
		}
	}
	if e.noteCount(t) != 3 {
		t.Fatalf("suppressed command must not write projection, notes=%d", e.noteCount(t))
	}
	ev := e.events(t)
	if got := ev[len(ev)-1].Event; got.Type != domain.EvHealthAlert || got.ChainDepth != 3 || last.Seqs[0] != got.Seq {
		t.Fatalf("health alert expected: %+v", got)
	}
}

func TestRegisterValidationAndTrustedLevel(t *testing.T) {
	e := setup(t)
	if err := e.x.Register(Spec{Name: "note.write"}); err == nil {
		t.Fatal("incomplete spec must be rejected")
	}
	if err := e.x.Register(Spec{Name: "note.write", Object: func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil }, Apply: func(*Ctx) (any, error) { return nil, nil }}); err == nil {
		t.Fatal("duplicate must be rejected")
	}
	// 伪造的 Web 级别（非人类）被统一授权拒绝。
	fake := Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "x", Via: domain.ViaCLI}, Level: authz.LevelWeb}
	if _, err := e.x.Do(bg, Request{Identity: fake, Command: "note.write", Input: in(noteIn{ID: "n", Body: "b"})}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("%v", err)
	}
}
