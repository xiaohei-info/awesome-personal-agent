package attention

// 讨论里 @ 主会话（§6.3、决定 100）：发言 @ 了 domain.MentionMain 时，给主 Agent 的常驻主会话发一条唤醒，内容是哪件事、
// 谁说的、原话和怎么回复。不建运行、不建子事项；它用带 --reply-to 的发言回复。一次 @ 一条：id 由发言决定，重复追赶不会再入队，
// 事项之后的变化也不作废它。主会话能不能唤醒在发言时已判过（叫不动整条不生效）；之后变得不能唤醒的，留 Web 未读。

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// MentionWakeID 是一条发言 @ 主会话的唤醒投递 id。
func MentionWakeID(comment string) string { return delivery.MentionPrefix + comment }

func (a *Attention) mentionWake(tx *store.Tx, pol policy.Policy, e domain.Event) error {
	var p struct {
		Progress *domain.Progress `json:"progress"`
	}
	if json.Unmarshal(e.Payload, &p) != nil || p.Progress == nil || !slices.Contains(p.Progress.Mentions, domain.MentionMain) {
		return nil
	}
	it, err := items.Get(tx, e.Object)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	route, _, err := MainRoute(tx, pol)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(p.Progress.Text)
	if r := []rune(text); len(r) > summaryRunes {
		text = string(r[:summaryRunes]) + "…"
	}
	who := "本人"
	if e.Actor.Type != domain.ActorHuman {
		who = e.Actor.Principal
	}
	summary := who + " 在讨论里 @ 了你：" + text + "\n回复：apa item comment " + it.ID + " --reply-to " + p.Progress.ID +
		" --text <回复>；读讨论：apa item thread " + it.ID
	_, _, err = a.o.Queue.Create(tx, domain.Delivery{ID: MentionWakeID(p.Progress.ID), Item: it.ID, Class: domain.DeliveryReminder, Route: route,
		Version: 1, Content: domain.DeliveryContent{Title: "讨论里有人叫你：" + it.Title, Summary: summary, WebURL: a.o.WebURL(it.ID)}})
	return err
}
