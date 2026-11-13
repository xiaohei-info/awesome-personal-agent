package delivery

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func TestBriefContentSummarizesSectionsWithoutBody(t *testing.T) {
	spec := json.RawMessage(`{"period":"2026-11-09 08:00","sections":[{"project":"alpha","body":"SECRET-BODY"},{"project":"知会","body":"x"}]}`)
	c := BriefContent(domain.Item{ID: "i1", Title: "每日简报", Spec: spec}, "/items/i1")
	if c.Title != "每日简报" || c.WebURL != "/items/i1" {
		t.Fatalf("title/url: %+v", c)
	}
	if c.Summary != "按项目分节：alpha、知会" {
		t.Fatalf("summary: %q", c.Summary)
	}
}

func TestBriefContentPrefersItemSummaryAndToleratesOddSpec(t *testing.T) {
	c := BriefContent(domain.Item{ID: "i1", Title: "t", Summary: "一句话", Spec: json.RawMessage(`{"sections":[{"project":"a"}]}`)}, "")
	if c.Summary != "一句话" {
		t.Fatalf("summary: %q", c.Summary)
	}
	if c := BriefContent(domain.Item{ID: "i1", Title: "t", Spec: json.RawMessage(`not json`)}, ""); c.Summary != "" || c.Title != "t" {
		t.Fatalf("odd spec: %+v", c)
	}
}

func TestBriefIDIsPerItem(t *testing.T) {
	if BriefID("a") == BriefID("b") || BriefID("a") != BriefID("a") {
		t.Fatal("brief delivery id must be a stable function of the item")
	}
}
