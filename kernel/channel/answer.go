package channel

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 命令名。
const (
	CmdAnswer     = "channel.answer"      // 渠道 ext 转达回答（POST /deliveries/{id}/answer）
	CmdFormAnswer = "channel.form_answer" // 一次性表单作答（POST /forms/{令牌}）
)

// extScope 是渠道 ext 密钥的 scope 前缀，Bearer 把它解析成 Principal "ext:channel.<名>"。
const extScope = "ext:channel."

// Options 配置回答命令。
type Options struct {
	Queue  *delivery.Queue
	Now    func() time.Time         // 默认 clock.Now；须与命令执行器用同一个时钟
	WebURL func(item string) string // 被拒绝时回给调用方的 Web 链接，默认 /items/<id>
}

type answerIn struct {
	Delivery string `json:"delivery"`
	Ref      string `json:"ref"`
	Sender   string `json:"sender"`
	Choice   string `json:"choice"`
	Text     string `json:"text"`
	Reason   string `json:"reason"`
}

type formIn struct {
	Token  string `json:"token"`
	Choice string `json:"choice"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

type result struct {
	ID string `json:"id"`
}

func parse(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	return nil
}

// objectOf 解析授权对象：全部字段来自账本。
func objectOf(tx *store.Tx, id string) (authz.Object, error) {
	it, err := items.Get(tx, id)
	if err != nil {
		return authz.Object{}, err
	}
	o := authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}
	if a := it.Ask; a != nil {
		o.Category, o.ScreeningPending = a.RiskClass, a.ScreeningPending
	}
	return o, nil
}

// Register 登记 channel.answer 与 channel.form_answer。决定的写入仍经 decisions.Answer（唯一写入模块），
// 本包只做渠道侧的校验：回答人白名单、ref 一致、版本未作废、web_only 拒绝，以及令牌一次性。
func Register(x *command.Executor, o Options) error {
	if o.WebURL == nil {
		o.WebURL = func(id string) string { return "/items/" + id }
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	a := &answerer{o}
	for _, s := range []command.Spec{a.answer(), a.formAnswer()} {
		if err := x.Register(s); err != nil {
			return err
		}
	}
	return nil
}

type answerer struct{ o Options }

func (a *answerer) load(tx *store.Tx, in json.RawMessage) (answerIn, domain.Delivery, error) {
	var v answerIn
	if err := parse(in, &v); err != nil {
		return v, domain.Delivery{}, err
	}
	d, err := a.o.Queue.Get(tx, v.Delivery)
	return v, d, err
}

func (a *answerer) answer() command.Spec {
	return command.Spec{Name: CmdAnswer, Action: authz.ActionChannelAnswer, Kinds: []auth.Kind{auth.Ext},
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			_, d, err := a.load(tx, in)
			if err != nil {
				return authz.Object{}, err
			}
			return objectOf(tx, d.Item)
		},
		// 渠道由投递的路由和调用的密钥共同确定：密钥只能转达自己渠道的投递（§10.3）。
		ChannelAnswer: func(tx *store.Tx, c command.Caller, in json.RawMessage) (string, string, string, error) {
			v, d, err := a.load(tx, in)
			if err != nil {
				return "", "", "", err
			}
			name, ok := strings.CutPrefix(d.Route, routePrefix)
			if !ok || extScope+name != c.Actor.Principal || reserved(name) {
				return "", "", "", domain.NewError(domain.CodePermissionDenied, "this key cannot answer deliveries of this route", "use the key of the channel that sent the message")
			}
			return name, v.Sender, a.o.WebURL(d.Item), nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, d, err := a.load(tx, in)
			if err != nil {
				return err
			}
			switch {
			case d.Class != domain.DeliveryDecision:
				return domain.NewError(domain.CodeInvalidRequest, "only decision deliveries can be answered", "answer from a decision message")
			case d.State == domain.DeliverySuperseded:
				return domain.NewError(domain.CodeConflict, "this message is outdated; the item has changed", "answer from the latest message or the Web UI")
			case d.ExternalRef == "" || v.Ref != d.ExternalRef:
				return domain.NewError(domain.CodePermissionDenied, "ref does not match the delivered message", "answer by replying to the message the delivery was sent as")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, d, err := a.load(c.Tx, c.Input)
			if err != nil {
				return nil, err
			}
			name, _ := strings.CutPrefix(d.Route, routePrefix)
			if err := decisions.Answer(c, d.Item, v.Choice, v.Text, name, v.Sender, v.Reason); err != nil {
				return nil, err
			}
			_, _, err = a.o.Queue.Confirm(c.Tx, d.ID, d.Version)
			return result{d.Item}, err
		}}
}

// reserved 挡住渠道名冒充其他决定入口（Resolution.ResolvedVia 与渠道名共用一个字段）。
func reserved(name string) bool {
	return name == "web" || name == "chat" || name == "timeout" || name == domain.ResolvedViaGrant
}

// formAnswer：令牌本身就是凭据，只对发放时的那一个决策、那一个版本有效；web_only 与初筛未返回在作答时重新判定
// （策略可能在发放后变了）。校验失败整体回滚，令牌不算用过。
func (a *answerer) formAnswer() command.Spec {
	return command.Spec{Name: CmdFormAnswer, Action: authz.ActionRead,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v formIn
			if err := parse(in, &v); err != nil {
				return authz.Object{}, err
			}
			f, err := PeekForm(tx, v.Token, a.o.Now())
			if err != nil {
				return authz.Object{}, err
			}
			return objectOf(tx, f.Item)
		},
		Apply: func(c *command.Ctx) (any, error) {
			var v formIn
			if err := parse(c.Input, &v); err != nil {
				return nil, err
			}
			f, err := RedeemForm(c.Tx, v.Token, c.Now())
			if err != nil {
				return nil, err
			}
			if ver, err := command.Version(c.Tx, f.Item); err != nil {
				return nil, err
			} else if ver != f.Version {
				return nil, domain.NewError(domain.CodeConflict, "this form link is outdated; the item has changed", "use the latest message or answer in the Web UI")
			}
			_, pol, err := policy.Current(c.Tx)
			if err != nil {
				return nil, err
			}
			if authz.WebOnly(pol, c.Object) {
				return nil, webLink(domain.CodeWebOnly, "this decision is web_only and cannot be answered from a form", a.o.WebURL(f.Item))
			}
			if c.Object.ScreeningPending {
				return nil, webLink(domain.CodeRiskScreeningPending, "web_only screening has not returned; form answers are not accepted yet", a.o.WebURL(f.Item))
			}
			return result{f.Item}, decisions.Answer(c, f.Item, v.Choice, v.Text, f.Channel, "", v.Reason)
		}}
}

func webLink(code domain.Code, why, url string) error {
	e := domain.NewError(code, why, "answer in the Web UI")
	e.WebURL = &url
	return e
}
