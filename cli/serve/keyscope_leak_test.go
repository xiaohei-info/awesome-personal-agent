package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kmaterials "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 暗号：只出现在「不给」的来源里——来源事件正文与摘要、出自它的事项正文与进展、与该事项相连的材料正文。
var leakMarks = []string{"LEAKEVTBODY", "LEAKEVTSUM", "LEAKITEMBODY", "LEAKITEMSUM", "LEAKPROGRESS", "LEAKMATERIAL"}

type leakSeed struct {
	event, item, material, source string
}

// seedHidden 在完整装配的内核上种一个来源与它的内容：事件挂在出自该来源的事项上，事项连着一份材料。
func seedHidden(t *testing.T, k *kernel) leakSeed {
	t.Helper()
	s := leakSeed{source: "src_hidden"}
	k.do(t, command.Chain{}, "source.register", map[string]any{"id": s.source, "name": "隐藏来源", "type": "webhook", "source_type": "mail", "config": map[string]any{"scheme": "shared"}})
	var it struct{ ID string }
	_ = json.Unmarshal(k.do(t, command.Chain{}, "item.create", map[string]any{"kind": "task", "title": "可以看到的标题", "phase": "open",
		"body": "LEAKITEMBODY 正文", "summary": "LEAKITEMSUM", "origin": map[string]any{"source": s.source, "ref": "r1"}}), &it)
	s.item = it.ID
	k.do(t, command.Chain{}, "item.progress", map[string]any{"id": s.item, "text": "LEAKPROGRESS 进展"})
	var pr struct{ ID string }
	_ = json.Unmarshal(k.do(t, command.Chain{}, "project.create", map[string]any{"name": "P"}), &pr)
	var mr kmaterials.Result
	_ = json.Unmarshal(k.do(t, command.Chain{}, "material.commit", map[string]any{"project": pr.ID, "kind": "minutes", "title": "材料标题", "content": "LEAKMATERIAL 内容", "reason": "r"}), &mr)
	s.material = mr.Material
	k.do(t, command.Chain{}, "item.relate", map[string]any{"from": s.item, "to": s.material, "type": domain.RelRelates})
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: s.source}, domain.SourceEvent{ID: "ev-hidden", Type: "mail.received", Subject: "事件标题",
			Summary: "LEAKEVTSUM", Detail: "LEAKEVTBODY " + strings.Repeat("正文", 50), Sender: "alice", OccurredAt: time.Now()}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, s.source)
		if err != nil {
			return err
		}
		s.event = evs[0].ID
		return runner.SetDisposition(tx, s.event, domain.DispositionAttached, s.item, "")
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func issue(t *testing.T, k *kernel, spec auth.Spec) string {
	t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) (err error) { _, secret, err = auth.New(nil).Issue(tx, spec); return }); err != nil {
		t.Fatal(err)
	}
	return secret
}

func getAs(k *kernel, secret, path string) (int, string) {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	k.srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// 外部钥匙没被给的来源，它的内容在任何 GET 路由里都不出现：按附录 B.2 把每条 GET 路由都打一遍，
// 路径参数依次换成事件、事项、材料、来源的 id。内部钥匙在同一批路由上读得到（证明种子确实可见、测试不是空转）。
func TestExternalKeyNeverSeesContentOfASourceItWasNotGiven(t *testing.T) {
	k := assembled(t)
	s := seedHidden(t, k)
	keys := map[string]string{
		"external delegate": issue(t, k, auth.Spec{Kind: auth.Delegate, Audience: auth.AudienceExternal}),
		"external observer": issue(t, k, auth.Spec{Kind: auth.Observer, SourceScopes: map[string]string{"src_other": auth.ScopeFull}}),
		"export ext":        issue(t, k, auth.Spec{Kind: auth.Ext, Scope: "export"}),
	}
	internal := issue(t, k, auth.Spec{Kind: auth.Delegate, Audience: auth.AudienceInternal})
	placeholder := regexp.MustCompile(`\{[^}]*\}`)
	seen := map[string]bool{}
	var leaks []string
	for _, r := range appendixRoutes(t) {
		if !slices.Contains(r.methods, "GET") && len(r.methods) > 0 {
			continue
		}
		if strings.HasPrefix(r.path, "/ingest/") || strings.HasPrefix(r.path, "/forms/") {
			continue
		}
		for _, id := range []string{s.event, s.item, s.material, s.source} {
			p := "/v1" + placeholder.ReplaceAllString(r.path, id)
			if seen[p] {
				continue
			}
			seen[p] = true
			for name, key := range keys {
				_, body := getAs(k, key, p)
				for _, m := range leakMarks {
					if strings.Contains(body, m) {
						leaks = append(leaks, name+" GET "+p+" → "+m)
					}
				}
			}
		}
	}
	if len(leaks) > 0 {
		t.Fatalf("content of a source the key was not given leaked:\n%s", strings.Join(leaks, "\n"))
	}
	for _, c := range []struct{ path, mark string }{
		{"/v1/items/" + s.item, "LEAKITEMBODY"}, {"/v1/items/" + s.item, "LEAKPROGRESS"}, {"/v1/materials/" + s.material, "LEAKMATERIAL"},
		{"/v1/source-events/" + s.event, "LEAKEVTBODY"}, {"/v1/items/" + s.item + "/messages", "LEAKEVTBODY"},
	} {
		if code, body := getAs(k, internal, c.path); code != 200 || !strings.Contains(body, c.mark) {
			t.Errorf("internal key must read %s in full (%s): %d %s", c.path, c.mark, code, body)
		}
	}
}
