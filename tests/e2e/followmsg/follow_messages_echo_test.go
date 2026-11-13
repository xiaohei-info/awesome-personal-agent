//go:build e2e

package followmsg

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// webItem 是 Web 事项详情用到的字段（GET /v1/items/{id}）。
type webItem struct {
	Title       string                      `json:"title"`
	Body        string                      `json:"body"`
	Phase       string                      `json:"phase"`
	WaitingOn   *struct{ Kind, Ref string } `json:"waiting_on"`
	Correlation []struct {
		Key    string `json:"key"`
		Domain string `json:"domain"`
	} `json:"-"`
	Proposal *struct {
		Sources []string `json:"sources"`
	} `json:"proposal"`
}

func (k *kit) webItem(id string) webItem {
	k.t.Helper()
	var out struct {
		Item        webItem `json:"item"`
		Correlation []struct {
			Key    string `json:"key"`
			Domain string `json:"domain"`
		} `json:"correlation_keys"`
	}
	if err := json.Unmarshal([]byte(k.webOK("GET", "/v1/items/"+id, "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	out.Item.Correlation = out.Correlation
	return out.Item
}

func (it webItem) hasKey(key string) bool {
	for _, c := range it.Correlation {
		if c.Key == key {
			return true
		}
	}
	return false
}

// timerExists 读账本里的定时表（只读），判断 id 对应的定时是否还在。
func (k *kit) timerExists(id string) bool {
	k.t.Helper()
	out, err := exec.Command("sqlite3", "-readonly", filepath.Join(k.Home, "ledger.db"),
		"SELECT count(*) FROM clock_timer WHERE id='"+id+"'").Output()
	if err != nil {
		k.t.Fatalf("读 clock_timer: %v", err)
	}
	return strings.TrimSpace(string(out)) == "1"
}

// clsSaw 报告 CLS_STUB 是否收到过含 s 的判断题。
func (k *kit) clsSaw(s string) bool {
	for _, b := range k.Cls.Batches() {
		if strings.Contains(b.BodyText, s) {
			return true
		}
	}
	return false
}

// newWebItem 在 Web 新建一个事项（直接进 open），返回 id。
func (k *kit) newWebItem(title string) string {
	k.t.Helper()
	b, _ := json.Marshal(map[string]any{"kind": "task", "title": title, "body": "原始正文 " + title,
		"project": k.Projects["acc-open"], "phase": "open"})
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(k.webOK("POST", "/v1/items", string(b), 201)), &out); err != nil || out.ID == "" {
		k.t.Fatal("Web 新建事项没有返回 id")
	}
	return out.ID
}

func (k *kit) webWaiting(id, kind, ref string) {
	k.t.Helper()
	b, _ := json.Marshal(map[string]any{"to": "waiting", "reason": "e2e 等回音", "waiting_on": map[string]string{"kind": kind, "ref": ref}})
	k.webOK("POST", "/v1/items/"+id+"/transition", string(b), 200)
}

// echoSteps 接 ACT-MSG-09，继续 flow-follow-messages 的 ACT-MSG-10..18（v0.2：本人事项的回音、关联键登记、清理）。
func echoSteps(k *kit) {
	// 判断型的 fm-14、fm-15 要经初筛：判断端点是 CLS_STUB，生成端点仍是 MODEL_STUB。
	k.WireEndpoints()
	k.Cls.Reset()
	for _, s := range []string{"fm-14", "fm-15"} {
		k.Cls.PYes(modelstub.Key{Contains: s}, 0.05)
	}

	var landlord, quote, plan string
	var echoSeq int64

	k.step("ACT-MSG-10 等外部回音的本人事项收到回音后回到 open", func(t *testing.T) {
		landlord = k.newWebItem("等房东确认维修时间")
		k.webWaiting(landlord, "external", "房东")
		followup := "followup_" + landlord
		if !k.timerExists(followup) {
			t.Fatal("waiting 的外部等待应有跟进定时")
		}
		r := k.Apa(k.DEL, "", "item", "relate", landlord, "--key", "fm-thread-1", "--evidence", "本人原话：房东的回复归到这件事")
		k.expect("item relate --key（直接会话）", r, 0)
		if !k.webItem(landlord).hasKey("fm-thread-1") {
			t.Fatal("事项详情应显示登记的关联键")
		}
		before, batches := k.itemCount(), len(k.Cls.Batches())

		id := k.emit("acc.reply", "周三上午可以", "fm-thread-1")
		k.waitFor("回音被处置", 20*time.Second, func() bool { e, ok := k.event(id); return ok && e.Disposition != "pending" })
		ev, _ := k.event(id)
		if ev.Disposition != "attached" || ev.AttachState != "processed" || ev.Item != landlord {
			t.Fatalf("回音应挂靠且 processed: %+v", ev)
		}
		it := k.webItem(landlord)
		if it.Phase != "open" || it.Title != "等房东确认维修时间" || it.Body != "原始正文 等房东确认维修时间" || it.WaitingOn != nil {
			t.Errorf("事项应回到 open 且标题、正文不变: %+v", it)
		}
		changes := k.audit("item.phase_changed", landlord)
		last := changes[len(changes)-1]
		if last.Actor.Type != "system" || !strings.Contains(last.Reason, "收到回音") || !strings.Contains(last.Reason, ev.ID) {
			t.Errorf("phase_changed 应由 system 记录并写明收到回音与来源事件: %+v", last)
		}
		echoSeq = last.Seq
		if k.timerExists(followup) {
			t.Error("未到期的跟进定时应取消")
		}
		if k.itemCount() != before {
			t.Errorf("不应生成新建议: %d → %d", before, k.itemCount())
		}
		if len(k.Cls.Batches()) != batches || k.stubRequestsAbout("周三上午可以") != 0 || k.clsSaw("周三上午可以") {
			t.Error("回音不进规则与判断，两份模型替身都不应有 fm-10 的请求")
		}
	})

	k.step("ACT-MSG-11 回到 open 的系统转移可撤销", func(t *testing.T) {
		k.webOK("POST", "/v1/items/"+landlord+"/revert", `{"event":`+itoa(echoSeq)+`}`, 200)
		it := k.webItem(landlord)
		if it.Phase != "waiting" || it.WaitingOn == nil || it.WaitingOn.Kind != "external" || it.WaitingOn.Ref != "房东" {
			t.Fatalf("撤销后应回到 waiting 且 waiting_on 不变: %+v", it)
		}
		changes := k.audit("item.phase_changed", landlord)
		if last := changes[len(changes)-1]; last.Actor.Type != "human" || last.Seq <= echoSeq {
			t.Errorf("撤销应记一条本人操作的 Event: %+v", last)
		}
		found := false
		for _, e := range k.events() {
			found = found || (e.Subject == "周三上午可以" && e.Item == landlord && e.Disposition == "attached")
		}
		if !found {
			t.Error("撤销不应丢失挂靠消息")
		}
	})

	k.step("ACT-MSG-12 等时间的事项收到回音只挂靠不转阶段", func(t *testing.T) {
		quote = k.newWebItem("下月再看报价")
		until := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
		k.webWaiting(quote, "time", until)
		k.expect("relate fm-thread-2", k.Apa(k.DEL, "", "item", "relate", quote, "--key", "fm-thread-2", "--evidence", "本人原话：报价的回复归到这件事"), 0)
		if !k.timerExists("wait_" + quote) {
			t.Fatal("等时间的事项应有到点定时")
		}
		id := k.emit("acc.reply", "报价更新了", "fm-thread-2")
		k.waitFor("回音被处置", 20*time.Second, func() bool { e, ok := k.event(id); return ok && e.Disposition != "pending" })
		ev, _ := k.event(id)
		if ev.Item != quote || ev.AttachState != "processed" {
			t.Fatalf("应挂到该事项且 processed: %+v", ev)
		}
		if it := k.webItem(quote); it.Phase != "waiting" || it.WaitingOn == nil || it.WaitingOn.Kind != "time" {
			t.Errorf("等时间的事项仍应 waiting: %+v", it)
		}
		if !k.timerExists("wait_" + quote) {
			t.Error("到点定时不应变")
		}
	})

	k.step("ACT-MSG-13 命中 proposed 事项时只更新来源数，不提醒", func(t *testing.T) {
		plan = k.idOf("propose", k.Apa(k.DEL, "", "propose", "--kind", "task", "--title", "换宽带套餐", "--project", k.Projects["acc-open"]))
		k.expect("relate fm-thread-3", k.Apa(k.DEL, "", "item", "relate", plan, "--key", "fm-thread-3", "--evidence", "本人原话：这条回复归到换套餐"), 0)
		items := k.itemCount()
		id := k.emit("acc.reply", "套餐有优惠", "fm-thread-3")
		k.waitFor("回音被处置", 20*time.Second, func() bool { e, ok := k.event(id); return ok && e.Disposition != "pending" })
		ev, _ := k.event(id)
		it := k.webItem(plan)
		if ev.Item != plan || it.Phase != "proposed" || it.Proposal == nil || !containsStr(it.Proposal.Sources, ev.ID) {
			t.Fatalf("建议仍应 proposed 且来源含 %s: ev=%+v item=%+v", ev.ID, ev, it)
		}
		if k.itemCount() != items || k.stubRequestsAbout("套餐有优惠") != 0 {
			t.Error("不应新建建议或调用模型")
		}
		if r := k.APIRaw("GET", "/v1/deliveries/next?wait=0", "", k.CHAN); r.Code != 204 {
			t.Errorf("命中 proposed 事项不提醒，渠道不应有投递: %d %s", r.Code, r.Body)
		}
	})

	k.step("ACT-MSG-14 移除关联键后不再归属", func(t *testing.T) {
		k.expect("relate --remove", k.Apa(k.DEL, "", "item", "relate", landlord, "--key", "fm-thread-1", "--remove", "--evidence", "本人原话：解除关联"), 0)
		if k.webItem(landlord).hasKey("fm-thread-1") {
			t.Fatal("事项详情不应再显示该键")
		}
		id := k.emit("acc.reply", "fm-14 解除关联后的来信", "fm-thread-1")
		ev := k.settled(id)
		if ev.Disposition != "ignored" || ev.Item == landlord {
			t.Errorf("fm-14 应走普通流水线并被初筛忽略: %+v", ev)
		}
		if !k.clsSaw("fm-14") {
			t.Error("fm-14 应经过初筛")
		}
	})

	k.step("ACT-MSG-15 关联键按去重域隔离", func(t *testing.T) {
		k.expect("relate other-domain", k.Apa(k.DEL, "", "item", "relate", quote, "--key", "fm-thread-4", "--domain", "other-domain", "--evidence", "本人原话：另一个域的线程"), 0)
		id := k.emit("acc.reply", "fm-15 异域来信", "fm-thread-4")
		ev := k.settled(id)
		if ev.Item == quote || ev.Disposition == "attached" {
			t.Errorf("跨去重域不应命中: %+v", ev)
		}
		if ev.Disposition != "ignored" {
			t.Errorf("fm-15 应走普通流水线被初筛忽略: %+v", ev)
		}
	})

	k.step("ACT-MSG-16 Web 事项详情登记关联来源线程", func(t *testing.T) {
		before := len(k.audit("item.updated", quote))
		k.webOK("POST", "/v1/items/"+quote+"/correlation-keys", `{"key":"fm-thread-5"}`, 200)
		if !k.webItem(quote).hasKey("fm-thread-5") {
			t.Fatal("事项详情应显示 fm-thread-5")
		}
		updated := k.audit("item.updated", quote)
		if len(updated) != before+1 || updated[len(updated)-1].Actor.Type != "human" {
			t.Errorf("应有操作者为本人的 item.updated: %+v", updated)
		}
		id := k.emit("acc.reply", "fm-16 来信", "fm-thread-5")
		k.waitFor("回音被处置", 20*time.Second, func() bool { e, ok := k.event(id); return ok && e.Disposition != "pending" })
		if ev, _ := k.event(id); ev.Item != quote || ev.Disposition != "attached" {
			t.Errorf("fm-16 应挂到该事项: %+v", ev)
		}
	})

	k.step("ACT-MSG-17 依据不足或凭据不够时不能登记关联键", func(t *testing.T) {
		k.expect("OBS relate", k.Apa(k.OBS, "", "item", "relate", quote, "--key", "fm-x"), exitDenied)
		k.expect("DEL 无依据 relate", k.Apa(k.DEL, "", "item", "relate", quote, "--key", "fm-y"), 23)
		it := k.webItem(quote)
		if it.hasKey("fm-x") || it.hasKey("fm-y") {
			t.Errorf("被拒绝的登记不应出现在详情: %+v", it.Correlation)
		}
	})

	k.step("ACT-MSG-18 清理", func(t *testing.T) {
		k.webOK("PATCH", "/v1/sources/"+srcID, `{"enabled":false}`, 200)
		k.must(os.RemoveAll(filepath.Join(k.Home, "packs", "acc-inbound")))
		k.StopKernel()
		k.StartKernelAgain()
		r := k.Apa(k.OBS, "", "pack", "list")
		k.expect("pack list", r, 0)
		if strings.Contains(r.Stdout, "acc-inbound") {
			t.Errorf("pack list 不应再有验收能力包: %s", r.Stdout)
		}
	})
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
