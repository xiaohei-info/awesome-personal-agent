// Command mailbackfill 给已入账的代码托管平台通知邮件回填它所说对象的规范链接（附录 D.2「通知邮件的对象链接」）。
// 新到的通知邮件由邮件适配器在拉取时填 link；本程序只补升级前入账、link 还空着的那些，取法与适配器同一处
// （content.NotificationLink 按线程头，content.NotificationBodyLink 按正文页脚），取不到的不写，不看标题。
//
// 一次性、幂等、可重复执行：只改 type 为 mail.received 且 link 为空的来源事件，写过的下一轮就不在候选里；
// 只改 link 这一列，不碰正文、合并键、建议与事件日志。默认只演练（只读打开），加 -apply 才在一个事务里写入。
//
//	go run ./sources/internal/mailbackfill -ledger <账本副本或 APA_HOME/ledger.db> [-apply]
//
// 输出是 JSON 行：每条要补的 {"event","link","by"}，最后一行 {"summary":{…}}。写入前先 `apa backup create`。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/sources/internal/content"
	_ "modernc.org/sqlite"
)

// Fix 是一条要补的链接；By 是取法：thread（线程头里的 Message-ID 写法）或 body（正文页脚）。
type Fix struct {
	Event string `json:"event"`
	Link  string `json:"link"`
	By    string `json:"by"`
}

// Summary 是一轮的统计：看过几封、按哪种取法补了几封、取不到的几封、涉及几条仍在建议里的事项、实际写入几行。
type Summary struct {
	Scanned   int  `json:"scanned"`
	ByThread  int  `json:"by_thread"`
	ByBody    int  `json:"by_body"`
	NoLink    int  `json:"no_link"`
	Proposals int  `json:"proposals"`
	Applied   int  `json:"applied"`
	DryRun    bool `json:"dry_run"`
}

func main() {
	ledger := flag.String("ledger", "", "path to ledger.db")
	apply := flag.Bool("apply", false, "write the links (default: dry run, read only)")
	flag.Parse()
	if *ledger == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: mailbackfill -ledger <ledger.db> [-apply]")
		os.Exit(2)
	}
	if _, err := os.Stat(*ledger); err != nil {
		fmt.Fprintln(os.Stderr, "mailbackfill:", err)
		os.Exit(2)
	}
	q := url.Values{"_pragma": {"busy_timeout(10000)"}}
	if !*apply {
		q.Set("mode", "ro")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: *ledger, RawQuery: q.Encode()}).String())
	if err == nil {
		db.SetMaxOpenConns(1)
		_, err = run(context.Background(), db, *apply, os.Stdout)
		db.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mailbackfill:", err)
		os.Exit(1)
	}
}

// run 找出要补的链接并按 apply 写入；写入在一个事务里，条件仍带 link = ”，与别处同时补过的不会被覆盖。
func run(ctx context.Context, db *sql.DB, apply bool, w io.Writer) (Summary, error) {
	s := Summary{DryRun: !apply}
	rows, err := db.QueryContext(ctx, `SELECT id, sender, event_id, correlation_keys,
		COALESCE((SELECT CAST(content AS TEXT) FROM sources_runner_event_part p WHERE p.event_id = e.id AND p.part = 'body'), detail)
		FROM sources_runner_event e WHERE type = 'mail.received' AND link = '' ORDER BY seq`)
	if err != nil {
		return s, fmt.Errorf("read source events (is this an APA ledger?): %w", err)
	}
	var fixes []Fix
	for rows.Next() {
		var id, sender, eventID, keys, body string
		if err := rows.Scan(&id, &sender, &eventID, &keys, &body); err != nil {
			rows.Close()
			return s, err
		}
		s.Scanned++
		ids := []string{eventID}
		var ks []string
		_ = json.Unmarshal([]byte(keys), &ks) // 入账时校验过的数组；读不出按没有线程头
		for _, k := range ks {
			ids = append(ids, strings.TrimPrefix(k, "thread:"))
		}
		f := Fix{Event: id, Link: content.NotificationLink(sender, ids...), By: "thread"}
		if f.Link == "" {
			f.Link, f.By = content.NotificationBodyLink(sender, body), "body"
		}
		switch {
		case f.Link == "":
			s.NoLink++
			continue
		case f.By == "thread":
			s.ByThread++
		default:
			s.ByBody++
		}
		fixes = append(fixes, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}
	if s.Proposals, err = proposals(ctx, db, fixes); err != nil {
		return s, err
	}
	enc := json.NewEncoder(w)
	for _, f := range fixes {
		if err := enc.Encode(f); err != nil {
			return s, err
		}
	}
	if apply && len(fixes) > 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return s, err
		}
		for _, f := range fixes {
			r, err := tx.ExecContext(ctx, `UPDATE sources_runner_event SET link = ? WHERE id = ? AND link = ''`, f.Link, f.Event)
			if err != nil {
				tx.Rollback()
				return s, err
			}
			n, _ := r.RowsAffected()
			s.Applied += int(n)
		}
		if err := tx.Commit(); err != nil {
			return s, err
		}
	}
	return s, enc.Encode(map[string]Summary{"summary": s})
}

// proposals 数仍在建议里、来源事件里有要补的那几封的事项。
func proposals(ctx context.Context, db *sql.DB, fixes []Fix) (int, error) {
	want := map[string]bool{}
	for _, f := range fixes {
		want[f.Event] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT data FROM items WHERE phase = 'proposed'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return 0, err
		}
		var it struct {
			Proposal struct {
				Sources []string `json:"sources"`
			} `json:"proposal"`
		}
		_ = json.Unmarshal([]byte(data), &it)
		for _, id := range it.Proposal.Sources {
			if want[id] {
				n++
				break
			}
		}
	}
	return n, rows.Err()
}
