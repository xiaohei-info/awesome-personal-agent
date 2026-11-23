package attention

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 决策卡片文案（生成型）由 decision-card 内置包声明（§7.4、§8.1、§13.5）。
//
// 时序：reconcile 发现待决决策的某个版本还没起草，就登记一行 pending 并暂不投递（与"初筛返回前不投递"同理）；
// Drafts 在事务外调模型，写回结果后在同一事务里重算该事项，投递随之产出。
// 退化：没配 Drafter 或模型端点时不登记、不等待；调用失败、输出不合 schema、信息不足或置信度不足时，
// 内容按规则，失败另记 health.alert（kind=decision-card，只进健康视图，不出投递）。路由只由投递矩阵定，不问模型。
const (
	DraftPack    = "decision-card"
	TemplateCard = "decision-card-copy"
	// DraftAlertKind 是起草失败 health.alert 的 payload.kind。
	DraftAlertKind = "decision-card"
)

const (
	draftPending = "pending"
	draftDone    = "done"
)

// Drafter 是生成型调用（*direct.Service 满足它）。
// 装配处给它一个 Attempts=1 的实例：起草挡在投递前，失败按规则继续，不退避重试。
type Drafter interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

var draftActor = domain.Actor{Type: domain.ActorSystem, Principal: "system:decision-card", Via: domain.ViaClock}

// Card 是生成型起草的决策卡片（decision-card-copy 的 output_schema）。
type Card struct {
	Title    string `json:"title"`
	Question string `json:"question"`
	Options  []struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Consequence string `json:"consequence"`
	} `json:"options"`
	Recommendation string `json:"recommendation"`
	WhyNow         string `json:"why_now"`
}

// drafted 是一个版本的起草结果；零值表示按规则。
type drafted struct {
	Card *Card
}

// content 按规则组装决策投递内容，有卡片时用卡片文案替换标题、摘要、选项说明与推荐。
func (d drafted) content(it domain.Item) domain.DeliveryContent {
	ask := it.Ask
	c := domain.DeliveryContent{Title: it.Title, Summary: ask.Question}
	if k := d.Card; k != nil {
		if k.Title != "" {
			c.Title = k.Title
		}
		if k.Question != "" {
			c.Summary = k.Question
		}
		if k.WhyNow != "" {
			c.Summary += "\n为什么现在：" + k.WhyNow
		}
	}
	for _, o := range ask.Options {
		line := o.ID + ": " + o.Label
		if d.Card != nil { // 选项按 id 对齐：回答仍按事项上的选项 id，卡片只换说明
			for _, ko := range d.Card.Options {
				if ko.ID != o.ID {
					continue
				}
				if ko.Label != "" {
					line = o.ID + ": " + ko.Label
				}
				if ko.Consequence != "" {
					line += "（" + ko.Consequence + "）"
				}
				break
			}
		}
		c.Options = append(c.Options, line)
	}
	c.Recommend = ask.Recommend
	if d.Card != nil && d.Card.Recommendation != "" {
		c.Recommend = d.Card.Recommendation
		if ask.Recommend != "" { // 推荐的选项仍是事项上的，卡片只补说明
			c.Recommend = ask.Recommend + ": " + d.Card.Recommendation
		}
	}
	return c
}

// draft 返回事项这个版本的起草结果；ready 为假表示起草未返回（已登记 pending），调用方暂不投递。
func (a *Attention) draft(tx *store.Tx, pol policy.Policy, it domain.Item, ver int64) (drafted, bool, error) {
	if a.o.Drafter == nil || pol.GenerateEndpoint.URL == "" || pol.GenerateEndpoint.Model == "" {
		return drafted{}, true, nil // 未配置：按规则（§5.6）
	}
	var (
		v           int64
		state, card string
		d           drafted
	)
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&v, &state, &card) },
		`SELECT version, state, card FROM attention_draft WHERE item = ?`, it.ID); err != nil {
		return d, false, err
	}
	if state != "" && v == ver {
		if card != "" {
			d.Card = new(Card)
			if err := json.Unmarshal([]byte(card), d.Card); err != nil {
				return d, false, err
			}
		}
		return d, state == draftDone, nil
	}
	_, err := tx.Exec(`INSERT INTO attention_draft (item, version, state) VALUES (?, ?, ?)
		ON CONFLICT(item) DO UPDATE SET version = excluded.version, state = excluded.state, card = ''`, it.ID, ver, draftPending)
	a.draftDue.Store(true)
	return drafted{}, false, err
}

// DraftDue 报告自上次调用以来是否登记过待起草的决策（装配处据它唤醒 Drafts，不占时钟循环）。
func (a *Attention) DraftDue() bool { return a.draftDue.Swap(false) }

type pendingDraft struct {
	item    string
	version int64
	card    direct.Call
	stale   bool // 事项已删或已无决策请求：不调用，写回后由重算清掉
}

// Drafts 起草全部 pending 的决策：事务外调模型，逐个写回并重算投递。只有存储错误才返回；
// 内核重启时遗留的 pending 也由它接着做。
func (a *Attention) Drafts(ctx context.Context, st *store.Store) error {
	if a.o.Drafter == nil {
		return nil
	}
	var todo []pendingDraft
	if err := st.View(ctx, func(tx *store.Tx) error {
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		var rows []pendingDraft
		if err := scan(tx, func(r *store.Rows) error {
			var p pendingDraft
			if err := r.Scan(&p.item, &p.version); err != nil {
				return err
			}
			rows = append(rows, p)
			return nil
		}, `SELECT item, version FROM attention_draft WHERE state = ? ORDER BY item`, draftPending); err != nil {
			return err
		}
		for _, p := range rows {
			it, err := items.Get(tx, p.item)
			if isNotFound(err) || err == nil && it.Ask == nil {
				p.stale = true
				todo = append(todo, p)
				continue
			}
			if err != nil {
				return err
			}
			depth, err := items.ChainDepth(tx, p.item)
			if err != nil {
				return err
			}
			p.card = draftCall(pv, it, depth)
			todo = append(todo, p)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, p := range todo {
		if p.stale {
			if err := st.Update(ctx, func(tx *store.Tx) error { return a.settleDraft(tx, p, "", nil) }); err != nil {
				return err
			}
			continue
		}
		card, cardErr := a.callCard(ctx, p.card)
		if ctx.Err() != nil {
			return ctx.Err() // 停机：留着 pending，下次启动接着做
		}
		if err := st.Update(ctx, func(tx *store.Tx) error { return a.settleDraft(tx, p, card, cardErr) }); err != nil {
			return err
		}
	}
	return nil
}

// draftCall 组织起草调用的输入。事项正文是外部内容，只进资料段。
func draftCall(pv int, it domain.Item, depth int) direct.Call {
	ask := it.Ask
	opts := make([]map[string]string, len(ask.Options))
	for i, o := range ask.Options {
		opts[i] = map[string]string{"id": o.ID, "label": o.Label}
	}
	in, _ := json.Marshal(map[string]any{"item": it.ID, "kind": it.Kind, "title": it.Title, "summary": it.Summary, "priority": it.Priority,
		"deadline": it.Deadline, "question": ask.Question, "options": opts, "recommend": ask.Recommend, "ask_deadline": ask.Deadline})
	base := direct.Call{Pack: DraftPack, Target: it.ID, Project: it.Project, NoStats: true, Actor: draftActor, ChainDepth: depth}
	card := base
	card.Template, card.Input, card.Material = TemplateCard, in, it.Body
	card.Basis = &domain.Basis{PolicyVersion: pv, RuleID: DraftPack + "/" + TemplateCard}
	return card
}

// callCard 返回卡片 JSON；信息不足或未配置返回空、无错误（按规则）。
func (a *Attention) callCard(ctx context.Context, c direct.Call) (string, error) {
	out, err := a.o.Drafter.Do(ctx, c)
	if err != nil || !out.Settled {
		return "", ignorable(err)
	}
	var k Card
	if err := json.Unmarshal(out.Final().Result, &k); err != nil {
		return "", err
	}
	b, err := json.Marshal(k)
	return string(b), err
}

// ignorable：模型端点在两次读策略之间被清空、或外发范围不允许送出，都不算失败。
func ignorable(err error) error {
	if errors.Is(err, model.ErrNotConfigured) || errors.Is(err, direct.ErrOutboundBlocked) { // 外发范围未选或 none：不起草；选定范围不会自动重起草，等事项下次变更
		return nil
	}
	return err
}

// settleDraft 写回起草结果并重算事项。事项已变到别的版本（行被重置或删掉）时什么都不写，新版本另起草。
// verdict 列是旧版投递推迟判定留下的，不再写也不再读（结构只增不改，§16.5）。
func (a *Attention) settleDraft(tx *store.Tx, p pendingDraft, card string, cardErr error) error {
	n, err := tx.Exec(`UPDATE attention_draft SET state = ?, card = ? WHERE item = ? AND version = ? AND state = ?`,
		draftDone, card, p.item, p.version, draftPending)
	if err != nil || n == 0 {
		return err
	}
	// 起草失败记一条，带所用模板：健康视图据此归类，并在同一模板之后成功调用时清除（字面值见 kernel/health.DraftKind）。
	if cardErr != nil {
		depth, err := items.ChainDepth(tx, p.item)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"kind": DraftAlertKind, "template": TemplateCard, "reason": cardErr.Error()})
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: p.item, Actor: draftActor,
			Reason: TemplateCard + " draft failed; delivered by policy rules: " + cardErr.Error(), Payload: payload, ChainDepth: depth, At: a.o.Now()}); err != nil {
			return err
		}
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	return a.reconcile(tx, pol, p.item)
}
