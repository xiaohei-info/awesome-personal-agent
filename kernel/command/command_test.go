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
	d := Bearer{Auth: e.au, Secret: dlg.Label, Origin: auth.Origin{Session: "s1", SessionKind: "direct"}}
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

// 同事务回调与命令同一个事务：看得到命令刚追加的事件（含序号），写入随命令一起提交；返回错误则整个命令回滚。
func TestInTxHookSharesTheCommandTransaction(t *testing.T) {
	e := setup(t)
	var seen []int64
	e.x.InTx(func(c *Ctx) error {
		for _, ev := range c.Events() {
			seen = append(seen, ev.Seq)
		}
		if _, err := c.Tx.Exec(`INSERT INTO tnote (id, body) VALUES (?, ?)`, "hook-"+parse(c.Input).ID, "from hook"); err != nil {
			return err
		}
		if parse(c.Input).Body == "veto" {
			return errors.New("hook says no")
		}
		return nil
	})
	res, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1", Body: "hi"})})
	if err != nil || len(seen) != 1 || seen[0] != res.Seqs[0] || e.noteCount(t) != 2 {
		t.Fatalf("res=%+v err=%v seen=%v notes=%d", res, err, seen, e.noteCount(t))
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n2", Body: "veto"})}); err == nil {
		t.Fatal("a failing hook must fail the command")
	}
	if e.noteCount(t) != 2 || len(e.events(t)) != 1 || len(e.commits) != 1 {
		t.Fatal("the command and the hook's writes must roll back together")
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
		`{"id":"n","body":"b","Chain_Depth":0}`,
		`{"id":"n","body":"b","VERSION":9}`,
		`{"id":"n","body":"b","SEQ":1}`,
		`{"id":"n","body":"b","Unattended":true}`,
		`{"id":"n","body":"b","ask":{"RISK_CLASS":"x","Screening_Pending":true}}`,
		`{"id":"n","body":"b","x":[{"y":{"BASIS":{}}}]}`,
		`{"id":"n","body":"b","spec":{},"EXT":{},"Ext2":{"Chain_depth":1}}`,
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
	// 规则动作不能省略 Chain 而悄悄开新链路。
	if _, err := write("n0", Chain{}, rule, "r-1"); exit(err) != domain.ExitUsage {
		t.Fatalf("rule action with new chain: %v", err)
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
	agent := Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	prev := "o0"
	var last Result
	for i := 0; i < 4; i++ {
		id := string(rune('a' + i))
		ch := Chain{}
		if i > 0 {
			ch = Chain{Mode: ChainInherit, From: prev}
		}
		r, err := e.x.Do(bg, Request{Identity: agent, Command: "note.write", Input: in(noteIn{ID: id, Body: "b"}), Chain: ch})
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

// 链路深度的推定（§14.9）：运行凭据继承运行记录的深度加一；未指明来源时沿用对象当前深度，不清零；
// 只有本人在 Web 委派开始新链路；本人在 Web 的操作超阈值也照常生效。
func TestChainDepthResolvedByCaller(t *testing.T) {
	e := setup(t)
	e.x.MaxChainDepth = 4
	if err := e.x.Register(Spec{Name: "note.delegate", Action: authz.ActionDelegate,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			return authz.Object{ID: parse(in).ID}, nil
		},
		Apply: func(c *Ctx) (any, error) {
			_, err := c.Emit(domain.Event{Type: "note.delegated", Object: parse(c.Input).ID, Payload: json.RawMessage(`{}`)})
			return nil, err
		}}); err != nil {
		t.Fatal(err)
	}
	inScope := func(*store.Tx, authz.Object) (bool, bool, error) { return true, false, nil }
	run := Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run:r1", Via: domain.ViaCLI, Run: "r1"},
		Level: authz.LevelRun, RunScope: inScope, RunDepth: 2}
	agent := Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	depthOf := func(who Trusted, cmd, id string) (int, Result) {
		t.Helper()
		r, err := e.x.Do(bg, Request{Identity: who, Command: cmd, Input: in(noteIn{ID: id, Body: "b"})})
		if err != nil {
			t.Fatal(err)
		}
		ev := e.events(t)
		return ev[len(ev)-1].Event.ChainDepth, r
	}
	// 运行凭据连写两次都是运行深度加一，不在同一运行里自己累加。
	for i := 0; i < 2; i++ {
		if d, _ := depthOf(run, "note.write", "n"); d != 3 {
			t.Fatalf("run write %d depth = %d, want 3", i, d)
		}
	}
	if d, _ := depthOf(agent, "note.write", "n"); d != 3 {
		t.Fatalf("agent write keeps object depth: %d", d)
	}
	if d, _ := depthOf(owner, "note.write", "n"); d != 3 {
		t.Fatalf("owner edit keeps object depth: %d", d)
	}
	if d, _ := depthOf(agent, "note.delegate", "n"); d != 3 {
		t.Fatalf("agent delegation keeps object depth: %d", d)
	}
	if d, _ := depthOf(owner, "note.delegate", "n"); d != 0 {
		t.Fatalf("owner delegation in Web starts a new chain: %d", d)
	}
	// 超阈值：自动方被拦下，本人照常生效且深度不清零。
	deep := Trusted(run)
	deep.RunDepth = 4
	if d, r := depthOf(deep, "note.write", "m"); !r.Suppressed || d != 5 {
		t.Fatal("run over threshold must be suppressed")
	}
	// 熔断记录本身带着超限深度（5），本人沿用它且照常生效。
	if d, r := depthOf(owner, "note.write", "m"); r.Suppressed || d != 5 {
		t.Fatalf("owner write over threshold: depth %d suppressed %v", d, r.Suppressed)
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

func TestRejectedAttemptDoesNotResetChainDepth(t *testing.T) {
	e := setup(t)
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "root", Body: "b"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "deep", Body: "b"}), Chain: Chain{Mode: ChainInherit, From: "root"}}); err != nil {
		t.Fatal(err)
	}
	// 在 deep（深度 1）上被拒绝的尝试会追加 depth 0 的 command.rejected。
	dlg := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	if _, err := e.x.Do(bg, Request{Identity: dlg, Command: "decision.answer", Input: in(noteIn{ID: "deep"}), Evidence: "x"}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("setup rejection: %v", err)
	}
	es := e.events(t)
	if es[len(es)-1].Event.Type != domain.EvCommandRejected {
		t.Fatal("rejection event expected last")
	}
	r, err := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "next", Body: "b"}), Chain: Chain{Mode: ChainInherit, From: "deep"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range e.events(t) {
		if en.Event.Seq == r.Seqs[0] && en.Event.ChainDepth != 2 {
			t.Fatalf("want depth 2, got %d", en.Event.ChainDepth)
		}
	}
}

// Spec.Actions 取代 Action：每个动作都要通过，第一个被拒绝的决定结果，并按该动作的规则留痕。
func TestMultipleActionsAllMustPass(t *testing.T) {
	e := setup(t)
	obj := func(*store.Tx, json.RawMessage) (authz.Object, error) {
		return authz.Object{ID: "o1", Executor: "stranger", ProjectInfo: &domain.Project{AllowedExecutors: []string{"a"}}}, nil
	}
	applied := 0
	acts := func(as ...authz.Action) func(*store.Tx, Caller, authz.Object, json.RawMessage) []authz.Action {
		return func(*store.Tx, Caller, authz.Object, json.RawMessage) []authz.Action { return as }
	}
	for name, as := range map[string][]authz.Action{
		"ok":     {authz.ActionUpdateOwn, authz.ActionRead},
		"checks": {authz.ActionRead, authz.ActionReassign},
	} {
		if err := e.x.Register(Spec{Name: "multi." + name, Action: authz.ActionRead, Actions: acts(as...), Object: obj,
			Apply: func(*Ctx) (any, error) { applied++; return nil, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "multi.ok"}); err != nil || applied != 1 {
		t.Fatalf("all allowed: %v applied=%d", err, applied)
	}
	before := len(e.events(t))
	if _, err := e.x.Do(bg, Request{Identity: owner, Command: "multi.checks"}); exit(err) != domain.ExitExecutorNotAllowed || applied != 1 {
		t.Fatalf("second action denies: %v applied=%d", err, applied)
	}
	if es := e.events(t); len(es) != before+1 || es[before].Event.Type != domain.EvCommandRejected || es[before].Event.Object != "o1" {
		t.Fatal("a rejection of any action must be recorded")
	}
}

// Bump 与管线共用同一版本序列。
func TestBumpSharesVersionSequence(t *testing.T) {
	e := setup(t)
	e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1", Body: "a"})})
	var v1, v2 int64
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		if v1, err = Bump(tx, "n1"); err != nil {
			return err
		}
		v2, err = Version(tx, "n1")
		return
	}); err != nil || v1 != 2 || v2 != 2 {
		t.Fatalf("bump: %d %d %v", v1, v2, err)
	}
	res, _ := e.x.Do(bg, Request{Identity: owner, Command: "note.write", Input: in(noteIn{ID: "n1", Body: "b"})})
	if res.Version != 3 {
		t.Fatalf("pipeline continues after a bump: %d", res.Version)
	}
}

// §14.5：档位只在管线一处判定。命令只声明 Tier，对象绑定（Bound，据账本）把第二档降到第一档；
// 第一档的 Event 不带 on_behalf_of，第二档要原话加会话，缺失为 23 并记 command.rejected。
func TestProxyTiersAreDecidedInThePipeline(t *testing.T) {
	e := setup(t)
	object := func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
		n := parse(in)
		return authz.Object{ID: n.ID, Assignee: n.Assign}, nil
	}
	apply := func(c *Ctx) (any, error) {
		_, err := c.Emit(domain.Event{Type: domain.EvItemUpdated, Object: c.Object.ID, Payload: json.RawMessage(`{}`), Evidence: c.Evidence})
		return nil, err
	}
	for _, s := range []Spec{
		{Name: "tier.work", Action: authz.ActionUpdateOwn, Tier: authz.TierWork, Object: object, Apply: apply},
		{Name: "tier.decide", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Object: object, Apply: apply},
		{Name: "tier.bound", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Object: object, Apply: apply,
			Bound: func(_ *store.Tx, c Caller, o authz.Object, _ json.RawMessage) (bool, error) {
				return o.Assignee == c.Actor.Principal, nil
			}},
	} {
		if err := e.x.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	if tier, ok := e.x.Tier("tier.bound"); !ok || tier != authz.TierDecide {
		t.Fatalf("declared tier = %v %v", tier, ok)
	}
	c := e.token(t, auth.Spec{Kind: auth.Delegate})
	me := Bearer{Auth: e.au, Secret: c.Label, Origin: auth.Origin{Session: "s1", SessionKind: "direct"}, OnBehalfOf: "owner"}
	noSession := Bearer{Auth: e.au, Secret: c.Label, OnBehalfOf: "owner"}
	lastActor := func() domain.Actor { es := e.events(t); return es[len(es)-1].Event.Actor }
	do := func(id Identity, cmd, ev string, n noteIn) error {
		_, err := e.x.Do(bg, Request{Identity: id, Command: cmd, Input: in(n), Evidence: ev})
		return err
	}

	// 第一档：不要原话，actor 是 agent 本身。
	if err := do(me, "tier.work", "", noteIn{ID: "a"}); err != nil || lastActor().OnBehalfOf != "" {
		t.Fatalf("tier one: %v %+v", err, lastActor())
	}
	// 第二档：缺原话、缺会话都是 23，各记一条 command.rejected，不执行。
	before, applied := len(e.events(t)), e.applied
	for _, id := range []Identity{me, noSession} {
		ev := ""
		if id == Identity(noSession) {
			ev = "owner: ok"
		}
		if err := do(id, "tier.decide", ev, noteIn{ID: "b"}); exit(err) != domain.ExitBasisRequired {
			t.Fatalf("tier two without full basis: %v", err)
		}
	}
	rej := e.events(t)[before:]
	if len(rej) != 2 || rej[0].Event.Type != domain.EvCommandRejected || rej[0].Event.Object != "b" || e.applied != applied {
		t.Fatalf("rejections = %+v", rej)
	}
	if err := do(me, "tier.decide", "owner: ok", noteIn{ID: "b"}); err != nil {
		t.Fatalf("tier two with words and session: %v", err)
	}
	if a := lastActor(); a.OnBehalfOf != "owner" || a.Session != "s1" {
		t.Fatalf("tier two actor = %+v", a)
	}
	// 绑定降档：assignee 即该凭据时按第一档；别人的事项仍按第二档。
	if err := do(me, "tier.bound", "", noteIn{ID: "c", Assign: c.ID}); err != nil || lastActor().OnBehalfOf != "" {
		t.Fatalf("bound: %v %+v", err, lastActor())
	}
	if err := do(me, "tier.bound", "", noteIn{ID: "d", Assign: "someone"}); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("unbound: %v", err)
	}
	// 绑定只来自账本：输入里自称不算（assignee 不等于凭据）。
	if err := do(me, "tier.bound", "", noteIn{ID: "e", Assign: "owner"}); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("claimed binding: %v", err)
	}
}

func TestOnRejectRunsInTheRejectionTransaction(t *testing.T) {
	e := setup(t)
	if err := e.x.Register(Spec{Name: "tier.decide", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Receipt: true,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			return authz.Object{ID: parse(in).ID}, nil
		},
		Apply: func(c *Ctx) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	var got []Rejection
	var hookErr error
	e.x.OnReject(func(tx *store.Tx, r Rejection) error {
		got = append(got, r)
		_, err := tx.Exec(`INSERT INTO tnote (id, body) VALUES ('hooked', 'x')`)
		if err != nil {
			return err
		}
		return hookErr
	})
	c := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	c.Origin = auth.Origin{Session: "s1", SessionKind: "direct"}
	do := func() error {
		_, err := e.x.Do(bg, Request{Identity: c, Command: "tier.decide", Input: in(noteIn{ID: "o1"})})
		return err
	}
	if exit(do()) != domain.ExitBasisRequired || len(got) != 1 || got[0].Object != "o1" || got[0].Exit != domain.ExitBasisRequired {
		t.Fatalf("hook not called with the rejection: %+v", got)
	}
	types := map[string]int{}
	for _, en := range e.events(t) {
		types[en.Event.Type]++
	}
	if types[domain.EvCommandRejected] != 1 || types[domain.EvHealthAlert] != 1 || e.noteCount(t) != 1 {
		t.Fatalf("rejection, alert and hook write must land together: %v notes=%d", types, e.noteCount(t))
	}
	// 回调失败：拒绝留痕、告警、回调写入一并回滚。
	hookErr = errors.New("boom")
	if err := do(); err == nil || exit(err) == domain.ExitBasisRequired {
		t.Fatalf("hook failure must surface, got %v", err)
	}
	n := 0
	for _, en := range e.events(t) {
		if en.Event.Type == domain.EvCommandRejected || en.Event.Type == domain.EvHealthAlert {
			n++
		}
	}
	if n != 2 || e.noteCount(t) != 1 {
		t.Fatalf("failed rejection transaction leaked: events=%d notes=%d", n, e.noteCount(t))
	}
}

func TestReceiptCommandRejectionsRecordHealthAlert(t *testing.T) {
	e := setup(t)
	reg := func(name string, receipt bool) {
		if err := e.x.Register(Spec{Name: name, Action: authz.ActionUpdateOwn, Receipt: receipt,
			Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
				return authz.Object{ID: parse(in).ID}, nil
			},
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				if parse(in).Body == "" {
					return domain.NewError(domain.CodeInvalidRequest, "body required", "add body")
				}
				return nil
			},
			Apply: func(c *Ctx) (any, error) { return nil, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	reg("rcpt.write", true)
	reg("plain.write", false)
	alerts := func() (out []domain.Event) {
		for _, en := range e.events(t) {
			if en.Event.Type == domain.EvHealthAlert {
				out = append(out, en.Event)
			}
		}
		return
	}
	del := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	obs := e.bearer(t, auth.Spec{Kind: auth.Observer})
	run := func(id Identity, cmd string, n noteIn) error {
		_, err := e.x.Do(bg, Request{Identity: id, Command: cmd, Input: in(n)})
		return err
	}
	// 2：校验失败；4：越权。回执类命令各记一条，对象为被写的事项，操作者为发起者。
	if exit(run(del, "rcpt.write", noteIn{ID: "o1"})) != domain.ExitUsage || exit(run(obs, "rcpt.write", noteIn{ID: "o2", Body: "x"})) != domain.ExitPermissionDenied {
		t.Fatal("expected 2 and 4")
	}
	as := alerts()
	if len(as) != 2 || as[0].Object != "o1" || as[1].Object != "o2" || as[0].Actor.Type != domain.ActorAgent {
		t.Fatalf("alerts = %+v", as)
	}
	var p struct {
		Kind, Command string
		Exit          int
	}
	_ = json.Unmarshal(as[0].Payload, &p)
	if p.Kind != ReceiptKind || p.Command != "rcpt.write" || p.Exit != domain.ExitUsage {
		t.Fatalf("payload = %+v", p)
	}
	// 非回执命令、以及不属于 2/4/23 的失败（冲突 6、未找到）不记告警；成功也不记。
	_ = run(del, "plain.write", noteIn{ID: "o3"})
	_ = run(obs, "plain.write", noteIn{ID: "o4", Body: "x"})
	if err := run(del, "rcpt.write", noteIn{ID: "o5", Body: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(alerts()) != 2 {
		t.Fatalf("unexpected alerts: %+v", alerts())
	}
}
