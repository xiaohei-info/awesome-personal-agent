package rulejudge

import (
	"context"
	"errors"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
)

type fake struct {
	pack, tmpl, id string
	err            error
}

func (f *fake) JudgeItem(_ context.Context, pack, tmpl string, it domain.Item) error {
	f.pack, f.tmpl, f.id = pack, tmpl, it.ID
	return f.err
}

func TestHandlerRoutesRuleCallToJudgeItem(t *testing.T) {
	f := &fake{}
	c := rules.Call{Rule: "acc-relay/r1", Action: packs.Action{"prompt": "ss-judge"}, Item: &domain.Item{ID: "it_1"}}
	if err := Handler(f)(context.Background(), c); err != nil || f.pack != "acc-relay" || f.tmpl != "ss-judge" || f.id != "it_1" {
		t.Fatalf("%v %+v", err, f)
	}
	f.err = errors.New("boom")
	if err := Handler(f)(context.Background(), c); !errors.Is(err, f.err) {
		t.Fatalf("judge error must surface, got %v", err)
	}
}

func TestHandlerRejectsMissingTemplateOrItem(t *testing.T) {
	f := &fake{}
	if err := Handler(f)(context.Background(), rules.Call{Rule: "p/r", Action: packs.Action{}, Item: &domain.Item{ID: "x"}}); err == nil {
		t.Fatal("want error without template")
	}
	if err := Handler(f)(context.Background(), rules.Call{Rule: "p/r", Action: packs.Action{"prompt": "t"}}); err == nil {
		t.Fatal("want error without item")
	}
	if f.id != "" {
		t.Fatal("judge must not run")
	}
}
