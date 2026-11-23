package task

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
)

// 目录给出每段的条数、条目位置、时间范围、并列的总数、字段名与一条样例；分组的条目展开计数，不是 JSON 的段只给名字与字节数。
func TestCatalogDescribesEachMaterial(t *testing.T) {
	long := strings.Repeat("长", 500)
	mats := []Material{
		{Name: "activity", Body: `{"activity":[{"project":"p","entries":[{"text":"` + long + `","on":"2026-11-20","at":"2026-11-22T23:00:00+08:00"},{"text":"b","at":"2026-11-22T09:00:00+08:00"}]},{"project":"q","entries":[{"text":"c","on":"2026-11-21"}]}]}`},
		{Name: "proposals", Body: `{"pending_total":232,"not_listed":200,"proposals":[{"id":"x","created_at":"2026-11-22T10:00:00+08:00"}]}`},
		{Name: "open_items", Body: `{"open_items":{"total":1,"items":[{"title":"t","updated_at":"2026-11-01T00:00:00Z"}]}}`},
		{Name: "context", Body: `{"context":{"now":"n","sources":[]}}`},
		{Name: "instance_params", Body: `not json`},
	}
	got := map[string]catalogEntry{}
	for _, c := range catalogOf(mats) {
		got[c.Name] = c
	}
	a := got["activity"]
	if a.Entries == nil || *a.Entries != 3 || a.List != "activity[].entries[]" || a.TimeFrom != "2026-11-20" || a.TimeTo != "2026-11-22T09:00:00+08:00" ||
		a.Bytes != len(mats[0].Body) || strings.Join(a.Fields, ",") != "at,on,text" || len(a.Sample) > sampleMaxBytes+200 {
		t.Fatalf("activity: %+v %s", a, a.Sample)
	}
	if p := got["proposals"]; p.Totals["pending_total"] != float64(232) || p.Totals["not_listed"] != float64(200) || *p.Entries != 1 || p.TimeFrom == "" {
		t.Fatalf("proposals: %+v", p)
	}
	if o := got["open_items"]; o.List != "open_items.items[]" || *o.Entries != 1 || o.Totals != nil {
		t.Fatalf("open_items: %+v", o)
	}
	if c := got["context"]; c.Entries != nil || strings.Join(c.Fields, ",") != "now,sources" {
		t.Fatalf("context: %+v", c)
	}
	if i := got["instance_params"]; i.Bytes != 8 || i.Fields != nil {
		t.Fatalf("non-JSON body: %+v", i)
	}
	if _, err := json.Marshal(catalogOf(mats)); err != nil {
		t.Fatal(err)
	}
}

// output.txt 只留末段时第一行写明截断量；没截断时原样。
func TestOutputFileMarksDroppedHead(t *testing.T) {
	if got := string(outputFile(proc.Result{Stdout: []byte("a\n")})); got != "a\n" {
		t.Fatalf("untruncated: %q", got)
	}
	got := outputFile(proc.Result{Stdout: []byte("tail\n"), StdoutTotal: 105, StdoutDropped: 100})
	first, rest, _ := strings.Cut(string(got), "\n")
	var h struct {
		Type    string `json:"type"`
		Total   int64  `json:"total_bytes"`
		Kept    int    `json:"kept_bytes"`
		Dropped int64  `json:"dropped_head_bytes"`
	}
	if err := json.Unmarshal([]byte(first), &h); err != nil || h.Type != "apa_output_truncated" || h.Total != 105 || h.Kept != 5 || h.Dropped != 100 || rest != "tail\n" {
		t.Fatalf("%v %+v %q", err, h, got)
	}
}
