package materials

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) vaults(t *testing.T, vs ...policy.NoteVault) {
	t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "vaults", func(p *policy.Policy) error { p.NoteVaults = vs; return nil })
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) link(t *testing.T, id command.Identity, in LinkIn) (LinkResult, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: "material.link", Input: b, Owner: "owner"})
	var out LinkResult
	if err == nil {
		_ = json.Unmarshal(r.Value, &out)
	}
	return out, err
}

func TestLinkRegistersAMaterialWithoutBodyOrRevision(t *testing.T) {
	e := setup(t)
	e.vaults(t, policy.NoteVault{Name: "notes", OpenURL: "obsidian://open?vault={vault}&file={path}"})
	r, err := e.link(t, worker, LinkIn{Link: "vault:notes/项目/设计.md", Title: "设计稿", Project: e.prj, Tags: []string{"d"}})
	if err != nil || !r.Created {
		t.Fatalf("link: %+v %v", r, err)
	}
	var m Material
	e.view(t, func(tx *store.Tx) (err error) { m, err = Get(tx, r.Material); return })
	if m.Form != FormLink || m.Link != "vault:notes/项目/设计.md" || m.Kind != "reference" || m.CurrentRev != "" || m.Location != "" {
		t.Fatalf("material = %+v", m)
	}
	var pol policy.Policy
	e.view(t, func(tx *store.Tx) (err error) { _, pol, err = policy.Current(tx); return })
	if got := OpenURL(pol, m); got != "obsidian://open?vault=notes&file=%E9%A1%B9%E7%9B%AE%2F%E8%AE%BE%E8%AE%A1.md" {
		t.Fatalf("open url %q", got)
	}
	var revs []Revision
	e.view(t, func(tx *store.Tx) (err error) { revs, err = History(tx, m.ID); return })
	if len(revs) != 0 {
		t.Fatalf("a linked material has no revisions, got %d", len(revs))
	}
	es := e.events(t, m.ID)
	if len(es) != 1 || es[0].Type != domain.EvMaterialRevised || es[0].Actor.Principal != "wrk" {
		t.Fatalf("events = %+v", es)
	}
	// 删除库登记后材料保留，只是不再有打开方式。
	e.vaults(t)
	e.view(t, func(tx *store.Tx) (err error) { m, err = Get(tx, m.ID); _, pol, _ = policy.Current(tx); return })
	if m.Link == "" || OpenURL(pol, m) != "" {
		t.Fatalf("after removing the vault: link %q open %q", m.Link, OpenURL(pol, m))
	}
}

func TestLinkIsIdempotentPerProjectAndKeepsFirstTitle(t *testing.T) {
	e := setup(t)
	e.vaults(t, policy.NoteVault{Name: "notes"})
	first, err := e.link(t, worker, LinkIn{Link: "vault:notes/a.md", Title: "第一次", Project: e.prj, Kind: "minutes"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.link(t, worker, LinkIn{Link: "vault:notes/a.md", Title: "第二次", Project: e.prj, Kind: "worknote"})
	if err != nil || again.Created || again.Material != first.Material {
		t.Fatalf("repeat registration: %+v %v", again, err)
	}
	var m Material
	e.view(t, func(tx *store.Tx) (err error) { m, err = Get(tx, first.Material); return })
	if m.Title != "第一次" || m.Kind != "minutes" {
		t.Fatalf("repeat registration changed the material: %+v", m)
	}
	if len(e.events(t, m.ID)) != 1 {
		t.Fatal("a repeat registration records no second event")
	}
	// 另一个项目里同一位置是另一条。
	var out struct{ ID string }
	b, _ := json.Marshal(map[string]any{"name": "other"})
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: "project.create", Input: b, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(r.Value, &out)
	other, err := e.link(t, worker, LinkIn{Link: "vault:notes/a.md", Title: "别的项目", Project: out.ID})
	if err != nil || !other.Created || other.Material == first.Material {
		t.Fatalf("other project: %+v %v", other, err)
	}
}

func TestLinkRejectsBadLocationsAndFields(t *testing.T) {
	e := setup(t)
	e.vaults(t, policy.NoteVault{Name: "notes"})
	ok := LinkIn{Link: "vault:notes/a.md", Title: "t", Project: e.prj}
	for name, mod := range map[string]func(*LinkIn){
		"absolute path":   func(in *LinkIn) { in.Link = "/home/me/notes/a.md" },
		"file url":        func(in *LinkIn) { in.Link = "file:///home/me/a.md" },
		"unregistered":    func(in *LinkIn) { in.Link = "vault:nope/a.md" },
		"dotdot":          func(in *LinkIn) { in.Link = "vault:notes/../a.md" },
		"dot segment":     func(in *LinkIn) { in.Link = "vault:notes/./a.md" },
		"empty segment":   func(in *LinkIn) { in.Link = "vault:notes/a//b.md" },
		"leading slash":   func(in *LinkIn) { in.Link = "vault:notes//a.md" },
		"backslash":       func(in *LinkIn) { in.Link = "vault:notes/a\\b.md" },
		"no path":         func(in *LinkIn) { in.Link = "vault:notes" },
		"no host":         func(in *LinkIn) { in.Link = "https:///x" },
		"other scheme":    func(in *LinkIn) { in.Link = "ftp://example.com/x" },
		"empty":           func(in *LinkIn) { in.Link = " " },
		"no title":        func(in *LinkIn) { in.Title = " " },
		"no project":      func(in *LinkIn) { in.Project = "" },
		"unknown project": func(in *LinkIn) { in.Project = "prj_nope" },
		"guideline":       func(in *LinkIn) { in.Kind = KindGuideline },
		"unknown kind":    func(in *LinkIn) { in.Kind = "nonsense" },
	} {
		in := ok
		mod(&in)
		if _, err := e.link(t, worker, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := e.link(t, worker, LinkIn{Link: "/abs", Title: "t", Project: e.prj})
	code(t, err, domain.CodeInvalidRequest)
	_, err = e.link(t, worker, LinkIn{Link: "vault:notes/a.md", Title: "t", Project: "prj_nope"})
	code(t, err, domain.CodeNotFound)
	// 观察者不能登记（授权由命令管线判定）。
	_, err = e.link(t, observer, ok)
	code(t, err, domain.CodePermissionDenied)
	var n int
	e.view(t, func(tx *store.Tx) error { ms, err := List(tx, ""); n = len(ms); return err })
	if n != 0 {
		t.Fatalf("%d materials registered by rejected requests", n)
	}
}

func TestNormalizeLocationCanonicalForms(t *testing.T) {
	vs := []policy.NoteVault{{Name: "é"}}
	cases := map[string]string{
		"vault:e\u0301/a/e\u0301.md":              "vault:é/a/é.md",
		"HTTPS://Example.COM/Path/CaseKept?Q=1#F": "https://example.com/Path/CaseKept?Q=1#F",
		"http://User:Pw@Host.Example:8080/x":      "http://User:Pw@host.example:8080/x",
		" https://example.com ":                   "https://example.com",
	}
	for in, want := range cases {
		got, err := NormalizeLocation(in, vs)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestMaterialFormIsInferredForRowsFromBeforeSchema8(t *testing.T) {
	e := setup(t)
	// 升级前的行：form 为空。读取时按原规则推定，且不改写已有行。
	e.must(t, worker, "", "material.commit", commitIn{Project: e.prj, Kind: "minutes", Title: "旧自有", Content: "x", Reason: "r"})
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec("UPDATE materials SET form = ''"); err != nil {
			return err
		}
		_, err := AddHosted(tx, e.prj, "design", "旧托管", nil, "git@h:r.git:docs/a.md")
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE materials SET form = '' WHERE location <> ''")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var ms []Material
	e.view(t, func(tx *store.Tx) (err error) { ms, err = List(tx, e.prj); return })
	got := map[string]string{}
	for _, m := range ms {
		got[m.Title] = m.Form
	}
	if got["旧自有"] != FormOwned || got["旧托管"] != FormHosted {
		t.Fatalf("inferred forms = %v", got)
	}
	var raw int64
	e.view(t, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT COUNT(*) FROM materials WHERE form = ''")
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&raw)
	})
	if raw != 2 {
		t.Fatalf("reading rewrote rows: %d still blank, want 2", raw)
	}
}
