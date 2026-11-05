package authz_test

import (
	"encoding/json"
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
