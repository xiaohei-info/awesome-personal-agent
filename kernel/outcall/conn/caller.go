package conn

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DefaultTimeout 是一次 Send 或 Test 的上限。
const DefaultTimeout = 20 * time.Second

// Target 是被调用的连接：id 为空表示未保存的草稿（只用于测试）。
type Target struct {
	ID       string
	Type     string
	Settings map[string]string
}

// Options 配置调用器。
type Options struct {
	Store   *store.Store
	Vault   *secrets.Vault
	Plugins []Plugin
	Timeout time.Duration // 默认 DefaultTimeout
}

// Caller 是连接插件调用器。无账本状态；每个连接一个信号量保证串行。
type Caller struct {
	o       Options
	plugins map[string]Plugin
	mu      sync.Mutex
	slots   map[string]chan struct{}
}

// New 返回调用器；同一类型登记两次或声明不合法返回错误。
func New(o Options) (*Caller, error) {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	c := &Caller{o: o, plugins: map[string]Plugin{}, slots: map[string]chan struct{}{}}
	for _, p := range o.Plugins {
		if err := p.Decl.Check(); err != nil {
			return nil, err
		}
		if _, dup := c.plugins[p.Decl.Type]; dup || p.New == nil {
			return nil, fmt.Errorf("conn: connector %q registered twice or without a factory", p.Decl.Type)
		}
		c.plugins[p.Decl.Type] = p
	}
	return c, nil
}

// Decls 返回全部接入声明，按类型排序。
func (c *Caller) Decls() []Decl {
	out := make([]Decl, 0, len(c.plugins))
	for _, p := range c.plugins {
		out = append(out, p.Decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Decl 按类型取声明。
func (c *Caller) Decl(typ string) (Decl, bool) {
	p, ok := c.plugins[typ]
	return p.Decl, ok
}

func (c *Caller) slot(id string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.slots[id]
	if !ok {
		s = make(chan struct{}, 1)
		c.slots[id] = s
	}
	return s
}

// secretsOf 解出连接声明过的凭据字段；draft 里给出的（非空）覆盖已保存的。
func (c *Caller) secretsOf(ctx context.Context, t Target, d Decl, draft map[string]string) (map[string]string, error) {
	keys := d.SecretKeys()
	out := map[string]string{}
	if t.ID != "" && len(keys) > 0 {
		if err := c.o.Store.View(ctx, func(tx *store.Tx) (err error) { out, err = c.o.Vault.Reveal(tx, t.ID, keys); return }); err != nil {
			return nil, err
		}
	}
	for _, k := range keys {
		if v, ok := draft[k]; ok {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
	}
	for _, f := range d.Fields {
		if f.Kind == KindSecret && f.Required && out[f.Key] == "" {
			return nil, Fail(ProblemCredentialsMissing, "credential "+f.Key+" is not set or cannot be decrypted")
		}
	}
	return out, nil
}

// scrub 把凭据值替换成 ***：插件与网络库的报错可能带出整条含凭据的地址（§5.4）。
func scrub(s string, sec map[string]string) string {
	vals := make([]string, 0, len(sec))
	for _, v := range sec {
		if v != "" {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) }) // 长的先换，免得短值是长值的一部分
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// clean 把插件错误折成带问题代码、细节已脱敏的 Problem。
func clean(err error, sec map[string]string) error {
	if err == nil {
		return nil
	}
	code := ProblemOf(err)
	detail := err.Error()
	if p, ok := err.(*Problem); ok {
		detail = p.Detail
	}
	return &Problem{Code: code, Detail: scrub(detail, sec)}
}

// call 在超时、串行与 panic 恢复之下调用 f；插件不理会 ctx 时，调用方仍在超时后返回，连接保持占用直到插件返回。
func (c *Caller) call(ctx context.Context, t Target, draft map[string]string, f func(context.Context, Channel) error) error {
	p, ok := c.plugins[t.Type]
	if !ok {
		return Fail(ProblemConfigInvalid, "unknown connection type "+t.Type)
	}
	sec, err := c.secretsOf(ctx, t, p.Decl, draft)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	key := t.ID
	if key == "" {
		key = "draft:" + t.Type
	}
	s := c.slot(key)
	select {
	case s <- struct{}{}:
	case <-ctx.Done():
		return Fail(ProblemTemporary, "the connection is still busy with an earlier call")
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-s }()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("connector panicked", "type", t.Type, "connection", t.ID, "panic", scrub(fmt.Sprint(r), sec))
				done <- Fail(ProblemTemporary, "the connector crashed")
			}
		}()
		ch, err := p.New(Config{Settings: t.Settings, Secrets: sec})
		if err == nil {
			err = f(ctx, ch)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return clean(err, sec)
	case <-ctx.Done():
		return Fail(ProblemUnreachable, "the connector did not answer in time")
	}
}

// Send 发送一条中立消息。只允许渠道发送器调用（红线检查 §4.3）。
func (c *Caller) Send(ctx context.Context, t Target, m Message) (Receipt, error) {
	var rc Receipt
	err := c.call(ctx, t, nil, func(ctx context.Context, ch Channel) (err error) {
		rc, err = ch.Send(ctx, m)
		if err == nil && rc.Ref == "" {
			err = Fail(ProblemTemporary, "the connector returned no message reference")
		}
		return err
	})
	return rc, err
}

// Check 是「测试」：先做插件的自检；sample 为真时再发一条内容固定的测试消息。
// draft 是表单里未保存的凭据（缺省的字段用已保存的），t.Settings 是表单里未保存的配置；不改账本。
func (c *Caller) Check(ctx context.Context, t Target, draft map[string]string, sample Message) error {
	return c.call(ctx, t, draft, func(ctx context.Context, ch Channel) error {
		if err := ch.Test(ctx); err != nil || !sample.Test {
			return err
		}
		_, err := ch.Send(ctx, sample)
		return err
	})
}
