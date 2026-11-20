//go:build e2e

package rawlifecycle

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var hi = 0.9

// eventByID 读某来源列表里的一条事件（当前口径）。
func (k *kit) eventByID(src, id string) (out listed) {
	for _, e := range k.events(src) {
		if e.ID == id {
			out = e
		}
	}
	return
}

// retentionDays 读来源登记里的 content_retention_days（空为不清理）。
func (k *kit) retentionDays(id string) *int {
	k.t.Helper()
	var s struct {
		Days *int `json:"content_retention_days"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "source", "show", id).Stdout), &s); err != nil {
		k.t.Fatal(err)
	}
	return s.Days
}

// TestFlowRawRetention 是验收 flow-raw-retention（ACT-RET-01…03）：保留天数只在 Web 设置；
// 测试时钟推进后，后台维护循环清掉超过天数且不挂未关闭事项的原文，事件行留元信息，取回的附件一并删除，挂在未关闭事项上的保留；
// 没设天数的来源不清理。
func TestFlowRawRetention(t *testing.T) {
	k := setup(t)
	var e1, e2, item, rawEarliest, metaEarliest listed
	var e1Blob string

	k.step("准备三个来源：acc-raw（不设保留）、acc-raw-ex（E1、E2）、acc-raw-meta", func(t *testing.T) {
		// 判断端点：E2 判为值得跟进（生成事项），其余判为忽略（没有事项）。
		k.WireEndpoints()
		k.Cls.SetDefaultPYes(0.05)
		k.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "Subject ret-e2"}, PYes: &hi})
		k.addStub("src_acc_raw", "mail", "full", "full")
		k.addStub("src_acc_raw_meta", "mail", "full", "meta")
		k.addStub("src_acc_raw_ex", "mail", "full", "excerpt")
		k.setJSONL("src_acc_raw_ex", []map[string]any{k.mailLike("ret-e1"), k.mailLike("ret-e2")}, "ret-ex")
		k.waitBatch("src_acc_raw_ex", "ret-ex")
		e1, e2 = k.eventByStubID("src_acc_raw_ex", "ret-e1"), k.eventByStubID("src_acc_raw_ex", "ret-e2")
		rawEarliest, metaEarliest = k.eventByStubID("src_acc_raw", "mail-like-1"), k.eventByStubID("src_acc_raw_meta", "mail-like-1")
	})

	k.step("ACT-RET-01 保留天数只在 Web 设置", func(t *testing.T) {
		before := len(k.audit("source.updated", "src_acc_raw_ex"))
		eq(t, "设置前", k.retentionDays("src_acc_raw_ex") == nil, true)
		if got := k.web("PUT", "/v1/settings/sources/src_acc_raw_ex/content-retention", `{"days": 1}`, 200); !strings.Contains(got, `"days":1`) {
			t.Errorf("应读回 days=1: %s", got)
		}
		if d := k.retentionDays("src_acc_raw_ex"); d == nil || *d != 1 {
			t.Errorf("来源登记的 content_retention_days 应为 1: %v", d)
		}
		if n := len(k.audit("source.updated", "src_acc_raw_ex")); n != before+1 {
			t.Errorf("设置应记一条 Event（source.updated）：%d → %d", before, n)
		}
		// 保留期是 3a（2026-11-20）：档位未放开的 delegate（DELO）得 23，observer 与 worker 在范围外，都是 403。
		for name, tok := range map[string]string{"DELO": k.DELO, "OBS": k.OBS, "WRK": k.WRK} {
			if r := k.APIRaw("PUT", "/v1/settings/sources/src_acc_raw_ex/content-retention", `{"days": 7}`, tok); r.Code != 403 {
				t.Errorf("%s 设保留天数应为 403: %d %s", name, r.Code, r.Body)
			}
		}
		if d := k.retentionDays("src_acc_raw_ex"); d == nil || *d != 1 {
			t.Errorf("被拒的调用不应改动保留天数: %v", d)
		}
		if got := k.web("PUT", "/v1/settings/sources/src_acc_raw_meta/content-retention", `{"days": null}`, 200); !strings.Contains(got, `"days":null`) {
			t.Errorf("null 应读回为空: %s", got)
		}
		if k.retentionDays("src_acc_raw_meta") != nil {
			t.Error("acc-raw-meta 设 null 后应为不清理")
		}
	})

	k.step("ACT-RET-02 超过天数且不挂未关闭事项的原文被清，挂在未关闭事项上的保留", func(t *testing.T) {
		k.until("E2 生成事项、E1 没有", func() bool {
			item, e1 = k.eventByID("src_acc_raw_ex", e2.ID), k.eventByID("src_acc_raw_ex", e1.ID)
			return item.Item != "" && e1.Disposition == "ignored"
		})
		if e1.Item != "" {
			t.Fatalf("E1 不应有事项: %+v", e1)
		}
		k.web("POST", "/v1/items/"+item.Item+"/accept", `{}`, 200) // 事项 open
		code, body := k.fetch(e1.ID, 0)
		var got manifestItem
		if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got.Blob == "" {
			t.Fatalf("E1 取回附件: HTTP %d %s", code, body)
		}
		e1Blob = k.blobFile(got.Blob)
		if !exists(e1Blob) {
			t.Fatal("取回的附件文件应在")
		}
		e2Body := *k.readEvent("", e2.ID, "", 200).Body

		k.TCAdvance(2 * 24 * 3600)
		if !k.WaitFor(100*time.Second, func() bool { return k.readEvent("", e1.ID, "", 200).Content.State == "purged" }) {
			t.Fatalf("100 秒内维护循环没有清掉 E1\n%s", k.ServiceLog())
		}

		r1 := k.readEvent("", e1.ID, "", 200)
		eq(t, "E1 的 content.state", r1.Content.State, "purged")
		if r1.Body != nil || r1.Original != nil || len(r1.Headers) != 0 || len(r1.Attachments) != 0 || len(r1.Content.Parts) != 0 {
			t.Errorf("清除后 E1 不应有任何原文部分: %+v", r1)
		}
		if r1.Event.ID != e1.ID || r1.Event.Subject != "Subject ret-e1" {
			t.Errorf("清除后事件行的元信息应还在: %+v", r1.Event)
		}
		var row listed
		for _, e := range k.events("src_acc_raw_ex") {
			if e.ID == e1.ID {
				row = e
			}
		}
		if row.ID == "" || row.Type != "mail.received" || row.Subject == "" || row.Disposition == "" || row.Content.State != "purged" {
			t.Errorf("清除后事件行仍应有类型、标题与处置: %+v", row)
		}
		if exists(e1Blob) {
			t.Error("E1 取回的附件应已删除")
		}
		r2 := k.readEvent("", e2.ID, "", 200)
		if r2.Content.State != "full" || r2.Body == nil || *r2.Body != e2Body || r2.Original == nil || len(r2.Attachments) == 0 {
			t.Errorf("挂在未关闭事项上的 E2 原文应完整: %+v", r2)
		}
		a := k.audit("source.content_purged", "src_acc_raw_ex")
		if len(a) != 1 {
			t.Fatalf("本轮应对 acc-raw-ex 记一条 source.content_purged，实际 %d 条", len(a))
		}
		var p struct {
			Events int   `json:"events"`
			Bytes  int64 `json:"bytes"`
		}
		if err := json.Unmarshal(a[0].Payload, &p); err != nil || p.Events != 1 || p.Bytes <= 0 {
			t.Errorf("审计事件应写明条数 1 与字节数: %s", a[0].Payload)
		}
		if evs := k.web("GET", "/v1/events", "", 200); strings.Contains(evs, "source.content_purged") {
			t.Error("source.content_purged 是内部审计事件，不应出现在 GET /v1/events")
		}
	})

	k.step("ACT-RET-03 未设保留天数的来源不清理", func(t *testing.T) {
		for name, e := range map[string]listed{"acc-raw": rawEarliest, "acc-raw-meta": metaEarliest} {
			r := k.readEvent("", e.ID, "", 200)
			if r.Content.State != "full" || r.Body == nil || *r.Body == "" || r.Original == nil {
				t.Errorf("%s 的最早事件原文应完整: %+v", name, r.Content)
			}
		}
		if n := len(k.audit("source.content_purged", "")); n != 1 {
			t.Errorf("全账本只应有 acc-raw-ex 那一条清除记录，实际 %d", n)
		}
	})
}
