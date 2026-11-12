// Package bands 把策略的判断设置（不确定带覆盖、影子模式，设计 §8.9、§8.10）落到一道判断题上。
//
// 设置本身只在 Web 写、记 policy.changed、校验下限不大于上限，都由 kernel/policy 与 /settings/judgment 负责；
// 本包只读、只依赖 policy 与 record：给定策略快照、模板与来源，决定这道题用哪条带、是否只记分；
// 由判断级联（kernel/agents/direct）在 prepare 里调用。
package bands

import (
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// Resolve 返回策略对（模板，来源）的不确定带覆盖：带 source 的条目优先，其次是模板级条目；都没有返回 nil，
// 调用方取模板声明的带。
func Resolve(p policy.Policy, template, source string) *record.Band {
	var byTemplate *record.Band
	for _, b := range p.Judgment.Bands {
		if b.Template != template {
			continue
		}
		got := &record.Band{Low: b.Low, High: b.High}
		if b.Source == "" {
			byTemplate = got
		} else if b.Source == source {
			return got
		}
	}
	return byTemplate
}

// Shadow 报告模板是否处于影子模式。
func Shadow(p policy.Policy, template string) bool {
	for _, t := range p.Judgment.Shadow {
		if t == template {
			return true
		}
	}
	return false
}
