package webhook

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

// 内置包的评论映射：验签通过才算已核实；只取新建的评论，编辑不匹配。
func TestBuiltinCommentMappingVerifiesOnlySignedSender(t *testing.T) {
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	if _, err := ld.Load(); err != nil {
		t.Fatal(err)
	}
	p, ok := ld.Get("inbound-triage")
	if !ok {
		t.Fatal("inbound-triage not loaded")
	}
	var m packs.WebhookMapping
	for _, x := range p.WebhookMaps {
		if x.ID == "github-issue-comments" {
			m = x
		}
	}
	body := []byte(`{"action":"created","issue":{"number":7,"title":"bug"},"comment":{"body":"looks good","html_url":"https://github.com/o/r/issues/7#issuecomment-1","created_at":"2026-11-01T00:00:00Z"},"repository":{"full_name":"o/r"},"sender":{"login":"bob"}}`)
	header := map[string]string{"x-github-event": "issue_comment", "x-github-delivery": "d1"} // 调用方交来的请求头键已小写
	c := newExprCtx(header, body)
	if ok, err := c.matches(m.Match); !ok || err != nil {
		t.Fatalf("created comment should match: %v %v", ok, err)
	}
	for _, signed := range []bool{true, false} {
		ev, err := mapEvent(m, c, body, "k", time.Now(), signed)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type != "issue.commented" || ev.Sender != "bob" || ev.SenderVerified != signed || len(ev.CorrelationKeys) != 1 || ev.CorrelationKeys[0] != "o/r#7" {
			t.Errorf("signed=%v: %+v", signed, ev)
		}
	}
	edited := newExprCtx(header, []byte(`{"action":"edited"}`))
	if ok, _ := edited.matches(m.Match); ok {
		t.Error("edited comment must not match")
	}
}
