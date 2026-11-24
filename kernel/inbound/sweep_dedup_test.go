package inbound

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 完全相同的正文只附一份并标注；成员独有的原文照附；去重后完整性校验照过（按成员逐个比）；第二遍零变化；撤销后每条都还原。
func TestMergeBodyDedupIdentical(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 3, true
	})
	list := "- PR #301：请评审，支付重试改造\n- PR #301：评审邮件，同一件事\n- 补充：复核编号 R55102"
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"PR 301","members":["N1","N2","N3"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR","confident":true},
		 {"new":"N3","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2","N3"],"title":"PR #301 评审与复核 R55102","summary":"PR #301 请评审，另有复核 R55102","body":`+fmt.Sprintf("%q", list)+`}`))
	same := "请评审 PR #301：支付重试改造。涉及 3 个文件，commit 9f3a1c2e，截止 2026-11-30。"
	uniq := "复核意见：编号 R55102，commit 7d41b0aa，需在 2026-12-02 前回复。"
	titles := []string{"PR #301 支付重试 平台通知", "PR #301 支付重试 评审邮件", "PR #301 复核 R55102"}
	for i, body := range []string{same, same, uniq} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", titles[i], map[string]any{"detail": body}))
	}
	n1, n2, n3 := e.byTitle(titles[0]), e.byTitle(titles[1]), e.byTitle(titles[2])
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Combined) != 1 || rep.Combined[0].Rejected != "" {
		t.Fatalf("run: %v %+v", err, rep)
	}
	body := e.item(n1.ID).Body
	note := fmt.Sprintf("（另有 1 条正文与此完全相同，不重复附上：【2/3】来源：%s · %s · 原题：「%s」）", sourceName(nil, originSource(n2)), n2.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), n2.Title)
	tail := fmt.Sprintf("【3/3】来源：%s · %s · 原题：「%s」\n%s", sourceName(nil, originSource(n3)), n3.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), n3.Title, uniq)
	switch {
	case strings.Count(body, same) != 1:
		t.Fatalf("identical body is attached once, got %d: %q", strings.Count(body, same), body)
	case !strings.Contains(body, note+"\n"+same):
		t.Fatalf("the kept copy says who else has it (source, time, title): want %q in %q", note, body)
	case !strings.Contains(body, tail) || strings.Contains(body, "【2/3】来源：") && strings.Count(body, "【2/3】") != 1:
		t.Fatalf("the member with its own text is attached in full: %q", body)
	}
	if ab := e.item(n1.ID).Proposal.Absorbed; len(ab) != 2 || ab[0].Body != same || ab[1].Body != same && ab[1].Body != uniq {
		t.Fatalf("absorbed still keeps every member's full body: %+v", ab)
	}
	e.verify()

	h, calls := e.head(), e.batchCalls()
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
		t.Fatalf("second pass changes nothing: %v %+v", err, rep)
	}
	if err := e.svc.Unmerge(bg, owner, n2.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []domain.Item{n1, n2, n3} {
		if back := e.item(m.ID); back.Body != m.Body || back.Title != m.Title || back.Phase != domain.PhaseProposed {
			t.Fatalf("undo restores %s", m.Title)
		}
	}
}

// 去重后原文校验不放宽：两条相同的长正文末尾有编号，被截掉、要点清单又没带上，两个成员都记缺失，整组退回。
func TestMergeBodyDedupStillChecksEveryMember(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting, p.Merge.BodyMaxBytes = policy.MergeSame, 3, true, policy.MinMergeBodyBytes
	})
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"账单","members":["N1","N2","N3"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"r","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"r","confident":true},
		 {"new":"N3","group":"G1","step":2,"action":"merge","target":"M1","reason":"r","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2","N3"],"title":"账单 B7701 三条通知","summary":"B7701","body":"- 账单 B7701 已出"}`))
	long := "账单 B7701 已出。" + strings.Repeat("这是一段很长的说明文字，没有任何编号。", 200) + "应还金额 Z99887766。"
	titles := []string{"账单 B7701（通知）", "账单 B7701（邮件）", "账单 B7701（短信）"}
	for i, body := range []string{long, long, "账单 B7701 已出，短信版。"} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", titles[i], map[string]any{"detail": body}))
	}
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Combined) != 1 {
		t.Fatalf("run: %v %+v", err, rep)
	}
	if mc := rep.Combined[0]; mc.Rejected != domain.RejectFactsMissing || strings.Join(mc.Missing, ",") != "原文：Z99887766,原文：Z99887766" {
		t.Fatalf("each member is checked on its own, the shared tail number is missing for both: %+v %+v", mc.Rejected, mc.Missing)
	}
	for _, title := range titles {
		if it := e.byTitle(title); it.Phase != domain.PhaseProposed || it.Proposal.Integrated != nil {
			t.Fatalf("%s stays as it was", title)
		}
	}
}

// 归一化后仍不同的一律保留：前缀、大小写不同、只差一个字、只差一个词，都各附各的，没有任何「完全相同」标注。
func TestIntegratedBodyNoFuzzyDedup(t *testing.T) {
	base := "请评审 PR #301。\n涉及 3 个文件，commit 9f3a1c2e。\n"
	bodies := []string{base, base[:len(base)/2], strings.ToUpper(base), strings.Replace(base, "3 个", "4 个", 1), strings.Replace(base, "请评审", "请复核", 1)}
	var set []*sweepRec
	for i, b := range bodies {
		set = append(set, &sweepRec{it: domain.Item{ID: fmt.Sprintf("it_%d", i), Title: fmt.Sprintf("t%d", i), Body: b}})
	}
	got := integratedBody("- 清单", set, set[0], nil, policy.DefaultMergeBodyBytes)
	if strings.Contains(got, "完全相同") {
		t.Fatalf("overlap is not identity: %q", got)
	}
	for i, b := range bodies {
		if !strings.Contains(got, fmt.Sprintf("【%d/5】", i+1)) || !strings.Contains(got, "原题：「"+set[i].it.Title+"」\n"+b) {
			t.Fatalf("member %d is attached on its own: %q", i+1, got)
		}
	}
}

// 轻量归一化只用于比较：只差 CRLF/CR、行尾空白、连续空行个数、首尾空白的判为相同只附一份，附上的是成员顺序在前那条的原始字节；只差一个词的仍各附各的。
func TestIntegratedBodyDedupNormalized(t *testing.T) {
	lf := "请评审 PR #301。\n涉及 3 个文件。\n\n截止 2026-11-30。"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	mk := func(bodies ...string) (set []*sweepRec) {
		for i, b := range bodies {
			set = append(set, &sweepRec{it: domain.Item{ID: fmt.Sprintf("it_%d", i), Title: fmt.Sprintf("t%d", i), Body: b}})
		}
		return
	}
	cases := []struct{ name, a, b string }{
		{"crlf", crlf, lf},
		{"cr", strings.ReplaceAll(lf, "\n", "\r"), lf},
		{"blank lines", strings.Replace(lf, "\n\n", "\n\n\n\n", 1), lf},
		{"trailing space and outer blanks", "\n  " + strings.ReplaceAll(lf, "\n", " \t\n") + "\n\n", "  " + lf},
	}
	for _, c := range cases {
		got := integratedBody("- 清单", mk(c.a, c.b), nil, nil, policy.DefaultMergeBodyBytes)
		if !strings.Contains(got, "（另有 1 条正文与此完全相同") || strings.Contains(got, "【2/2】来源：") && strings.Count(got, "【2/2】") != 1 {
			t.Fatalf("%s: judged identical, attached once: %q", c.name, got)
		}
		// 附上的是第一条的原始字节，不是归一化后的文本。
		if !strings.HasSuffix(got, "）\n"+c.a) {
			t.Fatalf("%s: raw bytes of the first member are attached: %q", c.name, got)
		}
	}
	if got := integratedBody("- 清单", mk(crlf, lf), nil, nil, policy.DefaultMergeBodyBytes); strings.Count(got, "\r\n") != strings.Count(crlf, "\r\n") {
		t.Fatalf("CRLF kept byte for byte: %q", got)
	}
	// 只差一个词：归一化后仍不同，两条都附。
	other := strings.Replace(crlf, "评审", "复核", 1)
	got := integratedBody("- 清单", mk(crlf, other), nil, nil, policy.DefaultMergeBodyBytes)
	if strings.Contains(got, "完全相同") || !strings.Contains(got, crlf) || !strings.Contains(got, other) {
		t.Fatalf("one word apart: both kept: %q", got)
	}
}

// 额度在去重之后分配：同样三条、同样上限，相同的两条合成一份后，留下的原文比「不完全相同」时保留得更多；多条相同按成员顺序留第一条并列出其余；结果不随调用次数变化。
func TestIntegratedBodyDedupQuotaAndIdempotent(t *testing.T) {
	x, y := strings.Repeat("甲", 1500), strings.Repeat("乙", 1500)
	mk := func(bodies ...string) (set []*sweepRec) {
		for i, b := range bodies {
			set = append(set, &sweepRec{it: domain.Item{ID: fmt.Sprintf("it_%d", i), Title: fmt.Sprintf("t%d", i), Body: b}})
		}
		return
	}
	kept := func(s string) int { return strings.Count(s, "甲") }
	dup, near := mk(x, x, y), mk(x, x+"。", y)
	a := integratedBody("- 清单", dup, dup[0], nil, policy.MinMergeBodyBytes)
	b := integratedBody("- 清单", near, near[0], nil, policy.MinMergeBodyBytes)
	if len(a) > policy.MinMergeBodyBytes || len(b) > policy.MinMergeBodyBytes || strings.Count(a, "完全相同") != 1 || strings.Contains(b, "完全相同") {
		t.Fatalf("caps hold and only the identical pair is marked: %d %d", len(a), len(b))
	}
	if strings.Count(a, "原文截断") != 2 || strings.Count(b, "原文截断") != 3 || kept(a) <= kept(b)/2 {
		t.Fatalf("dedup frees quota: kept 甲 %d (deduped) vs %d (both copies): %q", kept(a), kept(b), a)
	}
	if again := integratedBody("- 清单", dup, dup[0], nil, policy.MinMergeBodyBytes); again != a {
		t.Fatal("same input, same output")
	}

	three := mk(x, x, x)
	three[0].it.Title, three[2].it.Title = "第一条", "第三条"
	s := integratedBody("- 清单", three, three[2], nil, policy.DefaultMergeBodyBytes)
	if strings.Count(s, x) != 1 || !strings.Contains(s, "另有 2 条正文与此完全相同") || !strings.Contains(s, "【2/3】") || !strings.Contains(s, "原题：「第三条」") || !strings.HasPrefix(s[strings.Index(s, "【1/3】"):], "【1/3】来源：") {
		t.Fatalf("first member kept, the other two listed: %q", s)
	}
	// 空正文不算相同：各写「原文为空」。
	empty := integratedBody("- 清单", mk("", ""), nil, nil, policy.DefaultMergeBodyBytes)
	if strings.Count(empty, "（原文为空）") != 2 || strings.Contains(empty, "完全相同") {
		t.Fatalf("%q", empty)
	}
}
