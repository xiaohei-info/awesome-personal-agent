//go:build e2e

package delivery

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDLV01 对应 ACT-DLV-01：普通决策请求按路由产出投递，渠道 ext 领到的内容完整且不带来源正文。
func TestDLV01(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.setDisturb(disturb{Routes: route(nil)})

	const secretBody = "SECRET-SOURCE-BODY-不得出现在投递里"
	id := f.task("dlv-01", "--body", secretBody)
	f.ask(id)

	d := f.claimWithin(settle)
	if d == nil {
		t.Fatalf("CHAN %s 内没有领到投递", settle)
	}
	if d.Class != "decision" || d.Item != id || d.Route != "channel:acc-chan" {
		t.Errorf("投递归属不对: %+v", d)
	}
	c := d.Content
	if c.Title != "dlv-01" || c.Summary != "选哪个方案" || len(c.Options) != 2 || c.Recommend != "A" || c.WebURL == "" || d.Version < 1 {
		t.Errorf("投递缺字段（标题、摘要、选项、推荐、Web 链接、版本号）: %s", d.Raw)
	}
	if strings.Contains(d.Raw, secretBody) {
		t.Errorf("投递带了来源正文: %s", d.Raw)
	}
}

// TestDLV02 对应 ACT-DLV-02：静默中不投递，静默结束后由时钟补发。
func TestDLV02(t *testing.T) {
	t.Parallel()
	f := setup(t)
	end := time.Now().Add(2 * time.Minute).Truncate(time.Minute) // 静默到下一个整分钟，距现在 1~2 分钟
	f.setDisturb(disturb{Routes: route(nil), Quiet: quietAround(end)})

	id := f.task("dlv-02")
	f.ask(id)

	if d := f.claimWithin(settle); d != nil {
		t.Fatalf("静默中送出了投递: %+v", d)
	}
	got := f.claimUntil(end.Sub(time.Now())+90*time.Second, func(g []*delivery) bool { return len(g) > 0 })
	d := has(got, func(d *delivery) bool { return d.Item == id && d.Class == "decision" })
	if d == nil {
		t.Fatal("静默结束后投递丢失，没有被时钟补发")
	}
	if time.Now().Before(end) {
		t.Errorf("在静默结束 %s 之前就送出了投递", end.Format(time.RFC3339))
	}
}

// TestDLV03 对应 ACT-DLV-03：默认静默优先；开启临近截止豁免后，静默中也送达。
func TestDLV03(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.setDisturb(disturb{Routes: route(nil), Quiet: quietAround(time.Now().Add(2 * time.Hour))})
	deadline := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)

	before := f.task("dlv-03-before")
	f.ask(before, "--deadline", deadline)
	if d := f.claimWithin(settle); d != nil {
		t.Fatalf("未开豁免却绕过了静默: %+v", d)
	}

	f.setDisturb(disturb{Routes: route(nil), Quiet: quietAround(time.Now().Add(2 * time.Hour)), QuietExempt: true})
	after := f.task("dlv-03-after")
	f.ask(after, "--deadline", deadline)
	got := f.claimUntil(settle, func(g []*delivery) bool {
		return has(g, func(d *delivery) bool { return d.Item == after }) != nil
	})
	if has(got, func(d *delivery) bool { return d.Item == after && d.Class == "decision" }) == nil {
		t.Error("开启豁免后，临近截止的决策没有在静默时段内送达")
	}
}

// TestDLV04 对应 ACT-DLV-04：窗口内三条低优先级提醒合并为一条摘要，带选项的决策单独投递。
// 提醒来自"决策截止已过仍未处理"的再提醒（§7.4），三条低优先级事项同一时刻到期。
func TestDLV04(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.setDisturb(disturb{Routes: route(map[string]string{"reminder": "acc-chan"}), MergeWindowSec: 120, RemindOnDeadline: true})

	deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	titles := []string{"dlv-04-a", "dlv-04-b", "dlv-04-c"}
	for _, ti := range titles {
		f.ask(f.task(ti, "--priority", "low"), "--deadline", deadline)
	}
	choice := f.task("dlv-04-choice")
	f.ask(choice)

	isDigest := func(d *delivery) bool { return d.Class == "reminder" && strings.HasPrefix(d.ID, "digest:") }
	got := f.claimUntil(5*time.Minute, func(g []*delivery) bool { return has(g, isDigest) != nil })
	digest := has(got, isDigest)
	if digest == nil {
		t.Fatalf("合并窗口结束后没有摘要投递；领到 %d 条", len(got))
	}
	for _, ti := range titles {
		if !strings.Contains(digest.Content.Summary, "决策仍待回答："+ti) {
			t.Errorf("摘要缺少 %s: %s", ti, digest.Raw)
		}
	}
	// 再等一个时钟周期，确认没有逐条发送的提醒漏出来。
	got = append(got, f.claimUntil(40*time.Second, func([]*delivery) bool { return false })...)
	reminders, decisions := 0, map[string]bool{}
	for _, d := range got {
		switch d.Class {
		case "reminder":
			reminders++
		case "decision":
			decisions[d.Item] = true
		}
	}
	if reminders != 1 {
		t.Errorf("提醒应只有一条摘要，实际 %d 条", reminders)
	}
	if !decisions[choice] {
		t.Error("带选项的决策没有单独投递")
	}
}

// TestDLV05 对应 ACT-DLV-05：高优先级提醒路由到渠道，低优先级走注入。
func TestDLV05(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.setDisturb(disturb{Routes: route(map[string]string{"decision": "web", "reminder:high": "acc-chan", "reminder:low": "inject"}), RemindOnDeadline: true})

	deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	hi, lo := f.task("dlv-05-hi", "--priority", "high"), f.task("dlv-05-lo", "--priority", "low")
	f.ask(hi, "--deadline", deadline)
	f.ask(lo, "--deadline", deadline)

	got := f.claimUntil(2*time.Minute, func(g []*delivery) bool { return has(g, func(d *delivery) bool { return d.Item == hi }) != nil })
	if d := has(got, func(d *delivery) bool { return d.Item == hi && d.Class == "reminder" }); d == nil {
		t.Fatal("高优先级提醒没有被渠道领取")
	}
	if has(got, func(d *delivery) bool { return d.Item == lo }) != nil {
		t.Error("低优先级提醒被渠道领取了，应该走注入")
	}
	if d := f.claim(2); d != nil && d.Item == lo {
		t.Errorf("低优先级提醒被渠道领取了: %+v", d)
	}
	inject := f.apa(f.DEL, 0, "context", "--session", "s-dlv-05", "--scope", "project", "--project", f.Projects["acc-work"], "--format", "inject").Stdout
	if !strings.Contains(inject, "dlv-05-lo") {
		t.Errorf("低优先级提醒没有出现在会话注入里: %s", inject)
	}
}

// TestDLV07 对应 ACT-DLV-07：事项变化后旧投递作废，渠道领不到旧版本，也不能再确认它；
// 本人回答的那条（当前版本）投递记 acknowledged，不记 superseded（§6.9"回答即确认"）。
func TestDLV07(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.setDisturb(disturb{Routes: route(nil)})

	id := f.task("dlv-07")
	f.ask(id)
	// 等触达把投递写进队列（尚未被领取时它在健康视图的投递积压里，id 为 decision:<事项>:<版本>）。
	old := f.waitBacklog("decision:" + id + ":")
	ver := old[strings.LastIndex(old, ":")+1:]

	// 本人（DEL）改了事项的无关字段，待决决策以更高版本重发，old 成为低于当前版本的旧投递。
	f.apa(f.DEL, 0, "item", "update", id, "--title", "dlv-07-v2", "--reason", "改名", "--evidence", "事项变化")
	cur := f.waitBacklog("decision:" + id + ":")
	for end := time.Now().Add(settle); cur == old && time.Now().Before(end); time.Sleep(time.Second) {
		cur = f.waitBacklog("decision:" + id + ":")
	}
	if cur == old {
		t.Fatalf("再次 ask 后没有产生更高版本的投递，仍是 %s", old)
	}
	if strings.Contains(f.apa(f.OBS, 0, "health").Stdout, `"`+old+`"`) {
		t.Errorf("作废的投递 %s 仍在投递积压里", old)
	}
	if r := f.APIRaw("POST", "/v1/deliveries/"+old+"/ack", `{"version":`+ver+`}`, f.CHAN); r.Code != http.StatusConflict {
		t.Errorf("确认旧版本投递应为 409，实际 %d %s", r.Code, r.Body)
	}
	if d := f.claimWithin(settle); d == nil || d.ID == old {
		t.Fatalf("渠道应只领到当前版本投递 %s，实际 %+v", cur, d)
	}
	for _, d := range f.unread() {
		if d.ID == old {
			t.Errorf("作废的投递不应出现在 Web 未读: %+v", d)
		}
	}

	// 本人回答当前版本：该投递转 acknowledged，不是 superseded。
	f.webOK("POST", "/items/"+id+"/decide", `{"choice":"A"}`, http.StatusOK)
	if d := f.claimWithin(settle); d != nil {
		t.Fatalf("事项已回答，渠道还领到了投递: %+v", d)
	}
	if r := f.APIRaw("POST", "/v1/deliveries/"+cur+"/ack", `{"version":`+cur[strings.LastIndex(cur, ":")+1:]+`}`, f.CHAN); r.Code == http.StatusConflict {
		t.Errorf("被本人回答的投递应为 acknowledged 而不是 superseded，ack 却回 409: %s", r.Body)
	}
}

// TestDLV06 对应 ACT-DLV-06：路由指向没有运行的渠道，无人领取超限后退回 Web 未读（fallback_web）。
// 按验收文档把时限调小：config.yaml 的 delivery_unclaimed_after 取 20 秒（默认 30 分钟）。
func TestDLV06(t *testing.T) {
	t.Parallel()
	e := baselineNew(t)
	f := &fx{e, t}
	e.Init()
	e.ServeWith("delivery_unclaimed_after: 20s\ndelivery_backoff: 1s,2s\n")
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	f.setDisturb(disturb{Routes: route(map[string]string{"decision": "acc-dead"})})

	id := f.task("dlv-06")
	f.ask(id)

	var got *delivery
	for end := time.Now().Add(2 * time.Minute); got == nil && time.Now().Before(end); time.Sleep(2 * time.Second) {
		for _, d := range f.unread() {
			if d.Item == id {
				d := d
				got = &d
			}
		}
	}
	if got == nil {
		t.Fatal("无人领取超限后，投递没有出现在 Web 未读：投递消失")
	}
	if got.State != "fallback_web" || got.Class != "decision" || got.Content.Title != "dlv-06" {
		t.Errorf("退回 Web 未读的投递不对: %+v", got)
	}
}
