// Package packs 是能力包、实例与规则分组的 HTTP 路由（附录 B.2，设计 §6.12、§14.8）：
//
//	GET /packs
//	GET/POST /instances        PATCH /instances/{id}        POST /instances/{id}/run
//	GET /rules                 PATCH /rules/{id}/mode
//
// 路由只做传输：解码、把请求交给内核命令（授权、校验、事件都在 kernel/instances、kernel/rules 与命令管线）、渲染结果。
// 读对 observer 及以上开放；实例写是 delegate（命令按 manage_source 授权）。
// 规则模式切换是策略类设置：命令只放行 Web 会话，token 一律 web_only（退出码 20）并留拒绝事件——
// 这里不存在把规则切 auto 的 token 路径。规则 id 形如 "<包>/<组>/<id>"，放进路径时斜杠须写成 %2F。
package packs

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IdempotencyHeader 是写请求可选的幂等键头。
const IdempotencyHeader = "Idempotency-Key"

// Packs 是 /packs 读取已加载能力包的来源（packs.Loader 满足）。
type Packs interface {
	List() []*kp.Pack
	Source(name string) (kp.Source, bool)
}

type api struct {
	st *store.Store
	x  *command.Executor
	ps Packs
}

// Routes 返回路由分组；x 须已登记 instances 与 rules 命令。经 core.Deps.Groups 挂载。
func Routes(st *store.Store, x *command.Executor, ps Packs) core.RouteGroup {
	a := &api{st, x, ps}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/packs", Handler: readable(a.packs)})
		r.Handle(core.Route{Method: "GET", Path: "/packs/{pack}/prompts/{name}", Handler: readable(a.prompt)})
		r.Handle(core.Route{Method: "GET", Path: "/instances", Handler: readable(a.listInstances)})
		r.Handle(core.Route{Method: "POST", Path: "/instances", Handler: a.createInstance})
		r.Handle(core.Route{Method: "PATCH", Path: "/instances/{id}", Handler: a.patchInstance})
		r.Handle(core.Route{Method: "POST", Path: "/instances/{id}/run", Handler: a.runInstance})
		r.Handle(core.Route{Method: "GET", Path: "/rules", Handler: readable(a.listRules)})
		r.Handle(core.Route{Method: "PATCH", Path: "/rules/{id}/mode", Handler: a.setMode})
	}
}

// readable：读对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本（与 api/projects 一致）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
			return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read packs, instances or rules", "use an owner or agent credential")
		}
		return h(w, r, p)
	}
}

// prompt 返回已加载能力包里一个提示词的正文与判据（头部声明里只给判据），供设置页显示内置默认与"恢复默认"（§9.3）。
func (a *api) prompt(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	for _, pk := range a.ps.List() {
		if pk.Name != r.PathValue("pack") {
			continue
		}
		if pr, ok := pk.Prompts[r.PathValue("name")]; ok {
			return core.WriteJSON(w, http.StatusOK, map[string]string{"pack": pk.Name, "name": r.PathValue("name"), "agent": pr.Meta.Agent, "body": pr.Body, "criteria": pr.Meta.Criteria})
		}
	}
	return domain.NewError(domain.CodeNotFound, "no such prompt in the loaded packs", "list packs with GET /packs")
}

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

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

func (a *api) run(r *http.Request, p *core.Principal, cmd string, in map[string]json.RawMessage) (command.Result, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return command.Result{}, err
	}
	return a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw, IdemKey: r.Header.Get(IdempotencyHeader), WebURL: p.WebURL})
}

// ---- 能力包 ----

type packRule struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// packView 只含包的声明性元数据；提示词正文、模板与脚本类内容不外露。
type packView struct {
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	Description    string             `json:"description"`
	Source         kp.Source          `json:"source"`
	Hash           string             `json:"hash"`
	Kinds          []string           `json:"kinds"`
	InstanceParams map[string]any     `json:"instance_params,omitempty"`
	ClockRules     []packRule         `json:"clock_rules"`
	EventRules     []packRule         `json:"event_rules"`
	Views          map[string]kp.View `json:"views"`
	// SourceRules 按声明顺序列出来源规则；包列表本身按匹配顺序（包名）排列，首条命中即止（§9.3）。
	SourceRules []sourceRule `json:"source_rules"`
	// DefaultInstruction 是实例驱动的 call_agent 模板的默认内容要求（C.7）；Web 在本人没改时显示它并标"默认"。
	DefaultInstruction string `json:"default_instruction,omitempty"`
}

type sourceRule struct {
	ID       string         `json:"id"`
	Match    map[string]any `json:"match"`
	Where    []condition    `json:"where,omitempty"`
	Then     string         `json:"then"`
	Accept   string         `json:"accept,omitempty"`
	OnUpdate string         `json:"on_update,omitempty"`
}

type condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// defaultInstruction 取包里实例驱动的时钟规则所调模板的 default_instruction。
func defaultInstruction(p *kp.Pack) string {
	for _, c := range p.ClockRules {
		if !strings.Contains(c.Schedule, "{instance.") {
			continue
		}
		for _, a := range c.Do {
			if pr, _ := a["prompt"].(string); a["action"] == "call_agent" && p.Prompts[pr].Meta.DefaultInstruction != "" {
				return p.Prompts[pr].Meta.DefaultInstruction
			}
		}
	}
	return ""
}

func (a *api) packs(w http.ResponseWriter, _ *http.Request, _ *core.Principal) error {
	out := []packView{}
	for _, p := range a.ps.List() {
		src, _ := a.ps.Source(p.Name)
		v := packView{Name: p.Name, Version: p.Version, Description: p.Description, Source: src, Hash: p.Hash,
			Kinds: []string{}, InstanceParams: p.InstanceParams, ClockRules: []packRule{}, EventRules: []packRule{}, Views: p.Views,
			SourceRules: []sourceRule{}, DefaultInstruction: defaultInstruction(p)}
		for _, s := range p.SourceRules {
			x := sourceRule{ID: s.ID, Match: s.Match, Then: rules.Exit(s), Accept: s.Accept, OnUpdate: s.OnUpdate}
			for _, c := range s.Where {
				x.Where = append(x.Where, condition(c))
			}
			v.SourceRules = append(v.SourceRules, x)
		}
		for _, k := range p.Kinds {
			v.Kinds = append(v.Kinds, k.Name)
		}
		for _, c := range p.ClockRules {
			v.ClockRules = append(v.ClockRules, packRule{ID: c.ID, Type: c.Type, Schedule: c.Schedule, Mode: c.Mode})
		}
		for _, e := range p.EventRules {
			v.EventRules = append(v.EventRules, packRule{ID: e.ID, Mode: e.Mode})
		}
		out = append(out, v)
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"packs": out})
}

// ---- 实例 ----

type instanceView struct {
	instances.Instance
	Ready  *bool  `json:"agent_ready,omitempty"`
	Reason string `json:"agent_reason,omitempty"`
}

func (a *api) preview(tx *store.Tx, in instances.Instance) (instanceView, error) {
	v := instanceView{Instance: in}
	var params struct {
		Agent string `json:"agent_id"`
		Mode  string
	}
	if err := json.Unmarshal(in.Params, &params); err != nil {
		return v, err
	}
	if params.Mode == "create_item" {
		return v, nil
	}
	for _, pack := range a.ps.List() {
		if pack.Name != in.Pack {
			continue
		}
		for _, rule := range pack.ClockRules {
			if !strings.Contains(rule.Schedule, "{instance.") {
				continue
			}
			for _, action := range rule.Do {
				prompt, _ := action["prompt"].(string)
				if action["action"] != "call_agent" || pack.Prompts[prompt].Meta.Agent != "task" {
					continue
				}
				harness, _ := action["harness"].(string)
				_, pol, err := policy.Current(tx)
				if err != nil {
					return v, err
				}
				selected, err := executors.Resolve(tx, pol, executors.Selection{Schedule: params.Agent, Rule: pack.Name + "/clock_rules/" + rule.ID, Harness: harness})
				ready := selected.Reason == ""
				v.Ready = &ready
				v.Reason = selected.Reason
				return v, err
			}
		}
	}
	return v, nil
}
func (a *api) instance(r *http.Request, id string) (out instanceView, err error) {
	err = a.st.View(r.Context(), func(tx *store.Tx) error {
		in, err := instances.Get(tx, id)
		if err != nil {
			return err
		}
		out, err = a.preview(tx, in)
		return err
	})
	return
}
func (a *api) listInstances(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	out := []instanceView{}
	var ready bool
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		list, err := instances.List(tx)
		if err != nil {
			return err
		}
		for _, in := range list {
			v, err := a.preview(tx, in)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		ready, err = task.AgentReady(tx)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"instances": out, "agent_ready": ready})
}

// runInstance 立即跑一次（instance.run）：到期时刻取当前时刻，由规则执行照常处理。
func (a *api) runInstance(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	raw, _ := json.Marshal(id)
	if _, err := a.run(r, p, "instance.run", map[string]json.RawMessage{"id": raw}); err != nil {
		return err
	}
	return a.reply(w, r, http.StatusAccepted, id)
}

// reply 把命令结果指向的实例读回并渲染。
func (a *api) reply(w http.ResponseWriter, r *http.Request, status int, id string) error {
	in, err := a.instance(r, id)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, status, in)
}

func resultID(res command.Result) (string, error) {
	var ref struct {
		ID string `json:"id"`
	}
	return ref.ID, json.Unmarshal(res.Value, &ref)
}

func (a *api) createInstance(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := body(r)
	if err != nil {
		return err
	}
	res, err := a.run(r, p, "instance.create", in)
	if err != nil {
		return err
	}
	id, err := resultID(res)
	if err != nil {
		return err
	}
	return a.reply(w, r, http.StatusCreated, id)
}

// patchInstance：params、project 走 instance.update，enabled 走 instance.toggle。
// 两类同时出现时先 update 再 toggle；enabled 的形状已预先校验，toggle 的授权与 update 相同，所以第二步不会因请求本身而失败。
func (a *api) patchInstance(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	in, err := body(r)
	if err != nil {
		return err
	}
	toggle := map[string]json.RawMessage{}
	if en, ok := in["enabled"]; ok {
		// 先于 update 校验：否则 update 已提交后 toggle 才拒绝，调用方看到错误而改动已生效。
		var b *bool
		if json.Unmarshal(en, &b) != nil || b == nil {
			return invalid("enabled must be true or false", "send a JSON boolean")
		}
		toggle["enabled"] = en
		delete(in, "enabled")
	}
	if len(in) == 0 && len(toggle) == 0 {
		return invalid("nothing to update", "send params, project or enabled")
	}
	if len(in) > 0 {
		in["id"], _ = json.Marshal(id)
		if _, err := a.run(r, p, "instance.update", in); err != nil {
			return err
		}
	}
	if len(toggle) > 0 {
		toggle["id"], _ = json.Marshal(id)
		if _, err := a.run(r, p, "instance.toggle", toggle); err != nil {
			return err
		}
	}
	return a.reply(w, r, http.StatusOK, id)
}

// ---- 规则 ----

func (a *api) listRules(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var l []rules.State
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { l, err = rules.List(tx); return }); err != nil {
		return err
	}
	l = rules.Visible(a.ps, l)
	cls, run := rules.Classes(a.ps), rules.Runners(a.ps)
	for i := range l {
		l[i].Class, l[i].Runner = cls[l[i].Key], run[l[i].Key]
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"rules": l})
}

// setMode 把请求交给 rule.set_mode：只有 Web 会话的本人能通过，其余凭据由命令管线拒绝为 web_only。
func (a *api) setMode(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Mode string `json:"mode"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]string{"rule": r.PathValue("id"), "mode": in.Mode})
	res, err := a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: "rule.set_mode", Input: raw, IdemKey: r.Header.Get(IdempotencyHeader)})
	if err != nil {
		return err
	}
	var st rules.State
	if err := json.Unmarshal(res.Value, &st); err != nil {
		return err
	}
	st.Class, st.Runner = rules.Classes(a.ps)[st.Key], rules.Runners(a.ps)[st.Key]
	return core.WriteJSON(w, http.StatusOK, st)
}
