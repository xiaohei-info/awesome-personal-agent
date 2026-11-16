package modelstub

import (
	"encoding/json"
	"fmt"
)

// WBParams 是工作台脚本样例里随验收环境变的值；零值取默认。
type WBParams struct {
	ProjectA string // two-refs 候选 A 的项目，默认 acc-work
	ProjectB string // two-refs 候选 B 的项目，默认 acc-home
	AttachTo string // two-refs 候选 A 的 attach_to（应是清单内事项 id），默认 X1
}

// WBCaseNames 是工作台脚本样例的匹配键，同时是用例名。请求体含该串即命中对应脚本。
var WBCaseNames = []string{
	"two-refs", "bad-target", "legacy3", "cands-25", // triage-extract
	"urgent-true", "urgent-false", "urgent-missing", // triage-draft
}

func orDef(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func jsonReply(v any) Reply {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 只编码本文件里的固定结构
	}
	return Reply{Content: string(b)}
}

// WBCase 返回用例对应的脚本输出（附录 C.7：triage-extract 与 triage-draft 的 output_schema）。
func WBCase(name string, p WBParams) (Reply, error) {
	type m = map[string]any
	switch name {
	case "two-refs":
		return jsonReply(m{"candidates": []m{
			{"title": "把合同发给小王", "kind": "task", "summary": "合同需要补发", "project": orDef(p.ProjectA, "acc-work"),
				"refs": []int{0, 1}, "recommend": "attach", "attach_to": orDef(p.AttachTo, "X1"), "reason": "已有同一件事"},
			{"title": "订会议室", "kind": "task", "summary": "周五要订会议室", "project": orDef(p.ProjectB, "acc-home"), "refs": []int{2}},
		}}), nil
	case "bad-target":
		return jsonReply(m{"candidates": []m{
			{"title": "补充合同条款", "kind": "task", "summary": "指向不存在的事项", "project": "no-such-project",
				"refs": []int{0}, "recommend": "attach", "attach_to": "no-such-item", "reason": "编造的目标"},
		}}), nil
	case "legacy3":
		return jsonReply(m{"candidates": []m{{"title": "旧输出的候选", "kind": "task", "summary": "只有三个字段"}}}), nil
	case "cands-25":
		cs := make([]m, 25)
		for i := range cs {
			cs[i] = m{"title": fmt.Sprintf("候选 %02d", i+1), "kind": "task", "summary": fmt.Sprintf("第 %d 条候选", i+1)}
		}
		return jsonReply(m{"candidates": cs}), nil
	case "urgent-true", "urgent-false":
		return jsonReply(m{"title": "回复对方的报价", "kind": "task", "summary": "对方在等回复", "urgent": name == "urgent-true"}), nil
	case "urgent-missing":
		return jsonReply(m{"title": "回复对方的报价", "kind": "task", "summary": "对方在等回复"}), nil
	}
	return Reply{}, fmt.Errorf("modelstub: 未知工作台用例 %q", name)
}

// WBScript 把全部用例登记为规则：请求体含用例名即返回该用例（可经 POST /_stub/script 载入）。
func WBScript(p WBParams) Script {
	var sc Script
	for _, n := range WBCaseNames {
		r, _ := WBCase(n, p)
		sc.Rules = append(sc.Rules, Rule{Match: n, Reply: r})
	}
	return sc
}

// UseWBCase 让之后未被规则命中的请求都得到该用例（验收里的"MODEL_STUB 改用 legacy3"）。
func (s *Stub) UseWBCase(name string, p WBParams) error {
	r, err := WBCase(name, p)
	if err != nil {
		return err
	}
	s.SetDefault(r)
	return nil
}
