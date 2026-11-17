package connections

// Agent 准备的连接草稿（§7.6）：Agent 提交一份完整的有效值（可先测试），本人在 Web 确认这一份之后才新建或改连接、写路由。
// 沿用插件声明的"提交 → 待确认 → 本人确认，新版本取代旧版本"：提交与测试、撤回按 ActionSubmitPlugin（worker、delegate
// 与 Web，observer 与运行凭据不行），确认按 ActionChangeSettings（只在 Web）。草稿不可改，id 就是版本；凭据的属主是草稿 id，
// 与任何连接的凭据分开，确认时在保管库内部换属主。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 草稿命令（附录 B.1、B.2）。
const (
	CmdPropose   = "connection.propose"
	CmdDrafts    = "connection.drafts"
	CmdDraftTest = "connection.draft_test"
	CmdConfirm   = "connection.confirm"
	CmdDiscard   = "connection.discard"
)

// 草稿事件（内部类型，不进订阅流）。
const (
	EvProposed        = "connection.proposed"
	EvDraftSuperseded = "connection.draft_superseded"
	EvConfirmed       = "connection.confirmed"
	EvDraftDiscarded  = "connection.draft_discarded"
	EvDraftExpired    = "connection.draft_expired"
)

// 草稿状态。
const (
	DraftPending    = "pending"
	DraftConfirmed  = "confirmed"
	DraftSuperseded = "superseded"
	DraftDiscarded  = "discarded"
	DraftExpired    = "expired"
)

// DraftTTL 是草稿的有效期（§7.6）。
const DraftTTL = 24 * time.Hour

// draftShown 是已结束的草稿在列表里保留的时长。
const draftShown = 7 * 24 * time.Hour

// 凭据在草稿里的处置。
const (
	SecretSet   = "set"
	SecretClear = "clear"
	SecretKeep  = "keep"
)

// DraftMigration 是连接模块的第二步迁移（账本结构版本 9，只增表）。
var DraftMigration = store.Migration{
	Version: 2, Ledger: 9, Name: "ledger-v9-connection-drafts",
	SQL: `CREATE TABLE connections_draft (
    id TEXT PRIMARY KEY,
    target TEXT NOT NULL,
    connection TEXT NOT NULL DEFAULT '',
    base TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL,
    name TEXT NOT NULL,
    config TEXT NOT NULL,
    routes TEXT NOT NULL,
    secrets TEXT NOT NULL,
    changes TEXT NOT NULL DEFAULT '[]',
    state TEXT NOT NULL,
    proposed_by TEXT NOT NULL,
    test TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    closed_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE UNIQUE INDEX connections_draft_pending ON connections_draft(target) WHERE state = 'pending';`,
}

// SecretPlan 是草稿里一个凭据字段的处置；Hint 只对声明为接收目标的字段给出，不含明文全文。
type SecretPlan struct {
	Action string `json:"action"`
	Hint   string `json:"hint,omitempty"`
	Length int    `json:"length,omitempty"`
}

// DraftTest 是草稿最近一次测试的结果。
type DraftTest struct {
	OK      bool      `json:"ok"`
	Problem string    `json:"problem,omitempty"`
	At      time.Time `json:"at"`
}

// Draft 是读接口的形状（不含凭据值）。
type Draft struct {
	ID         string                `json:"id"`
	Type       string                `json:"type"`
	Name       string                `json:"name"`
	Connection string                `json:"connection,omitempty"`
	Config     map[string]string     `json:"config"`
	Routes     []string              `json:"routes"`
	Secrets    map[string]SecretPlan `json:"secrets"`
	Changes    []string              `json:"changes,omitempty"`
	State      string                `json:"state"`
	ProposedBy string                `json:"proposed_by"`
	Test       *DraftTest            `json:"test,omitempty"`
	CreatedAt  time.Time             `json:"created_at"`
	ExpiresAt  time.Time             `json:"expires_at"`
	ClosedAt   *time.Time            `json:"closed_at,omitempty"`
	WebPath    string                `json:"web_path"`
	base       string                // 改配置草稿提交时目标连接的指纹（名字、配置、凭据密文行），确认时比对
	target     string
}

type proposeInput struct {
	Type       string            `json:"type"`
	Name       string            `json:"name,omitempty"`
	Connection string            `json:"connection,omitempty"`
	Config     map[string]string `json:"config,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
	Routes     *[]string         `json:"routes,omitempty"`
}

var classes = []string{string(domain.DeliveryDecision), string(domain.DeliveryBrief), string(domain.DeliveryReminder), string(domain.DeliveryHealth)}

// registerDrafts 登记草稿的五条命令。
func (s *Service) registerDrafts(x *command.Executor) error {
	work := func(name string, apply func(*command.Ctx) (any, error)) command.Spec {
		return command.Spec{Name: name, Action: authz.ActionSubmitPlugin, Tier: authz.TierWork, Object: objectOf, Apply: apply}
	}
	for _, sp := range []command.Spec{
		work(CmdPropose, s.applyPropose),
		{Name: CmdDrafts, Action: authz.ActionSubmitPlugin, Object: objectOf, Apply: s.applyDrafts},
		work(CmdDraftTest, s.applyDraftTest),
		work(CmdDiscard, s.applyDiscard),
		{Name: CmdConfirm, Action: authz.ActionChangeSettings, Tier: authz.TierNone, Object: objectOf, Apply: s.applyConfirm},
	} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

// proposer 是草稿的提交者：凭据 id；Web 本人为 "web"。
func proposer(c command.Caller) string {
	if c.Credential != "" {
		return c.Credential
	}
	return "web"
}

func isWeb(c command.Caller) bool { return c.Level == authz.LevelWeb }

// mask 给标识接收目标的凭据一个可核对但不泄密的样子：不少于 12 个字符时只露首尾各 3 个，否则只给长度。
func mask(v string) SecretPlan {
	n := utf8.RuneCountInString(v)
	p := SecretPlan{Action: SecretSet, Length: n}
	if n >= 12 {
		r := []rune(v)
		p.Hint = string(r[:3]) + "…" + string(r[n-3:])
	}
	return p
}

func newDraftID(tx *store.Tx) (string, error) {
	for {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := "dr-" + hex.EncodeToString(b)
		if _, err := getDraft(tx, id); isNotFound(err) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
}

// routedTo 是打扰策略里类别级路由指向这个连接的类别。
func routedTo(tx *store.Tx, id string) ([]string, error) {
	_, p, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range classes {
		if p.Disturb.Routes[c] == "channel:"+id {
			out = append(out, c)
		}
	}
	return out, nil
}

func cleanRoutes(in []string) ([]string, error) {
	out := []string{}
	for _, c := range in {
		if !slices.Contains(classes, c) {
			return nil, invalid("route class "+c+" is not one of decision, brief, reminder, health", "fix --route and retry")
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) applyPropose(c *command.Ctx) (any, error) {
	var in proposeInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	now := c.Now()
	d := Draft{ID: "", Config: map[string]string{}, Secrets: map[string]SecretPlan{}, State: DraftPending, ProposedBy: proposer(c.Caller),
		CreatedAt: now.UTC(), ExpiresAt: now.Add(DraftTTL).UTC()}
	var (
		decl   conn.Decl
		cur    record
		usable map[string]bool
	)
	if in.Connection != "" {
		var err error
		if cur, err = get(c.Tx, in.Connection); err != nil {
			return nil, err
		}
		if in.Type != "" && in.Type != cur.Type {
			return nil, invalid("a draft cannot change the type of connection "+cur.ID, "propose a new connection instead")
		}
		if in.Name != "" && in.Name != cur.Name {
			return nil, invalid("a draft cannot rename a connection", "ask the owner to rename it in the Web settings")
		}
		in.Type, in.Name = cur.Type, cur.Name
		if d.base, err = fingerprint(c.Tx, cur); err != nil {
			return nil, err
		}
		d.Connection, d.target = cur.ID, cur.ID
		if usable, err = s.v.Usable(c.Tx, cur.ID); err != nil {
			return nil, err
		}
	}
	decl, ok := s.decl(in.Type)
	if !ok {
		return nil, invalid("unknown connection type "+in.Type, "choose a type from GET /connections/types")
	}
	if decl.Direction != conn.DirectionChannel {
		return nil, invalid("drafts are only for reminder channels", "ask the owner to set it up in the Web settings")
	}
	changed, err := planSecrets(decl, in.Secrets)
	if err != nil {
		return nil, err
	}
	have := func(k string) bool {
		if v, given := in.Secrets[k]; given {
			return v != ""
		}
		return usable[k]
	}
	cfg := in.Config
	if d.Connection != "" { // 改配置：没给的字段取现用值，草稿里存完整的有效值
		cfg = map[string]string{}
		for k, v := range cur.Config {
			cfg[k] = v
		}
		for k, v := range in.Config {
			cfg[k] = v
		}
	}
	if d.Config, err = decl.Normalize(cfg, have); err != nil {
		return nil, fieldErr(err)
	}
	if in.Name == "" {
		in.Name = decl.Default.Zh
	}
	if d.Name, err = cleanName(in.Name); err != nil {
		return nil, err
	}
	if d.Connection == "" {
		if err := nameFree(c.Tx, d.Name, ""); err != nil {
			return nil, err
		}
		d.target = "new:" + d.Name
	}
	switch {
	case in.Routes != nil:
		if d.Routes, err = cleanRoutes(*in.Routes); err != nil {
			return nil, err
		}
	case d.Connection != "":
		if d.Routes, err = routedTo(c.Tx, d.Connection); err != nil {
			return nil, err
		}
	default:
		d.Routes = []string{}
	}
	d.Type = decl.Type
	if d.ID, err = newDraftID(c.Tx); err != nil {
		return nil, err
	}
	d.WebPath = webPath(d.ID)
	var keep []string
	for _, f := range decl.Fields {
		if f.Kind != conn.KindSecret {
			continue
		}
		v, given := in.Secrets[f.Key]
		switch {
		case given && v == "":
			d.Secrets[f.Key] = SecretPlan{Action: SecretClear}
		case given:
			p := SecretPlan{Action: SecretSet}
			if f.Target {
				p = mask(v)
			}
			d.Secrets[f.Key] = p
			if err := s.v.Put(c.Tx, d.ID, f.Key, v, now); err != nil {
				return nil, err
			}
		case usable[f.Key]:
			d.Secrets[f.Key] = SecretPlan{Action: SecretKeep}
			keep = append(keep, f.Key)
		}
	}
	if len(keep) > 0 { // 沿用的凭据也复制到草稿名下：草稿测试与确认只看草稿自己的凭据
		if err := s.v.Copy(c.Tx, d.Connection, d.ID, keep, now); err != nil {
			return nil, err
		}
	}
	if d.Connection != "" {
		d.Changes = diff(cur.Config, d.Config, changed, d.Secrets)
		if old, err := routedTo(c.Tx, d.Connection); err != nil {
			return nil, err
		} else if !slices.Equal(old, d.Routes) {
			d.Changes = append(d.Changes, "routes")
			sort.Strings(d.Changes)
		}
	}
	if err := s.supersede(c, d.target); err != nil {
		return nil, err
	}
	if err := insertDraft(c.Tx, d); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvProposed, Object: d.ID, Payload: draftPayload(d, changed)}); err != nil {
		return nil, err
	}
	return d, nil
}

// diff 列出改配置草稿与现用值不同的字段名（凭据只按"替换或清除"算）。
func diff(old, cfg map[string]string, secretsChanged []string, plans map[string]SecretPlan) []string {
	var out []string
	for k, v := range cfg {
		if old[k] != v {
			out = append(out, k)
		}
	}
	for k := range old {
		if _, ok := cfg[k]; !ok {
			out = append(out, k)
		}
	}
	for _, k := range secretsChanged {
		if plans[k].Action != SecretKeep {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// fingerprint 是连接现用值的指纹：名字、非凭据配置与凭据密文行（每次写入换 nonce，所以替换凭据也会变）。启停不算。
func fingerprint(tx *store.Tx, r record) (string, error) {
	cfg, err := json.Marshal(r.Config) // map 的键按序编码
	if err != nil {
		return "", err
	}
	stamp, err := secrets.Stamp(tx, r.ID)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(r.Name + "\x00" + string(cfg) + "\x00" + stamp))
	return hex.EncodeToString(h[:]), nil
}

// supersede 让同一目标仍待确认的旧草稿转为已取代，并删掉它的凭据。
func (s *Service) supersede(c *command.Ctx, target string) error {
	old, err := listDrafts(c.Tx, "WHERE target = ? AND state = ?", target, DraftPending)
	if err != nil {
		return err
	}
	for _, o := range old {
		if err := closeDraft(c.Tx, o.ID, DraftSuperseded, c.Now()); err != nil {
			return err
		}
		o.State = DraftSuperseded
		if _, err := c.Emit(domain.Event{Type: EvDraftSuperseded, Object: o.ID, Payload: draftPayload(o, nil)}); err != nil {
			return err
		}
	}
	return nil
}

func draftPayload(d Draft, changed []string) json.RawMessage {
	b, err := json.Marshal(map[string]any{"id": d.ID, "type": d.Type, "name": d.Name, "connection": d.Connection,
		"routes": d.Routes, "state": d.State, "secrets_changed": changed, "changes": d.Changes})
	if err != nil {
		panic(err) // 固定结构
	}
	return b
}

func (s *Service) applyDrafts(c *command.Ctx) (any, error) {
	ds, err := listDrafts(c.Tx, "WHERE state = ? OR closed_at >= ?", DraftPending, c.Now().Add(-draftShown).UnixNano())
	if err != nil {
		return nil, err
	}
	out := []Draft{}
	for _, d := range ds {
		if isWeb(c.Caller) || d.ProposedBy == proposer(c.Caller) {
			out = append(out, d)
		}
	}
	return map[string]any{"drafts": out}, nil
}

// ownPending 读一份待确认、未过期、由调用者提交（或调用者是 Web 本人）的草稿；别人的草稿按不存在处理。
func ownPending(c *command.Ctx) (Draft, error) {
	var in idInput
	if err := strict(c.Input, &in); err != nil {
		return Draft{}, err
	}
	d, err := getDraft(c.Tx, in.ID)
	if err == nil && !isWeb(c.Caller) && d.ProposedBy != proposer(c.Caller) {
		err = draftNotFound(in.ID)
	}
	if err != nil {
		return Draft{}, err
	}
	return d, pendingErr(d, c.Now())
}

func draftNotFound(id string) error {
	return domain.NewError(domain.CodeNotFound, "connection draft "+id+" not found", "list your drafts with apa connection drafts")
}

// pendingErr：草稿不是待确认、或已过期时给出确定的冲突原因（why 以原因码开头）。
func pendingErr(d Draft, now time.Time) error {
	reason := ""
	switch {
	case d.State != DraftPending:
		reason = "draft_" + d.State
	case !now.Before(d.ExpiresAt):
		reason = "draft_expired"
	default:
		return nil
	}
	return domain.NewError(domain.CodeConflict, reason+": the draft is no longer waiting for confirmation", "ask the Agent to propose the channel again")
}

// DraftTarget 是草稿测试要调用的插件目标：凭据按草稿 id 取，只能是草稿里的目标（§7.6）。
type DraftTarget struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Settings map[string]string `json:"settings"`
}

func (s *Service) applyDraftTest(c *command.Ctx) (any, error) {
	d, err := ownPending(c)
	if err != nil {
		return nil, err
	}
	if !isWeb(c.Caller) && keepsLiveSecrets(d) && slices.ContainsFunc(d.Changes, func(k string) bool { return k != "routes" }) {
		return nil, domain.NewError(domain.CodeWebOnly,
			"draft_test_kept_secrets: the draft reuses the connection's saved credentials with a changed address or receiver; they only go where the owner has confirmed",
			"give every credential again in a new draft to test it, or ask the owner to confirm it in the Web settings")
	}
	return DraftTarget{ID: d.ID, Type: d.Type, Settings: d.Config}, nil
}

// keepsLiveSecrets：草稿沿用了已有连接的生效凭据（提交时复制到草稿名下）。这些凭据只发往本人确认过的去处，
// 所以草稿的配置或任一凭据与现用值不同时（只改路由除外），Agent 不能测，本人确认后才发（§7.6）。
func keepsLiveSecrets(d Draft) bool {
	for _, p := range d.Secrets {
		if p.Action == SecretKeep {
			return true
		}
	}
	return false
}

// SetDraftTest 记下草稿最近一次测试的结果（覆盖写、不记 Event，与连接现状同类）。草稿已不存在时什么也不写。
func SetDraftTest(tx *store.Tx, id string, ok bool, problem string, now time.Time) error {
	b, err := json.Marshal(DraftTest{OK: ok, Problem: problem, At: now.UTC()})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE connections_draft SET test = ? WHERE id = ?`, string(b), id)
	return err
}

func (s *Service) applyDiscard(c *command.Ctx) (any, error) {
	d, err := ownPending(c)
	if err != nil {
		return nil, err
	}
	if err := closeDraft(c.Tx, d.ID, DraftDiscarded, c.Now()); err != nil {
		return nil, err
	}
	d.State = DraftDiscarded
	if _, err := c.Emit(domain.Event{Type: EvDraftDiscarded, Object: d.ID, Payload: draftPayload(d, nil)}); err != nil {
		return nil, err
	}
	return map[string]string{"id": d.ID, "state": d.State}, nil
}

// applyConfirm 只在 Web：在一个事务里核对草稿仍待确认、未过期、改配置时目标连接没被改过，
// 然后新建（启用）或改连接、换凭据属主、写路由。任一核对不过都不生效。
func (s *Service) applyConfirm(c *command.Ctx) (any, error) {
	var in idInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	d, err := getDraft(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	if err := pendingErr(d, c.Now()); err != nil {
		return nil, err
	}
	decl, ok := s.decl(d.Type)
	if !ok {
		return nil, invalid("this connection type is not available in this build", "discard the draft")
	}
	now := c.Now()
	var r record
	var moved, cleared []string
	for k, p := range d.Secrets {
		if p.Action == SecretClear {
			cleared = append(cleared, k)
		} else {
			moved = append(moved, k)
		}
	}
	sort.Strings(moved)
	sort.Strings(cleared)
	if d.Connection == "" {
		if err := nameFree(c.Tx, d.Name, ""); err != nil {
			return nil, domain.NewError(domain.CodeConflict, "name_taken: another connection is already called "+d.Name, "ask the Agent to propose it under another name")
		}
		id, err := newID(c.Tx, decl.Direction)
		if err != nil {
			return nil, err
		}
		r = record{ID: id, Direction: decl.Direction, Type: d.Type, Name: d.Name, Config: d.Config, Enabled: true, CreatedAt: now, UpdatedAt: now}
		if err := insert(c.Tx, r); err != nil {
			return nil, err
		}
	} else {
		changed := false
		if r, err = get(c.Tx, d.Connection); err == nil {
			var fp string
			fp, err = fingerprint(c.Tx, r)
			changed = fp != d.base
		}
		if isNotFound(err) || changed {
			return nil, domain.NewError(domain.CodeConflict, "connection_changed: the connection changed after the draft was proposed", "ask the Agent to propose again")
		} else if err != nil {
			return nil, err
		}
		r.Config, r.UpdatedAt = d.Config, now // 启停不变：改配置的草稿只换配置、凭据与路由
		if err := update(c.Tx, r); err != nil {
			return nil, err
		}
	}
	if err := s.v.Move(c.Tx, d.ID, r.ID, moved, now); err != nil {
		return nil, err
	}
	for _, k := range cleared {
		if err := secrets.Delete(c.Tx, r.ID, k); err != nil {
			return nil, err
		}
	}
	if err := secrets.DeleteOwner(c.Tx, d.ID); err != nil {
		return nil, err
	}
	if err := setRoutes(c, r.ID, d.Routes); err != nil {
		return nil, err
	}
	ev := EvCreated
	if d.Connection != "" {
		ev = EvConfigured
	}
	if _, err := c.Emit(domain.Event{Type: ev, Object: r.ID, Payload: payload(r, append(moved, cleared...))}); err != nil {
		return nil, err
	}
	if _, err := c.Tx.Exec(`UPDATE connections_draft SET state = ?, closed_at = ?, connection = ? WHERE id = ?`, DraftConfirmed, now.UnixNano(), r.ID, d.ID); err != nil {
		return nil, err
	}
	d.State, d.Connection = DraftConfirmed, r.ID
	if _, err := c.Emit(domain.Event{Type: EvConfirmed, Object: d.ID, Payload: draftPayload(d, nil)}); err != nil {
		return nil, err
	}
	return s.view(c.Tx, r)
}

// setRoutes 让草稿里的类别指向这个连接；原来指向它而草稿没勾的类别回到默认路由。经策略模块写入（本人 Web 操作）。
func setRoutes(c *command.Ctx, id string, want []string) error {
	route := "channel:" + id
	def := policy.Default().Disturb.Routes
	_, err := policy.Update(c.Tx, c.Caller.Actor, "web settings: confirm connection draft", func(p *policy.Policy) error {
		if p.Disturb.Routes == nil {
			p.Disturb.Routes = map[string]string{}
		}
		for _, cl := range classes {
			switch {
			case slices.Contains(want, cl):
				p.Disturb.Routes[cl] = route
			case p.Disturb.Routes[cl] == route:
				p.Disturb.Routes[cl] = def[cl]
			}
		}
		return nil
	})
	return err
}

var draftActor = domain.Actor{Type: domain.ActorSystem, Principal: "connections", Via: domain.ViaClock}

// ExpireDrafts 把到期仍待确认的草稿转为已过期，连同它的凭据一起删除（§7.6）；由维护循环按时钟调用。
func ExpireDrafts(ctx context.Context, st *store.Store, now time.Time) error {
	return st.Update(ctx, func(tx *store.Tx) error {
		ds, err := listDrafts(tx, "WHERE state = ? AND expires_at <= ?", DraftPending, now.UnixNano())
		if err != nil {
			return err
		}
		for _, d := range ds {
			if err := closeDraft(tx, d.ID, DraftExpired, now); err != nil {
				return err
			}
			d.State = DraftExpired
			if _, err := eventlog.Append(tx, domain.Event{Type: EvDraftExpired, Object: d.ID, Actor: draftActor, Payload: draftPayload(d, nil), At: now}); err != nil {
				return err
			}
		}
		return nil
	})
}

// closeDraft 结束一份草稿并删掉它名下的凭据（只删草稿自己的属主，不碰任何连接的凭据）。
func closeDraft(tx *store.Tx, id, state string, now time.Time) error {
	if _, err := tx.Exec(`UPDATE connections_draft SET state = ?, closed_at = ? WHERE id = ?`, state, now.UnixNano(), id); err != nil {
		return err
	}
	return secrets.DeleteOwner(tx, id)
}

func insertDraft(tx *store.Tx, d Draft) error {
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	if d.Changes == nil {
		d.Changes = []string{}
	}
	_, err := tx.Exec(`INSERT INTO connections_draft (id, target, connection, base, type, name, config, routes, secrets, changes, state, proposed_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.target, d.Connection, d.base, d.Type, d.Name, enc(d.Config), enc(d.Routes), enc(d.Secrets), enc(d.Changes), d.State, d.ProposedBy,
		d.CreatedAt.UnixNano(), d.ExpiresAt.UnixNano())
	return err
}

func listDrafts(tx *store.Tx, where string, args ...any) ([]Draft, error) {
	rs, err := tx.Query(`SELECT id, target, connection, base, type, name, config, routes, secrets, changes, state, proposed_by, test, created_at, expires_at, closed_at
		FROM connections_draft `+where+` ORDER BY state = 'pending' DESC, created_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Draft
	for rs.Next() {
		var (
			d                               Draft
			cfg, routes, sec, changes, test string
			created, expires, closed        int64
		)
		if err := rs.Scan(&d.ID, &d.target, &d.Connection, &d.base, &d.Type, &d.Name, &cfg, &routes, &sec, &changes, &d.State, &d.ProposedBy, &test,
			&created, &expires, &closed); err != nil {
			return nil, err
		}
		for _, x := range []struct {
			raw string
			v   any
		}{{cfg, &d.Config}, {routes, &d.Routes}, {sec, &d.Secrets}, {changes, &d.Changes}} {
			if err := json.Unmarshal([]byte(x.raw), x.v); err != nil {
				return nil, err
			}
		}
		if test != "" {
			d.Test = &DraftTest{}
			if err := json.Unmarshal([]byte(test), d.Test); err != nil {
				return nil, err
			}
		}
		d.CreatedAt, d.ExpiresAt, d.ClosedAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC(), at(closed)
		d.WebPath = webPath(d.ID)
		out = append(out, d)
	}
	return out, rs.Err()
}

// webPath 是草稿待确认卡在 Web 的位置（给 Agent 转交本人）。
func webPath(id string) string { return "/settings/channels#draft-" + id }

func getDraft(tx *store.Tx, id string) (Draft, error) {
	ds, err := listDrafts(tx, "WHERE id = ?", id)
	if err != nil {
		return Draft{}, err
	}
	if len(ds) == 0 {
		return Draft{}, draftNotFound(id)
	}
	return ds[0], nil
}
