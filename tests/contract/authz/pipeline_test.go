package authz_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

// ACT-AUTH-03：delegate 改任意事项必须附证据；带证据成功，Event 带 on_behalf_of 与本人原话。
func TestDelegateEditsAnyItemOnlyWithEvidence(t *testing.T) {
	e := setup(t)
	other := e.item(otherHost, "other")
	wantReject(t, e.api("DEL", "PATCH", "/v1/items/"+other, patch("edited"), ""), domain.ExitUsage)
	if s := e.get(other).Summary; s != "" {
		t.Fatalf("item changed without evidence: %q", s)
	}
	const words = "owner said: yes, edit it (chat#42)"
	if rec := e.api("DEL", "PATCH", "/v1/items/"+other, patch("edited"), words); rec.Code != 200 {
		t.Fatalf("with evidence: %d %s", rec.Code, rec.Body)
	}
	if s := e.get(other).Summary; s != "edited" {
		t.Fatalf("item not updated with evidence: %q", s)
	}
	var found bool
	for _, en := range e.events(domain.EvItemUpdated) {
		if en.Event.Object == other && en.Event.Actor.OnBehalfOf == "owner" && strings.Contains(en.Event.Evidence, "chat#42") {
			found = true
		}
	}
	if !found {
		for _, en := range e.events(domain.EvItemUpdated) {
			t.Logf("%+v ev=%q", en.Event.Actor, en.Event.Evidence)
		}
		t.Fatal("update Event lacks on_behalf_of and the owner's words")
	}
}

// 设计 §14.3/§14.5：delegate 的每个代理写操作都须附 on_behalf_of 与证据，不止事项命令。
// 逐条经 HTTP 登记的写路由：缺证据退出码 2 且账本不变，带证据放行；owner 不带证据照常写。
func TestDelegateProxyWritesNeedEvidenceAcrossModules(t *testing.T) {
	e := setup(t)
	n := 0
	fresh := func(tmpl string) string { // 每次用新项目名与新项目，写入互不依赖
		n++
		rec := e.web("POST", "/v1/projects", fmt.Sprintf(`{"name":"base%d"}`, n))
		var out struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return strings.NewReplacer("{p}", out.ID, "{n}", fmt.Sprint(n)).Replace(tmpl)
	}
	parent := e.item("", "parent")
	writes := []struct{ name, method, path, body string }{
		{"project.create", "POST", "/v1/projects", `{"name":"new{n}"}`},
		{"project.update", "PATCH", "/v1/projects/{p}", `{"intro":"s"}`},
		{"project.repo.add", "POST", "/v1/projects/{p}/repos", `{"op":"add_remote","remote":"https://example.com/r{n}.git"}`},
		{"delegate.create", "POST", "/v1/delegations", `{"parent":"` + parent + `","assignee":"` + workHost + `","title":"t"}`},
	}
	for _, w := range writes {
		path, body := fresh(w.path), fresh(w.body)
		head := e.head()
		for _, ev := range []string{"", "  "} {
			wantReject(t, e.api("DEL", w.method, path, body, ev), domain.ExitUsage)
		}
		if e.head() != head {
			t.Fatalf("%s: rejected delegate write appended events", w.name)
		}
		if rec := e.api("DEL", w.method, path, fresh(w.body), "owner: go ahead (session s1)"); rec.Code >= 300 {
			t.Fatalf("%s with evidence: %d %s", w.name, rec.Code, rec.Body)
		}
		if rec := e.web(w.method, fresh(w.path), fresh(w.body)); rec.Code >= 300 {
			t.Fatalf("%s as owner without evidence: %d %s", w.name, rec.Code, rec.Body)
		}
	}
}
