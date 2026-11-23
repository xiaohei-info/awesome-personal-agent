package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ledger 用内核的真实迁移建一份账本（表结构与线上同源），再按列直接放入几封邮件事件与一条建议。
func ledger(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, items.Module, registry.Module, runner.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	const pr = "https://github.com/o/r/pull/226"
	foot := "\nReply to this email directly, view it on GitHub:\n" + pr + "#issuecomment-1\n"
	add := func(tx *store.Tx, id, sender, eventID, keys, link, body string) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified) VALUES ('src_mail', ?, ?, 'src_mail', 'run_1', 'mail.received', 'Re: [o/r] x (PR #226)', '', ?, ?, 0, ?, ?, 0)`,
			eventID, id, body, link, keys, sender)
		return err
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		for _, e := range [][6]string{
			{"sev_body", "GitHub <notifications@github.com>", "m1", "[]", "", "评审意见" + foot},                              // 正文页脚：补
			{"sev_thread", "notifications@github.com", "<o/r/pull/226/review/7@github.com>", "[]", "", "无页脚"},             // 线程头：补
			{"sev_ci", "notifications@github.com", "m3", "[]", "", "Run failed\nhttps://github.com/o/r/actions/runs/9\n"}, // 解析不到：不写
			{"sev_title", "notifications@github.com", "m4", "[]", "", "标题里写着 PR #226，正文什么都没有"},                            // 只有标题：不写
			{"sev_other", "someone@example.com", "m5", "[]", "", "转发" + foot},                                             // 不是通知地址：不写
			{"sev_linked", "notifications@github.com", "m6", "[]", "https://github.com/o/r/pull/1", "x" + foot},           // 已有链接：不动
		} {
			if err := add(tx, e[0], e[1], e[2], e[3], e[4], e[5]); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO items (id, phase, kind, project, assignee, data) VALUES ('itm_p', 'proposed', 'task', '', '', '{"proposal":{"sources":["sev_body"]}}')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "ledger.db")
}

func open(t *testing.T, path string, ro bool) *sql.DB {
	t.Helper()
	q := url.Values{}
	if ro {
		q.Set("mode", "ro")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func links(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id, link FROM sources_runner_event`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, l string
		if err := rows.Scan(&id, &l); err != nil {
			t.Fatal(err)
		}
		out[id] = l
	}
	return out
}

// 演练不写；写入只补取得到的两封，解析不到的、只有标题的、不是通知地址的、已有链接的都不动；跑第二遍 0 变更。
func TestBackfillDryRunApplyAndIdempotent(t *testing.T) {
	path := ledger(t)
	before := links(t, open(t, path, true))

	var out bytes.Buffer
	s, err := run(context.Background(), open(t, path, true), false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Summary{Scanned: 5, ByThread: 1, ByBody: 1, NoLink: 3, Proposals: 1, DryRun: true}); s != want {
		t.Fatalf("dry run: %+v, want %+v", s, want)
	}
	if got := out.String(); !strings.Contains(got, `{"event":"sev_body","link":"https://github.com/o/r/pull/226","by":"body"}`) ||
		!strings.Contains(got, `{"event":"sev_thread","link":"https://github.com/o/r/pull/226","by":"thread"}`) || strings.Count(got, "\n") != 3 {
		t.Fatalf("dry run output:\n%s", got)
	}
	if after := links(t, open(t, path, true)); !equal(after, before) {
		t.Fatalf("a dry run wrote: %v", after)
	}

	db := open(t, path, false)
	s, err = run(context.Background(), db, true, &out)
	if err != nil || s.Applied != 2 || s.ByBody != 1 || s.ByThread != 1 {
		t.Fatalf("apply: %+v %v", s, err)
	}
	got := links(t, db)
	want := map[string]string{"sev_body": "https://github.com/o/r/pull/226", "sev_thread": "https://github.com/o/r/pull/226",
		"sev_ci": "", "sev_title": "", "sev_other": "", "sev_linked": "https://github.com/o/r/pull/1"}
	if !equal(got, want) {
		t.Fatalf("links after apply: %v", got)
	}
	s, err = run(context.Background(), db, true, &out)
	if want := (Summary{Scanned: 3, NoLink: 3}); err != nil || s != want {
		t.Fatalf("second run must change nothing: %+v %v", s, err)
	}
	if !equal(links(t, db), want) {
		t.Fatal("second run changed links")
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
