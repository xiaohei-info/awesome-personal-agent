package inbound

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 强制保留的四类各认哪些写法、不强制的认不认进来。键是「标题 摘要」，值是认出的 类别:片段。
func TestKeyFactsClassify(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		// 强制：金额（含币种与数字）
		{"amount code after", "提现 840 USDT 已到账", []string{"金额:840 USDT"}},
		{"amount code before", "转出 USD 1,200.50 与 $30", []string{"金额:$30", "金额:USD 1,200.50"}},
		{"amount nbsp after time", "12:30:28 CNY\u00a020.00 与 7.90 CNY", []string{"日期时间:12:30:28", "金额:CNY\u00a020.00", "金额:7.90 CNY"}},
		{"amount cjk", "报销 300 元，另付 12.5 万元", []string{"金额:12.5 万元", "金额:300 元"}},
		// 强制：日期与时间
		{"date iso", "截止 2026-11-30，2026/12/01 复核", []string{"日期时间:2026-11-30", "日期时间:2026/12/01"}},
		{"date cjk", "11月16日 与 2026年11月17日", []string{"日期时间:11月16日", "日期时间:2026年11月17日"}},
		{"time", "UTC 22:55 开会，2026-11-16T19:10:41Z 失败", []string{"日期时间:19:10:41", "日期时间:2026-11-16", "日期时间:22:55"}},
		{"month-day", "会议改到 11-28 14:00，编号 2536-0704、5-11", []string{"日期时间:11-28", "日期时间:14:00"}},
		{"time cjk", "下午 3点30分 到场", []string{"日期时间:3点30分"}},
		{"month day", "改到 11/16 交付", []string{"日期时间:11/16"}},
		// 强制：单据类编号
		{"pr", "评审 PR #217 与 MR!42，另见 org/app#220", []string{"单据编号:#220", "单据编号:MR!42", "单据编号:PR #217"}},
		{"ticket key", "GT-395 保留期清除", []string{"单据编号:GT-395"}},
		{"cjk document", "工单号 88123，订单 20261124001，工单 7 号", []string{"单据编号:工单号 88123", "单据编号:订单 20261124001"}},
		{"run id", "Run ID: 123456789 失败，order no. 55102", []string{"单据编号:Run ID: 123456789", "单据编号:order no. 55102"}},
		{"prefixed number", "复核编号 R55102，账单 B18630823", []string{"单据编号:B18630823"}},
		{"phase names", "PR0 阶段与 PR-7、PR 3 个文件", nil},
		{"cve id", "漏洞 CVE-2018-3258", []string{"单据编号:CVE-2018-3258"}},
		{"pr link", "见 https://github.com/o/r/pull/217 。", []string{"单据编号:https://github.com/o/r/pull/217"}},
		// 强制：明确的数量
		{"quantity", "共 50 件，3 笔，12个", []string{"数量:12个", "数量:3 笔", "数量:50 件"}},
		// 不强制：版本号、CI job 与 workflow 名、注释条数、耗时秒数、文件数与行数、孤立的无单位数字、哈希、地址片段、普通链接
		{"version", "文档 §6.10、§6.15，v2，1.2.3，Go 1.22", nil},
		{"job names", "build 失败（e2e-smoke 通过），workflow ci", nil},
		{"counts", "3 个文件，120 行，5 条评论，7 条注释，2 个 job，第 3 个，3 个月", nil},
		{"duration", "耗时 2 minutes and 14.0 seconds，90 秒", nil},
		{"bare number", "proposals（≤50 + pending_total）总数 1234", nil},
		{"hash and address", "提交 ad42948，地址 0x82df…5057，sha256", nil},
		{"plain link", "详见 https://example.com/docs/guide 与 UTF-8、SHA-256", nil},
		{"fraction", "完成 3/4，比例 16:9，9/50", nil},
	}
	for _, c := range cases {
		var got []string
		for _, f := range keyFacts(c.text, "") {
			got = append(got, f.Kind+":"+f.Text)
			if f.Why == "" {
				t.Errorf("%s: %q has no reason", c.name, f.Text)
			}
		}
		slices.Sort(got)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}
}

// 缺失的判定：数字按数字边界比，写法不同的中文日期只要求月和日的数字在；报告里一句话写清哪一项、哪一类、为什么。
func TestMissingFactsExplained(t *testing.T) {
	facts := keyFacts("核实 Bitget 840 USDT 提现", "2026-11-16 22:55 到账，PR #217，共 50 件，11月16日")
	if miss := missingFacts("Bitget 提现 840 USDT，2026-11-16 22:55，PR #217，50 件，11 月 16 日", facts); len(miss) != 0 {
		t.Fatalf("all there: %v", miss)
	}
	// 只留版本、job 名之类的文字，每类都缺：PR 写成 PR 217 也算在。
	miss := missingFacts("1840 万，11月16号，PR 217，e2e-smoke，6.10", facts)
	var got []string
	for _, f := range miss {
		got = append(got, f.Kind+":"+f.Text)
	}
	slices.Sort(got)
	if want := []string{"数量:50 件", "日期时间:2026-11-16", "日期时间:22:55", "金额:840 USDT"}; !slices.Equal(got, want) {
		t.Fatalf("missing: got %q, want %q", got, want)
	}
	if s := miss[0].String(); !strings.Contains(s, "「") || !strings.Contains(s, miss[0].Kind) || !strings.Contains(s, miss[0].Why) {
		t.Fatalf("report names the item, its class and the reason: %s", s)
	}
}

// 整合校验走完整流水线：四类强制事实各缺一次都退回（facts_missing），理由里写明缺哪一项、判成哪一类、为什么；
// 只差版本号、job 名、无单位的孤立数字的不退回；第二遍都零变化。
func TestMergeFactsRelaxedPipeline(t *testing.T) {
	cases := []struct {
		name   string
		titles [2]string
		// 模型整合出的标题、摘要、要点清单
		title, summary, list string
		kind, why, text      string // 退回时：缺的类别、原因里的话、缺的片段；空即应当整合成功
	}{
		{"missing amount", [2]string{"提现 840 USDT 已到账", "提现回执补发"}, "提现回执", "两条提现通知", "- 提现已到账\n- 回执补发", factAmount, "数字后紧跟币种", "840 USDT"},
		{"missing date", [2]string{"会议改期到 2026-11-30", "会议室已订"}, "会议改期", "会议改期并订了会议室", "- 会议改期\n- 会议室已订", factDate, "完整的年月日", "2026-11-30"},
		{"missing pr number", [2]string{"请评审 PR #4521", "PR 评审邮件提醒"}, "评审请求", "有一个 PR 请评审", "- 请评审\n- 邮件提醒", factDocument, "带 PR/MR 前缀的编号", "PR #4521"},
		{"missing quantity", [2]string{"共 50 件待发货", "发货单已生成"}, "发货", "待发货与发货单", "- 待发货\n- 发货单已生成", factQuantity, "数字后带量词「件」", "50 件"},
		{"only version differs", [2]string{"文档升级到 v2 与 6.10", "文档 6.15 校对"}, "文档升级与校对", "文档升级并校对", "- 升级\n- 校对", "", "", ""},
		{"only job name differs", [2]string{"build 失败（e2e-smoke 通过）", "ci 工作流 build 失败邮件"}, "ci build 失败", "build 失败", "- build 失败\n- 邮件", "", "", ""},
		{"bare number differs", [2]string{"会议地点 301，上限 50", "会议地点确认"}, "会议地点", "地点已定", "- 地点\n- 确认", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, true)
			e.svc.o.Now = e.Now
			e.setPolicy(func(p *policy.Policy) {
				p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 2, true
			})
			e.stub.When(batchMark, reply(`{"group":"G1","topic":"t","members":["N1","N2"]}`,
				`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一件事","confident":true},
				 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一件事","confident":true}`,
				fmt.Sprintf(`{"merge":"M1","group":"G1","members":["N1","N2"],"title":%q,"summary":%q,"body":%q}`, c.title, c.summary, c.list)))
			for i, title := range c.titles {
				e.now = e.now.Add(time.Minute)
				e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", title, nil))
			}
			a, b := e.byTitle(c.titles[0]), e.byTitle(c.titles[1])
			rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
			if err != nil || len(rep.Combined) != 1 {
				t.Fatalf("run: %v %+v", err, rep)
			}
			mc := rep.Combined[0]
			integrated := e.item(a.ID).Proposal.Integrated != nil || e.item(b.ID).Proposal.Integrated != nil
			if c.kind == "" {
				if mc.Rejected != "" || len(mc.Missing) != 0 || !integrated {
					t.Fatalf("only non-mandatory details differ, the merge goes through: %+v", mc)
				}
			} else {
				why := e.item(a.ID).Proposal.Sweep.Why
				if mc.Rejected != domain.RejectFactsMissing || len(mc.Missing) == 0 || len(mc.MissingWhy) != len(mc.Missing) || integrated {
					t.Fatalf("a mandatory fact is gone, the merge is rejected: %+v", mc)
				}
				for _, s := range []string{c.kind, c.why, "「" + c.text + "」"} {
					if !strings.Contains(mc.Why, s) || !strings.Contains(why, s) || !strings.Contains(strings.Join(mc.MissingWhy, "\n"), s) {
						t.Fatalf("the reason names the item, its class and why it is mandatory: %q in %q / %q / %v", s, mc.Why, why, mc.MissingWhy)
					}
				}
				if sw := e.item(a.ID).Proposal.Sweep; sw.Rejected != domain.RejectFactsMissing || sw.Action != domain.SweepKeep {
					t.Fatalf("the members stay: %+v", sw)
				}
			}
			e.verify()
			h, calls := e.head(), e.batchCalls()
			if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
				t.Fatalf("second pass changes nothing: %v %+v", err, rep)
			}
		})
	}
}

// 千分位与小数点的边界：1,200 与 1200 是同一个数；200 不会在 1,200 或 5.200 里算找到；中文日期换成 2026.11.16 的写法仍算在。
func TestMissingFactsNumberBoundaries(t *testing.T) {
	cases := []struct {
		source, text string
		missing      bool
	}{
		{"报销 1200 元", "报销 1,200 元", false},
		{"转账 1,200 USDT", "转账 1200 USDT", false},
		{"转账 1,234,567 USDT", "转账 1234567 USDT", false},
		{"共 200 件", "共 1,200 件", true},
		{"付 200 元", "付 5.200 元", true},
		{"付 5.2 元", "付 15.2 元", true},
		{"付 5.2 元。", "付 5.2 元。", false},
		{"共 50 件", "共 50.5 件", true},
		{"11月16日", "2026.11.16", false},
	}
	for _, c := range cases {
		facts := keyFacts(c.source, "")
		if len(facts) != 1 {
			t.Fatalf("%q: expected one fact, got %v", c.source, facts)
		}
		if got := len(missingFacts(c.text, facts)) == 1; got != c.missing {
			t.Errorf("%q in %q: missing=%v, want %v", c.source, c.text, got, c.missing)
		}
	}
}
