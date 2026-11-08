package authz_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) get(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return })
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

func patch(summary string) string {
	b, _ := json.Marshal(map[string]string{"summary": summary})
	return string(b)
}

// ACT-AUTH-01：observer 只读加 proposed 新建，其余写操作退出码 4，账本不变。
func TestObserverReadsAndProposesOnly(t *testing.T) {
	e := setup(t)
	if rec := e.api("OBS", "GET", "/v1/items", "", ""); rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	rec := e.api("OBS", "POST", "/v1/items", `{"kind":"task","title":"suggestion"}`, "")
	if rec.Code != 201 {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if p := e.get(created.ID).Phase; p != domain.PhaseProposed {
		t.Fatalf("observer-created item must be proposed, got %q", p)
	}
	victim := e.item(workHost, "someone's item")
	head := e.head()
	wantReject(t, e.api("OBS", "PATCH", "/v1/items/"+victim, patch("x"), ""), domain.ExitPermissionDenied)
	wantReject(t, e.api("OBS", "POST", "/v1/delegations", `{"parent":"`+victim+`","assignee":"`+workHost+`","title":"t"}`, ""), domain.ExitPermissionDenied)
	wantReject(t, e.api("OBS", "POST", "/v1/projects", `{"name":"P"}`, ""), domain.ExitPermissionDenied)
	if s := e.get(victim).Summary; s != "" {
		t.Fatalf("item changed by observer: %q", s)
	}
	if got := e.head(); got != head {
		t.Fatalf("rejected observer writes appended events: head %d → %d", head, got)
	}
}

// ACT-AUTH-02：worker 只能改分配给自己的事项；建项目退出码 4。
func TestWorkerCanOnlyUpdateOwnItems(t *testing.T) {
	e := setup(t)
	mine, other := e.item(workHost, "mine"), e.item(otherHost, "other")
	if rec := e.api("WRK", "PATCH", "/v1/items/"+mine, patch("progress"), ""); rec.Code != 200 {
		t.Fatalf("own item: %d %s", rec.Code, rec.Body)
	}
	if s := e.get(mine).Summary; s != "progress" {
		t.Fatalf("own item not updated: %q", s)
	}
	wantReject(t, e.api("WRK", "PATCH", "/v1/items/"+other, patch("hijack"), ""), domain.ExitPermissionDenied)
	wantReject(t, e.api("WRK", "PATCH", "/v1/items/"+other, `{"assignee":"`+workHost+`"}`, ""), domain.ExitPermissionDenied)
	wantReject(t, e.api("WRK", "POST", "/v1/projects", `{"name":"P"}`, ""), domain.ExitPermissionDenied)
	if it := e.get(other); it.Summary != "" || it.Assignee != otherHost {
		t.Fatalf("other's item changed: %+v", it)
	}
}

// ACT-AUTH-03：delegate 改不属于自己的事项是第二档，要本人原话加会话引用；缺任何一个为 23、记 command.rejected、事项不变；
// 都带了才成功，Event 带 on_behalf_of、本人原话与会话。
func TestDelegateEditsAnyItemOnlyWithWordsAndSession(t *testing.T) {
	e := setup(t)
	other := e.item(otherHost, "other")
	const words = "owner said: yes, edit it (chat#42)"
	for _, c := range []struct{ session, evidence string }{{"", ""}, {"s1", ""}, {"s1", "  "}, {"", words}} {
		head, rej := e.head(), e.rejected()
		de := wantReject(t, e.apiIn("DEL", c.session, "PATCH", "/v1/items/"+other, patch("edited"), c.evidence), domain.ExitBasisRequired)
		if de.Code != domain.CodeBasisRequired {
			t.Fatalf("code %s", de.Code)
		}
		if s := e.get(other).Summary; s != "" {
			t.Fatalf("item changed without full basis %+v: %q", c, s)
		}
		if e.rejected() != rej+1 || e.head() != head+1 {
			t.Fatalf("%+v: a 23 must append exactly one command.rejected (rejected %d→%d, head %d→%d)", c, rej, e.rejected(), head, e.head())
		}
	}
	if rec := e.apiIn("DEL", "s1", "PATCH", "/v1/items/"+other, patch("edited"), words); rec.Code != 200 {
		t.Fatalf("with words and session: %d %s", rec.Code, rec.Body)
	}
	if s := e.get(other).Summary; s != "edited" {
		t.Fatalf("item not updated: %q", s)
	}
	for _, en := range e.events(domain.EvItemUpdated) {
		a := en.Event.Actor
		if en.Event.Object == other && a.OnBehalfOf == "owner" && a.Session == "s1" && strings.Contains(en.Event.Evidence, "chat#42") {
			return
		}
	}
	t.Fatal("update Event lacks on_behalf_of, the owner's words or the session")
}

// 第一档：delegate 对绑定的事项（assignee 等于该凭据）改动不要本人原话，Event 的 actor 是 agent 本身、不带 on_behalf_of；
// 带了原话照常记录。绑定只来自账本，换一个事项或另一个凭据就不成立。
func TestDelegateOwnWorkNeedsNoWords(t *testing.T) {
	e := setup(t)
	mine := e.item(e.principal("DEL"), "mine")
	if rec := e.api("DEL", "PATCH", "/v1/items/"+mine, patch("progress"), ""); rec.Code != 200 {
		t.Fatalf("bound item without words: %d %s", rec.Code, rec.Body)
	}
	if rec := e.api("DEL", "PATCH", "/v1/items/"+mine, patch("noted"), "owner: fyi"); rec.Code != 200 {
		t.Fatalf("bound item with words: %d %s", rec.Code, rec.Body)
	}
	ups := e.events(domain.EvItemUpdated)
	var n int
	for _, en := range ups {
		if en.Event.Object != mine {
			continue
		}
		n++
		if en.Event.Actor.OnBehalfOf != "" || en.Event.Actor.Principal != e.principal("DEL") {
			t.Fatalf("tier one event must be the agent itself: %+v", en.Event.Actor)
		}
	}
	if n != 2 {
		t.Fatalf("events on the bound item = %d", n)
	}
	// 另一个 delegate 凭据对同一事项不绑定，按第二档。
	e.issue("DEL2", auth.Spec{Kind: auth.Delegate})
	rej := e.rejected()
	wantReject(t, e.api("DEL2", "PATCH", "/v1/items/"+mine, patch("hijack"), ""), domain.ExitBasisRequired)
	if e.rejected() != rej+1 || e.get(mine).Summary != "noted" {
		t.Fatal("an unbound credential must be rejected and leave the item unchanged")
	}
}

// 绑定的另一半：委派子事项由发起它的 delegate 凭据绑定（账本里 delegate.create 的操作者），主会话回执不要原话；
// 别的 delegate 凭据的回执按第二档。报 done 无结果证据对任何 kind 都是 2 且事项不变，第一档也一样。
func TestDelegateReceiptBindingAndDoneEvidence(t *testing.T) {
	e := setup(t)
	e.issue("DEL2", auth.Spec{Kind: auth.Delegate})
	parent := e.item("", "parent")
	rec := e.api("DEL", "POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`","title":"t"}`, "")
	if rec.Code >= 300 {
		t.Fatalf("delegate without words (tier one): %d %s", rec.Code, rec.Body)
	}
	var child struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &child)
	report := func(who, outcome, extra string) *httptest.ResponseRecorder {
		return e.api(who, "POST", "/v1/items/"+child.ID+"/report", `{"outcome":"`+outcome+`","summary":"s"`+extra+`}`, "")
	}
	rej := e.rejected()
	wantReject(t, report("DEL2", "progress", ""), domain.ExitBasisRequired)
	if e.rejected() != rej+1 {
		t.Fatal("unbound receipt must record command.rejected")
	}
	wantReject(t, report("DEL", "done", ""), domain.ExitUsage)
	if p := e.get(child.ID).Phase; p != domain.PhaseOpen {
		t.Fatalf("done without result evidence moved the child to %s", p)
	}
	if rec := report("DEL", "progress", ""); rec.Code != 200 {
		t.Fatalf("initiator progress without words: %d %s", rec.Code, rec.Body)
	}
	if rec := report("DEL", "done", `,"evidence":{"pr":"https://x/pr/1"}`); rec.Code != 200 {
		t.Fatalf("initiator done with result: %d %s", rec.Code, rec.Body)
	}
	for _, en := range e.events(domain.EvDelegationReported) {
		if en.Event.Actor.OnBehalfOf != "" {
			t.Fatalf("tier one receipt carries on_behalf_of: %+v", en.Event.Actor)
		}
	}
	task := e.item(e.principal("DEL"), "plain task") // 非 delegation 的事项，同样要结果证据
	wantReject(t, e.api("DEL", "POST", "/v1/items/"+task+"/report", `{"outcome":"done","summary":"s"}`, ""), domain.ExitUsage)
	if p := e.get(task).Phase; p != domain.PhaseOpen {
		t.Fatalf("done without result moved a task to %s", p)
	}
}

// 设计 §14.5 / 附录 B.1：每条代理写命令的档位。第二档里只接受本人原话的管理命令（项目）：
// 缺原话或缺会话为 23 且账本只多一条 command.rejected，带齐放行；owner 不带任何依据照常写。
func TestDelegateManagementWritesNeedWordsAndSession(t *testing.T) {
	e := setup(t)
	n := 0
	fresh := func(tmpl string) string { // 每次用新项目名与新项目，写入互不依赖
		n++
		rec := e.web("POST", "/v1/projects", fmt.Sprintf(`{"name":"base%d"}`, n))
		var out struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return strings.NewReplacer("{p}", out.ID, "{n}", fmt.Sprint(n)).Replace(tmpl)
	}
	writes := []struct{ name, method, path, body string }{
		{"project.create", "POST", "/v1/projects", `{"name":"new{n}"}`},
		{"project.update", "PATCH", "/v1/projects/{p}", `{"intro":"s"}`},
		{"project.repo.add", "POST", "/v1/projects/{p}/repos", `{"op":"add_remote","remote":"https://example.com/r{n}.git"}`},
	}
	for _, w := range writes {
		path, body := fresh(w.path), fresh(w.body)
		for _, c := range []struct{ session, evidence string }{{"", ""}, {"s1", ""}, {"s1", "  "}, {"", "owner: go ahead"}} {
			head, rej := e.head(), e.rejected()
			wantReject(t, e.apiIn("DEL", c.session, w.method, path, body, c.evidence), domain.ExitBasisRequired)
			if e.head() != head+1 || e.rejected() != rej+1 {
				t.Fatalf("%s %+v: a 23 appends exactly one command.rejected", w.name, c)
			}
		}
		if rec := e.apiIn("DEL", "s1", w.method, path, fresh(w.body), "owner: go ahead"); rec.Code >= 300 {
			t.Fatalf("%s with words and session: %d %s", w.name, rec.Code, rec.Body)
		}
		if rec := e.web(w.method, fresh(w.path), fresh(w.body)); rec.Code >= 300 {
			t.Fatalf("%s as owner without evidence: %d %s", w.name, rec.Code, rec.Body)
		}
	}
}
