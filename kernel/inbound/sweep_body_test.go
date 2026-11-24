package inbound

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 整合正文 = 要点清单 + 按成员逐字附上的来源原文（来源、时间、原题、原摘要）；原文里的数字没进要点清单也照样通过（原文附全了）；
// 被并成员的全文进去处的 absorbed；第二遍零变化；撤销后每一条的正文都还原成全文。
func TestMergeBodyListThenOriginals(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 2, true
	})
	list := "- 2026-11-16，邮件，交易所账户：提现 500 USDT，结论：已完成\n- 2026-11-19，邮件，交易所账户：提现 120 USDT，结论：已完成"
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"提现通知","members":["N1","N2"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2"],"title":"提现完成 500 与 120 USDT","summary":"两笔提现已完成：\n- 提现 500 USDT\n- 提现 120 USDT","body":`+fmt.Sprintf("%q", list)+`}`))
	details := map[string]string{
		"提现 500 USDT 已完成": "交易所通知：提现 500 USDT 于 2026-11-16 14:02 到账，交易 ID TX778812，手续费 1.5 USDT。",
		"提现 120 USDT 已完成": "交易所通知：提现 120 USDT 于 2026-11-19 09:30 到账，交易 ID TX778990，手续费 1.5 USDT。",
	}
	for i, title := range []string{"提现 500 USDT 已完成", "提现 120 USDT 已完成"} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", title, map[string]any{"detail": details[title]}))
	}
	n1, n2 := e.byTitle("提现 500 USDT 已完成"), e.byTitle("提现 120 USDT 已完成")
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Combined) != 1 || rep.Combined[0].Rejected != "" {
		t.Fatalf("run: %v %+v", err, rep)
	}
	c := e.item(n1.ID)
	body := c.Body
	if !strings.HasPrefix(body, list+"\n\n---\n来源原文（共 2 条，按成员顺序逐字附上）") || strings.Contains(body, "截断") {
		t.Fatalf("the list comes first, then the originals: %q", body)
	}
	for i, m := range []domain.Item{n1, n2} {
		head := fmt.Sprintf("【%d/2】来源：%s · %s · 原题：「%s」\n%s", i+1, sourceName(nil, originSource(m)), m.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), m.Title, m.Body)
		if m.Body != details[m.Title] || !strings.Contains(body, head) {
			t.Fatalf("member %d is attached verbatim with source, time and title: %q in %q", i+1, head, body)
		}
	}
	if rep.Combined[0].Body != body || c.Proposal.Integrated.ToBody != body || c.Proposal.Integrated.Body != n1.Body {
		t.Fatalf("the report shows the landed body; the carrier's own original is kept: %+v", c.Proposal.Integrated)
	}
	if ab := c.Proposal.Absorbed; len(ab) != 1 || ab[0].Item != n2.ID || ab[0].Body != n2.Body || ab[0].Summary != n2.Summary || ab[0].Title != n2.Title {
		t.Fatalf("absorbed carries the folded member's full body: %+v", ab)
	}
	e.verify()

	h, calls := e.head(), e.batchCalls()
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
		t.Fatalf("second pass changes nothing: %v %+v", err, rep)
	}

	if err := e.svc.Unmerge(bg, owner, n2.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []domain.Item{n1, n2} {
		if back := e.item(m.ID); back.Phase != domain.PhaseProposed || back.Title != m.Title || back.Body != m.Body || back.Proposal.Integrated != nil || len(back.Proposal.Absorbed) != 0 {
			t.Fatalf("undo restores %s in full: %+v", m.Title, back)
		}
	}
	e.verify()
}

// 超长：总长按 body_max_bytes 封顶，要点清单整段保留，原文均分余下额度、按字节截断并写明截掉多少与全文在哪；
// 原文里的关键编号落在被截掉的部分、要点清单又没带上的，整组退回保留（facts_missing，报告里带「原文：」）。
func TestMergeBodyTruncatedAndOriginalFactsChecked(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting, p.Merge.BodyMaxBytes = policy.MergeSame, 4, true, policy.MinMergeBodyBytes
	})
	fill := strings.Repeat("这是一段很长的说明文字，没有任何编号。", 120) // 每条约 6.8 KB，两条远超 4 KiB
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"扣款通知","members":["N1","N2"]},{"group":"G2","topic":"账单通知","members":["N3","N4"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"r","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"r","confident":true},
		 {"new":"N3","group":"G2","step":2,"action":"merge","target":"M2","reason":"r","confident":true},
		 {"new":"N4","group":"G2","step":2,"action":"merge","target":"M2","reason":"r","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2"],"title":"自动扣款成功 K3301 与 K3302","summary":"两笔：K3301、K3302","body":"- 扣款 K3301，结论：成功\n- 扣款 K3302，结论：成功"},
		 {"merge":"M2","group":"G2","members":["N3","N4"],"title":"账单已出 B7701 与 B7702","summary":"两份：B7701、B7702","body":"- 账单 B7701，结论：已出\n- 账单 B7702，结论：已出"}`))
	details := map[string]string{
		"自动扣款成功 K3301": "扣款 K3301 成功。" + fill,
		"自动扣款成功 K3302": "扣款 K3302 成功。" + fill,
		"账单已出 B7701":   "账单 B7701 已出。" + fill + "应还金额 Z99887766。", // 关键编号在末尾，会被截掉
		"账单已出 B7702":   "账单 B7702 已出。" + fill,
	}
	for i, title := range []string{"自动扣款成功 K3301", "自动扣款成功 K3302", "账单已出 B7701", "账单已出 B7702"} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", title, map[string]any{"detail": details[title]}))
	}
	k1, k2, b1, b2 := e.byTitle("自动扣款成功 K3301"), e.byTitle("自动扣款成功 K3302"), e.byTitle("账单已出 B7701"), e.byTitle("账单已出 B7702")
	if k1.Body != details[k1.Title] || b1.Body != details[b1.Title] {
		t.Fatalf("fixture bodies land verbatim: %d %d", len(k1.Body), len(b1.Body))
	}
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Combined) != 2 {
		t.Fatalf("run: %v %+v", err, rep)
	}

	c := e.item(k1.ID)
	body := c.Body
	if c.Proposal.Integrated == nil || len(body) > policy.MinMergeBodyBytes || !strings.HasPrefix(body, "- 扣款 K3301，结论：成功\n- 扣款 K3302，结论：成功\n\n---\n") {
		t.Fatalf("capped at body_max_bytes with the list intact: %d %q", len(body), body)
	}
	for _, m := range []domain.Item{k1, k2} {
		i := strings.Index(body, "原题：「"+m.Title+"」\n")
		if i < 0 {
			t.Fatalf("%s has its header", m.Title)
		}
		var total, kept, cut int
		var where string
		rest := body[i:]
		j := strings.Index(rest, "……（原文截断：")
		if j < 0 {
			t.Fatalf("%s is cut with a note: %q", m.Title, rest)
		}
		if _, err := fmt.Sscanf(rest[j:], "……（原文截断：全文 %d 字节，这里保留前 %d 字节，截掉 %d 字节；全文存于%s", &total, &kept, &cut, &where); err != nil {
			t.Fatalf("note: %v %q", err, rest[j:])
		}
		excerpt := strings.TrimSuffix(rest[len("原题：「"+m.Title+"」\n"):j], "\n")
		if total != len(m.Body) || kept+cut != total || kept != len(excerpt) || !strings.HasPrefix(m.Body, excerpt) || kept < 1000 {
			t.Fatalf("%s: the note says how much was cut: total %d kept %d cut %d excerpt %d", m.Title, total, kept, cut, len(excerpt))
		}
		want := "本条 proposal.integrated.body"
		if m.ID != k1.ID {
			want = "本条 proposal.absorbed 里建议 " + m.ID + " 的 body"
		}
		if !strings.Contains(rest[j:], want) {
			t.Fatalf("%s: the note says where the full text is (%s): %q", m.Title, want, rest[j:j+400])
		}
	}
	if ab := c.Proposal.Absorbed; len(ab) != 1 || ab[0].Body != k2.Body {
		t.Fatalf("absorbed keeps the full, uncut body: %+v", ab)
	}

	if mc := rep.Combined[1]; mc.Rejected != domain.RejectFactsMissing || strings.Join(mc.Missing, ",") != "原文：z99887766" {
		t.Fatalf("a key number cut from an original rejects the merge: %+v", mc.Missing)
	}
	for _, m := range []domain.Item{b1, b2} {
		if it := e.item(m.ID); it.Phase != domain.PhaseProposed || it.Proposal.Integrated != nil || it.Proposal.Sweep.Rejected != domain.RejectFactsMissing || it.Body != m.Body || it.Title != m.Title {
			t.Fatalf("%s stays as it was: %+v", m.Title, it.Proposal.Sweep)
		}
	}
	e.verify()

	if err := e.svc.Unmerge(bg, owner, k2.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []domain.Item{k1, k2} {
		if back := e.item(m.ID); back.Body != m.Body || back.Title != m.Title || back.Phase != domain.PhaseProposed {
			t.Fatalf("undo restores the full body of %s", m.Title)
		}
	}
}

// 要点清单本身就超过上限时：清单照样整段保留，每条原文写明保留 0 字节，不静默丢弃。
func TestIntegratedBodyListOverLimit(t *testing.T) {
	m := &sweepRec{it: domain.Item{ID: "it_a", Title: "甲", Body: "原文甲"}}
	n := &sweepRec{it: domain.Item{ID: "it_b", Title: "乙"}}
	list := strings.Repeat("- 很长的要点\n", 400)
	got := integratedBody(list, []*sweepRec{m, n}, m, nil, policy.MinMergeBodyBytes)
	if !strings.HasPrefix(got, list) || !strings.Contains(got, "全文 9 字节，这里保留前 0 字节，截掉 9 字节；全文存于本条 proposal.integrated.body") || !strings.Contains(got, "原题：「乙」\n（原文为空）") {
		t.Fatalf("%q", got[len(list):])
	}
	// 一短一长：短的原样附全，用不完的额度给长的。
	m.it.Body, n.it.Body = "短", strings.Repeat("长", 3000)
	got = integratedBody("- a\n- b", []*sweepRec{m, n}, m, nil, policy.MinMergeBodyBytes)
	if !strings.Contains(got, "原题：「甲」\n短\n") || len(got) > policy.MinMergeBodyBytes || strings.Count(got, "原文截断") != 1 {
		t.Fatalf("water-fill: %d %q", len(got), got)
	}
}
