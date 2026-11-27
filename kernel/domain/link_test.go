package domain

import "testing"

func TestLinkRoundTrip(t *testing.T) {
	s := "- " + MDLink("评审 [v2]\n稿", "https://apa.example/items/i1") + " —— 开始 [资料](x)"
	if got, want := StripLinks(s), "- 评审 【v2】 稿 —— 开始 [资料](x)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
