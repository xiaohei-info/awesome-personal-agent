package task

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// rawSource 登记一个来源（external 为空表示外部范围未选择）并写入事件，返回事件 id（与传入顺序一致）。
func (e *env) rawSource(id, external string, evs ...domain.SourceEvent) []string {
	e.t.Helper()
	var ids []string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sources_registry_source
			(id, name, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
			VALUES (?, ?, 'script', 'mail', '', '', 0, '', '', '', 'full', ?, '', '', 1, '')`, id, "名-"+id, external); err != nil {
			return err
		}
		src := domain.Source{ID: id}
		for _, ev := range evs {
			if err := runner.AppendInternal(tx, src, ev); err != nil {
				return err
			}
		}
		got, err := runner.Events(tx, id)
		for _, g := range got {
			ids = append(ids, g.ID)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return ids
}

func (e *env) digest(ids ...string) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, id := range ids {
			if err := runner.SetDisposition(tx, id, domain.DispositionDigest, "", "test"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

type rawEntry struct {
	Title string `json:"title"`
	Raw   []struct {
		Event    string `json:"event"`
		File     string `json:"file"`
		Material struct {
			Scope    string `json:"scope"`
			Mode     string `json:"mode"`
			Sent     int    `json:"sent_chars"`
			Original int    `json:"original_chars"`
		} `json:"material"`
	} `json:"raw"`
}

func runBrief(e *env) (runDir string, m map[string]string) {
	e.t.Helper()
	e.harness("hx", "summary")
	e.setDefaultHarness("hx")
	id := e.openItemWithOrigin("instance:ins_1")
	if err := e.svc.CallAgent(AgentOptions{})(bg, e.call("brief", id)); err != nil {
		e.t.Fatal(err)
	}
	run := e.waitResult().Run.ID
	return e.runs + "/" + run, materialsOf(e, run)
}

// ACT-BRF-05/06：知会引用的来源事件按该来源对外部端点的外发范围写原文文件：全文写全文，摘录写摘录，其余不写。
func TestBriefRawFilesFollowOutboundScope(t *testing.T) {
	e := setup(t)
	long := strings.Repeat("长", 800)
	full := e.rawSource("src_full", "full", domain.SourceEvent{ID: "f1", Type: "mail", Subject: "全文邮件", Summary: "摘要F", Detail: "FULL-BODY-" + long, OccurredAt: t0})
	exc := e.rawSource("src_exc", "excerpt", domain.SourceEvent{ID: "x1", Type: "mail", Subject: "摘录邮件", Summary: "摘要X", Detail: "EXC-HEAD-" + long + "SECRET-TAIL", OccurredAt: t0})
	meta := e.rawSource("src_meta", "meta", domain.SourceEvent{ID: "m1", Type: "mail", Subject: "元信息邮件", Detail: "META-BODY", OccurredAt: t0})
	unset := e.rawSource("src_unset", "", domain.SourceEvent{ID: "u1", Type: "mail", Subject: "未选择邮件", Detail: "UNSET-BODY", OccurredAt: t0})
	e.digest(full[0], exc[0], meta[0], unset[0])

	dir, m := runBrief(e)
	var d struct {
		Digest []rawEntry `json:"digest"`
	}
	if err := json.Unmarshal([]byte(m["digest"]), &d); err != nil || len(d.Digest) != 4 {
		t.Fatalf("digest: %v %s", err, m["digest"])
	}
	by := map[string]rawEntry{}
	for _, en := range d.Digest {
		if len(en.Raw) != 1 {
			t.Fatalf("each digest entry names its source event: %+v", en)
		}
		by[en.Title] = en
	}
	read := func(title string) string {
		t.Helper()
		f := by[title].Raw[0].File
		if f == "" {
			t.Fatalf("%s must have a file", title)
		}
		st, err := os.Stat(dir + "/" + f)
		if err != nil || st.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s must be read-only: %v %v", f, err, st)
		}
		b, _ := os.ReadFile(dir + "/" + f)
		return string(b)
	}
	if txt := read("全文邮件"); !strings.Contains(txt, "FULL-BODY-") || !strings.Contains(txt, long) || by["全文邮件"].Raw[0].Material.Scope != "full" || by["全文邮件"].Raw[0].Material.Mode != "none" {
		t.Fatalf("full scope writes the whole body: %.200s %+v", txt, by["全文邮件"])
	}
	txt := read("摘录邮件")
	if !strings.Contains(txt, "EXC-HEAD-") || strings.Contains(txt, "SECRET-TAIL") || !strings.Contains(txt, "摘要X") {
		t.Fatalf("excerpt scope writes summary and the first 500 chars only: %.300s", txt)
	}
	for _, title := range []string{"元信息邮件", "未选择邮件"} {
		r := by[title].Raw[0]
		if r.File != "" || r.Material.Mode != rawModeNotWritten {
			t.Fatalf("%s: scope %q writes no file: %+v", title, r.Material.Scope, r)
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		if b, _ := os.ReadFile(dir + "/" + en.Name()); strings.Contains(string(b), "META-BODY") || strings.Contains(string(b), "UNSET-BODY") {
			t.Fatalf("bodies outside the outbound scope must not reach the run dir: %s", en.Name())
		}
	}
}

// ACT-BRF-06：单个文件不超过 20000 个字符，裁剪量写在文件头与条目的 material 里；账本里的原文不变。
func TestBriefRawFileTrimmedToFileBudget(t *testing.T) {
	e := setup(t)
	big := strings.Repeat("甲", 30000)
	ids := e.rawSource("src_big", "full", domain.SourceEvent{ID: "b1", Type: "mail", Subject: "长邮件", Detail: big, OccurredAt: t0})
	e.digest(ids[0])
	dir, m := runBrief(e)
	var d struct {
		Digest []rawEntry `json:"digest"`
	}
	_ = json.Unmarshal([]byte(m["digest"]), &d)
	r := d.Digest[0].Raw[0]
	b, err := os.ReadFile(dir + "/" + r.File)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(string(b))); n > maxRawFileChars || r.Material.Mode != "head_tail" || r.Material.Original < 30000 || r.Material.Sent >= r.Material.Original {
		t.Fatalf("file must stay within %d chars and record the trim: %d %+v", maxRawFileChars, n, r.Material)
	}
	if !strings.Contains(string(b), "省略") || !strings.Contains(string(b), "原文") {
		t.Fatalf("file header must state the trim: %.200s", b)
	}
	var stored string
	_ = e.st.View(bg, func(tx *store.Tx) error {
		p, err := runner.ReadPart(tx, ids[0], runner.PartBody)
		stored = string(p.Content)
		return err
	})
	if stored != big {
		t.Fatalf("ledger original must not be trimmed: %d", len(stored))
	}
}

// ACT-BRF-08：全部文件合计不超过 400000 个字符，超出的从最旧的条目起只留摘要；建议引用的来源事件同样出文件。
func TestBriefRawTotalBudgetDropsOldestAndCoversProposals(t *testing.T) {
	e := setup(t)
	body := strings.Repeat("乙", 19000)
	var evs []domain.SourceEvent
	for i := 0; i < 22; i++ { // 22 × 约 19000 = 超过 400000，最旧的几条只留摘要
		evs = append(evs, domain.SourceEvent{ID: "t" + string(rune('a'+i)), Type: "mail", Subject: "邮件" + string(rune('a'+i)), Detail: body, OccurredAt: t0.Add(time.Duration(i) * time.Minute)})
	}
	ids := e.rawSource("src_many", "full", evs...)
	e.digest(ids[:21]...)
	e.must(kernel, "item.create", map[string]any{"kind": "task", "title": "来自邮件的建议", "phase": "proposed",
		"proposal": map[string]any{"recommend": "accept", "reason": "r", "sources": []string{ids[21]}}})

	dir, m := runBrief(e)
	var d struct {
		Digest []rawEntry `json:"digest"`
	}
	var p struct {
		Proposals []rawEntry `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(m["digest"]), &d); err != nil || len(d.Digest) != 21 {
		t.Fatalf("digest: %v", err)
	}
	_ = json.Unmarshal([]byte(m["proposals"]), &p)
	if len(p.Proposals) != 1 || len(p.Proposals[0].Raw) != 1 || p.Proposals[0].Raw[0].File == "" {
		t.Fatalf("proposal must carry its source event file: %s", m["proposals"])
	}
	total, summaryOnly := 0, 0
	for _, en := range append(d.Digest, p.Proposals...) {
		r := en.Raw[0]
		if r.File == "" {
			if r.Material.Mode != rawModeSummaryOnly {
				t.Fatalf("an entry without a file must say why: %+v", r)
			}
			summaryOnly++
			continue
		}
		b, _ := os.ReadFile(dir + "/" + r.File)
		total += len([]rune(string(b)))
	}
	if total > maxRawTotalChars || summaryOnly == 0 {
		t.Fatalf("total %d (limit %d), summary-only %d", total, maxRawTotalChars, summaryOnly)
	}
	if d.Digest[0].Raw[0].File != "" || d.Digest[20].Raw[0].File == "" { // 最旧的先被收成摘要，最新的保住
		t.Fatalf("oldest must drop first: %+v / %+v", d.Digest[0].Raw[0], d.Digest[20].Raw[0])
	}
}
