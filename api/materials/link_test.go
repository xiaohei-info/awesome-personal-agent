package materials

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) setVaults(vs ...policy.NoteVault) {
	e.t.Helper()
	err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "vaults",
			func(p *policy.Policy) error { p.NoteVaults = vs; return nil })
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

type linkShown struct {
	Material struct {
		km.Material
		OpenURL string `json:"open_url"`
	}
	Relations []domain.Relation
	Revision  *km.Revision
}

func (e *env) shown(who, id string) linkShown {
	e.t.Helper()
	rec := e.do(who, "GET", "/v1/materials/"+id, "")
	want(e.t, rec, 200, "")
	var s linkShown
	into(e.t, rec, &s)
	return s
}

// flow-link-material：登记、幂等、读取（形态、位置、打开地址）、笔记库登记被删后材料保留。
func TestLinkedMaterialRegisterReadAndVaultRemoval(t *testing.T) {
	e := setup(t)
	e.setVaults(policy.NoteVault{Name: "notes", OpenURL: "obsidian://open?vault={vault}&file={path}"})
	body := `{"link":"vault:notes/项目/设计 稿.md","title":"设计稿","project":"` + e.prj + `","tags":["d"]}`

	rec := e.do("wrk", "POST", "/v1/materials", body)
	want(t, rec, 201, "")
	var first struct {
		Material string
		Created  bool
	}
	into(t, rec, &first)
	if first.Material == "" || !first.Created {
		t.Fatalf("create: %s", rec.Body)
	}
	// 重复登记：200 与已有的那条，不改标题与 kind。
	rec = e.do("wrk", "POST", "/v1/materials", `{"link":"vault:notes/项目/设计 稿.md","title":"别的标题","project":"`+e.prj+`","kind":"minutes"}`)
	want(t, rec, 200, "")
	var again struct{ Material string }
	into(t, rec, &again)
	if again.Material != first.Material {
		t.Fatalf("repeat registration returned %s, want %s", again.Material, first.Material)
	}

	s := e.shown("obs", first.Material)
	if s.Material.Form != "link" || s.Material.Link != "vault:notes/项目/设计 稿.md" || s.Material.Title != "设计稿" || s.Material.Kind != "reference" || s.Revision != nil {
		t.Fatalf("show: %+v", s)
	}
	if s.Material.OpenURL != "obsidian://open?vault=notes&file=%E9%A1%B9%E7%9B%AE%2F%E8%AE%BE%E8%AE%A1%20%E7%A8%BF.md" {
		t.Fatalf("open_url = %q", s.Material.OpenURL)
	}
	// 修订接口对链接型没有内容可读。
	var hist struct{ Revisions []km.Revision }
	into(t, e.do("obs", "GET", "/v1/materials/"+first.Material+"/revisions", ""), &hist)
	if len(hist.Revisions) != 0 {
		t.Fatalf("a linked material has no revisions: %+v", hist)
	}

	// 其他形态也带 form；列表里每条都有。
	owned, _ := e.commit("wrk", `{"project":"`+e.prj+`","kind":"minutes","title":"N","content":"x","reason":"r"}`)
	var list struct {
		Materials []struct {
			ID, Form, Link string
			OpenURL        string `json:"open_url"`
		}
	}
	into(t, e.do("obs", "GET", "/v1/materials?project="+e.prj, ""), &list)
	forms := map[string]string{}
	for _, m := range list.Materials {
		forms[m.ID] = m.Form
	}
	if forms[first.Material] != "link" || forms[owned.Material] != "owned" || len(forms) != 2 {
		t.Fatalf("forms = %v", forms)
	}

	// 标题照常可搜，没有正文。
	var hits struct{ Hits []struct{ ID string } }
	into(t, e.do("obs", "GET", "/v1/search?type=material&q="+"设计", ""), &hits)
	if len(hits.Hits) != 1 || hits.Hits[0].ID != first.Material {
		t.Fatalf("search: %+v", hits)
	}

	// 删除库的登记：材料保留，只是不再有 open_url。
	e.setVaults()
	s = e.shown("obs", first.Material)
	if s.Material.Link == "" || s.Material.OpenURL != "" {
		t.Fatalf("after removing the vault: %+v", s.Material)
	}
}

func TestLinkedMaterialRejections(t *testing.T) {
	e := setup(t)
	e.setVaults(policy.NoteVault{Name: "notes"})
	post := func(who, link string) *httptest.ResponseRecorder {
		return e.do(who, "POST", "/v1/materials", `{"link":"`+link+`","title":"t","project":"`+e.prj+`"}`)
	}
	want(t, post("obs", "vault:notes/a.md"), 403, "permission_denied")
	want(t, post("", "vault:notes/a.md"), 401, "")
	for _, bad := range []string{"/home/me/a.md", "file:///a.md", "vault:nope/a.md", "vault:notes/../a.md", "ftp://x/y"} {
		want(t, post("wrk", bad), 400, "invalid_request")
	}
	want(t, e.do("wrk", "POST", "/v1/materials", `{"link":"vault:notes/a.md","title":"t"}`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"link":"vault:notes/a.md","title":"t","project":"`+e.prj+`","kind":"guideline"}`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"link":"vault:notes/a.md","title":"t","project":"`+e.prj+`","content":"x"}`), 400, "invalid_request")
	var list struct{ Materials []any }
	into(t, e.do("obs", "GET", "/v1/materials", ""), &list)
	if len(list.Materials) != 0 {
		t.Fatalf("rejected requests registered %d materials", len(list.Materials))
	}
}

func TestNoteVaultsReadIsOpenToAgentsButNotToExtOrAnonymous(t *testing.T) {
	e := setup(t)
	var out struct{ Vaults []policy.NoteVault }
	rec := e.do("obs", "GET", "/v1/note-vaults", "")
	want(t, rec, 200, "")
	if into(t, rec, &out); out.Vaults == nil || len(out.Vaults) != 0 {
		t.Fatalf("no vaults registered reads as an empty list: %s", rec.Body)
	}
	e.setVaults(policy.NoteVault{Name: "notes", OpenURL: "app://{vault}/{path}"}, policy.NoteVault{Name: "plain"})
	rec = e.do("wrk", "GET", "/v1/note-vaults", "")
	want(t, rec, 200, "")
	into(t, rec, &out)
	if len(out.Vaults) != 2 || out.Vaults[0].Name != "notes" || out.Vaults[0].OpenURL != "app://{vault}/{path}" || out.Vaults[1].OpenURL != "" {
		t.Fatalf("vaults: %s", rec.Body)
	}
	want(t, e.do("", "GET", "/v1/note-vaults", ""), 401, "")
	want(t, e.do("ext", "GET", "/v1/note-vaults", ""), 403, "permission_denied")
	want(t, e.do("export", "GET", "/v1/note-vaults", ""), 200, "")
}

// flow-web-vault-materials：PATCH /materials/{id} 只有 Web 登录能改标题、kind 与标签，位置不可改。
func TestPatchLinkedMaterialWebOnly(t *testing.T) {
	e := setup(t)
	e.setVaults(policy.NoteVault{Name: "notes"})
	rec := e.do("wrk", "POST", "/v1/materials", `{"link":"vault:notes/a.md","title":"旧","project":"`+e.prj+`"}`)
	want(t, rec, 201, "")
	var c struct{ Material string }
	into(t, rec, &c)
	url := "/v1/materials/" + c.Material
	body := `{"title":"新","kind":"minutes","tags":["x"],"reason":"整理"}`

	for _, who := range []string{"obs", "wrk", "dlg"} {
		if rec := e.do(who, "PATCH", url, body); rec.Code != 403 {
			t.Fatalf("%s PATCH = %d %s", who, rec.Code, rec.Body)
		}
	}
	want(t, e.do("web", "PATCH", url, body), 200, "")
	s := e.shown("obs", c.Material)
	if s.Material.Title != "新" || s.Material.Kind != "minutes" || len(s.Material.Tags) != 1 || s.Material.Link != "vault:notes/a.md" {
		t.Fatalf("after patch: %+v", s.Material)
	}
	// 位置不可改；体里不许带 id；自有型不在此改。
	if rec := e.do("web", "PATCH", url, `{"link":"vault:notes/b.md"}`); rec.Code != 400 {
		t.Fatalf("link patch = %d", rec.Code)
	}
	if rec := e.do("web", "PATCH", url, `{"id":"x","title":"t"}`); rec.Code != 400 {
		t.Fatalf("id in body = %d", rec.Code)
	}
	owned, _ := e.commit("wrk", `{"project":"`+e.prj+`","kind":"minutes","title":"N","content":"x","reason":"r"}`)
	if rec := e.do("web", "PATCH", "/v1/materials/"+owned.Material, `{"title":"t"}`); rec.Code != 409 {
		t.Fatalf("owned patch = %d %s", rec.Code, rec.Body)
	}
}
