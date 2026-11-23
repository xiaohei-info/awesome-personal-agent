package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 假内核：两件迁移事项（m 缺完成日期、有出处；u 缺完成日期、没出处），记录收到的 PATCH。
func fake(t *testing.T, patches *[]string) *httptest.Server {
	completed := map[string]string{"m": "", "u": ""}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			b, _ := io.ReadAll(r.Body)
			id := strings.TrimPrefix(r.URL.Path, "/v1/items/")
			var body struct {
				Ext struct {
					Workbench map[string]string `json:"workbench"`
				} `json:"ext"`
			}
			if err := json.Unmarshal(b, &body); err != nil {
				t.Fatal(err)
			}
			completed[id] = body.Ext.Workbench["completed"]
			*patches = append(*patches, id+" "+r.Header.Get("If-Match")+" "+string(b))
			io.WriteString(w, `{}`)
			return
		}
		if r.URL.Path == "/v1/items" {
			io.WriteString(w, `{"done_unknown":["u"]}`)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/items/")
		io.WriteString(w, `{"item":{"version":7,"phase":"done","ext":{"workbench":{"owner":"x","completed":"`+completed[id]+`"},"keep":1}},"progress":[{"id":"m.p1","on":"2026-10-18"}]}`)
	}))
}

func TestBackfill(t *testing.T) {
	var patches []string
	srv := fake(t, &patches)
	defer srv.Close()
	dec := map[string]decision{"m": {Date: "2026-10-18", Progress: "m.p1"}, "u": {Unknown: true}}
	var out strings.Builder

	if err := run(srv.Client(), srv.URL, "t", dec, false, &out); err != nil || len(patches) != 0 {
		t.Fatalf("dry run: %v, patches %v", err, patches)
	}
	if err := run(srv.Client(), srv.URL, "t", dec, true, &out); err != nil {
		t.Fatal(err)
	}
	const want = `m "7" {"ext":{"keep":1,"workbench":{"completed":"2026-10-18","owner":"x"}},"reason":"补工作台迁移事项的完成日期，出处：进展 m.p1"}`
	if len(patches) != 1 || patches[0] != want {
		t.Fatalf("patches = %q", patches)
	}
	if err := run(srv.Client(), srv.URL, "t", dec, true, &out); err != nil || len(patches) != 1 {
		t.Fatalf("second run must write nothing: %v, patches %d", err, len(patches))
	}

	bad := map[string]decision{"u": {Date: "2026-10-18", Progress: "u.p9"}}
	if err := run(srv.Client(), srv.URL, "t", bad, true, &out); err == nil || !strings.Contains(err.Error(), "没有出处不写") || len(patches) != 1 {
		t.Fatalf("no evidence must refuse: %v", err)
	}
	if err := run(srv.Client(), srv.URL, "t", map[string]decision{"m": {Date: "2026-10-19", Progress: "m.p1"}}, true, &out); err == nil || !strings.Contains(err.Error(), "不覆盖") {
		t.Fatalf("must not overwrite: %v", err)
	}
}
