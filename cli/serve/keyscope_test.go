package serve

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kmaterials "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func registerSource(t *testing.T, k *kernel, id string) {
	t.Helper()
	k.do(t, command.Chain{}, "source.register", map[string]any{"id": id, "name": "名-" + id, "type": "webhook", "source_type": "mail", "config": map[string]any{"scheme": "shared"}})
}

func attachEvent(t *testing.T, k *kernel, source, key, item string) {
	t.Helper()
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: source}, domain.SourceEvent{ID: key, Type: "mail.received", Subject: "s-" + key, Detail: "d-" + key, OccurredAt: time.Now()}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, source)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.DedupeKey.EventID == key {
				return runner.SetDisposition(tx, ev.ID, domain.DispositionAttached, item, "")
			}
		}
		t.Fatalf("event %s not found", key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 事项与材料按来源档位裁（§14.14）：一条事项的来源是 origin 加上挂在它上面的消息的来源，取最严；材料取与它相连的事项里最严的；
// 不出自任何来源的事项按全文。摘录留正文与进展前 500 字，元信息去掉正文、摘要与进展正文，不给的不出现在列表里、单条 403；
// 外部钥匙先裁后搜，正文命中不暴露看不到正文的事项。内部钥匙全部全文。
func TestExternalKeyItemsAndMaterialsTakeTheStrictestSource(t *testing.T) {
	k := assembled(t)
	for _, id := range []string{"src_a", "src_b"} {
		registerSource(t, k, id)
	}
	long := "BODYWORD " + strings.Repeat("长", 700)
	var it, plain struct{ ID string }
	_ = json.Unmarshal(k.do(t, command.Chain{}, "item.create", map[string]any{"kind": "task", "title": "混合事项", "phase": "open",
		"body": long, "summary": "摘要", "origin": map[string]any{"source": "src_a", "ref": "r"}}), &it)
	k.do(t, command.Chain{}, "item.progress", map[string]any{"id": it.ID, "text": "进展 " + strings.Repeat("进", 700)})
	attachEvent(t, k, "src_b", "b1", it.ID)
	_ = json.Unmarshal(k.do(t, command.Chain{}, "item.create", map[string]any{"kind": "task", "title": "本人写的", "phase": "open", "body": "自己写的正文"}), &plain)
	var pr struct{ ID string }
	_ = json.Unmarshal(k.do(t, command.Chain{}, "project.create", map[string]any{"name": "P"}), &pr)
	var mr kmaterials.Result
	_ = json.Unmarshal(k.do(t, command.Chain{}, "material.commit", map[string]any{"project": pr.ID, "kind": "minutes", "title": "纪要", "content": "MATWORD " + strings.Repeat("材", 700), "reason": "r"}), &mr)
	k.do(t, command.Chain{}, "item.relate", map[string]any{"from": it.ID, "to": mr.Material, "type": domain.RelRelates})

	excerpt := issue(t, k, auth.Spec{Kind: auth.Observer, SourceScopes: map[string]string{"src_a": auth.ScopeFull, "src_b": auth.ScopeExcerpt}})
	meta := issue(t, k, auth.Spec{Kind: auth.Observer, SourceScopes: map[string]string{"src_a": auth.ScopeFull, "src_b": auth.ScopeMeta}})
	none := issue(t, k, auth.Spec{Kind: auth.Observer, SourceScopes: map[string]string{"src_a": auth.ScopeFull}})
	internal := issue(t, k, auth.Spec{Kind: auth.Observer, Audience: auth.AudienceInternal})

	type itemRead struct {
		Item         domain.Item       `json:"item"`
		Progress     []domain.Progress `json:"progress"`
		ContentScope string            `json:"content_scope"`
		Trimmed      *struct {
			TotalChars    int `json:"total_chars"`
			ReturnedChars int `json:"returned_chars"`
		} `json:"trimmed"`
	}
	read := func(key, path string, v any) int {
		t.Helper()
		code, body := getAs(k, key, path)
		if code == 200 {
			if err := json.Unmarshal([]byte(body), v); err != nil {
				t.Fatalf("%s: %v %s", path, err, body)
			}
		}
		return code
	}

	var ex itemRead
	if read(excerpt, "/v1/items/"+it.ID, &ex) != 200 || ex.ContentScope != "excerpt" || utf8.RuneCountInString(ex.Item.Body) != 500 || ex.Item.Summary != "摘要" ||
		ex.Trimmed == nil || ex.Trimmed.TotalChars != utf8.RuneCountInString(long) || ex.Trimmed.ReturnedChars != 500 || len(ex.Progress) != 1 || utf8.RuneCountInString(ex.Progress[0].Text) != 500 {
		t.Fatalf("excerpt (src_a full, src_b excerpt → excerpt): %+v", ex)
	}
	var me itemRead
	if read(meta, "/v1/items/"+it.ID, &me) != 200 || me.ContentScope != "meta" || me.Item.Body != "" || me.Item.Summary != "" || me.Item.Title != "混合事项" ||
		len(me.Progress) != 1 || me.Progress[0].Text != "" {
		t.Fatalf("meta keeps the title and drops body, summary and progress text: %+v", me)
	}
	if code, body := getAs(k, none, "/v1/items/"+it.ID); code != 403 || !strings.Contains(body, "out_of_scope") {
		t.Fatalf("a source the key was not given makes the whole item unreachable: %d %s", code, body)
	}
	var pl itemRead
	if read(none, "/v1/items/"+plain.ID, &pl) != 200 || pl.ContentScope != "" || pl.Item.Body != "自己写的正文" {
		t.Fatalf("an item from no source is read in full: %+v", pl)
	}
	var in itemRead
	if read(internal, "/v1/items/"+it.ID, &in) != 200 || in.ContentScope != "" || in.Item.Body != long {
		t.Fatalf("internal key reads in full: %+v", in.ContentScope)
	}

	type itemList struct {
		Items         []domain.Item     `json:"items"`
		ContentScopes map[string]string `json:"content_scopes"`
	}
	ids := func(l itemList) (out []string) {
		for _, x := range l.Items {
			out = append(out, x.ID)
		}
		return
	}
	var nl, ml itemList
	read(none, "/v1/items", &nl)
	if !reflect.DeepEqual(ids(nl), []string{plain.ID}) {
		t.Fatalf("an unreachable item is left out of the list: %v", ids(nl))
	}
	read(meta, "/v1/items", &ml)
	if ml.ContentScopes[it.ID] != "meta" || len(ml.Items) != 2 || strings.Contains(getBody(k, meta, "/v1/items"), "BODYWORD") {
		t.Fatalf("list marks the trimmed item: %+v", ml.ContentScopes)
	}
	var sm, si itemList
	read(meta, "/v1/items?q=BODYWORD", &sm)
	read(internal, "/v1/items?q=BODYWORD", &si)
	if len(sm.Items) != 0 || len(si.Items) != 1 {
		t.Fatalf("an external key searches the trimmed content only: meta %v internal %v", ids(sm), ids(si))
	}
	day := time.Now().Format("2006-01-02")
	var bd struct {
		Progress map[string][]domain.Progress `json:"progress"`
	}
	read(meta, "/v1/items?progress_from="+day+"&progress_to="+day, &bd)
	if ps := bd.Progress[it.ID]; len(ps) != 1 || ps[0].Text != "" {
		t.Fatalf("progress by day follows the item's level: %+v", bd.Progress)
	}
	// 回放：元信息档只给信封（类型与时间线，不含载荷里的正文）；经过带进展正文，只在全文档位可读；不给的拒绝。
	if code, body := getAs(k, meta, "/v1/why/"+it.ID); code != 200 || !strings.Contains(body, `"steps"`) || strings.Contains(body, "BODYWORD") || strings.Contains(body, "进进进") {
		t.Fatalf("meta replay is envelope only: %d %.300s", code, body)
	}
	if code, _ := getAs(k, meta, "/v1/items/"+it.ID+"/trace"); code != 403 {
		t.Fatalf("trace below full is refused: %d", code)
	}
	if code, _ := getAs(k, none, "/v1/why/"+it.ID); code != 403 {
		t.Fatalf("replay of an unreachable item is refused: %d", code)
	}
	if code, _ := getAs(k, internal, "/v1/items/"+it.ID+"/trace"); code != 200 { // 内部钥匙按凭据级别，不看档位
		t.Fatalf("internal trace: %d", code)
	}
	if strings.Contains(getBody(k, none, "/v1/today"), it.ID) {
		t.Fatal("today must leave out an unreachable item")
	}

	type matRead struct {
		Revision     *struct{ Content string } `json:"revision"`
		ContentScope string                    `json:"content_scope"`
	}
	var mx, mm matRead
	if read(excerpt, "/v1/materials/"+mr.Material, &mx) != 200 || mx.ContentScope != "excerpt" || mx.Revision == nil || utf8.RuneCountInString(mx.Revision.Content) != 500 {
		t.Fatalf("material follows the strictest of its items: %+v", mx)
	}
	if read(meta, "/v1/materials/"+mr.Material, &mm) != 200 || mm.ContentScope != "meta" || mm.Revision == nil || mm.Revision.Content != "" {
		t.Fatalf("meta material has no content: %+v", mm)
	}
	if code, _ := getAs(k, none, "/v1/materials/"+mr.Material); code != 403 {
		t.Fatalf("unreachable material: %d", code)
	}
	for _, p := range []string{"/v1/materials", "/v1/materials/search?q=MATWORD", "/v1/materials/" + mr.Material + "/revisions"} {
		if b := getBody(k, none, p); strings.Contains(b, mr.Material) && !strings.Contains(b, "out_of_scope") {
			t.Fatalf("%s must not show the unreachable material: %s", p, b)
		}
	}
	if b := getBody(k, meta, "/v1/materials/search?q=MATWORD"); strings.Contains(b, mr.Material) {
		t.Fatalf("search must not reveal a material whose content the key cannot read: %s", b)
	}
	if b := getBody(k, internal, "/v1/materials/search?q=MATWORD"); !strings.Contains(b, mr.Material) {
		t.Fatalf("internal search finds it: %s", b)
	}
}

func getBody(k *kernel, key, path string) string {
	_, b := getAs(k, key, path)
	return b
}

// 一次性迁移（§16.5）：升级前的 token 按各来源今天的外发范围换算成它今天经 token 实际读到的档位（full、excerpt 照给，其余 meta），
// ext 写空表；audience 不动（外部）；记一条 credential.scopes_migrated；再跑一次什么都不变。
func TestMigrateKeyScopesKeepsWhatTokensReadToday(t *testing.T) {
	k := assembled(t)
	for id, ext := range map[string]any{"src_full": "full", "src_ex": "excerpt", "src_meta": "meta", "src_none": "none", "src_unset": nil} {
		registerSource(t, k, id)
		if ext != nil {
			k.do(t, command.Chain{}, "source.set_outbound", map[string]any{"id": id, "external": ext})
		}
	}
	ctx := context.Background()
	au := auth.New(nil)
	var tok, export auth.Credential
	if err := k.st.Update(ctx, func(tx *store.Tx) (err error) {
		if tok, _, err = au.Issue(tx, auth.Spec{Kind: auth.Delegate}); err != nil {
			return err
		}
		if export, _, err = au.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "export"}); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE auth_credential SET source_scopes = ''`) // 升级前的行
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := migrateKeyScopes(ctx, k.st, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := migrateKeyScopes(ctx, k.st, time.Now); err != nil { // 幂等
		t.Fatal(err)
	}
	want := map[string]string{"src_full": "full", "src_ex": "excerpt", "src_meta": "meta", "src_none": "meta", "src_unset": "meta"}
	if err := k.st.View(ctx, func(tx *store.Tx) error {
		got, err := au.Get(tx, tok.ID)
		if err != nil {
			return err
		}
		if got.Audience != auth.AudienceExternal || !reflect.DeepEqual(got.SourceScopes, want) {
			t.Errorf("token: %+v", got)
		}
		if got, err = au.Get(tx, export.ID); err != nil || len(got.SourceScopes) != 0 {
			t.Errorf("export ext gives nothing: %+v %v", got, err)
		}
		es, err := eventlog.ReadObject(tx, "settings:credentials")
		n := 0
		for _, e := range es {
			if e.Event.Type == EvScopesMigrated {
				n++
			}
		}
		if err != nil || n != 1 {
			t.Errorf("one migration event, got %d %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
