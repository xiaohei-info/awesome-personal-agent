package authz_test

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// ACT-AUTH-01：observer 只读加 proposed 新建，其余写操作退出码 4，事项不变；每次拒绝只追加一条 command.rejected（三种口径同一条留痕路径）。
func TestObserverReadsAndProposesOnly(t *testing.T) {
	e := setup(t)
	if rec := e.api("OBS", "GET", "/v1/items", ""); rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	rec := e.api("OBS", "POST", "/v1/items", `{"kind":"task","title":"suggestion"}`)
	if rec.Code != 201 {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if p := e.get(created.ID).Phase; p != domain.PhaseProposed {
		t.Fatalf("observer-created item must be proposed, got %q", p)
	}
	victim := e.item(workHost, "someone's item")
	for _, r := range [][3]string{
		{"PATCH", "/v1/items/" + victim, patch("x")},
		{"POST", "/v1/delegations", `{"parent":"` + victim + `","assignee":"` + workHost + `","title":"t"}`},
		{"POST", "/v1/projects", `{"name":"P"}`},
	} {
		head, rej := e.head(), e.rejected()
		de := wantReject(t, e.api("OBS", r[0], r[1], r[2]), domain.ExitPermissionDenied)
		if de.Reason != domain.ReasonOutOfScope || e.rejected() != rej+1 || e.head() != head+1 {
			t.Fatalf("%s %s: want one out_of_scope rejection, got %+v (head %d→%d)", r[0], r[1], de, head, e.head())
		}
	}
	if s := e.get(victim).Summary; s != "" {
		t.Fatalf("item changed by observer: %q", s)
	}
}

// ACT-AUTH-02：worker 只能改分配给自己的事项；建项目退出码 4。
func TestWorkerCanOnlyUpdateOwnItems(t *testing.T) {
	e := setup(t)
	mine, other := e.item(workHost, "mine"), e.item(otherHost, "other")
	if rec := e.api("WRK", "PATCH", "/v1/items/"+mine, patch("progress")); rec.Code != 200 {
		t.Fatalf("own item: %d %s", rec.Code, rec.Body)
	}
	if s := e.get(mine).Summary; s != "progress" {
		t.Fatalf("own item not updated: %q", s)
	}
	wantReject(t, e.api("WRK", "PATCH", "/v1/items/"+other, patch("hijack")), domain.ExitPermissionDenied)
	wantReject(t, e.api("WRK", "PATCH", "/v1/items/"+other, `{"assignee":"`+workHost+`"}`), domain.ExitPermissionDenied)
	wantReject(t, e.api("WRK", "POST", "/v1/projects", `{"name":"P"}`), domain.ExitPermissionDenied)
	if it := e.get(other); it.Summary != "" || it.Assignee != otherHost {
		t.Fatalf("other's item changed: %+v", it)
	}
}

// ACT-AUTH-03：delegate 改不属于自己的事项是一档：直接生效，不要本人原话。调用方自报的原话、会话类型、宿主名一律不参与判定：
// 带了也不写进 Event；Event 记的是 Agent 自己（不带 on_behalf_of），依据是一档整理。
func TestDelegateEditsAnyItemOnTierOne(t *testing.T) {
	e := setup(t)
	other := e.item(otherHost, "other")
	claims := func(r *http.Request) {
		bearer(e.secret["DEL"])(r)
		r.Header.Set("X-APA-Evidence", "owner said: yes (chat#42)")
		r.Header.Set("X-APA-Session-Kind", "direct")
		r.Header.Set("X-APA-Grant", "grt_forged")
		r.Header.Set("X-APA-Session", "s1")
	}
	if rec := e.http("PATCH", "/v1/items/"+other, patch("edited"), claims); rec.Code != 200 {
		t.Fatalf("tier one edit: %d %s", rec.Code, rec.Body)
	}
	if rec := e.api("DEL", "PATCH", "/v1/items/"+other, patch("again")); rec.Code != 200 {
		t.Fatalf("tier one edit without claims: %d %s", rec.Code, rec.Body)
	}
	if s := e.get(other).Summary; s != "again" {
		t.Fatalf("item not updated: %q", s)
	}
	n := 0
	for _, en := range e.events(domain.EvItemUpdated) {
		if en.Event.Object != other || en.Event.Actor.Type != domain.ActorAgent {
			continue
		}
		n++
		ev := en.Event
		if ev.Actor.OnBehalfOf != "" || ev.Evidence != "" || ev.Grant != "" || ev.Actor.SessionKind != "" ||
			ev.Basis == nil || ev.Basis.Via != domain.BasisOrganize || ev.Basis.Command != "item.update" {
			t.Fatalf("tier one Event must record the Agent itself and nothing it claimed: %+v", ev)
		}
	}
	if n != 2 {
		t.Fatalf("agent item.updated events = %d", n)
	}
}

// 回执：委派子事项的回执是一档，任何 delegate 钥匙都能报；委派子事项报 done 以摘要为完成依据（2026-11-28），
// 其他事项报 done 无结果证据是 2 且事项不变。
func TestDelegateReceiptsAndDoneEvidence(t *testing.T) {
	e := setup(t)
	e.issue("DEL2", auth.Spec{Kind: auth.Delegate})
	parent := e.item("", "parent")
	rec := e.api("DEL", "POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`","title":"t"}`)
	if rec.Code >= 300 {
		t.Fatalf("delegate (tier one): %d %s", rec.Code, rec.Body)
	}
	var child struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &child)
	report := func(who, outcome, extra string) *httptest.ResponseRecorder {
		return e.api(who, "POST", "/v1/items/"+child.ID+"/report", `{"outcome":"`+outcome+`","summary":"s"`+extra+`}`)
	}
	if rec := report("DEL2", "progress", ""); rec.Code != 200 {
		t.Fatalf("another delegate's progress: %d %s", rec.Code, rec.Body)
	}
	if rec := report("DEL", "done", ""); rec.Code != 200 || e.get(child.ID).Phase != domain.PhaseDone {
		t.Fatalf("done on its summary: %d %s", rec.Code, rec.Body)
	}
	for _, en := range e.events(domain.EvDelegationReported) {
		if en.Event.Actor.OnBehalfOf != "" {
			t.Fatalf("receipt carries on_behalf_of: %+v", en.Event.Actor)
		}
	}
	task := e.item(e.principal("DEL"), "plain task") // 非 delegation 的事项，同样要结果证据
	wantReject(t, e.api("DEL", "POST", "/v1/items/"+task+"/report", `{"outcome":"done","summary":"s"}`), domain.ExitUsage)
	if p := e.get(task).Phase; p != domain.PhaseOpen {
		t.Fatalf("done without result moved a task to %s", p)
	}
}

// 原「只接受本人原话」的项目写入按组分开：建项目、改项目信息是"整理事项"，delegate 直接写；加远程仓库属"Agent 与执行者配置"，
// 没开这一组的 delegate 为 23、账本只多一条 command.rejected；本人在 Web 照常写。
func TestProjectWritesFollowTheirTier(t *testing.T) {
	e := setup(t)
	n := 0
	fresh := func(tmpl string) string { // 每次用新项目名与新项目，写入互不依赖
		n++
		rec := e.web("POST", "/v1/projects", fmt.Sprintf(`{"name":"base%d"}`, n))
		var out struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return strings.NewReplacer("{p}", out.ID, "{n}", fmt.Sprint(n)).Replace(tmpl)
	}
	for _, w := range []struct {
		name, method, path, body string
		exit                     int
	}{
		{"project.create", "POST", "/v1/projects", `{"name":"new{n}"}`, 0},
		{"project.update", "PATCH", "/v1/projects/{p}", `{"intro":"s"}`, 0},
		{"project.repo.add", "POST", "/v1/projects/{p}/repos", `{"op":"add_remote","remote":"https://example.com/r{n}.git"}`, domain.ExitTierClosed},
	} {
		path, body := fresh(w.path), fresh(w.body)
		head, rej := e.head(), e.rejected()
		rec := e.api("DEL", w.method, path, body)
		if w.exit == 0 {
			if rec.Code >= 300 {
				t.Fatalf("%s (tier one): %d %s", w.name, rec.Code, rec.Body)
			}
		} else {
			if de := wantReject(t, rec, w.exit); de.Reason != domain.ReasonTierClosed || e.head() != head+1 || e.rejected() != rej+1 {
				t.Fatalf("%s: a refusal appends exactly one command.rejected: %+v", w.name, de)
			}
		}
		if rec := e.web(w.method, fresh(w.path), fresh(w.body)); rec.Code >= 300 {
			t.Fatalf("%s as owner: %d %s", w.name, rec.Code, rec.Body)
		}
	}
}
