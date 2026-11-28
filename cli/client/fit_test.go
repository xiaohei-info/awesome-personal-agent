package client

import (
	"strconv"
	"strings"
	"testing"
)

// Fit 把续页游标与"下一步"一起量进上限：补写说明行之后整份仍不超过上限，删掉的行由游标接上。
func TestFitCountsTheHintsItAdds(t *testing.T) {
	var rows []any
	for i := range 40 {
		rows = append(rows, map[string]any{"id": "itm_" + strconv.Itoa(i), "title": strings.Repeat("长", 90)})
	}
	out := map[string]any{"items": rows}
	Fit(out, "items", 4000, func(kept int) {
		out["next_cursor"] = strconv.Itoa(kept)
		out["next"] = []string{strings.Repeat("apa item list --cursor ", 8) + strconv.Itoa(kept), "apa search <words>"}
	})
	kept := len(out["items"].([]any))
	if n := Size(out); n > 4000 || kept == 0 || kept == 40 || out["next_cursor"] != strconv.Itoa(kept) {
		t.Fatalf("size %d, kept %d, cursor %v", n, kept, out["next_cursor"])
	}
}

func TestQuote(t *testing.T) {
	if got := Quote("it's 北辰"); got != `'it'\''s 北辰'` {
		t.Fatalf("quote: %s", got)
	}
}
