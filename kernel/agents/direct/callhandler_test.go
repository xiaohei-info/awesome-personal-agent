package direct

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func ruleCall(tmpl, title string) rules.Call {
	return rules.Call{Rule: "jp/r", Action: packs.Action{"prompt": tmpl}, Seq: 3,
		Item: &domain.Item{ID: "it_1", Kind: domain.KindTask, Title: title, Body: "正文 " + title, Project: "p1"}}
}

// ACT-CAS-18：事件规则 call_agent 调自定义 single_shot 判断型模板（触发事项不是委派子事项）：
// 答案只留痕（判断记录），规则动作成功；落入带内取 on_undecided，不问对话端点。
func TestJudgeHandlerWritesRecordForNonDelegationItem(t *testing.T) {
	e := setupJudge(t, true, true)
	e.cls.PYes(modelstub.Key{Contains: "acc-ss-h1"}, 0.5)
	if err := e.svc.CallHandler()(bg, ruleCall("ss", "acc-ss-h1")); err != nil {
		t.Fatal(err)
	}
	var rs []record.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		rs, _, err = record.List(tx, record.Filter{Subject: "it_1"})
		return
	}); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].DecidedBy != "default" || rs[0].Answer != "no" || len(e.stub.Requests()) != 0 {
		t.Fatalf("records=%+v chat requests=%d", rs, len(e.stub.Requests()))
	}
}

func TestJudgeHandlerNeedsItemAndTemplate(t *testing.T) {
	e := setupJudge(t, true, true)
	c := ruleCall("ss", "x")
	c.Item = nil
	if err := e.svc.CallHandler()(bg, c); err == nil {
		t.Fatal("want error without triggering item")
	}
	if err := e.svc.CallHandler()(bg, ruleCall("", "x")); err == nil {
		t.Fatal("want error without template")
	}
}
