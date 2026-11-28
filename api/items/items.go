// Package items 是事项分组的 HTTP 路由（附录 B.2，设计 §7.1、§14.3）：
// GET/POST /items、GET/PATCH /items/{id}、POST /items/{id}/transition|relations|comments|revert|start、GET /today。
// 讨论流 GET /items/{id}/thread 在 api/why（与处理经过同一份合成）。
//
// 路由里没有业务规则：写入全部映射到内核命令（item.create/update/transition/relate/revert/start），
// 授权（含"只有 Web 登录才能撤销"）、校验、内核字段防写、乐观版本与幂等都由命令管线判定。
// 本层只做 HTTP 到命令请求的搬运：
//   - 路径里的 id 注入请求体（体里不许再写 id/from，避免两处不一致）；
//   - Idempotency-Key 头 → 幂等键；If-Match 头 → 期望版本（版本是内核字段，不能写在请求体里）；
//   - 体里的 evidence 只是说明或结果证据，不参与权限判定（本人原话已取消，§14.3）。
package items

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// IfMatchHeader 携带期望的对象版本（整数，可带引号）；不符返回 conflict。
	IfMatchHeader = "If-Match"
)

// Routes 返回事项路由分组，交给 core.Deps.Groups。x 须已经 kitems.Register。
// staleAfter 给"今天"久未推进一组的阈值（取自 stale 规则，热加载后即时生效）；省略时用默认的 14 天。
func Routes(x *command.Executor, st *store.Store, staleAfter ...func() time.Duration) core.RouteGroup {
	h := &handlers{x: x, st: st, staleAfter: func() time.Duration { return kitems.StaleAfter }}
	if len(staleAfter) > 0 && staleAfter[0] != nil {
		h.staleAfter = staleAfter[0]
	}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/items", Handler: h.list})
		r.Handle(core.Route{Method: "GET", Path: "/items/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "POST", Path: "/items", Handler: h.write("item.create", "", http.StatusCreated)})
		r.Handle(core.Route{Method: "PATCH", Path: "/items/{id}", Handler: h.write("item.update", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/transition", Handler: h.write("item.transition", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/relations", Handler: h.write("item.relate", "from", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/correlation-keys", Handler: h.write("item.relate", "from", http.StatusOK)})
		r.Handle(core.Route{Method: "DELETE", Path: "/items/{id}/correlation-keys", Handler: h.write("item.relate", "from", http.StatusOK)})
		// 带 mentions 的发言是 delegate.mention（写发言并叫起被 @ 的执行者或主会话，§6.3），其余是 item.progress。
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/comments", Handler: h.writeWith(commentCommand, "id", http.StatusCreated, h.withComment)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/attach", Handler: h.write(inbound.CmdAttach, "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/proposals/batch", Handler: h.batch})
		r.Handle(core.Route{Method: "GET", Path: "/today", Handler: h.today})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/revert", Handler: h.write("item.revert", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/start", Handler: h.write("item.start", "id", http.StatusAccepted)})
	}
}

type handlers struct {
	x          *command.Executor
	st         *store.Store
	staleAfter func() time.Duration
}

// readable：读事项对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本，只有导出 ext 例外（§10.3）。
func readable(p *core.Principal) error {
	if auth.IsExportExt(p.Caller.Kind, p.Caller.Actor.Principal) {
		return nil
	}
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read items", "use an owner or agent credential")
	}
	return nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	q := r.URL.Query()
	f := kitems.Filter{Phase: domain.Phase(q.Get("phase")), Kind: domain.Kind(q.Get("kind")),
		Project: q.Get("project"), Assignee: q.Get("assignee"), OriginSource: q.Get("origin_source"), OriginRef: q.Get("origin_ref")}
	sq := search.ItemQuery{Tag: q.Get("tag"), PlannedFor: q.Get("planned_for"), Sort: q.Get("sort"), From: q.Get("from"), To: q.Get("to")}
	page, err := pageOf(q)
	if err != nil {
		return err
	}
	// q：全文走统一索引（§7.1），命中的按相关度排在前面；读取闸在裁后的原文上复核（§14.14）。
	var ranked []string
	if text := q.Get("q"); text != "" {
		if ranked, err = search.Rank(r.Context(), h.st, text, func(tx *store.Tx) search.Gate { return readscope.New(tx, p.Caller).Gate() }); err != nil {
			return err
		}
	}
	// progress_from、progress_to：只列这段日子里有进展的事项，并附上这些进展（「已完成」页按周数进展，附录 B.2）。
	pf, pt := q.Get("progress_from"), q.Get("progress_to")
	if pf != "" || pt != "" {
		if err := search.CheckRange("progress_from", "progress_to", pf, pt); err != nil {
			return err
		}
	}
	var (
		its     []domain.Item
		rejs    = map[string]Rejection{}
		done    map[string]time.Time
		undone  map[string]bool
		within  map[string][]domain.Progress
		origin  map[string]inbound.Origin
		dis     map[string]inbound.Dismissal
		inboxID string
		scopes  map[string]string
	)
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if its, err = kitems.List(tx, f); err != nil {
			return err
		}
		if f.Phase == domain.PhaseDone {
			if done, undone, err = kitems.DoneAt(tx); err != nil {
				return err
			}
		}
		if pf != "" {
			if within, err = kitems.ProgressIn(tx, pf, pt); err != nil {
				return err
			}
			its = slices.DeleteFunc(its, func(it domain.Item) bool { return within[it.ID] == nil })
		}
		if q.Get("q") != "" {
			its = byRank(its, ranked)
		}
		// 外部钥匙：不给的事项去掉，其余按档位裁正文（§14.14）。
		if its, scopes, err = readscope.New(tx, p.Caller).Items(its); err != nil {
			return err
		}
		for _, it := range its {
			if rj, err := LastRejection(tx, it); err != nil {
				return err
			} else if rj != nil {
				rejs[it.ID] = *rj
			}
		}
		switch f.Phase {
		case domain.PhaseProposed:
			origin, err = inbound.Origins(tx, its)
			if err == nil {
				inboxID, _, err = projects.InboxID(tx)
			}
		case domain.PhaseDropped:
			dis, err = inbound.Dismissals(tx, its)
		}
		return err
	}); err != nil {
		return err
	}
	if its, err = search.FilterItems(its, sq); err != nil {
		return err
	}
	if its == nil {
		its = []domain.Item{}
	}
	out := map[string]any{}
	if page.limit > 0 { // 带 limit 时只回这一页，给总数与续页游标（附录 B.2）；不带时与以前一样回全部
		out["total"] = len(its)
		end := min(len(its), page.offset+page.limit)
		if end < len(its) {
			out["next_cursor"] = strconv.Itoa(end)
		}
		its = its[min(page.offset, end):end]
		in := map[string]bool{}
		for _, it := range its {
			in[it.ID] = true
		}
		maps.DeleteFunc(rejs, func(id string, _ Rejection) bool { return !in[id] })
		maps.DeleteFunc(scopes, func(id, _ string) bool { return !in[id] })
	}
	out["items"], out["last_rejections"] = its, rejs
	if len(scopes) > 0 {
		out["content_scopes"] = scopes
	}
	if done != nil { // 只读已完成时给出各自的完成时刻，「已完成」页按它分周（附录 B.2）
		at := map[string]time.Time{}
		for _, it := range its {
			if t, ok := done[it.ID]; ok {
				at[it.ID] = t
			}
		}
		out["done_at"] = at
		unknown := []string{} // 迁移来、没记完成日期的：不给时刻，页面如实标"完成日期不详"
		for _, it := range its {
			if undone[it.ID] {
				unknown = append(unknown, it.ID)
			}
		}
		out["done_unknown"] = unknown
	}
	if origin != nil { // 只读建议时给出各条的来源事件类型、上游对象与对象已结束的依据，以及现算的分组（附录 B.2、主文档 §7.2）
		at := map[string]inbound.Origin{}
		for _, it := range its {
			if o, ok := origin[it.ID]; ok {
				at[it.ID] = o
			}
		}
		out["origins"] = at
		out["groups"] = inbound.Groups(its, at, inboxID)
	}
	if dis != nil { // 只读已放弃时给出忽略记录（批次、理由、时刻），建议页的「已忽略」据此按批列出与撤销（附录 B.2）
		at := map[string]inbound.Dismissal{}
		for _, it := range its {
			if d, ok := dis[it.ID]; ok {
				at[it.ID] = d
			}
		}
		out["dismissals"] = at
	}
	if within != nil {
		at := map[string][]domain.Progress{}
		for _, it := range its {
			at[it.ID] = within[it.ID]
			if t, cut := scopes[it.ID]; cut { // 外部钥匙：进展正文与事项正文同档裁（§14.14）
				at[it.ID] = readscope.TrimProgress(within[it.ID], t)
			}
		}
		out["progress"] = at
	}
	if sq.From != "" { // 按范围读时给出本机日界，日历页不用浏览器时区猜（§7.1）
		now := clock.Now().In(time.Local)
		out["today"], out["utc_offset"] = kitems.Today(now), now.Format("-07:00")
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

type page struct{ limit, offset int }

// pageOf 读 limit 与 cursor：都不带时不分页（Web 的列表页照旧读全部）。cursor 是上一页给的 next_cursor。
// HTTP 不设条数上限：升级前 limit 被忽略，带大数读全部的调用方照旧拿到全部；有界输出由命令行守（至多 200，§17.2）。
func pageOf(q url.Values) (page, error) {
	var p page
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return p, domain.NewError(domain.CodeInvalidRequest, "limit must be a positive integer", "fix limit and retry")
		}
		p.limit = n
	}
	if s := q.Get("cursor"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || p.limit == 0 {
			return p, domain.NewError(domain.CodeInvalidRequest, "cursor must come from next_cursor and go with limit", "drop cursor to start from the first page")
		}
		p.offset = n
	}
	return p, nil
}

// byRank 只留 ranked 里的事项，按 ranked 的顺序（相关的在前）。
func byRank(its []domain.Item, ranked []string) []domain.Item {
	at := make(map[string]int, len(ranked))
	for i, id := range ranked {
		at[id] = i
	}
	out := slices.DeleteFunc(its, func(it domain.Item) bool { _, ok := at[it.ID]; return !ok })
	slices.SortStableFunc(out, func(a, b domain.Item) int { return at[a.ID] - at[b.ID] })
	return out
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var (
		it    domain.Item
		rels  []domain.Relation
		rej   *Rejection
		prog  []domain.Progress
		keys  []kitems.Correlation
		scope string
		cut   *readscope.Trimmed
	)
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if it, err = kitems.Get(tx, r.PathValue("id")); err != nil {
			return err
		}
		body := it.Body
		if it, scope, err = readscope.New(tx, p.Caller).Item(it); err != nil {
			return err
		}
		cut = readscope.TrimmedOf(body, it.Body)
		if keys, err = kitems.Correlations(tx, it.ID); err != nil {
			return err
		}
		if rels, err = kitems.Relations(tx, it.ID); err != nil {
			return err
		}
		if prog, err = kitems.Progress(tx, it.ID); err != nil {
			return err
		}
		rej, err = LastRejection(tx, it)
		return err
	}); err != nil {
		return err
	}
	prog = readscope.TrimProgress(prog, scope)
	if prog == nil {
		prog = []domain.Progress{}
	}
	if rels == nil {
		rels = []domain.Relation{}
	}
	if keys == nil {
		keys = []kitems.Correlation{}
	}
	out := map[string]any{"item": it, "relations": rels, "progress": prog, "correlation_keys": keys, "last_rejection": rej}
	if scope != readscope.Full { // 只有外部钥匙会被裁：写明按哪一档给的、正文裁掉了多少
		out["content_scope"] = scope
		if cut != nil {
			out["trimmed"] = cut
		}
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// today：四组取材全在内核（kitems.TodayItems）。
func (h *handlers) today(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var out struct {
		kitems.TodayList
		ContentScopes map[string]string `json:"content_scopes,omitempty"`
	}
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		l, err := kitems.TodayItemsAfter(tx, clock.Now(), h.staleAfter())
		if err != nil {
			return err
		}
		rs := readscope.New(tx, p.Caller)
		out.ContentScopes = map[string]string{}
		for _, g := range []*[]domain.Item{&l.Planned, &l.Due, &l.Pending, &l.Stale, &l.Overdue, &l.HighPriority} {
			kept, scopes, err := rs.Items(*g)
			if err != nil {
				return err
			}
			*g = kept
			for id, t := range scopes {
				out.ContentScopes[id] = t
			}
		}
		out.TodayList = l
		return nil
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// write 把请求体原样交给命令。pathKey 非空时把路径 id 写入体的该键（体里自带同名键直接拒绝）。
func (h *handlers) write(cmd, pathKey string, status int, extra ...func(r *http.Request, id string, out map[string]any) error) core.Handler {
	return h.writeWith(func(map[string]json.RawMessage) string { return cmd }, pathKey, status, extra...)
}

// commentCommand 按请求体选发言的命令：mentions 非空走 delegate.mention；空数组等于没给。
func commentCommand(body map[string]json.RawMessage) string {
	if raw, ok := body["mentions"]; ok {
		var ms []string
		if json.Unmarshal(raw, &ms) != nil || len(ms) > 0 {
			return "delegate.mention" // 形状不对的交给命令报错
		}
		delete(body, "mentions")
	}
	return "item.progress"
}

// writeWith 同 write，命令由请求体决定。
func (h *handlers) writeWith(cmdOf func(map[string]json.RawMessage) string, pathKey string, status int, extra ...func(r *http.Request, id string, out map[string]any) error) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if body == nil { // JSON null
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
		}
		if pathKey != "" {
			for k := range body {
				if strings.EqualFold(k, "id") || strings.EqualFold(k, pathKey) {
					return domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
				}
			}
			id, _ := json.Marshal(r.PathValue("id"))
			body[pathKey] = id
		}
		if r.Method == http.MethodDelete { // DELETE …/correlation-keys：移除
			body["remove"] = json.RawMessage("true")
		}
		cmd := cmdOf(body)
		in, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req := command.Request{Identity: p.Identity, Command: cmd, Input: in,
			IdemKey: r.Header.Get(IdempotencyHeader), WebURL: p.WebURL}
		if v := r.Header.Get(IfMatchHeader); v != "" {
			n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
			if err != nil || n < 0 {
				return domain.NewError(domain.CodeInvalidRequest, IfMatchHeader+" must be an object version number", "send the version from the last read")
			}
			req.ExpectVersion = &n
		}
		res, err := h.x.Do(context.WithoutCancel(r.Context()), req)
		if err != nil {
			return err
		}
		var out struct {
			ID    string   `json:"id"`
			Items []string `json:"items"` // 带 @ 的发言建出的委派子事项
		}
		if err := json.Unmarshal(res.Value, &out); err != nil {
			return err
		}
		if res.Replayed {
			status = http.StatusOK
		}
		resp := map[string]any{"id": out.ID, "version": res.Version, "replayed": res.Replayed}
		if len(out.Items) > 0 {
			resp["items"] = out.Items
		}
		if !res.Replayed { // 重放时"最后一条"可能已是别的进展，不附
			for _, f := range extra {
				if err := f(r, out.ID, resp); err != nil {
					return err
				}
			}
		}
		return core.WriteJSON(w, status, resp)
	}
}

// withComment 在发言的响应里附上刚追加的那条（附录 B.2）。
func (h *handlers) withComment(r *http.Request, id string, out map[string]any) error {
	return h.st.View(r.Context(), func(tx *store.Tx) error {
		ps, err := kitems.Progress(tx, id)
		if err == nil && len(ps) > 0 {
			out["comment"] = ps[len(ps)-1]
		}
		return err
	})
}

// Rejection 是事项上最近一次因档位未放开被拒的 Agent 操作（退出码 23，§14.3）：事项详情、事项列表、决策待办与建议流据此标明
// "谁的什么操作被拒、为什么、事项未被修改"。只带机制（操作者、授权动作、原因码、原因），不含账本用户内容。
// Grant 只出现在旧模型的拒绝记录上。
// 被拒的写入从不生效，所以这里没有"是否已改"的字段：凡出现即未生效。
type Rejection struct {
	Seq    int64        `json:"seq"`
	At     time.Time    `json:"at"`
	Actor  domain.Actor `json:"actor"`
	Action string       `json:"action,omitempty"` // 被拒的授权动作（如 edit_any_item、answer_decision）
	Code   domain.Code  `json:"code"`
	Reason string       `json:"reason,omitempty"` // 更细的稳定原因（domain.Reason*）
	Why    string       `json:"why"`
	Grant  string       `json:"grant,omitempty"`
}

// LastRejection 返回未终结事项上仍未被处理的最近一次 23 拒绝，读的是内核已记的 command.rejected，不重新判定。
// needs_input、proposed：其后有回答或阶段变化即视为已处理。open、waiting：拒绝不再转阶段（§14.5），其后事项有任何变更
// （改字段、写进展、转阶段、提问或回答）即被取代。
func LastRejection(tx *store.Tx, it domain.Item) (*Rejection, error) {
	if it.Phase.Terminal() {
		return nil, nil
	}
	live := it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting
	es, err := eventlog.ReadObject(tx, it.ID)
	if err != nil {
		return nil, err
	}
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i].Event
		switch e.Type {
		case domain.EvDecisionResolved, domain.EvItemPhaseChanged:
			return nil, nil
		case domain.EvItemUpdated, domain.EvItemProgressAdded, domain.EvDecisionRequested:
			if live {
				return nil, nil
			}
		case domain.EvCommandRejected:
			var pl struct {
				Action string      `json:"action"`
				Code   domain.Code `json:"code"`
				Reason string      `json:"reason"`
				Exit   int         `json:"exit"`
				Why    string      `json:"why"`
				Grant  string      `json:"grant"`
			}
			if json.Unmarshal(e.Payload, &pl) != nil || pl.Exit != domain.ExitBasisRequired {
				continue
			}
			return &Rejection{Seq: e.Seq, At: e.At, Actor: e.Actor, Action: pl.Action, Code: pl.Code, Reason: pl.Reason, Why: pl.Why, Grant: pl.Grant}, nil
		}
	}
	return nil, nil
}

// batch 处理 POST /proposals/batch（§7.2、附录 B.2）：请求体 {"mode":"recommended","filter":{…}} 或
// {"mode":"explicit","actions":[…]}；每条建议各自成败、各记一条 Event，依据与身份校验对整批一视同仁。
func (h *handlers) batch(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var b inbound.BatchRequest
	var raw json.RawMessage
	if err := core.DecodeJSON(r, &raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "request body is not a valid batch", "send {\"mode\":\"recommended\"|\"explicit\", …}")
	}
	req := command.Request{Identity: p.Identity, IdemKey: r.Header.Get(IdempotencyHeader), WebURL: p.WebURL}
	res, err := inbound.Batch(context.WithoutCancel(r.Context()), h.x, h.st, req, b)
	if err != nil {
		return err
	}
	if res == nil {
		res = []inbound.BatchResult{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"results": res})
}
