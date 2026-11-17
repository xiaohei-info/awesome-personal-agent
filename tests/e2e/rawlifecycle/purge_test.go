//go:build e2e

package rawlifecycle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// judgments 读某主体（来源事件）的判断记录条数。
func (k *kit) judgments(subject string) int {
	k.t.Helper()
	var out struct {
		Judgments []json.RawMessage `json:"judgments"`
	}
	if err := json.Unmarshal([]byte(k.web("GET", "/v1/judgments?subject="+subject, "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	return len(out.Judgments)
}

// TestFlowRawPurge 是验收 flow-raw-purge（ACT-PRG-01…03）：本人在 Web 物理删除一条来源事件（或连同事项一并删除），
// 单条读取 404、原文与取回的附件与判断记录都不在，去重键留墓碑使同一事件不再入账；删事项时勾选才连带来源事件。
// 页面上的确认弹窗由 web/src/features/purge 的组件测试覆盖，这里走它调用的同一个 POST /v1/settings/purge。
func TestFlowRawPurge(t *testing.T) {
	k := setup(t)
	var p1, p2, i1, i2 listed
	var p1Blob string
	var eventsBefore int

	k.step("准备 acc-raw：四封已判断的 mail-like 事件，各自生成事项", func(t *testing.T) {
		k.WireEndpoints()
		k.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "Subject prg-"}, PYes: &hi}) // 判为值得跟进：事件有判断记录并生成事项
		k.addStub("src_acc_raw", "mail", "full", "full")
		k.setJSONL("src_acc_raw", []map[string]any{k.mailLike("prg-p1"), k.mailLike("prg-p2"), k.mailLike("prg-i1"), k.mailLike("prg-i2")}, "prg-1")
		k.waitBatch("src_acc_raw", "prg-1")
		var got [4]*listed
		for n, s := range []string{"prg-p1", "prg-p2", "prg-i1", "prg-i2"} {
			e := k.eventByStubID("src_acc_raw", s)
			k.until(s+" 有事项", func() bool { e = k.eventByID("src_acc_raw", e.ID); return e.Item != "" })
			got[n] = &e
		}
		p1, p2, i1, i2 = *got[0], *got[1], *got[2], *got[3]
		eventsBefore = k.show("src_acc_raw").Stats.Events
	})

	k.step("ACT-PRG-01 在 Web 物理删除一条来源事件", func(t *testing.T) {
		if k.judgments(p1.ID) == 0 {
			t.Fatal("P1 应已有判断记录")
		}
		code, body := k.fetch(p1.ID, 0)
		var att manifestItem
		if code != 200 || json.Unmarshal([]byte(body), &att) != nil || att.Blob == "" {
			t.Fatalf("P1 取回附件: HTTP %d %s", code, body)
		}
		p1Blob = k.blobFile(att.Blob)
		if !exists(p1Blob) {
			t.Fatal("取回的附件文件应在")
		}
		k.apa(k.OBS, 0, "why", p1.ID)

		if r := k.APIRaw("POST", "/v1/settings/purge", `{"object":"`+p2.ID+`"}`, k.DEL); r.Code != 403 {
			t.Errorf("DEL 物理删除应为 403: %d %s", r.Code, r.Body)
		}
		k.readEvent("", p2.ID, "", 200)
		if r := k.Web("POST", "/v1/settings/purge", `{"object":"`+p1.ID+`","with_source_events":true}`); r.Code != 400 {
			t.Errorf("对来源事件带 with_source_events 应为 400: %d %s", r.Code, r.Body)
		}
		k.readEvent("", p1.ID, "", 200)

		k.web("POST", "/v1/settings/purge", `{"object":"`+p1.ID+`"}`, 200)
		k.readEvent("", p1.ID, "", 404)
		if exists(p1Blob) {
			t.Error("P1 取回的附件（无其他引用）应已删除")
		}
		eq(t, "P1 的判断记录条数", k.judgments(p1.ID), 0)
		if e := k.eventByID("src_acc_raw", p1.ID); e.ID != "" {
			t.Errorf("事件列表不应再有 P1: %+v", e)
		}
		if a := k.audit("purge.executed", p1.ID); len(a) != 1 {
			t.Errorf("应另记一条 purge.executed，实际 %d", len(a))
		}
		k.readEvent("", p2.ID, "", 200) // 其他事件不受影响
	})

	k.step("ACT-PRG-02 去重键墓碑阻止同一事件重新入账", func(t *testing.T) {
		k.setJSONL("src_acc_raw", []map[string]any{k.mailLike("prg-p1")}, "prg-2") // 同一事件 id 再来一次
		k.waitBatch("src_acc_raw", "prg-2")
		k.settle("src_acc_raw", 2)
		for _, e := range k.events("src_acc_raw") {
			if e.DedupeKey.EventID == "prg-p1" {
				t.Errorf("已删除的事件不应重新入账: %+v", e)
			}
		}
		for _, r := range k.rejects("src_acc_raw") {
			if strings.Contains(r.Raw, "prg-p1") {
				t.Errorf("已删除的事件不应进被拒收的事件: %+v", r)
			}
		}
		eq(t, "来源事件数", k.show("src_acc_raw").Stats.Events, eventsBefore-1)
	})

	k.step("ACT-PRG-03 删除事项时勾选才连同它的来源事件", func(t *testing.T) {
		body2 := *k.readEvent("", i2.ID, "", 200).Body
		k.web("POST", "/v1/settings/purge", `{"object":"`+i1.Item+`","with_source_events":true}`, 200)
		k.readEvent("", i1.ID, "", 404)
		k.web("POST", "/v1/settings/purge", `{"object":"`+i2.Item+`"}`, 200)
		r := k.readEvent("", i2.ID, "", 200)
		if r.Content.State != "full" || r.Body == nil || *r.Body != body2 || r.Original == nil {
			t.Errorf("不勾选时来源事件应仍在且原文完整: %+v", r.Content)
		}
	})
}
