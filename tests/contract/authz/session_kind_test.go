package authz_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 会话类型（§14.5、附录 B.3）：凭本人原话的代理写入只在 direct 下接受，走真实凭据、命令管线与 HTTP 骨干。

const skdWords = "owner: MARKER-SKD yes, do it"

func (e *env) asKind(name, session, kind string) command.Identity {
	return command.Bearer{Auth: e.au, Secret: e.secret[name], Origin: auth.Origin{Harness: "acc-stub", Session: session, SessionKind: kind}, OnBehalfOf: "owner"}
}

// ACT-SKD-03…06、ACT-SKD-08、ACT-SKD-13、ACT-AUTH-08、ACT-INJ-06：group、unknown、未上报与不认识的值都是 23 / session_not_direct，
// 错误体带 Web 链接、不带用户内容，账本多一条 command.rejected 并记下会话类型；direct 放行，Event 记会话类型。
func TestOwnerWordsOnlyInDirectSessions(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	other := e.item(otherHost, "MARKER-SKD other")
	for _, kind := range []string{"group", "unknown", "", "weird"} {
		head, rej := e.head(), e.rejected()
		rec := e.apiKind("DEL", "skd-s", kind, "PATCH", "/v1/items/"+other, patch("edited"), skdWords)
		de := wantReject(t, rec, domain.ExitBasisRequired)
		if de.Code != domain.CodeSessionNotDirect || rec.Code != 403 || de.WebURL == nil || *de.WebURL == "" {
			t.Fatalf("kind %q: %d %+v", kind, rec.Code, de)
		}
		if strings.Contains(rec.Body.String(), "MARKER-SKD") || strings.Contains(rec.Body.String(), "yes, do it") {
			t.Fatalf("kind %q: error body carries user content: %s", kind, rec.Body)
		}
		if e.rejected() != rej+1 || e.head() < head+1 {
			t.Fatalf("kind %q: want one command.rejected", kind)
		}
		if e.get(other).Summary != "" {
			t.Fatalf("kind %q: item changed", kind)
		}
		// 拒绝留痕：凭据、会话引用、会话类型与原因。
		last := e.events(domain.EvCommandRejected)
		a := last[len(last)-1].Event
		var pl map[string]any
		_ = json.Unmarshal(a.Payload, &pl)
		wantKind := kind
		if kind == "weird" {
			wantKind = "unknown"
		}
		if a.Actor.Principal != e.principal("DEL") || a.Actor.Session != "skd-s" || a.Actor.SessionKind != wantKind || pl["code"] != "session_not_direct" {
			t.Fatalf("kind %q: rejection record %+v %v", kind, a.Actor, pl)
		}
		if strings.Contains(string(a.Payload)+a.Reason, "MARKER-SKD") {
			t.Fatalf("kind %q: rejection record carries user content", kind)
		}
		e.answerAsk(t, other) // 被拒后事项转 needs_input，让下一轮从 open 开始
	}
	rec := e.apiKind("DEL", "skd-s", "direct", "PATCH", "/v1/items/"+other, patch("edited"), skdWords)
	if rec.Code != 200 || e.get(other).Summary != "edited" {
		t.Fatalf("direct: %d %s", rec.Code, rec.Body)
	}
	for _, en := range e.events(domain.EvItemUpdated) {
		a := en.Event.Actor
		if en.Event.Object == other && a.SessionKind == "direct" && a.Session == "skd-s" && a.Harness == "acc-stub" && a.OnBehalfOf == "owner" {
			return
		}
	}
	t.Fatal("accepted write must record harness, session and session kind in the actor")
}

// answerAsk 在 Web 回答内核为被拒绝的代答生成的 ask，让事项回到 open。
func (e *env) answerAsk(t *testing.T, id string) {
	t.Helper()
	it := e.get(id)
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil {
		t.Fatalf("a rejected write must turn the item to needs_input with a kernel ask: %+v", it)
	}
	e.cmd(owner, "decision.answer", map[string]any{"id": id, "choice": it.Ask.Options[0].ID})
	if e.get(id).Phase != domain.PhaseOpen {
		t.Fatalf("item did not return to open: %s", e.get(id).Phase)
	}
}

// ACT-SKD-09、ACT-SKD-10、ACT-SKD-11、ACT-DEC-09：授权单、第一档（agent 自己的工作）、读取与委派回执都不看会话类型。
func TestSessionKindDoesNotGateGrantsOwnWorkOrReads(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	g := e.grant("DEL", nil)
	for _, kind := range []string{"group", "unknown", ""} {
		id := e.asked()
		e.cmd(e.asKind("DEL", "s", kind), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID))
		if r := e.get(id).Ask.Resolution; r == nil || r.ResolvedVia != domain.ResolvedViaGrant {
			t.Fatalf("grant under kind %q: %+v", kind, r)
		}
	}
	mine := e.item(e.principal("DEL"), "mine")
	for _, kind := range []string{"group", ""} {
		if rec := e.apiKind("DEL", "s", kind, "PATCH", "/v1/items/"+mine, patch("half done"), ""); rec.Code != 200 {
			t.Fatalf("own work under kind %q: %d %s", kind, rec.Code, rec.Body)
		}
		if rec := e.apiKind("DEL", "s", kind, "GET", "/v1/items/"+mine, "", ""); rec.Code != 200 {
			t.Fatalf("read under kind %q: %d %s", kind, rec.Code, rec.Body)
		}
	}
	parent := e.item("", "parent")
	rec := e.apiKind("DEL", "s", "", "POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`","title":"t"}`, "")
	var child struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &child)
	if rec.Code >= 300 || child.ID == "" {
		t.Fatalf("delegate without a session kind: %d %s", rec.Code, rec.Body)
	}
	rec = e.apiKind("DEL", "s", "", "POST", "/v1/items/"+child.ID+"/report", `{"outcome":"progress","summary":"s"}`, "")
	if rec.Code != 200 {
		t.Fatalf("receipt without a session kind: %d %s", rec.Code, rec.Body)
	}
}

// ACT-AUTH-09…12（SPIKE-DONE-AUTHZ）：delegate 把本人的事项标为 done 属第二档——无依据 23 并转 needs_input，
// direct 会话的原话放行，group 为 session_not_direct，授权单只在范围内放行。
func TestDelegateDoneOnOwnersItemNeedsWordsInDirectSessionOrGrant(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	done := func(id string) string { return `{"to":"done","reason":"done"}` }
	i9 := e.item("", "i9")
	head := e.rejected()
	wantReject(t, e.apiKind("DEL", "", "", "POST", "/v1/items/"+i9+"/transition", done(i9), ""), domain.ExitBasisRequired)
	if e.get(i9).Phase != domain.PhaseNeedsInput || e.rejected() != head+1 {
		t.Fatalf("no basis: %s", e.get(i9).Phase)
	}
	i12 := e.item("", "i12")
	de := wantReject(t, e.apiKind("DEL", "s", "group", "POST", "/v1/items/"+i12+"/transition", done(i12), "做完了"), domain.ExitBasisRequired)
	if de.Code != domain.CodeSessionNotDirect || e.get(i12).Phase == domain.PhaseDone {
		t.Fatalf("group: %+v %s", de, e.get(i12).Phase)
	}
	i10 := e.item("", "i10")
	if rec := e.apiKind("DEL", "s", "direct", "POST", "/v1/items/"+i10+"/transition", done(i10), "这件事做完了"); rec.Code != 200 || e.get(i10).Phase != domain.PhaseDone {
		t.Fatalf("direct: %d %s", rec.Code, rec.Body)
	}
	for _, en := range e.events(domain.EvItemPhaseChanged) {
		if a := en.Event.Actor; en.Event.Object == i10 && a.OnBehalfOf == "owner" && a.Session == "s" && a.SessionKind == "direct" && en.Event.Evidence != "" {
			return
		}
	}
	t.Fatal("done by words must record on_behalf_of, evidence, session and session kind")
}

// ACT-AUTH-11（SPIKE-DONE-AUTHZ）：授权单把本人的事项标为完成——范围内放行且 Event 记 grant；范围外为 23 且事项转 needs_input；
// 授权单的 actions 不含 item.transition 也是 23。授权单不看会话类型（这里不带会话类型）。
func TestGrantAuthorisesDoneOnlyInScope(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	home := e.cmd(owner, "project.create", map[string]any{"name": "acc-home"})
	open := e.cmd(owner, "project.create", map[string]any{"name": "acc-open"})
	mk := func(project string) string {
		return e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": "mine", "phase": "open", "project": project})
	}
	done := map[string]any{"to": "done", "reason": "done"}
	with := func(id string) map[string]any {
		return map[string]any{"id": id, "to": "done", "reason": done["reason"]}
	}
	g := e.grant("DEL", func(in *policy.GrantInput) {
		in.Scope = policy.Scope{Projects: []string{home}}
		in.Actions = []string{"item.transition"}
	})
	inScope, outScope := mk(home), mk(open)

	e.cmd(e.asKind("DEL", "", ""), "item.transition", with(inScope), withGrant(g.ID))
	if e.get(inScope).Phase != domain.PhaseDone {
		t.Fatalf("in scope: %s", e.get(inScope).Phase)
	}
	if evs := e.events4(inScope); func() bool {
		for _, ev := range evs {
			if ev.Type == domain.EvItemPhaseChanged && ev.Grant == g.ID {
				return false
			}
		}
		return true
	}() {
		t.Fatal("the transition Event must record the grant")
	}

	rej := e.rejected()
	e.wantExit(domain.ExitBasisRequired, e.asKind("DEL", "", ""), "item.transition", with(outScope), withGrant(g.ID))
	if e.get(outScope).Phase != domain.PhaseNeedsInput || e.rejected() != rej+1 {
		t.Fatalf("out of scope: %s", e.get(outScope).Phase)
	}

	noAction := e.grant("DEL", func(in *policy.GrantInput) {
		in.Scope = policy.Scope{Projects: []string{home}}
		in.Actions = []string{"decision.answer"}
	})
	other := mk(home)
	e.wantExit(domain.ExitBasisRequired, e.asKind("DEL", "", ""), "item.transition", with(other), withGrant(noAction.ID))
	if e.get(other).Phase == domain.PhaseDone {
		t.Fatal("a grant without item.transition completed the item")
	}
}
