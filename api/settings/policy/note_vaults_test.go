package policy

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// ACT-LNK-03/04：笔记库登记只在 Web，整体替换，记 policy.changed；坏模板与重名被拒且不写版本。
func TestNoteVaultsSettings(t *testing.T) {
	e := setup(t)
	const path = "/v1/settings/note-vaults"

	rec := e.do("GET", path, "", e.web)
	var got struct {
		Version int                 `json:"version"`
		Vaults  []kpolicy.NoteVault `json:"vaults"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Vaults == nil || len(got.Vaults) != 0 {
		t.Fatalf("empty GET: %d %s", rec.Code, rec.Body)
	}

	put := `{"vaults":[{"name":"notes","open_url":"obsidian://open?vault={vault}&file={path}"},{"name":"wiki","open_url":"https://wiki.example/{vault}/{path}"}]}`
	if rec = e.do("PUT", path, put, e.web); rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	v, p := e.current()
	if v != 1 || len(p.NoteVaults) != 2 || p.NoteVaults[1].Name != "wiki" {
		t.Fatalf("stored: v%d %+v", v, p.NoteVaults)
	}
	if types := e.eventTypes(); len(types) != 1 || types[0] != domain.EvPolicyChanged {
		t.Fatalf("events = %v", types)
	}

	// 整体替换：只给一个库，另一个即被移除。
	if rec = e.do("PUT", path, `{"vaults":[{"name":"notes","open_url":"obsidian://open?vault={vault}&file={path}"}]}`, e.web); rec.Code != 200 {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	if _, p = e.current(); len(p.NoteVaults) != 1 || p.NoteVaults[0].Name != "notes" {
		t.Fatalf("not replaced: %+v", p.NoteVaults)
	}
	// 同样的内容再存一次：不产生版本。
	before, _ := e.current()
	e.do("PUT", path, `{"vaults":[{"name":"notes","open_url":"obsidian://open?vault={vault}&file={path}"}]}`, e.web)
	if after, _ := e.current(); after != before {
		t.Fatalf("an unchanged save wrote version %d", after)
	}
	// 清空登记。
	if rec = e.do("PUT", path, `{"vaults":[]}`, e.web); rec.Code != 200 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if _, p = e.current(); len(p.NoteVaults) != 0 {
		t.Fatalf("not cleared: %+v", p.NoteVaults)
	}

	before, _ = e.current()
	for name, body := range map[string]string{
		"duplicate":   `{"vaults":[{"name":"a","open_url":"x://a"},{"name":"a","open_url":"x://b"}]}`,
		"no protocol": `{"vaults":[{"name":"a","open_url":"{vault}/{path}"}]}`,
		"javascript":  `{"vaults":[{"name":"a","open_url":"javascript:alert(1)"}]}`,
		"data":        `{"vaults":[{"name":"a","open_url":"data:text/html,x"}]}`,
		"vbscript":    `{"vaults":[{"name":"a","open_url":"vbscript:x"}]}`,
		"file":        `{"vaults":[{"name":"a","open_url":"file:///etc/passwd"}]}`,
		"unknown key": `{"vaults":[{"name":"a","open_url":"x://a","extra":1}]}`,
		"other route": `{"background_run_timeout_sec":1800}`,
	} {
		rec = e.do("PUT", path, body, e.web)
		if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if after, _ := e.current(); after != before {
		t.Fatalf("rejected saves wrote version %d", after)
	}
}
