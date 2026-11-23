// Package sources 是来源的 HTTP 路由（附录 B.2、设计 §9.5、§14.12）：
//
//	GET/POST /sources        GET/PATCH /sources/{id}
//	GET /sources/declared-name          接入层为一份登记声明的显示名（中英），新登记表单预填用
//	GET /settings/sources/{id}/self-sender  运行一次来源程序的 identity 模式，取本人身份供预填（只 Web）
//	PUT /settings/sources/{id}/trusted-senders  只替换这个来源的转发白名单（只 Web；需确认的来源只能清空）
//	POST /sources/{id}/webhook          创建或轮换 webhook 接收密钥
//	DELETE /sources/{id}/webhook        吊销该来源的全部接收密钥，并删除 HMAC 密钥文件
//	GET /sources/{id}/ingests           入站记录
//	POST /ingests/{id}/replay           重放
//	GET /sources/{id}/events            来源事件，可按 disposition 过滤（逗号分隔多个），after + limit 分页；detail 是预览，另带 content 清单
//	GET /source-events/{id}[?part=]     单条读取来源事件及其原文（Web 全文，token 按外发范围裁剪，运行凭据与 ext 密钥 403）
//	GET /storage                        存储用量：账本、附件、各来源原文
//	GET /sources/{id}/rejects           没有入账的输出行及原因，after + limit 分页；sort=recent 从新到旧
//	GET /source-events/{id}/outbound-preview  一条事件在 full、excerpt、meta 下送给模型的内容（只读）
//	POST /source-events/{id}/propose    把未判定事件或 digest 事件转为建议
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
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	apipolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
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
	// CmdFetchAttachment 记取回的附件：本人在 Web 或 delegate 附本人原话发起（§7.6、§14.5）。
	CmdFetchAttachment = "source.fetch_attachment"
)

// 事件类型：只记机制信息，不含密钥与 payload。
const (
	EvWebhookCreated = "source.webhook_key_created"
	EvWebhookRotated = "source.webhook_key_rotated"
	EvWebhookRevoked = "source.webhook_key_revoked"
	EvReplayed       = "source.replay_requested"
	// EvAttachmentFetched 是内部审计事件（不进对外事件类型表）：载荷 {"event", "n", "name", "blob", "size"}，不含附件内容。
	EvAttachmentFetched = "source.attachment_fetched"
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
	// Split 把误并入建议 from 的来源事件拆成一条新建议，返回新建议的 id（§9.3）。
	Split(ctx context.Context, id command.Identity, event, from string) (string, error)
	// Unmerge 撤销跨来源合并的一条折叠：被折叠的建议回到建议页（§9.3）。
	Unmerge(ctx context.Context, id command.Identity, item string) error
	// BacklogRules 是积压放开第 1 步：只跑规则，不调模型（§9.3）。req 带调用者身份与本人原话。
	BacklogRules(ctx context.Context, req command.Request, source string) (inbound.RulesResult, error)
	// BacklogScore 是积压放开第 2 步：影子打分，只写判断记录，不改处置（§9.3）。没有判断端点时是 409。
	BacklogScore(ctx context.Context, req command.Request, source string) (inbound.ScoreResult, error)
	// BacklogRejudge 是积压放开第 4 步：放开重判，进入建议流的数量受每日上限约束（§9.3）。limit 大于 0 时本次至多放这么多条进入建议流。
	BacklogRejudge(ctx context.Context, req command.Request, source string, limit int) (inbound.ReleaseResult, error)
}

const (
	defaultEventLimit = 100
	maxEventLimit     = 500
)

// Namer 返回接入层为登记 s 声明的显示名（中英）；没有声明时 ok 为 false。由装配方给出（§9.1）。
type Namer func(s domain.Source) (zh, en string, ok bool)

// Text 是中英两份文字。
type Text struct {
	Zh string `json:"zh"`
	En string `json:"en"`
}

// Sender 是来源发件人可信的结论（§9.1）：trust 为 verified 或 unverified；可信时 hint 是格式提示；
// self 为 identity 时可经 self-sender 取本人身份。
type Sender struct {
	Trust string `json:"trust"`
	Hint  *Text  `json:"hint,omitempty"`
	Self  string `json:"self,omitempty"`
}

// Declared 是装配方按接入声明给出的查询（§9.1）；零值字段等于没有声明。
type Declared struct {
	Name   Namer
	Sender func(s domain.Source) Sender
	// SelfSender 运行来源程序的 identity 模式，返回本人身份（§9.2）。
	SelfSender func(ctx context.Context, s domain.Source) (string, error)
	// CanFetch 报告来源的程序是否在接入声明里写了 content.attachments: fetch（附录 D.8）；nil 等于都不支持。
	CanFetch func(s domain.Source) bool
	// FetchAttachment 经外呼调用器运行来源程序的 fetch 模式，返回第 n 个附件的字节（§7.6）：超时 60 秒、不超过 32 MiB；
	// 运行失败、超时与超限返回 fetch_failed（502）。
	FetchAttachment func(ctx context.Context, s domain.Source, ev domain.SourceEvent, n int, att runner.AttachmentRef) ([]byte, error)
	// BlobsDir 是内容寻址附件目录，GET /storage 据此算附件占用；空串按没有附件算。
	BlobsDir string
}

// sourceView 是读接口给出的来源：登记加发件人结论；stats 另含事件数与原文字节数（读时汇总，与 /storage 同源）。
type sourceView struct {
	domain.Source
	Stats  json.RawMessage `json:"stats"`
	Sender *Sender         `json:"sender,omitempty"`
}

func (a *api) view(s domain.Source, u runner.Usage) (sourceView, error) {
	stats, err := withStats(s, u)
	if err != nil {
		return sourceView{}, err
	}
	v := sourceView{Source: s, Stats: stats}
	if a.dc.Sender != nil {
		sd := a.dc.Sender(s)
		v.Sender = &sd
	}
	return v, nil
}

type api struct {
	dc Declared
	st *store.Store
	x  *command.Executor
	rp Replayer
	pr Proposer
	au *auth.Auth
}

// Mount 在 x 上登记本包的两条命令并返回路由分组，挂到 core.Deps.Groups 即生效。
// x 须已由 registry.Register 登记来源命令；au 用于签发 webhook 密钥；dc 为零值等于没有任何接入层声明。
func Mount(st *store.Store, x *command.Executor, au *auth.Auth, rp Replayer, pr Proposer, dc Declared) (core.RouteGroup, error) {
	a := &api{st: st, x: x, rp: rp, pr: pr, au: au, dc: dc}
	for _, s := range []command.Spec{
		{Name: CmdWebhookKey, Action: authz.ActionManageSource, Tier: authz.TierPower, Page: "/sources", Object: idObject, Validate: a.validateWebhookKey, Apply: a.applyWebhookKey},
		{Name: CmdWebhookRevoke, Action: authz.ActionManageSource, Tier: authz.TierPower, Page: "/sources", Object: idObject, Validate: a.validateWebhookKey, Apply: a.applyWebhookRevoke},
		{Name: CmdReplay, Action: authz.ActionManageSource, Tier: authz.TierOrganize, Object: idObject, Validate: a.validateReplay, Apply: a.applyReplay},
		{Name: CmdFetchAttachment, Action: authz.ActionManageSource, Tier: authz.TierOrganize, Object: idObject, KeepVersion: true,
			Validate: a.validateFetch, Apply: a.applyFetch},
	} {
		if err := x.Register(s); err != nil {
			return nil, err
		}
	}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/sources", Handler: readable(a.list)})
		r.Handle(core.Route{Method: "POST", Path: "/sources", Handler: a.create})
		r.Handle(core.Route{Method: "GET", Path: "/sources/declared-name", Handler: readable(a.declaredName)})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}", Handler: readable(a.get)})
		r.Handle(core.Route{Method: "GET", Path: "/settings/sources/{id}/self-sender", Handler: a.selfSender})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/sources/{id}/trusted-senders", MaxBody: 64 << 10, Handler: a.trustedSenders})
		r.Handle(core.Route{Method: "PATCH", Path: "/sources/{id}", Handler: a.patch})
		r.Handle(core.Route{Method: "POST", Path: "/sources/{id}/webhook", MaxBody: 1 << 10, Handler: a.webhookKey})
		r.Handle(core.Route{Method: "DELETE", Path: "/sources/{id}/webhook", MaxBody: 1, Handler: a.webhookRevoke})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/ingests", Handler: readable(a.ingests)})
		r.Handle(core.Route{Method: "POST", Path: "/ingests/{id}/replay", MaxBody: 1 << 10, Handler: a.replay})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/events", Handler: readable(a.events)})
		r.Handle(core.Route{Method: "GET", Path: "/source-events/{id}", Handler: readable(a.sourceEvent)})
		r.Handle(core.Route{Method: "GET", Path: "/storage", Handler: readable(a.storage)})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/attachments/{n}/fetch", MaxBody: 1 << 10, Handler: a.fetchAttachment})
		r.Handle(core.Route{Method: "GET", Path: "/sources/{id}/rejects", Handler: readable(a.rejects)})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/propose", MaxBody: 1 << 10, Handler: a.propose})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/ignore", MaxBody: 1 << 10, Handler: a.ignoreOne})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/{id}/split", MaxBody: 1 << 10, Handler: a.split})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/unmerge", MaxBody: 1 << 10, Handler: a.unmerge})
		r.Handle(core.Route{Method: "GET", Path: "/merge/activity", Handler: readable(a.mergeActivity)})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/rules", MaxBody: 1 << 10, Handler: a.backlogRules})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/score", MaxBody: 1 << 10, Handler: a.backlogScore})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/rejudge", MaxBody: 1 << 10, Handler: a.backlogRejudge})
		r.Handle(core.Route{Method: "GET", Path: "/source-events/restorable", Handler: readable(a.restorable)})
		r.Handle(core.Route{Method: "GET", Path: "/source-events/{id}/outbound-preview", Handler: readable(a.outboundPreview)})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/restore", MaxBody: 1 << 10, Handler: a.restore})
		r.Handle(core.Route{Method: "POST", Path: "/source-events/ignore", MaxBody: 1 << 10, Handler: a.ignoreBatch})
	}, nil
}

// readable：读来源对 observer 及以上开放；ext 与 webhook 凭据不能读账本（与 api/projects 一致）。
// readable：webhook 密钥与非导出的 ext 密钥（渠道、转发）不读来源；导出 ext 密钥是外部程序的钥匙，按它的逐来源档位读（§14.14）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if (p.Caller.Kind == auth.Ext && !auth.IsExportExt(p.Caller.Kind, p.Caller.Actor.Principal)) || p.Caller.Kind == auth.Webhook {
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
	req := command.Request{Identity: p.Identity, Command: cmd, Input: raw, WebURL: p.WebURL}
	if idem {
		req.IdemKey = r.Header.Get(IdempotencyHeader)
	}
	return a.x.Do(r.Context(), req)
}

// runInput 以结构体输入执行命令；preflight 为真时只做认证、授权与校验，不写账本。
func (a *api) runInput(r *http.Request, p *core.Principal, cmd string, in any, preflight bool) (command.Result, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return command.Result{}, err
	}
	return a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw, WebURL: p.WebURL, Preflight: preflight})
}

func (a *api) source(r *http.Request, id string) (s domain.Source, err error) {
	err = a.st.View(r.Context(), func(tx *store.Tx) (e error) { s, e = registry.Get(tx, id); return })
	return
}

// ---- 来源登记 ----

// forExternal：外部钥匙只看得到给了它的来源，且看不到来源在本机怎么跑（命令、配置、凭据路径、游标、送模型端点的范围）。
func forExternal(s domain.Source) domain.Source {
	s.Command, s.CredentialsPath, s.Config, s.Cursor, s.Outbound = "", "", nil, "", domain.Outbound{}
	return s
}

func (a *api) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var out []sourceView
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		ss, err := registry.List(tx)
		if err != nil {
			return err
		}
		usage, err := runner.UsageBySource(tx)
		if err != nil {
			return err
		}
		out = make([]sourceView, 0, len(ss))
		rs := readscope.New(tx, p.Caller)
		for _, s := range ss {
			if !rs.All() {
				if rs.Tier(s.ID) == readscope.None {
					continue
				}
				s = forExternal(s)
			}
			v, err := a.view(s, usage[s.ID])
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// declaredName：按查询参数 type、command、source_type、config（JSON 对象）给出接入层声明的名字；没有声明时 name 为 null。
func (a *api) declaredName(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	q := r.URL.Query()
	s := domain.Source{Type: domain.SourceType(q.Get("type")), Command: q.Get("command"), SourceKind: q.Get("source_type")}
	if c := q.Get("config"); c != "" {
		if !json.Valid([]byte(c)) {
			return invalid("config must be JSON", "fix config")
		}
		s.Config = json.RawMessage(c)
	}
	var name any
	if a.dc.Name != nil {
		if zh, en, ok := a.dc.Name(s); ok {
			name = map[string]string{"zh": zh, "en": en}
		}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"name": name})
}

func (a *api) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var v sourceView
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		s, err := registry.Get(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if p.Caller.External() {
			if _, err := sourceTier(tx, p, s.ID); err != nil {
				return err
			}
			s = forExternal(s)
		}
		usage, err := runner.UsageBySource(tx)
		if err != nil {
			return err
		}
		v, err = a.view(s, usage[s.ID])
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, v)
}

// selfSender：只对声明了 identity 模式的来源；不落账、不缓存。只 Web：它会在主机上运行来源程序。
func (a *api) selfSender(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if !p.Web {
		return domain.NewError(domain.CodeWebOnly, "only the owner in the Web UI can look up their own identity", "open the source in the Web UI")
	}
	s, err := a.source(r, r.PathValue("id"))
	if err != nil {
		return err
	}
	if a.dc.Sender == nil || a.dc.SelfSender == nil || a.dc.Sender(s).Self != "identity" {
		return domain.NewError(domain.CodeNotFound, "source "+s.ID+" cannot tell who you are", "type your identity by hand")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second) // 运行本身 10 秒超时，另留排队时间
	defer cancel()
	who, err := a.dc.SelfSender(ctx, s)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"sender": who})
}

// trustedSenders 只替换这个来源的转发白名单（§14.6）。需确认的来源只接受空名单（清掉以前填的、本来就不生效的条目），
// 非空返回 sender_unverifiable：名单在那里不会生效，不让人以为填了有用。
func (a *api) trustedSenders(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	// 信任的发件人属 3a 改日常（§14.3）：Web 本人，或档位放开到 3a 的 Agent。
	basis, err := apipolicy.Authorize(a.st, r, p, authz.TierRules, "/sources/"+r.PathValue("id"), "settings:/settings/sources/{id}/trusted-senders")
	if err != nil {
		return err
	}
	s, err := a.source(r, r.PathValue("id"))
	if err != nil {
		return err
	}
	var in struct {
		Senders []string `json:"senders"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	if len(in.Senders) > 0 && (a.dc.Sender == nil || a.dc.Sender(s).Trust != "verified") {
		return invalid("sender_unverifiable: source "+s.ID+" cannot verify who sent a message, so a trusted-sender list would have no effect", "only clear the list for this source")
	}
	v, pol, err := apipolicy.UpdateKey(a.st, r, p, basis, "settings: trusted senders of source "+s.ID, func(np *kpolicy.Policy) {
		if np.ForwardAllowlist == nil {
			np.ForwardAllowlist = map[string][]string{}
		}
		np.ForwardAllowlist[s.ID] = in.Senders
	})
	if err != nil {
		return err
	}
	out := pol.ForwardAllowlist[s.ID]
	if out == nil {
		out = []string{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"version": v, "senders": out})
}

// create：type=script 是可执行命令的登记，走只 Web 的命令；其余走 delegate 可用的命令。
func (a *api) create(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := body(r)
	if err != nil {
		return err
	}
	var newID string
	_ = json.Unmarshal(in["id"], &newID)
	if err := a.rejectOutbound(r, p, newID, in); err != nil {
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
	if err := a.rejectOutbound(r, p, id, in); err != nil {
		return err
	}
	enabled, hasEnabled := in["enabled"]
	name, hasName := in["name"] // 显示名走 source.rename，不进登记内容的更新（类型、命令等）
	delete(in, "enabled")
	delete(in, "name")
	if len(in) == 0 && !hasEnabled && !hasName {
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
	if hasName {
		n := map[string]json.RawMessage{"name": name}
		n["id"], _ = json.Marshal(id)
		if res, err = a.run(r, p, registry.CmdRename, n, true); err != nil {
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

// rejectOutbound：这两条路由从不写 outbound（§14.14）。非 Web 调用者的尝试交给内核的设置命令判定，
// 以得到规定的退出码（delegate 20 并留拒绝事件，其余 4）；Web 本人则被指向 settings 路由。
func (a *api) rejectOutbound(r *http.Request, p *core.Principal, id string, in map[string]json.RawMessage) error {
	if _, ok := in["outbound"]; !ok {
		return nil
	}
	if !p.Web {
		raw, _ := json.Marshal(id)
		if _, err := a.run(r, p, registry.CmdSetOutbound, map[string]json.RawMessage{"id": raw}, false); err != nil {
			return err
		}
	}
	return domain.NewError(domain.CodeWebOnly, "outbound is not part of the source registration or update", "set it in the Web UI (PUT /v1/settings/sources/{id}/outbound)")
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
	if err := internalOnly(p, "raw ingests"); err != nil {
		return err
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

// scoredPage 按影子分数从高到低返回一页，没有分数的排最后。顺序是整体的，所以 after/next_after 在这里是已返回的条数（偏移），
// 不是落盘序号。shortcut: 每次翻页都重读并重排该来源的全部匹配事件；积压到数万条时再给分数建索引。
func scoredPage(tx *store.Tx, source string, ds []domain.Disposition, after int64, limit int) ([]inbound.Scored, int64, error) {
	var all []domain.SourceEvent
	for cur := int64(0); ; {
		page, next, err := runner.EventPage(tx, source, ds, cur, maxEventLimit)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, page...)
		if next == 0 {
			break
		}
		cur = next
	}
	sorted, err := inbound.SortByScore(tx, all)
	if err != nil {
		return nil, 0, err
	}
	if int64(len(sorted)) <= after {
		return []inbound.Scored{}, 0, nil
	}
	sorted = sorted[after:]
	if len(sorted) > limit {
		return sorted[:limit], after + int64(limit), nil
	}
	return sorted, 0, nil
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
	bySort := q.Get("sort")
	if bySort != "" && bySort != "score" && bySort != "recent" {
		return invalid("sort is not valid", "use sort=score or sort=recent, or leave it out for the order the events arrived in")
	}
	id := r.PathValue("id")
	var (
		evs  []domain.SourceEvent
		next int64
		scrd []inbound.Scored
		out  = map[string]any{}
	)
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		if !(p.Web && inbound.IsCaptureSource(id)) { // 对话内容只给本人；其他凭据和登记外的来源一样是 404
			if _, err := registry.Get(tx, id); err != nil {
				return err
			}
		}
		scope, err := sourceTier(tx, p, id) // 列表的预览同样按档位裁，不再对外部钥匙一律给 2000 字
		if err != nil {
			return err
		}
		switch bySort {
		case "score":
			if scrd, next, err = scoredPage(tx, id, ds, after, limit); err != nil {
				return err
			}
			out["events"], err = viewScored(tx, scrd, scope)
			return err
		case "recent": // 从新到旧，after/next_after 是偏移（同 score）
			evs, next, err = runner.EventPageRecent(tx, id, ds, after, limit)
		default:
			evs, next, err = runner.EventPage(tx, id, ds, after, limit)
		}
		if err != nil {
			return err
		}
		out["events"], err = viewEvents(tx, evs, scope) // 正文只给预览与清单，全文经 GET /source-events/{id}
		return err
	}); err != nil {
		return err
	}
	if next != 0 {
		out["next_after"] = next
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// outboundPreview 返回一条来源事件按 full、excerpt、meta 裁剪后实际送给模型的内容（§14.14），与外发同源，不写 Event。
// 读取口径与 GET /sources/{id}/events 相同：这是未过滤的外部内容，运行凭据不可用；对话捕获的内部来源只给 Web。
func (a *api) outboundPreview(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := internalOnly(p, "the outbound preview"); err != nil {
		return err
	}
	var ev domain.SourceEvent
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		var err error
		if ev, err = runner.Event(tx, r.PathValue("id")); err != nil {
			return err
		}
		if inbound.IsCaptureSource(ev.SourceID) {
			if !p.Web { // 对话内容只给本人；其他凭据看来就是不存在
				return domain.NewError(domain.CodeNotFound, "source event "+ev.ID+" not found", "list the source's events")
			}
			return nil
		}
		_, err = registry.Get(tx, ev.SourceID)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, inbound.OutboundPreview(ev))
}

func (a *api) propose(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	ev, err := a.pr.ProposeUnjudged(r.Context(), p.Identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	return a.writeEvent(w, r, p, ev)
}

// split 把并入某条建议的来源事件拆出来另成一条建议：请求体 {"from": 建议 id}，返回 {"item": 新建议 id}。
func (a *api) split(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		From string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.From == "" {
		return domain.NewError(domain.CodeInvalidRequest, "body must be {\"from\": \"<proposal id>\"}", "name the proposal the source was merged into")
	}
	item, err := a.pr.Split(r.Context(), p.Identity, r.PathValue("id"), in.From)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"item": item})
}

// unmerge 撤销跨来源合并的一条折叠，返回 {"item": 回到建议页的那条}。
func (a *api) unmerge(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	if err := a.pr.Unmerge(r.Context(), p.Identity, id); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"item": id})
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
	return a.writeEvent(w, r, p, ev)
}

// writeEvent 以列表口径（预览加清单）返回一条来源事件：写命令的响应也不带正文全文，全文只经单条读取；同样按调用者的档位裁。
func (a *api) writeEvent(w http.ResponseWriter, r *http.Request, p *core.Principal, ev domain.SourceEvent) error {
	var v eventView
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		scope := scopeFull
		if !inbound.IsCaptureSource(ev.SourceID) {
			if scope = readscope.New(tx, p.Caller).Tier(ev.SourceID); scope == readscope.None {
				scope = scopeMeta // 写命令已经成功，响应只带元信息
			}
		}
		var err error
		v, err = viewEvent(tx, ev, scope)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, v)
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

// backlogRules 积压只跑规则：请求体 {"source"?}；返回 {"attached","disposed","remaining"}。授权（本人原话或 Web）由命令管线判定。
func (a *api) backlogRules(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Source string `json:"source,omitempty"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	res, err := a.pr.BacklogRules(r.Context(), command.Request{Identity: p.Identity, WebURL: p.WebURL}, in.Source)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}

// backlogScore 积压影子打分：请求体 {"source"?}；返回 {"scored","skipped"}。授权同 backlogRules；没有判断端点是 409。
func (a *api) backlogScore(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Source string `json:"source,omitempty"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	res, err := a.pr.BacklogScore(r.Context(), command.Request{Identity: p.Identity, WebURL: p.WebURL}, in.Source)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}

// backlogRejudge 积压放开重判：请求体 {"source"?, "limit"?}；返回 {"requeued","deferred"}。授权同 backlogRules。
func (a *api) backlogRejudge(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Source string `json:"source,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Limit < 0 {
		return invalid("limit must not be negative", "leave limit out, or give a positive number")
	}
	res, err := a.pr.BacklogRejudge(r.Context(), command.Request{Identity: p.Identity, WebURL: p.WebURL}, in.Source, in.Limit)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}

// restorable 预览可放回积压的旧判断忽略事件：from、to 必填（RFC 3339），source 可选；只读。命中的是未过滤的外部事件，运行凭据不能预览。
func (a *api) restorable(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credential cannot preview restorable source events", "ask the owner to preview from the source page or the CLI")
	}
	q := r.URL.Query()
	var t [2]time.Time
	for i, k := range []string{"from", "to"} {
		if q.Get(k) == "" {
			return invalid(k+" is required", "give from and to as RFC 3339 times")
		}
		var err error
		if t[i], err = time.Parse(time.RFC3339, q.Get(k)); err != nil {
			return invalid(k+" is not an RFC 3339 time", "for example 2026-01-02T15:04:05Z")
		}
	}
	var out inbound.RestorePreview
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if err = inbound.RestoreWindow(tx, t[0], t[1], q.Get("source")); err != nil {
			return err
		}
		out, err = inbound.PreviewRestore(tx, t[0], t[1], q.Get("source"))
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// restore 把旧级联忽略的事件放回积压：请求体 {"from","to","source"?}；返回 {"restored": n}。授权（本人原话或 Web，不接受授权单）由命令管线判定；可带 Idempotency-Key。
func (a *api) restore(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
		Source string    `json:"source,omitempty"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	m := map[string]json.RawMessage{}
	m["from"], _ = json.Marshal(in.From)
	m["to"], _ = json.Marshal(in.To)
	if in.Source != "" {
		m["source"], _ = json.Marshal(in.Source)
	}
	res, err := a.run(r, p, inbound.CmdBacklogRestore, m, true)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res.Value)
}

// rejects 返回来源没有入账的输出行及原因。原始行在落账时已截断到 512 字节；与入站记录的请求头同一规则，含接收密钥明文前缀的整行遮蔽。
// 原始行是未过滤的外部输出，运行凭据不能读（与 events、ingests 一致）。
func (a *api) rejects(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := internalOnly(p, "source rejects"); err != nil {
		return err
	}
	after, limit, err := pageParams(r.URL.Query())
	if err != nil {
		return err
	}
	recent := r.URL.Query().Get("sort") == "recent" // 从新到旧，after/next_after 是偏移（同 events）
	if sort := r.URL.Query().Get("sort"); sort != "" && !recent {
		return invalid("sort is not valid", "use sort=recent for newest first, or leave it out for the order the lines were rejected in")
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
		if recent {
			rs, next, err = runner.RejectPageRecent(tx, id, after, limit)
		} else {
			rs, next, err = runner.RejectPage(tx, id, after, limit)
		}
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
