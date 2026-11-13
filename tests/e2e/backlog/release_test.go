//go:build e2e

package backlog

import (
	"cmp"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	hi, mid, low = "[FX-HIGH]", "[FX-MID]", "[FX-LOW]"
	words        = "本人：放开"
	perDay       = 5
	midOnlySrc   = "src_script_deploy" // score_markers 里只含 [FX-MID] 事件的来源
)

// manifest 读夹具清单里的整数。
func (k *kit) manifestInt(keys ...string) int {
	k.t.Helper()
	var v any
	raw, ok := k.fx.Raw[keys[0]]
	if !ok {
		k.t.Fatalf("manifest 缺 %s", keys[0])
	}
	k.must(json.Unmarshal(raw, &v))
	for _, key := range keys[1:] {
		v = v.(map[string]any)[key]
	}
	f, _ := v.(float64)
	return int(f)
}

func mark(subject string) string {
	for _, m := range []string{hi, mid, low} {
		if strings.Contains(subject, m) {
			return m
		}
	}
	return ""
}

type rejudgeResult struct{ Requeued, Deferred int }

func (k *kit) rejudge(args ...string) rejudgeResult {
	k.t.Helper()
	var r rejudgeResult
	nb := len(k.clsLog())
	k.ownerOK(words, &r, append([]string{"source", "backlog", "rejudge"}, args...)...)
	k.t.Logf("rejudge %v -> %+v 新增判断批次 %d", args, r, len(k.clsLog())-nb)
	return r
}

// freshProposals 是 before 之后新进入建议流的来源事件。
func (k *kit) freshProposals(before map[string]srcEvent) (fresh []srcEvent) {
	for id, e := range k.proposed() {
		if _, old := before[id]; !old {
			fresh = append(fresh, e)
		}
	}
	return
}

// TestBacklogReleaseFlow 是 flow-backlog-release：本人把升级后冻结的积压分四步放开，经新的级联重判，进入建议流的数量不超过每日上限。
func TestBacklogReleaseFlow(t *testing.T) {
	k := newKit(t, nil, true)

	// 开始时的账本快照：每条积压事件的原处置与原因，ACT-BKL-13 逐条核对。
	type orig struct{ Disposition, Reason, Mark string }
	origins := map[string]orig{}
	scores := map[string]float64{}
	var attachable, structured int

	k.step("ACT-BKL-01 准备已升级的夹具副本与替身脚本", func(t *testing.T) {
		k.cases()
		var v struct {
			Schema struct{ Ledger int }
		}
		k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "version").Stdout), &v))
		if v.Schema.Ledger < 3 {
			t.Fatalf("apa version 的 schema.ledger 应为升级后的值: %+v", v)
		}
		// 替身脚本生效：向两个替身各发一条测试请求得到配置的结果。
		for mark, want := range map[string]float64{hi: 0.90, mid: 0.40, low: 0.05} {
			if got := k.clsAsk(mark + " 测试"); got != want {
				t.Fatalf("CLS_STUB 对 %s 应答 %v，期望 %v", mark, got, want)
			}
		}
		if got := k.chatDraft("ping"); !strings.Contains(got, "起草标题") {
			t.Fatalf("MODEL_STUB 生成请求应返回合法起草结果: %s", got)
		}
		var web struct {
			Judge    struct{ Locality, Protocol string } `json:"judge_endpoint"`
			Generate struct{ Locality string }           `json:"generate_endpoint"`
		}
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/model-endpoints", "", 200)), &web))
		if web.Judge.Locality != "local" || web.Generate.Locality != "local" {
			t.Fatalf("两个端点应为 local: %+v", web)
		}
		for _, b := range k.backlog() {
			origins[b.ID] = orig{b.Disposition, b.Unjudged, mark(b.Subject)}
		}
		attachable, structured = k.manifestInt("attachable"), k.manifestInt("structured_rule_hits")
		if len(origins) == 0 || attachable == 0 || structured == 0 {
			t.Fatalf("清单应有积压与规则命中: backlog=%d attachable=%d structured=%d", len(origins), attachable, structured)
		}
		k.clear()
	})

	k.step("ACT-BKL-02 列出积压，旧原因原样返回", func(t *testing.T) {
		var list struct {
			Events []srcEvent
			Total  int
		}
		k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "source", "backlog", "list").Stdout), &list))
		want := k.manifestInt("backlog", "expired")
		for _, n := range []int{k.manifestInt("backlog", "unjudged", "model_unavailable"), k.manifestInt("backlog", "unjudged", "undecided"), k.manifestInt("backlog", "unjudged", "none")} {
			want += n
		}
		if len(list.Events) != want || list.Total != want {
			t.Fatalf("CLI 列出 %d/%d 条，清单 unjudged+expired=%d", len(list.Events), list.Total, want)
		}
		// 与 HTTP 逐来源列出的一致，原因原样。
		if got := len(k.backlog()); got != want {
			t.Fatalf("HTTP 列出 %d 条，清单 %d", got, want)
		}
		undecided, empty := 0, 0
		for _, e := range list.Events {
			if o := origins[e.ID]; o.Reason != e.Unjudged || o.Disposition != e.Disposition {
				t.Fatalf("旧原因被改写: %+v → %+v", o, e)
			}
			switch e.Unjudged {
			case "undecided":
				undecided++
			case "":
				empty++
			}
		}
		_, detail, signals := k.unjudgedHealth()
		if undecided == 0 || empty == 0 || num(detail, "undecided") != k.manifestInt("backlog", "health", "undecided") || signals != 13 {
			t.Fatalf("undecided=%d 原因为空=%d health.detail=%v signals=%d", undecided, empty, detail, signals)
		}
	})

	k.step("ACT-BKL-03 第 1 步只跑规则，零模型调用", func(t *testing.T) {
		total := len(k.backlog())
		seq := k.maxSeq()
		var res struct{ Attached, Disposed, Remaining int }
		k.ownerOK("本人：先只跑规则", &res, "source", "backlog", "rules")
		if res.Attached != attachable || res.Disposed != structured || res.Remaining != total-attachable-structured {
			t.Fatalf("rules %+v，清单 attachable=%d structured=%d 积压=%d", res, attachable, structured, total)
		}
		if len(k.eventRequests()) != 0 {
			t.Fatalf("只跑规则不应有模型请求: cls=%d model=%d", len(k.clsLog()), len(k.modelLog()))
		}
		perEvent := map[string]int{}
		for _, e := range k.audit("inbound.disposed", seq) {
			perEvent[e.Object]++
		}
		if len(perEvent) != res.Disposed {
			t.Fatalf("被处置 %d 条，inbound.disposed 覆盖 %d 条（挂靠的 %d 条不产生处置 Event，看 attached 处置）", res.Disposed, len(perEvent), res.Attached)
		}
		if got := len(k.all("attached")); got < res.Attached {
			t.Fatalf("挂靠 %d 条，来源页 attached 只有 %d 条", res.Attached, got)
		}
		for id, n := range perEvent {
			if n != 1 {
				t.Fatalf("事件 %s 有 %d 条 inbound.disposed", id, n)
			}
		}
		if got := len(k.backlog()); got != res.Remaining {
			t.Fatalf("积压剩 %d 条，输出 remaining=%d", got, res.Remaining)
		}
	})

	k.step("ACT-BKL-04 积压放开命令缺本人原话被拒", func(t *testing.T) {
		before := k.distribution()
		for _, step := range []string{"rules", "score", "rejudge"} {
			if r := k.Apa(k.DEL, "", "source", "backlog", step); r.Code != 23 {
				t.Fatalf("不带 --evidence 运行 %s 退出码 %d，期望 23\n%s%s", step, r.Code, r.Stdout, r.Stderr)
			}
		}
		g := k.run(k.DEL, []string{"APA_SESSION=e2e-session", "APA_SESSION_KIND=group"}, "source", "backlog", "rules", "--evidence", "本人：群里说")
		if g.Code != 23 || !strings.Contains(g.Stderr+g.Stdout, "session_not_direct") {
			t.Fatalf("group 会话运行退出码 %d，期望 23 session_not_direct\n%s%s", g.Code, g.Stdout, g.Stderr)
		}
		if r := k.Apa(k.OBS, "", "source", "backlog", "rules", "--evidence", "本人：只读"); r.Code != 4 {
			t.Fatalf("observer 退出码 %d，期望 4\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		if after := k.distribution(); after != before || len(k.eventRequests()) != 0 {
			t.Fatalf("被拒的命令改变了积压或调了模型\n%s\n%s", before, after)
		}
	})

	var remaining int
	k.step("ACT-BKL-05 第 2 步影子打分只写判断记录、不改处置", func(t *testing.T) {
		before := k.distribution()
		remaining = len(k.backlog())
		proposedBefore := len(k.proposed())
		var res struct{ Scored, Skipped int }
		k.ownerOK("本人：影子打分", &res, "source", "backlog", "score")
		if res.Scored != remaining {
			t.Fatalf("scored=%d，ACT-BKL-03 的 remaining=%d（%+v）", res.Scored, remaining, res)
		}
		if after := k.distribution(); after != before {
			t.Fatalf("打分改变了处置分布\n%s\n%s", before, after)
		}
		var shown int
		for _, e := range k.backlog() {
			sc := mark(e.Subject)
			js := k.judgmentsOf(e.ID)
			if len(js) != 1 {
				t.Fatalf("事件 %s 应恰有一条判断记录，实际 %d", e.ID, len(js))
			}
			if shown < 6 { // 抽查：带 shadow 标记、概率同 $CLS_CASES
				j := js[0]
				if shown == 0 {
					_ = k.judgment(js[0].ID) // apa judgment show 也能读到
				}
				want := map[string]float64{hi: 0.90, mid: 0.40, low: 0.05}[sc]
				if !j.Shadow || len(j.Levels) == 0 || j.Levels[0].PYes == nil || *j.Levels[0].PYes != want {
					t.Fatalf("事件 %s（%s）的判断记录 %+v", e.ID, sc, j)
				}
				shown++
			}
		}
		for _, b := range k.clsLog() {
			if b.Questions > 32 {
				t.Fatalf("判断端点一批 %d 题，超过 max_batch 32", b.Questions)
			}
		}
		if len(k.clsLog()) == 0 || len(k.judgeRequests()) != 0 {
			t.Fatalf("打分应只走判断端点: cls 批次=%d model 判断题=%d", len(k.clsLog()), len(k.judgeRequests()))
		}
		if len(k.proposed()) != proposedBefore {
			t.Fatal("打分新建了事项")
		}
	})

	k.step("ACT-BKL-06 来源页与 CLI 可按分数排序", func(t *testing.T) {
		var cli struct{ Events []srcEvent }
		k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "source", "backlog", "list", "--sort", "score").Stdout), &cli))
		rank := map[string]int{hi: 0, mid: 1, low: 2}
		prev := -1
		for _, e := range cli.Events {
			if e.Score == nil {
				t.Fatalf("事件 %s 没有分数", e.ID)
			}
			scores[e.ID] = *e.Score
			if r := rank[mark(e.Subject)]; r < prev {
				t.Fatalf("CLI 顺序不对：%s 排在较低分之后", e.Subject)
			} else {
				prev = r
			}
		}
		if len(cli.Events) != remaining || !strings.Contains(cli.Events[0].Subject, hi) || !strings.Contains(cli.Events[len(cli.Events)-1].Subject, low) {
			t.Fatalf("CLI 排序列出 %d 条（积压 %d）", len(cli.Events), remaining)
		}
		// HTTP：逐来源按分数从高到低。
		for _, s := range k.sources() {
			es := k.events(s, "unjudged,expired", "score")
			for i := 1; i < len(es); i++ {
				if es[i-1].Score == nil || es[i].Score == nil || *es[i-1].Score < *es[i].Score {
					t.Fatalf("来源 %s 的 HTTP 排序不是分数从高到低: %v %v", s, es[i-1].Score, es[i].Score)
				}
			}
		}
		for _, e := range cli.Events {
			want := map[string]float64{hi: 0.90, mid: 0.40, low: 0.05}[mark(e.Subject)]
			if *e.Score != want {
				t.Fatalf("事件 %s 分数 %v，期望 %v", e.Subject, *e.Score, want)
			}
		}
	})

	var proposedByOwner int
	k.step("ACT-BKL-07 第 3 步本人从两端各处理一批并在 Web 定不确定带", func(t *testing.T) {
		var cli struct{ Events []srcEvent }
		k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "source", "backlog", "list", "--sort", "score").Stdout), &cli))
		top, bottom := cli.Events[:3], cli.Events[len(cli.Events)-3:]
		seq := k.maxSeq()
		k.clear()
		for _, e := range top {
			if !strings.Contains(e.Subject, hi) {
				t.Fatalf("最高端应是 %s: %s", hi, e.Subject)
			}
			k.webOK("POST", "/v1/source-events/"+e.ID+"/propose", `{}`, 200)
		}
		for _, e := range bottom {
			if !strings.Contains(e.Subject, low) {
				t.Fatalf("最低端应是 %s: %s", low, e.Subject)
			}
			k.webOK("POST", "/v1/source-events/"+e.ID+"/ignore", `{}`, 200)
		}
		proposedByOwner = 3
		disposed := k.audit("inbound.disposed", seq)
		if len(disposed) != 6 {
			t.Fatalf("应有 6 条 inbound.disposed，实际 %d", len(disposed))
		}
		for _, d := range disposed {
			if d.Actor.Principal != "owner" {
				t.Fatalf("操作者应是本人: %+v", d.Actor)
			}
		}
		for _, e := range top {
			if got := k.eventByID(e.Source, e.ID).Disposition; got != "proposed" {
				t.Fatalf("%s 应为 proposed，实际 %s", e.ID, got)
			}
		}
		for _, e := range bottom {
			if got := k.eventByID(e.Source, e.ID).Disposition; got != "ignored" {
				t.Fatalf("%s 应为 ignored，实际 %s", e.ID, got)
			}
		}
		// "判断与校准"页：分数分布；定不确定带 0.15–0.60，保存后事件流有 policy.changed（操作者是本人）。
		var stats struct{ Histogram []struct{ Count int } }
		k.get("/v1/judgments/stats?use=inbound_screen", &stats)
		total := 0
		for _, b := range stats.Histogram {
			total += b.Count
		}
		if total == 0 {
			t.Fatalf("判断与校准页没有分数分布: %+v", stats)
		}
		seq = k.maxSeq()
		k.webOK("PUT", "/v1/settings/judgment", `{"bands":[{"template":"triage-screen","low":0.15,"high":0.60}]}`, 200)
		ch := k.audit("policy.changed", seq)
		if len(ch) != 1 || ch[0].Actor.Principal != "owner" {
			t.Fatalf("不确定带变更应有一条本人的 policy.changed: %+v", ch)
		}
		if len(k.eventRequests()) != 0 {
			t.Fatal("第 3 步不应有模型请求")
		}
	})

	k.step("ACT-BKL-08 被忽略的事件不占每日额度", func(t *testing.T) {
		before := k.proposed()
		src := k.events(midOnlySrc, "unjudged,expired", "")
		if len(src) <= perDay {
			t.Fatalf("来源 %s 的积压应大于 %d，实际 %d", midOnlySrc, perDay, len(src))
		}
		for _, e := range src {
			if mark(e.Subject) != mid {
				t.Fatalf("来源 %s 应只含 %s: %s", midOnlySrc, mid, e.Subject)
			}
		}
		res := k.rejudge("--source", midOnlySrc)
		if res.Requeued != len(src) || res.Deferred != 0 {
			t.Fatalf("rejudge --source: %+v，期望 requeued=%d deferred=0", res, len(src))
		}
		for _, e := range src {
			got := k.eventByID(midOnlySrc, e.ID)
			if _, ok := k.decision(e.ID); got.Disposition != "ignored" || !ok {
				t.Fatalf("事件 %s 应被忽略并有判断记录: %+v", e.ID, got)
			}
		}
		for _, e := range src[:3] {
			j, _ := k.decision(e.ID)
			if j.DecidedBy != "chat" && j.DecidedBy != "generate" || len(j.Levels) != 2 || j.Levels[0].Backend != "classifier" || j.Levels[1].Backend != "chat" || j.Levels[1].Outcome == "" {
				t.Fatalf("应由分类端点（带内）升级到对话端点（答否）定案: %+v", j)
			}
		}
		if got := len(k.freshProposals(before)); got != 0 {
			t.Fatalf("被忽略的事件不应进入建议流，新增 %d", got)
		}
	})

	k.step("ACT-BKL-09 第 4 步放开重判按分数从高到低并受每日上限约束", func(t *testing.T) {
		before := k.proposed()
		seq := k.maxSeq()
		clsBefore := len(k.clsLog())
		backlogBefore := len(k.backlog())
		res := k.rejudge()
		fresh := k.freshProposals(before)
		if len(fresh) != perDay {
			t.Fatalf("今天新进入建议流 %d 条，期望恰 %d（本人 Web 转建议的 %d 条是第 3 步的人工操作）", len(fresh), perDay, proposedByOwner)
		}
		for _, e := range fresh {
			if !strings.Contains(e.Subject, hi) {
				t.Fatalf("新建议应都是 %s: %s", hi, e.Subject)
			}
			var it struct{ Item struct{ Title, Body string } }
			k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "item", "show", e.Item).Stdout), &it))
			if it.Item.Title == "" {
				t.Fatalf("新建议 %s 没有标题", e.Item)
			}
		}
		remainingNow := len(k.backlog())
		if res.Deferred != remainingNow {
			t.Fatalf("deferred=%d，剩余积压 %d", res.Deferred, remainingNow)
		}
		requeued := k.audit("inbound.requeued", seq)
		if len(requeued) != res.Requeued {
			t.Fatalf("inbound.requeued %d 条，requeued=%d", len(requeued), res.Requeued)
		}
		prev := 2.0
		for _, r := range requeued {
			if r.Actor.Principal == "kernel:inbound" {
				t.Fatalf("inbound.requeued 的操作者应是调用者: %+v", r.Actor)
			}
			if s, ok := scores[r.Object]; ok { // 放回的顺序：分数从高到低
				if s > prev {
					t.Fatalf("放回顺序不是分数从高到低: %v 之后是 %v", prev, s)
				}
				prev = s
			}
		}
		batches := k.clsLog()[clsBefore:]
		if len(batches) == 0 {
			t.Fatal("初筛没有提交到判断端点")
		}
		multi := false
		for _, b := range batches {
			if b.Questions > 32 {
				t.Fatalf("一批 %d 题，超过 32", b.Questions)
			}
			multi = multi || b.Questions > 1
		}
		if !multi {
			t.Fatal("初筛逐条单题提交，应按批")
		}
		if backlogBefore-res.Requeued != remainingNow {
			t.Fatalf("积压 %d - 放回 %d ≠ 剩余 %d", backlogBefore, res.Requeued, remainingNow)
		}
	})

	k.step("ACT-BKL-10 当天额度用完后再次放开只推迟不放行", func(t *testing.T) {
		before := k.proposed()
		remainingNow := len(k.backlog())
		for _, args := range [][]string{nil, {"--limit", "3"}} {
			res := k.rejudge(args...)
			if got := len(k.freshProposals(before)); got != 0 || res.Requeued != 0 || res.Deferred != remainingNow || len(k.backlog()) != remainingNow {
				t.Fatalf("同一天再放开 %v: 新增建议 %d，%+v，积压 %d→%d", args, got, res, remainingNow, len(k.backlog()))
			}
		}
	})

	k.step("ACT-BKL-11 推进到次日后继续放开", func(t *testing.T) {
		k.TCAdvance(86400)
		before := k.proposed()
		first := k.rejudge("--limit", "3")
		if got := len(k.freshProposals(before)); got != 3 {
			t.Fatalf("次日 --limit 3 新增建议 %d 条（%+v）", got, first)
		}
		second := k.rejudge()
		fresh := k.freshProposals(before)
		if len(fresh) != perDay {
			t.Fatalf("次日合计新增建议 %d 条，期望 %d（第二次 %+v）", len(fresh), perDay, second)
		}
		if second.Deferred != len(k.backlog()) || second.Deferred == 0 {
			t.Fatalf("其余应 deferred: %+v 积压 %d", second, len(k.backlog()))
		}
		for _, e := range fresh {
			if !strings.Contains(e.Subject, hi) {
				t.Fatalf("次日新建议仍应按分数从高到低先出 %s: %s", hi, e.Subject)
			}
		}
	})

	k.step("ACT-BKL-12 放开重判中途崩溃后可接续，不重复不遗漏", func(t *testing.T) {
		k.TCAdvance(86400) // T0+2d
		before := k.proposed()
		seqStart := k.maxSeq()
		// 判断端点 max_batch 改为 2，整批时延 5 秒；起草请求也慢 5 秒，让已放回的事件停在 pending 时进程被杀。
		// 分类端点的结果会在 7 天窗口内复用前几天预取的答案，所以"第 2 批请求"未必出现；这里以"起草请求出现（事件已放回、在处理中）"为杀进程的时机。
		k.Cls.SetFault(modelstub.ClsFault{DelayMS: 5000})
		k.ModelStub.SetDefault(modelstub.Reply{Content: draftReply, Delay: 5 * time.Second})
		k.wire("local", "local", 2)
		k.clear()
		drafts := func() (n int) {
			for _, r := range k.modelLog() {
				if !baseline.IsJudgeRequest(r) {
					n++
				}
			}
			return
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // 进程被杀后这条命令失败，退出码不重要
			defer wg.Done()
			cmd := exec.Command(k.Bin["apa"], "source", "backlog", "rejudge", "--evidence", words)
			cmd.Env = append(os.Environ(), "APA_HOME="+k.Home, "APA_URL="+k.URL, "APA_TOKEN="+k.DEL, "APA_SESSION=e2e-session", "APA_SESSION_KIND=direct")
			_ = cmd.Run()
		}()
		if !k.WaitFor(60*time.Second, func() bool { return drafts() >= 1 }) {
			t.Fatalf("放开重判没有走到起草: 判断批次 %d\n%s", len(k.clsLog()), k.ServiceLog())
		}
		if len(k.all("pending")) == 0 {
			t.Fatal("杀进程时应有已放回、停在 pending 的事件")
		}
		k.KillKernel()
		wg.Wait()
		for _, b := range k.clsLog() {
			if b.Questions > 2 {
				t.Fatalf("max_batch=2 时一批 %d 题", b.Questions)
			}
		}
		k.Cls.SetFault(modelstub.ClsFault{})
		k.ModelStub.SetDefault(modelstub.Reply{Content: draftReply})
		k.StartKernelAgain() // 偏移量文件不变，时间仍为 T0+2d
		k.TCAdvance(900)     // 等待 15 分钟
		if !k.WaitFor(60*time.Second, func() bool { return len(k.all("pending")) == 0 }) {
			t.Fatalf("重启后事件仍卡在 pending: %d 条", len(k.all("pending")))
		}
		k.rejudge()
		fresh := k.freshProposals(before)
		if len(fresh) > perDay {
			t.Fatalf("崩溃后当天进入建议流 %d 条，超过 %d", len(fresh), perDay)
		}
		items := map[string]string{}
		for _, e := range k.proposed() {
			if e.Item == "" {
				continue
			}
			if other, dup := items[e.Item]; dup {
				t.Fatalf("事项 %s 对应两个来源事件 %s、%s", e.Item, other, e.ID)
			}
			items[e.Item] = e.ID
		}
		for _, typ := range []string{"inbound.requeued", "inbound.disposed"} {
			per := map[string]int{}
			for _, r := range k.audit(typ, seqStart) {
				per[r.Object]++
			}
			if typ == "inbound.requeued" && len(per) == 0 {
				t.Fatal("没有任何 inbound.requeued")
			}
			for id, n := range per {
				if n != 1 {
					t.Fatalf("事件 %s 有 %d 条 %s", id, n, typ)
				}
			}
		}
	})

	k.step("ACT-BKL-13 已到期与旧原因的事件同样被放开", func(t *testing.T) {
		k.Cls.Reset()
		k.cases()
		k.wire("local", "local", 0)
		days := 0
		for ; days < 90; days++ {
			k.TCAdvance(86400)
			if res := k.rejudge(); res.Deferred == 0 {
				break
			}
		}
		if days >= 90 {
			t.Fatalf("90 天内积压没有放完: 剩 %d", len(k.backlog()))
		}
		// 原处置为 expired、原因为 undecided 或为空的事件，现在都已被处置；经放回重判的各有判断记录与 inbound.requeued。
		final := map[string]srcEvent{}
		for _, e := range k.all("proposed,ignored,attached") {
			final[e.ID] = e
		}
		requeued, disposed := map[string]bool{}, map[string]bool{}
		for _, r := range k.audit("inbound.requeued", 0) {
			requeued[r.Object] = true
		}
		for _, d := range k.audit("inbound.disposed", 0) {
			disposed[d.Object] = true
		}
		checked, lowDirect := 0, 0
		for id, o := range origins {
			if o.Disposition != "expired" && o.Reason != "undecided" && o.Reason != "" {
				continue
			}
			e, ok := final[id]
			if !ok {
				t.Fatalf("事件 %s（原 %s/%s）没有被处置", id, o.Disposition, o.Reason)
			}
			if !requeued[id] && !disposed[id] {
				t.Fatalf("事件 %s（原 %s/%s）既没有 inbound.requeued 也没有 inbound.disposed", id, o.Disposition, o.Reason)
			}
			if !requeued[id] {
				continue // 第 1 步规则直接处置，或本人在第 3 步亲自处置
			}
			checked++
			j, ok := k.decision(id)
			if !ok {
				t.Fatalf("重判过的事件 %s 没有判断记录", id)
			}
			if o.Mark == low && e.Disposition == "ignored" {
				if j.DecidedBy != "classifier" || len(j.Levels) != 1 {
					t.Fatalf("%s 应被分类端点直接按否定案: %+v", e.Subject, j)
				}
				lowDirect++
			}
		}
		if checked == 0 || lowDirect == 0 {
			t.Fatalf("没有核对到重判过的旧事件: checked=%d lowDirect=%d", checked, lowDirect)
		}
	})

	k.step("ACT-BKL-14 全部放完后积压里只剩本人留下的", func(t *testing.T) {
		var list struct{ Total int }
		k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "source", "backlog", "list").Stdout), &list))
		count, _, _ := k.unjudgedHealth()
		if list.Total != 0 || count != 0 {
			t.Fatalf("积压 %d 条，health count=%d", list.Total, count)
		}
	})
}

// TestBacklogReleaseWithoutJudgeEndpoint 是 flow-backlog-release 的后半（ACT-BKL-15..20）：没有判断端点、端点位置为外部且来源外发范围未选。
// 与前半一样各自用一份新的夹具副本和一套新的替身；放开重判的每日上限与前半相同。
func TestBacklogReleaseWithoutJudgeEndpoint(t *testing.T) {
	k := newKit(t, nil, false)
	k.step("ACT-BKL-15 准备没有判断端点的第二份副本", func(t *testing.T) {
		k.wireGenerateOnly()
		yes := true
		k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: hi}, Answer: "yes", Sufficient: &yes})
		k.ModelStub.Judge(modelstub.ChatJudge{Answer: "no", Sufficient: &yes})
		k.ModelStub.SetDefault(modelstub.Reply{Content: draftReply})
		var res struct{ Attached, Disposed, Remaining int }
		k.ownerOK("本人：先只跑规则", &res, "source", "backlog", "rules")
		if res.Attached != k.manifestInt("attachable") || res.Disposed != k.manifestInt("structured_rule_hits") {
			t.Fatalf("只跑规则: %+v", res)
		}
		var ep map[string]any
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/model-endpoints", "", 200)), &ep))
		if ep["judge_endpoint"] != nil || ep["generate_endpoint"] == nil {
			t.Fatalf("设置页应显示判断端点未配置、生成端点已配置: %v", ep)
		}
		k.clear()
	})

	k.step("ACT-BKL-16 没有判断端点时影子打分被拒", func(t *testing.T) {
		before := len(k.allJudgments())
		r := k.owner("本人：打分", "source", "backlog", "score")
		if r.Code != 6 || !strings.Contains(r.Stdout+r.Stderr, "judge endpoint") {
			t.Fatalf("score 退出码 %d，期望 6（HTTP 409）并说明没有判断端点\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		if got := k.APIRaw("POST", "/v1/source-events/score", "{}", k.DEL); got.Code == 200 {
			t.Fatalf("不带原话的 HTTP 打分不应成功: %d", got.Code)
		}
		if len(k.allJudgments()) != before || len(k.eventRequests()) != 0 {
			t.Fatalf("被拒的打分写了判断记录或调了模型: 记录 %d→%d 请求 %d", before, len(k.allJudgments()), len(k.modelLog()))
		}
	})

	k.step("ACT-BKL-17 没有判断端点时由对话端点逐条回答同一道初筛题并受上限约束", func(t *testing.T) {
		before := k.proposed()
		order := k.backlog()
		res := k.rejudge()
		fresh := k.freshProposals(before)
		if len(fresh) != perDay {
			t.Fatalf("今天新增建议 %d 条，期望恰 %d（%+v）", len(fresh), perDay, res)
		}
		if res.Deferred != len(k.backlog()) || res.Deferred == 0 {
			t.Fatalf("其余应 deferred: %+v 积压 %d", res, len(k.backlog()))
		}
		reqs := k.judgeRequests()
		if len(reqs) == 0 {
			t.Fatal("对话端点没有收到判断题")
		}
		var seen []string
		for _, r := range reqs {
			if n := strings.Count(r.BodyText, "triage-screen") + strings.Count(plain(r.BodyText), "sev_"); n == 0 {
				t.Fatalf("判断请求里没有这道初筛题: %s", r.BodyText)
			}
			ids := sevIDs(plain(r.BodyText))
			if len(ids) != 1 {
				t.Fatalf("每个判断请求只含一道题（一个事件），实际 %v", ids)
			}
			seen = append(seen, ids[0])
		}
		rank := map[string]int{}
		for i, e := range order {
			rank[e.ID] = i
		}
		var inLedgerOrder []string
		for _, id := range seen {
			inLedgerOrder = append(inLedgerOrder, id)
		}
		if !slices.IsSortedFunc(inLedgerOrder, func(a, b string) int { return cmp.Compare(a, b) }) {
			t.Fatalf("处理顺序不是落盘顺序: %v", inLedgerOrder)
		}
		for _, e := range fresh {
			j, ok := k.decision(e.ID)
			answered := 0 // 没有判断端点时分类端点一级记为 skipped（设计 §8.10 levels[].outcome），真正作答的只有对话端点
			for _, l := range j.Levels {
				if l.Outcome != "skipped" {
					answered++
					if l.Backend != "chat" {
						answered += 100
					}
				}
			}
			if !ok || answered != 1 || j.DecidedBy != "chat" {
				t.Fatalf("%s 的判断记录只应有对话端点一级作答: %+v", e.Subject, j)
			}
		}
		for _, e := range k.all("ignored") {
			if !strings.Contains(e.Subject, mid) && !strings.Contains(e.Subject, low) {
				continue
			}
			if j, ok := k.decision(e.ID); ok && j.DecidedBy == "chat" && j.Answer == "no" && e.Disposition != "ignored" {
				t.Fatalf("答否的事件应被忽略: %+v", e)
			}
		}
	})

	k3 := newKit(t, nil, false)
	k3.step("ACT-BKL-18 把端点改为外部并保持来源外发范围未选", func(t *testing.T) {
		k3.wire("external", "external", 0)
		k3.cases()
		for _, s := range k3.sources() {
			var sh struct {
				Outbound struct {
					Local    string  `json:"local"`
					External *string `json:"external"`
				} `json:"outbound"`
			}
			k3.get("/v1/sources/"+s, &sh)
			if sh.Outbound.External != nil {
				t.Fatalf("来源 %s 的外部外发范围应为未选择: %+v", s, sh)
			}
		}
		var res struct{ Attached, Disposed, Remaining int }
		k3.ownerOK("本人：先只跑规则", &res, "source", "backlog", "rules")
		k3.clear()
	})

	k3.step("ACT-BKL-19 外发范围未选时重判记为 outbound_unset 而不送出", func(t *testing.T) {
		before := k3.proposed()
		k3.owner("本人：打分", "source", "backlog", "score")
		k3.rejudge()
		for _, b := range k3.clsLog() {
			if carriesEvent(b.BodyText) {
				t.Fatalf("来源事件内容被发往外部判断端点: %s", b.BodyText)
			}
		}
		for _, r := range k3.modelLog() {
			if carriesEvent(r.BodyText) {
				t.Fatalf("来源事件内容被发往外部生成端点: %s", r.BodyText)
			}
		}
		unset := 0
		for _, e := range k3.backlog() {
			if e.Unjudged == "outbound_unset" {
				unset++
			}
		}
		_, detail, _ := k3.unjudgedHealth()
		if unset == 0 || num(detail, "outbound_unset") != unset {
			t.Fatalf("outbound_unset 事件 %d 条，health detail=%v", unset, detail)
		}
		if got := len(k3.freshProposals(before)); got != 0 {
			t.Fatalf("没有新建议才对，实际 %d", got)
		}
		for _, e := range k3.all("ignored") {
			if _, ok := k3.decision(e.ID); ok && has(e.Subject, "[FX-") {
				t.Fatalf("事件被忽略却没有放行判断: %+v", e)
			}
		}
	})
}

// allJudgments 是全部判断记录。
func (k *kit) allJudgments() (out []judgmentView) {
	k.t.Helper()
	for after := ""; ; {
		var page struct {
			Judgments []judgmentView
			NextAfter string `json:"next_after"`
		}
		k.get("/v1/judgments?limit=500&after="+after, &page)
		out = append(out, page.Judgments...)
		if page.NextAfter == "" {
			return
		}
		after = page.NextAfter
	}
}

var sevRe = regexp.MustCompile(`sev_[0-9a-f]+`)

// sevIDs 是文本里提到的来源事件 id（去重、保持出现顺序）。
func sevIDs(s string) (out []string) {
	for _, id := range sevRe.FindAllString(s, -1) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return
}

// plain 把 JSON 请求体里的转义还原后再比对。
func plain(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(t + "\n")
			var inner any
			if len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Unmarshal([]byte(t), &inner) == nil {
				walk(inner)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				sb.WriteString(k + "\n")
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}

// eventByID 读一个来源里的一条事件（全部处置）。
func (k *kit) eventByID(src, id string) srcEvent {
	k.t.Helper()
	for _, e := range k.events(src, "pending,unjudged,expired,proposed,ignored,attached", "") {
		if e.ID == id {
			return e
		}
	}
	k.t.Fatalf("来源 %s 没有事件 %s", src, id)
	return srcEvent{}
}
