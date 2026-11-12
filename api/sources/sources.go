// Package sources 是来源的 HTTP 路由（附录 B.2、设计 §9.5、§14.12）：
//
//	GET/POST /sources        GET/PATCH /sources/{id}
//	POST /sources/{id}/webhook          创建或轮换 webhook 接收密钥
//	DELETE /sources/{id}/webhook        吊销该来源的全部接收密钥，并删除 HMAC 密钥文件
//	GET /sources/{id}/ingests           入站记录
//	POST /ingests/{id}/replay           重放
//	GET /sources/{id}/events            来源事件，可按 disposition 过滤（逗号分隔多个），after + limit 分页
//	GET /sources/{id}/rejects           没有入账的输出行及原因，after + limit 分页
//	POST /source-events/{id}/propose    把未判定事件转为建议
//	POST /source-events/{id}/ignore     忽略一条未判定事件
//	POST /source-events/ignore          批量忽略未判定积压：按来源或全部
//
// 路由只做传输：解码、选命令、渲染。授权与校验都在内核命令管线：可执行命令（script 来源）走只 Web 的命令，
// delegate 得到 web_only（退出码 20，留痕）；pull 的命令须在随发行附带的清单内。/ingest/{source_id} 接收路由不在本包。
package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
)

// 本包登记的命令：内核没有对应命令的两个 delegate 级动作（§9.5），在此补成命令以得到授权与事件。
const (
	CmdWebhookKey    = "source.webhook_key"
	CmdWebhookRevoke = "source.webhook_revoke"
	CmdReplay        = "source.replay"
)

// 事件类型：只记机制信息，不含密钥与 payload。
const (
	EvWebhookCreated = "source.webhook_key_created"
	EvWebhookRotated = "source.webhook_key_rotated"
	EvWebhookRevoked = "source.webhook_key_revoked"
	EvReplayed       = "source.replay_requested"
)

// webhookKeyPrefix 是 kernel/auth 签发 webhook 接收密钥的明文前缀（auth 未导出它）。
const webhookKeyPrefix = "apa_whk_"

// RotateGrace 是轮换后旧密钥继续有效的时间（"新旧短期并存"，§9.5）。
const RotateGrace = time.Hour

// IdempotencyHeader 是写请求可选的幂等键头（不用于 webhook 密钥：明文不进幂等结果表）。
const IdempotencyHeader = "Idempotency-Key"

// Replayer 重放一条入站记录，并在签发或轮换接收密钥时按校验方案保管明文（*webhook.Service 满足它）。
type Replayer interface {
	Replay(ctx context.Context, ingestID string) error
	StoreKey(tx *store.Tx, sourceID, secret string) error // secret 为空：删除明文文件
}

// Proposer 把未判定事件转为建议（*inbound.Service 满足它）。
type Proposer interface {
	ProposeUnjudged(ctx context.Context, id command.Identity, event string) (domain.SourceEvent, error)
}

const (
	defaultEventLimit = 100
	maxEventLimit     = 500
)

type api struct {
	st *store.Store
	x  *command.Executor
	rp Replayer
	pr Proposer
	au *auth.Auth
}

// Mount 在 x 上登记本包的两条命令并返回路由分组，挂到 core.Deps.Groups 即生效。
// x 须已由 registry.Register 登记来源命令；au 用于签发 webhook 密钥。
func Mount(st *store.Store, x *command.Executor, au *auth.Auth, rp Replayer, pr Proposer) (core.RouteGroup, error) {
	a := &api{st: st, x: x, rp: rp, pr: pr, au: au}
	for _, s := range []command.Spec{
		{Name: CmdWebhookKey, Action: authz.ActionManageSource, Tier: authz.TierWords, Object: idObject, Validate: a.validateWebhookKey, Apply: a.applyWebhookKey},
		{Name: CmdWebhookRevoke, Action: authz.ActionManageSource, Tier: authz.TierWords, Object: idObject, Validate: a.validateWebhookKey, Apply: a.applyWebhookRevoke},
		{Name: CmdReplay, Action: authz.ActionManageSource, Tier: authz.TierWords, Object: idObject, Validate: a.validateReplay, Apply: a.applyReplay},
	} {
		if err := x.Register(s); err != nil {
			return nil, err
		}
	}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/sources", Handler: readable(a.list)})
		r.Handle(core.Route{Method: "POST", Path: "/sources", Handler: a.create})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}", Handler: readable(a.get)})
		r.Handle(core.Route{Method: "PATCH", Path: "/sources/{id}", Handler: a.patch})
		r.Handle(core.Route{Method: "POST", Path: "/sources/{id}/webhook", MaxBody: 1 << 10, Handler: a.webhookKey})
		r.Handle(core.Route{Method: "DELETE", Path: "/sources/{id}/webhook", MaxBody: 1, Handler: a.webhookRevoke})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/ingests", Handler: readable(a.ingests)})
		r.Handle(core.Route{Method: "POST", Path: "/ingests/{id}/replay", MaxBody: 1 << 10, Handler: a.replay})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/events", Handler: readable(a.events)})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/rejects", Handler: readable(a.rejects)})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/propose", MaxBody: 1 << 10, Handler: a.propose})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/ignore", MaxBody: 1 << 10, Handler: a.ignoreOne})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/ignore", MaxBody: 1 << 10, Handler: a.ignoreBatch})
	}, nil
}

// readable：读来源对 observer 及以上开放；ext 与 webhook 凭据不能读账本（与 api/projects 一致）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
			return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read sources", "use an owner or agent credential")
		}
		return h(w, r, p)
	}
}

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func idObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var h struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(in, &h); err != nil {
		return authz.Object{}, invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	return authz.Object{ID: h.ID}, nil
}

func body(r *http.Request) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := core.DecodeJSON(r, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, invalid("request body must be a JSON object", "send a JSON object")
	}
	return m, nil
}

func (a *api) run(r *http.Request, p *core.Principal, cmd string, in map[string]json.RawMessage, idem bool) (command.Result, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return command.Result{}, err
	}
	req := command.Request{Identity: p.Identity, Command: cmd, Input: raw, Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL}
	if idem {
		req.IdemKey = r.Header.Get(IdempotencyHeader)
	}
	return a.x.Do(r.Context(), req)
}

func (a *api) source(r *http.Request, id string) (s domain.Source, err error) {
	err = a.st.View(r.Context(), func(tx *store.Tx) (e error) { s, e = registry.Get(tx, id); return })
	return
}

// ---- 来源登记 ----

func (a *api) list(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var ss []domain.Source
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { ss, err = registry.List(tx); return }); err != nil {
		return err
	}
	if ss == nil {
		ss = []domain.Source{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"sources": ss})
}

func (a *api) get(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	s, err := a.source(r, r.PathValue("id"))
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, s)
}

// create：type=script 是可执行命令的登记，走只 Web 的命令；其余走 delegate 可用的命令。
func (a *api) create(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := body(r)
	if err != nil {
		return err
	}
	cmd := registry.CmdRegister
	var typ string
	if raw, ok := in["type"]; ok && json.Unmarshal(raw, &typ) == nil && domain.SourceType(typ) == domain.SourceScript {
		cmd = registry.CmdRegisterExec
	}
	res, err := a.run(r, p, cmd, in, true)
	if err != nil {
		return err
	}
	return writeValue(w, http.StatusCreated, res)
}

// patch：可改字段交给 update 命令（script 来源走只 Web 的那条）；enabled 单独交给启停命令。
func (a *api) patch(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	in, err := body(r)
	if err != nil {
		return err
	}
	cur, err := a.source(r, id)
	if err != nil {
		return err
	}
	enabled, hasEnabled := in["enabled"]
	delete(in, "enabled")
	if len(in) == 0 && !hasEnabled {
		return invalid("nothing to update", "send at least one field")
	}
	var res command.Result
	if len(in) > 0 {
		in["id"], _ = json.Marshal(id)
		cmd := registry.CmdUpdate
		if cur.Type == domain.SourceScript {
			cmd = registry.CmdUpdateExec
		}
		if res, err = a.run(r, p, cmd, in, true); err != nil {
			return err
		}
	}
	if hasEnabled {
		var on bool
		if json.Unmarshal(enabled, &on) != nil {
			return invalid("enabled must be true or false", "fix enabled")
		}
		e := map[string]json.RawMessage{"enabled": enabled}
		e["id"], _ = json.Marshal(id)
		if res, err = a.run(r, p, registry.CmdSetEnabled, e, true); err != nil {
			return err
		}
	}
	return writeValue(w, http.StatusOK, res)
}

func writeValue(w http.ResponseWriter, status int, res command.Result) error {
	return core.WriteJSON(w, status, res.Value)
}

// ---- webhook 接收密钥 ----

func (a *api) validateWebhookKey(tx *store.Tx, o authz.Object, _ json.RawMessage) error {
	s, err := registry.Get(tx, o.ID)
	if err != nil {
		return err
	}
	if s.Type != domain.SourceWebhook {
		return invalid("only webhook sources have a receive key", "register a webhook source first")
	}
	return nil
}

// applyWebhookKey：没有有效密钥就创建，有就轮换（旧的在 RotateGrace 后失效）。明文只在响应里出现一次；
// HMAC 方案的来源另由 webhook 模块在同一事务里把明文写进密钥文件（§14.2），账本与事件里只有凭据 id。
func (a *api) applyWebhookKey(c *command.Ctx) (any, error) {
	id := c.Object.ID
	all, err := a.au.List(c.Tx)
	if err != nil {
		return nil, err
	}
	var current string
	for _, cr := range all { // 按签发顺序，取最后一个仍有效的
		if cr.Kind == auth.Webhook && cr.Scope == id && cr.RevokedAt.IsZero() && cr.ExpiresAt.IsZero() {
			current = cr.ID
		}
	}
	var (
		cred   auth.Credential
		secret string
		ev     = EvWebhookCreated
	)
	if current == "" {
		cred, secret, err = a.au.Issue(c.Tx, auth.Spec{Kind: auth.Webhook, Scope: id})
	} else {
		ev = EvWebhookRotated
		cred, secret, err = a.au.Rotate(c.Tx, current, RotateGrace)
	}
	if err != nil {
		return nil, err
	}
	if err := a.rp.StoreKey(c.Tx, id, secret); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"credential": cred.ID})
	if _, err := c.Emit(domain.Event{Type: ev, Object: id, Payload: payload}); err != nil {
		return nil, err
	}
	return map[string]any{"credential": cred, "secret": secret, "url": "/ingest/" + id, "rotated": current != ""}, nil
}

// applyWebhookRevoke 吊销该来源全部有效的接收密钥（含宽限期内的旧密钥），并在同一事务里删除明文密钥文件。
// 删除失败整个命令回滚、账本不变，调用方重试；不会出现"账本已吊销、明文仍在"的半完成状态。
func (a *api) applyWebhookRevoke(c *command.Ctx) (any, error) {
	id := c.Object.ID
	all, err := a.au.List(c.Tx)
	if err != nil {
		return nil, err
	}
	var revoked []string
	for _, cr := range all {
		if cr.Kind == auth.Webhook && cr.Scope == id && cr.RevokedAt.IsZero() {
			if err := a.au.Revoke(c.Tx, cr.ID); err != nil {
				return nil, err
			}
			revoked = append(revoked, cr.ID)
		}
	}
	if err := a.rp.StoreKey(c.Tx, id, ""); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string][]string{"credentials": revoked})
	if _, err := c.Emit(domain.Event{Type: EvWebhookRevoked, Object: id, Payload: payload}); err != nil {
		return nil, err
	}
	return map[string]any{"revoked": len(revoked)}, nil
}

func (a *api) webhookRevoke(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in := map[string]json.RawMessage{}
	in["id"], _ = json.Marshal(r.PathValue("id"))
	res, err := a.run(r, p, CmdWebhookRevoke, in, false)
	if err != nil {
		return err
	}
	return writeValue(w, http.StatusOK, res)
}

func (a *api) webhookKey(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in := map[string]json.RawMessage{}
	in["id"], _ = json.Marshal(r.PathValue("id"))
	res, err := a.run(r, p, CmdWebhookKey, in, false) // 无幂等键：明文不进幂等结果表
	if err != nil {
		return err
	}
	status := http.StatusCreated
	var out struct {
		Rotated bool `json:"rotated"`
	}
	if json.Unmarshal(res.Value, &out) == nil && out.Rotated {
		status = http.StatusOK
	}
	return writeValue(w, status, res)
}

// ---- 入站记录与重放 ----

// ingestView 是排查视图：原始 payload、校验结果、转换结果。payload 不是 UTF-8 时以 base64 给出。
type ingestView struct {
	ID          string            `json:"id"`
	SourceID    string            `json:"source_id"`
	DeliveryKey string            `json:"delivery_key"`
	ReceivedAt  time.Time         `json:"received_at"`
	State       string            `json:"state"`
	Signed      bool              `json:"signed"`
	Gen         int               `json:"gen"`
	Events      int               `json:"events"`
	Reason      string            `json:"reason,omitempty"`
	Headers     map[string]string `json:"headers"`
	Body        *string           `json:"body,omitempty"`
	BodyBase64  string            `json:"body_base64,omitempty"`
	BodySize    int               `json:"body_size"`
	Purged      bool              `json:"purged"`
}

func viewOf(g webhook.Ingest) ingestView {
	v := ingestView{ID: g.ID, SourceID: g.SourceID, DeliveryKey: g.DeliveryKey, ReceivedAt: g.ReceivedAt, State: g.State, Signed: g.Signed,
		Gen: g.Gen, Events: g.Events, Reason: g.Reason, Headers: g.Headers, BodySize: g.BodySize, Purged: g.Purged}
	// token 方案的接收密钥放在方案指定的请求头里，内核只丢弃 Authorization 等通用凭据头；排查视图不能把它交给读者。
	v.Headers = make(map[string]string, len(g.Headers))
	for k, h := range g.Headers {
		if strings.Contains(h, webhookKeyPrefix) {
			h = "[redacted]"
		}
		v.Headers[k] = h
	}
	switch {
	case g.Purged || len(g.Body) == 0:
	case utf8.Valid(g.Body):
		s := string(g.Body)
		v.Body = &s
	default:
		v.BodyBase64 = base64.StdEncoding.EncodeToString(g.Body)
	}
	return v
}

func (a *api) ingests(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	// 原始请求体未经转发白名单，也无从按发送者过滤：发送者要等转换后才有（§14.3、§14.6）。
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credential cannot read raw ingests", "read forwarded messages from the context instead")
	}
	id := r.PathValue("id")
	var gs []webhook.Ingest
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		if _, err := registry.Get(tx, id); err != nil {
			return err
		}
		var err error
		gs, err = webhook.Ingests(tx, id)
		return err
	}); err != nil {
		return err
	}
	out := make([]ingestView, 0, len(gs))
	for _, g := range gs {
		out = append(out, viewOf(g))
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"ingests": out})
}

func (a *api) validateReplay(tx *store.Tx, o authz.Object, _ json.RawMessage) error {
	_, err := webhook.IngestByID(tx, o.ID)
	return err
}

// applyReplay 只留痕与授权；重放本身由 webhook.Service 在命令提交后执行（它自己开事务）。
func (a *api) applyReplay(c *command.Ctx) (any, error) {
	g, err := webhook.IngestByID(c.Tx, c.Object.ID)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"ingest": g.ID})
	if _, err := c.Emit(domain.Event{Type: EvReplayed, Object: g.SourceID, Payload: payload}); err != nil {
		return nil, err
	}
	return map[string]string{"id": g.ID}, nil
}

func (a *api) replay(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	in := map[string]json.RawMessage{}
	in["id"], _ = json.Marshal(id)
	if _, err := a.run(r, p, CmdReplay, in, false); err != nil {
		return err
	}
	if err := a.rp.Replay(r.Context(), id); err != nil {
		return err
	}
	var g webhook.Ingest
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { g, err = webhook.IngestByID(tx, id); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, viewOf(g))
}

// ---- 来源事件 ----

// pageParams 解析 after + limit 分页参数（来源事件与拒收记录共用）。
func pageParams(q url.Values) (after int64, limit int, err error) {
	if s := q.Get("after"); s != "" {
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n < 0 {
			return 0, 0, invalid("after must be a non-negative integer", "use next_after from the previous page")
		}
		after = n
	}
	limit = defaultEventLimit
	if s := q.Get("limit"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > maxEventLimit {
			return 0, 0, invalid("limit is out of range", "use 1 <= limit <= "+strconv.Itoa(maxEventLimit))
		}
		limit = n
	}
	return after, limit, nil
}

func (a *api) events(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	// 这里是未过滤的外部内容，运行凭据只能经转发白名单读挂靠消息（§9.4、§14.3）。
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credential cannot list source events", "read forwarded messages from the context instead")
	}
	q := r.URL.Query()
	var ds []domain.Disposition
	if raw := q.Get("disposition"); raw != "" {
		for _, x := range strings.Split(raw, ",") {
			d := domain.Disposition(x)
			switch d {
			case domain.DispositionPending, domain.DispositionAttached, domain.DispositionProposed, domain.DispositionIgnored,
				domain.DispositionUnjudged, domain.DispositionExpired, domain.DispositionDigest, domain.DispositionAlerted:
			default:
				return invalid("disposition is not valid", "use pending, attached, proposed, ignored, digest, alerted, unjudged or expired, comma separated")
			}
			ds = append(ds, d)
		}
	}
	after, limit, err := pageParams(q)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var (
		evs  []domain.SourceEvent
		next int64
	)
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		if !(p.Web && inbound.IsCaptureSource(id)) { // 对话内容只给本人；其他凭据和登记外的来源一样是 404
			if _, err := registry.Get(tx, id); err != nil {
				return err
			}
		}
		var err error
		evs, next, err = runner.EventPage(tx, id, ds, after, limit)
		return err
	}); err != nil {
		return err
	}
	out := map[string]any{"events": evs}
	if next != 0 {
		out["next_after"] = next
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

func (a *api) propose(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	ev, err := a.pr.ProposeUnjudged(r.Context(), p.Identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, ev)
}

// ignoreOne 忽略一条积压事件，返回更新后的来源事件；不在积压里是 409。
func (a *api) ignoreOne(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	in := map[string]json.RawMessage{}
	in["event"], _ = json.Marshal(id)
	if _, err := a.run(r, p, inbound.CmdIgnoreUnjudged, in, true); err != nil {
		return err
	}
	var ev domain.SourceEvent
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, id); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, ev)
}

// ignoreBatch 忽略积压：请求体 {"source": "<id>"} 只忽略该来源，{"all": true} 忽略全部；返回 {"ignored": n}。
func (a *api) ignoreBatch(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Source string `json:"source,omitempty"`
		All    bool   `json:"all,omitempty"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	m := map[string]json.RawMessage{}
	if in.Source != "" {
		m["source"], _ = json.Marshal(in.Source)
	}
	if in.All {
		m["all"] = json.RawMessage("true")
	}
	res, err := a.run(r, p, inbound.CmdIgnoreUnjudged, m, true)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res.Value)
}

// rejects 返回来源没有入账的输出行及原因。原始行在落账时已截断到 512 字节；与入站记录的请求头同一规则，含接收密钥明文前缀的整行遮蔽。
// 原始行是未过滤的外部输出，运行凭据不能读（与 events、ingests 一致）。
func (a *api) rejects(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credential cannot list source rejects", "read forwarded messages from the context instead")
	}
	after, limit, err := pageParams(r.URL.Query())
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var (
		rs   []runner.Reject
		next int64
	)
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		if _, err := registry.Get(tx, id); err != nil {
			return err
		}
		var err error
		rs, next, err = runner.RejectPage(tx, id, after, limit)
		return err
	}); err != nil {
		return err
	}
	if rs == nil {
		rs = []runner.Reject{}
	}
	for i := range rs {
		if strings.Contains(rs[i].Raw, webhookKeyPrefix) {
			rs[i].Raw = "[redacted]"
		}
	}
	out := map[string]any{"rejects": rs}
	if next != 0 {
		out["next_after"] = next
	}
	return core.WriteJSON(w, http.StatusOK, out)
}
