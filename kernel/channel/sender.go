package channel

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/connections"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Sender 是渠道发送器（设计 §2.2）：投递队列的又一个消费方，与渠道 ext 的长轮询并列。
// 对每个启用中的渠道连接，按路由 channel:<连接 id> 领取，经外呼调用器交给插件发送，再回报送达或失败。
// 它不写投递表（只调队列的 Claim、Ack、Fail），不决定发不发、发什么；退避、超限转 Web 未读、版本作废都是队列的规则。
type Sender struct {
	Store *store.Store
	Queue *delivery.Queue
	Conns *connections.Service
	Call  *conn.Caller
	// Base 是 Web 的绝对地址（public_url，未配置时是本机地址），投递里的相对链接据它补全。
	Base string
	Now  func() time.Time
}

// perConnection 是一轮里每个连接最多发几条，免得一个连接的积压占满一轮。
const perConnection = 20

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return clock.Now()
}

// Drain 发一轮：每个启用中的渠道连接依次领取发送，遇到第一次失败就换下一个连接（失败的已进退避）。
func (s *Sender) Drain(ctx context.Context) error {
	var cs []connections.Connection
	if err := s.Store.View(ctx, func(tx *store.Tx) (err error) { cs, err = s.Conns.List(tx, conn.DirectionChannel); return }); err != nil {
		return err
	}
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		for range perConnection {
			more, err := s.one(ctx, c)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
	}
	return nil
}

// one 领取并发送一条；返回是否应继续领取下一条。
func (s *Sender) one(ctx context.Context, c connections.Connection) (bool, error) {
	var d *domain.Delivery
	if err := s.Store.Update(ctx, func(tx *store.Tx) (err error) { d, err = s.Queue.Claim(tx, delivery.ChannelRoute(c.ID)); return }); err != nil || d == nil {
		return false, err
	}
	rc, sendErr := s.Call.Send(ctx, connections.Target(c), s.message(*d))
	if ctx.Err() != nil && sendErr != nil {
		return false, nil // 关停中：不回报，租约到期后重新可领
	}
	err := s.Store.Update(ctx, func(tx *store.Tx) error {
		now := s.now()
		if sendErr == nil {
			if _, _, err := s.Queue.Ack(tx, d.ID, d.Version, rc.Ref); err != nil {
				return err
			}
			return connections.SetStatus(tx, c.ID, "", "", now)
		}
		if _, _, err := s.Queue.Fail(tx, d.ID, d.Version, sendErr.Error()); err != nil {
			return err
		}
		detail := sendErr.Error()
		if p, ok := sendErr.(*conn.Problem); ok {
			detail = p.Detail
		}
		return connections.SetStatus(tx, c.ID, conn.ProblemOf(sendErr), detail, now)
	})
	return sendErr == nil && err == nil, err
}

func (s *Sender) abs(u string) string {
	if u == "" || !strings.HasPrefix(u, "/") {
		return u
	}
	return strings.TrimRight(s.Base, "/") + u
}

// message 把投递内容转成中立消息。web_only 的决策内容里本来就没有选项、推荐与表单链接（触达写入时已去掉），这里再守一道。
func (s *Sender) message(d domain.Delivery) conn.Message {
	c := d.Content
	m := conn.Message{Key: d.ID + "@" + strconv.FormatInt(d.Version, 10), Class: string(d.Class), Title: domain.StripLinks(c.Title), Summary: domain.StripLinks(c.Summary),
		WebURL: s.abs(c.WebURL), WebOnly: c.WebOnly}
	if c.WebOnly {
		return m
	}
	m.Recommend, m.FormURL = c.Recommend, s.abs(c.FormURL)
	for _, o := range c.Options {
		m.Options = append(m.Options, conn.Option{ID: o, Label: o})
	}
	return m
}

// TestMessage 是「发一条测试」的固定内容；标题与摘要由内核给定，插件只渲染。
func TestMessage(base string) conn.Message {
	return conn.Message{Key: "test@" + clock.Now().UTC().Format("20060102T150405.000"), Class: string(domain.DeliveryReminder), Test: true,
		Title: "APA 测试提醒", Summary: "能看到这条，说明提醒会送到这里。", WebURL: strings.TrimRight(base, "/") + "/settings/channels"}
}

// Probe 是「测试」：插件自检，send 为真时再发一条内容固定的测试消息。draft 是未保存的凭据（编辑时缺省的用已保存的）。
// record 为真时把结果写进连接现状（只对已保存的连接）；草稿测试不改账本。
func (s *Sender) Probe(ctx context.Context, t conn.Target, draft map[string]string, send, record bool) error {
	var sample conn.Message
	if send {
		sample = TestMessage(s.Base)
	}
	err := s.Call.Check(ctx, t, draft, sample)
	if !record || t.ID == "" {
		return err
	}
	problem, detail := "", ""
	if err != nil {
		problem, detail = conn.ProblemOf(err), err.Error()
		if p, ok := err.(*conn.Problem); ok {
			detail = p.Detail
		}
	}
	if uerr := s.Store.Update(ctx, func(tx *store.Tx) error { return connections.SetStatus(tx, t.ID, problem, detail, s.now()) }); uerr != nil {
		return uerr
	}
	return err
}
