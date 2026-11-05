package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *Engine) handler(name string) (Handler, bool) {
	if h, ok := e.o.Handlers[name]; ok {
		return h, true
	}
	switch name {
	case "set_phase":
		return e.setPhase, true
	case "add_tag":
		return e.addTag, true
	case "create_item":
		return e.createItem, true
	case "create_timer":
		return e.createTimer, true
	}
	return nil, false
}

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// fresh 读事项当前状态：动作要基于执行时刻的事项，而不是触发事件里的快照。
func (e *Engine) fresh(ctx context.Context, c Call) (domain.Item, error) {
	if c.Item == nil {
		return domain.Item{}, invalid("action " + str(c.Action, "action") + " needs an item, but this rule has none")
	}
	var it domain.Item
	err := e.o.Store.View(ctx, func(tx *store.Tx) (err error) { it, err = items.Get(tx, c.Item.ID); return })
	return it, err
}

func (e *Engine) setPhase(ctx context.Context, c Call) error {
	to := str(c.Action, "phase")
	it, err := e.fresh(ctx, c)
	if err != nil {
		return err
	}
	if string(it.Phase) == to {
		return nil
	}
	in := map[string]any{"id": it.ID, "to": to, "reason": "rule " + c.Rule}
	if to == string(domain.PhaseWaiting) && it.WaitingOn == nil {
		// waiting 必须说明在等什么；规则没有更具体的信息，记为等待外部。
		in["waiting_on"] = domain.WaitingOn{Kind: domain.WaitingExternal, Ref: "rule " + c.Rule}
	}
	_, err = c.Do("item.transition", in)
	return err
}

func (e *Engine) addTag(ctx context.Context, c Call) error {
	tag := str(c.Action, "tag")
	it, err := e.fresh(ctx, c)
	if err != nil {
		return err
	}
	if slices.Contains(it.Tags, tag) {
		return nil
	}
	_, err = c.Do("item.update", map[string]any{"id": it.ID, "tags": append(slices.Clone(it.Tags), tag), "reason": "rule " + c.Rule})
	return err
}

// createItem 建一个提议阶段的事项（由本人接受后才进入工作），项目取自触发事项。
// 动作只有 kind、assignee、parent 三个字段：标题取触发事项的标题（时钟规则取规则键）；
// parent 为 "trigger" 表示以触发事项为父，否则是事项 id。assignee 过项目允许的执行者校验。
func (e *Engine) createItem(_ context.Context, c Call) error {
	title := c.Rule
	in := map[string]any{"kind": str(c.Action, "kind"), "reason": "rule " + c.Rule}
	if c.Item != nil {
		title = c.Item.Title
		if c.Item.Project != "" {
			in["project"] = c.Item.Project
		}
	}
	in["title"] = title
	if a := str(c.Action, "assignee"); a != "" {
		in["assignee"] = a
	}
	res, err := c.Do("item.create", in)
	if err != nil {
		return err
	}
	parent := str(c.Action, "parent")
	if parent == "" {
		return nil
	}
	if parent == "trigger" {
		if c.Item == nil {
			return invalid("create_item parent=trigger needs a triggering item")
		}
		parent = c.Item.ID
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil {
		return err
	}
	_, err = c.Do("item.relate", map[string]any{"from": out.ID, "to": parent, "type": domain.RelParent, "reason": "rule " + c.Rule})
	return err
}

// createTimer：after（时长）或 at（RFC 3339）建一次性提醒，schedule（固定间隔）建周期 Timer。
// Timer 的目标是触发事项；时钟规则没有事项时目标是规则自己。
func (e *Engine) createTimer(_ context.Context, c Call) error {
	target := objectOf(c.Rule)
	if c.Item != nil {
		target = c.Item.ID
	}
	in := timerIn{Target: target, Kind: "remind", Reason: "rule " + c.Rule}
	switch {
	case str(c.Action, "after") != "":
		d, err := time.ParseDuration(str(c.Action, "after"))
		if err != nil || d <= 0 {
			return invalid("create_timer after must be a positive duration such as 24h")
		}
		in.At = e.now().Add(d)
	case str(c.Action, "at") != "":
		at, err := time.Parse(time.RFC3339, str(c.Action, "at"))
		if err != nil {
			return invalid("create_timer at must be an RFC 3339 timestamp")
		}
		in.At = at
	case str(c.Action, "schedule") != "":
		s := str(c.Action, "schedule")
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return invalid(fmt.Sprintf("create_timer schedule %q must be a fixed interval such as 24h", s))
		}
		in.Kind, in.Every, in.At = "recur", s, e.now().Add(d)
	default:
		return invalid("create_timer needs one of after, at, schedule")
	}
	_, err := c.Do("rule.create_timer", in)
	return err
}
