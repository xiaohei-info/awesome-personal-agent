package modelstub

import "strings"

// Key 是两个替身共用的"题目匹配键"：Template（模板名）、Contains（输入关键字）、
// EventID（来源事件 id）里填了的项都必须出现在题目内容里才命中，与调用顺序无关。
// 题目内容：分类端点为 question+criteria+input+material；对话端点为全部消息正文。
type Key struct {
	Template string `json:"template,omitempty"`
	Contains string `json:"contains,omitempty"`
	EventID  string `json:"event_id,omitempty"`
}

func (k Key) matches(text string) bool {
	for _, p := range []string{k.Template, k.Contains, k.EventID} {
		if p != "" && !strings.Contains(text, p) {
			return false
		}
	}
	return true
}
