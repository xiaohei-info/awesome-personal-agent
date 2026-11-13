package direct

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// ACT-CAS-18：事件规则 call_agent 调自定义 single_shot 判断型模板（触发事项不是委派子事项）：
// 答案只留痕（判断记录），规则动作成功；落入带内取 on_undecided，不问对话端点。
func TestJudgeHandlerWritesRecordForNonDelegationItem(t *testing.T) {
	e := setupJudge(t, true, true)
	e.cls.PYes(modelstub.Key{Contains: "acc-ss-h1"}, 0.5)
	if err := e.svc.JudgeItem(bg, "jp", "ss", domain.Item{ID: "it_1", Kind: domain.KindTask, Title: "acc-ss-h1", Body: "正文", Project: "p1"}); err != nil {
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
