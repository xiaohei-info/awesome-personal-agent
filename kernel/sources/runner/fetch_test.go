package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func fetchSource(t *testing.T) (*env, domain.Source, domain.SourceEvent) {
	t.Helper()
	e := setup(t, defaults())
	pull(e, "src_f", `cat >/dev/null
echo '{"id":"m-1","type":"mail.received","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"d","attachments":[{"name":"a.pdf","size":10,"ref":"h1"},{"name":"b.png","size":2,"inline":true}]}'
echo '{"cursor":"c9"}'
`)
	s, _ := e.source("src_f")
	evs := e.events("src_f")
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	return e, s, evs[0]
}

// fetch 模式的运行计划：程序名加 fetch 为唯一参数；stdin 是 {source_id, config, event_id, attachment}，event_id 是来源程序自己的事件 id；
// 超时 60 秒，stdout 限 32 MiB 且按字节保留；走本人显式触发的短队列，Target 带 fetch: 前缀，不是拉取。
func TestFetchPlanIsFetchModeOfTheSourceProgram(t *testing.T) {
	e, s, ev := fetchSource(t)
	req, err := e.r.FetchPlan(s, ev, 0, AttachmentRef{Name: "a.pdf", Ref: "h1"}, e.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Spec.Command) != 2 || req.Spec.Command[1] != "fetch" || req.Run.TimeoutSec != 60 || req.Spec.StdoutLimit != 32<<20 || !req.Explicit ||
		req.Run.Category != domain.RunSource || !strings.HasPrefix(req.Run.Target, FetchPrefix) {
		t.Fatalf("plan: %+v", req)
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(req.Spec.Stdin, &in); err != nil || string(in["event_id"]) != `"m-1"` || string(in["source_id"]) != `"src_f"` ||
		string(in["attachment"]) != `{"ref":"h1","name":"a.pdf"}` || in["config"] == nil {
		t.Fatalf("stdin: %s", req.Spec.Stdin)
	}
	if _, err := e.r.FetchPlan(s, ev, 1, AttachmentRef{Name: "b.png"}, e.Now()); err == nil {
		t.Fatal("an attachment without a handle cannot be fetched")
	}
}

// fetch 模式的运行结果不是拉取结果：Handle 忽略它，游标、事件与来源的失败计数都不动。
func TestFetchRunResultIsNotAPullResult(t *testing.T) {
	e, s, ev := fetchSource(t)
	_, before := e.source("src_f")
	req, err := e.r.FetchPlan(s, ev, 0, AttachmentRef{Name: "a.pdf", Ref: "h1"}, e.Now())
	if err != nil {
		t.Fatal(err)
	}
	exit := 3
	run := req.Run
	run.State, run.ExitCode = domain.RunFailed, &exit
	if err := e.r.Handle(bg, proc.Result{Run: run, Stdout: []byte(`{"id":"x","type":"t"}` + "\n"), Reason: "exit status 3"}); err != nil {
		t.Fatal(err)
	}
	src, after := e.source("src_f")
	if src.Cursor != "c9" || after != before || !src.Enabled || len(e.events("src_f")) != 1 {
		t.Fatalf("a fetch result must not touch the source: cursor %q enabled %v stats %+v→%+v", src.Cursor, src.Enabled, before, after)
	}
}

// 记 blob：清单项的其余字段原样保留，原文部分与事件行的字节数同步；越界与没有清单是 not_found。
func TestSetAttachmentBlob(t *testing.T) {
	e, _, ev := fetchSource(t)
	hash := strings.Repeat("a", 64)
	before := e.manifest(ev.ID)
	var item json.RawMessage
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { item, err = SetAttachmentBlob(tx, ev.ID, 0, hash); return }); err != nil {
		t.Fatal(err)
	}
	if string(item) != `{"blob":"apa://blob/`+hash+`","name":"a.pdf","ref":"h1","size":10}` {
		t.Fatalf("item: %s", item)
	}
	after := e.manifest(ev.ID)
	pd, _ := e.part(ev.ID, PartAttachments)
	if after.Bytes != before.Bytes+int64(len(`,"blob":"apa://blob/`+hash+`"`)) || pd.Bytes != int64(len(pd.Content)) || pd.SHA256 != sum(string(pd.Content)) {
		t.Fatalf("bytes: %d → %d, part %+v", before.Bytes, after.Bytes, pd.PartInfo)
	}
	if list, err := e.attachments(ev.ID); err != nil || len(list) != 2 || list[0].Blob != "apa://blob/"+hash || list[1].Blob != "" {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, n := range []int{-1, 2} {
		err := e.st.Update(bg, func(tx *store.Tx) error { _, err := SetAttachmentBlob(tx, ev.ID, n, hash); return err })
		if de, ok := err.(*domain.Error); !ok || de.Code != domain.CodeNotFound {
			t.Errorf("n=%d: %v", n, err)
		}
	}
	err := e.st.Update(bg, func(tx *store.Tx) error { _, err := SetAttachmentBlob(tx, "sev_nope", 0, hash); return err })
	if de, ok := err.(*domain.Error); !ok || de.Code != domain.CodeNotFound {
		t.Errorf("unknown event: %v", err)
	}
}

func (e *env) attachments(id string) (l []AttachmentRef, err error) {
	err = e.st.View(bg, func(tx *store.Tx) (err error) { l, err = EventAttachments(tx, id); return })
	return
}
