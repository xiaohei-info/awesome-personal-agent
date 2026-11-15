package channel

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// IdentifyTTL 是识别口令的有效期（§14.6、附录 B.2）。
const IdentifyTTL = 10 * time.Minute

// 识别状态（GET /settings/channels/{name}/identify 的 state）。
const (
	IdentifyWaiting = "waiting"
	IdentifyGot     = "got"
	IdentifyExpired = "expired"
)

// 口令字符去掉了易混的 0、1、I、L、O。
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// Identification 是一次识别的状态；Sender、Display 只在 got 时有值。
type Identification struct {
	Code      string    `json:"code,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Sender    string    `json:"sender,omitempty"`
	Display   string    `json:"display,omitempty"`
}

// Identifier 保管"识别我的 ID"的一次性口令：每个渠道同时一个，只在内存里，内核重启即作废，
// 不进账本；名单只在本人在 Web 点"加上"时经策略写入（§14.6）。
type Identifier struct {
	mu  sync.Mutex
	now func() time.Time
	by  map[string]*Identification // 渠道名 → 当前识别
}

// NewIdentifier 创建识别口令保管处；now 为 nil 用内核时钟。
func NewIdentifier(now func() time.Time) *Identifier {
	if now == nil {
		now = clock.Now
	}
	return &Identifier{now: now, by: map[string]*Identification{}}
}

// Start 为渠道发起识别：新口令作废该渠道之前的口令。
func (i *Identifier) Start(channel string) (Identification, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return Identification{}, err
	}
	code := []byte("apa-")
	for _, x := range b {
		code = append(code, codeAlphabet[int(x)%len(codeAlphabet)])
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	id := &Identification{Code: string(code), ExpiresAt: i.now().Add(IdentifyTTL), State: IdentifyWaiting}
	i.by[channel] = id
	return *id, nil
}

// Status 返回渠道当前的识别状态；没有发起过返回 not_found。口令不回传，免得被别的页面读去冒用。
func (i *Identifier) Status(channel string) (Identification, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	id, ok := i.by[channel]
	if !ok {
		return Identification{}, domain.NewError(domain.CodeNotFound, "no identification was started for this channel", "start one from the channel settings")
	}
	out := *id
	if out.State == IdentifyWaiting && !i.now().Before(out.ExpiresAt) {
		out.State = IdentifyExpired
	}
	out.Code = ""
	return out, nil
}

// Report 记下渠道 ext 上报的发送者：口令必须属于这个渠道、仍在等待且未过期，否则一律 not_found
// （不区分原因，免得被用来试探口令）。口令只能用一次。
func (i *Identifier) Report(channel, code, sender, display string) error {
	sender = strings.TrimSpace(sender)
	i.mu.Lock()
	defer i.mu.Unlock()
	id, ok := i.by[channel]
	if !ok || sender == "" || id.State != IdentifyWaiting || !i.now().Before(id.ExpiresAt) || !strings.EqualFold(strings.TrimSpace(code), id.Code) {
		return domain.NewError(domain.CodeNotFound, "this identification code is not valid for this channel", "ask the owner to start a new identification in the Web UI")
	}
	id.State, id.Sender, id.Display = IdentifyGot, sender, strings.TrimSpace(display)
	return nil
}
