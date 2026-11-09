// Package guideline 是缺规范检测（设计 §12.6、§7.3）：check 类时钟规则到期时，找出有 repos 而无 Guideline 的活跃项目，
// 触发规范起草。
//
//   - 本机有检出时，经任务型专属 agent 在本机起草；否则建一个父事项并委派子事项给执行者起草。
//   - 起草结果由起草方经 material.commit 落为 proposed，生效须本人在 Web 确认；本包不写材料。
//   - 无仓库项目不检测。项目已有 Guideline（任何状态的材料，含托管型）不起草；上一次起草的事项还未结束时不重复起草，
//     本人丢弃了起草事项则不再起草，起草事项做完而仍无规范则下一轮重新起草。
//   - 写入一律经命令模型（规则给的入口）与任务型入口；只写自己的起草记录表，只读项目、材料与事项，
//     不起子进程、不调网络，不直写其他模块的数据。
//   - 起草失败（含执行者不被项目允许）返回真实错误，由规则引擎记 health.alert；不伪造成功。
package guideline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 登记起草记录：每个项目最近一次起草事项，用来避免重复起草。
var Module = store.Module{Name: "guideline", Migrations: []store.Migration{{
	Version: 1, Name: "create-guideline-draft",
	SQL: `CREATE TABLE guideline_draft (
	project TEXT PRIMARY KEY,
	item TEXT NOT NULL
) WITHOUT ROWID;`,
}}}

// Drafter 是任务型专属 agent 的入口（*task.Service 满足它）。
type Drafter interface {
	Launch(ctx context.Context, t task.Task) (task.Outcome, error)
}

// Options 配置处理器。Store、Drafter、Host 必填。
type Options struct {
	Store   *store.Store
	Drafter Drafter
	// Host 是本机主机名：仓库有 Host 等于它的检出才算"本机有检出"。
	Host string
	// Harness 是本机起草用的无头运行声明名，空则用 Web 设置的默认 harness（与任务型同一规则，§13.6）。
	// Executor 是无本机检出时受托起草的执行者 id（运行配置 guideline_draft_executor，§16.2），空则起草事项转 needs_input。
	Harness, Executor string
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the rule or the guideline check options")
}

// Handler 返回 call_agent 动作的处理器：动作的 prompt 字段是任务型起草模板名，能力包取规则键的包名。
// 它只服务时钟规则（没有触发事项）。
func Handler(o Options) (rules.Handler, error) {
	if o.Store == nil || o.Drafter == nil || o.Host == "" {
		return nil, errors.New("guideline: Store, Drafter and Host are required")
	}
	return func(ctx context.Context, c rules.Call) error { return o.run(ctx, c) }, nil
}

// missing 是缺规范的项目。
type missing struct {
	project  domain.Project
	checkout *domain.Checkout // 本机检出；空表示要委派
	repos    []string
}

func (o Options) run(ctx context.Context, c rules.Call) error {
	tmpl, _ := c.Action["prompt"].(string)
	if tmpl == "" {
		return invalid("call_agent needs a prompt template name")
	}
	if c.Item != nil {
		return invalid("guideline check is a clock rule; it has no triggering item")
	}
	ms, err := o.scan(ctx)
	if err != nil {
		return err
	}
	pack, _, _ := strings.Cut(c.Rule, "/")
	var errs []error
	for _, m := range ms {
		item, err := o.draft(ctx, c, pack, tmpl, m)
		if item != "" {
			if rerr := o.record(ctx, m.project.ID, item); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("guideline: project %s: %w", m.project.ID, err))
		}
	}
	return errors.Join(errs...)
}

// scan 找出该起草的项目：活跃、有 repos、没有任何 Guideline、没有在途或被本人丢弃的起草事项。
func (o Options) scan(ctx context.Context) (out []missing, err error) {
	err = o.Store.View(ctx, func(tx *store.Tx) error {
		ps, err := projects.List(tx)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.Status != domain.ProjectActive || len(p.Repos) == 0 {
				continue
			}
			if has, err := hasGuideline(tx, p.ID); err != nil || has {
				if err != nil {
					return err
				}
				continue
			}
			if busy, err := drafting(tx, p.ID); err != nil || busy {
				if err != nil {
					return err
				}
				continue
			}
			out = append(out, o.plan(p))
		}
		return nil
	})
	return
}

func (o Options) plan(p domain.Project) missing {
	m := missing{project: p, checkout: p.LocalCheckout(o.Host)}
	for _, r := range p.Repos {
		m.repos = append(m.repos, r.Remote)
	}
	return m
}

func hasGuideline(tx *store.Tx, project string) (bool, error) {
	ms, err := materials.List(tx, project)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.Kind == materials.KindGuideline {
			return true, nil
		}
	}
	return false, nil
}

// drafting 判断上一次起草是否还在途：事项未结束，或被本人丢弃（不再打扰）。
func drafting(tx *store.Tx, project string) (bool, error) {
	r, err := tx.Query(`SELECT item FROM guideline_draft WHERE project = ?`, project)
	if err != nil {
		return false, err
	}
	var item string
	found := r.Next()
	if found {
		err = r.Scan(&item)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil || !found {
		return false, err
	}
	it, err := items.Get(tx, item)
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeNotFound {
		return false, nil // 事项已不在：重新起草
	}
	if err != nil {
		return false, err
	}
	return it.Phase != domain.PhaseDone, nil
}

func (o Options) record(ctx context.Context, project, item string) error {
	return o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO guideline_draft (project, item) VALUES (?, ?) ON CONFLICT(project) DO UPDATE SET item = excluded.item`, project, item)
		return err
	})
}

func title(p domain.Project) string { return "起草项目规范：" + p.Name }

// draft 起草一个项目的规范，返回起草事项 id（已建出就返回，即使后续失败，让它被记录而不重复建）。
func (o Options) draft(ctx context.Context, c rules.Call, pack, tmpl string, m missing) (string, error) {
	if m.checkout != nil {
		return o.local(ctx, c, pack, tmpl, m)
	}
	return o.delegate(c, m)
}

func brief(m missing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "项目「%s」（%s）有仓库但还没有规范（Guideline）。请阅读仓库，起草一份项目规范，"+
		"用 `apa material commit --kind guideline --project %s` 提交；它只会成为待确认的修订，由本人确认后生效。\n\n仓库：\n",
		m.project.Name, m.project.ID, m.project.ID)
	for _, r := range m.repos {
		b.WriteString("- " + r + "\n")
	}
	return b.String()
}

func (o Options) local(ctx context.Context, c rules.Call, pack, tmpl string, m missing) (string, error) {
	body, _ := json.Marshal(map[string]any{"project": m.project.ID, "repos": m.project.Repos, "checkout": m.checkout})
	out, err := o.Drafter.Launch(ctx, task.Task{Pack: pack, Template: tmpl, Title: title(m.project), Project: m.project.ID,
		Harness: o.Harness, Actor: domain.Actor{Type: domain.ActorAgent, Principal: "rule:" + c.Rule, Via: domain.ViaRule},
		Materials: []task.Material{{Name: "guideline-check", Body: string(body)}}})
	return out.Item, err
}

// noExecutor 是未配置委派执行者时的起草事项：转 needs_input 写明原因，交本人处理，之后不会每轮重复建。
func (o Options) noExecutor(c rules.Call, m missing) (string, error) {
	ask := map[string]any{"question": "无法委派起草项目规范：运行配置没有 guideline_draft_executor，且本机没有该项目的检出",
		"options": []domain.AskOption{{ID: "retry", Label: "已配置，下次检测重新起草", Next: domain.PhaseDone},
			{ID: "drop", Label: "不需要起草", Next: domain.PhaseDropped}}}
	res, err := c.Do("item.create", map[string]any{"kind": domain.KindTask, "title": title(m.project), "phase": domain.PhaseNeedsInput,
		"project": m.project.ID, "ask": ask, "reason": "rule " + c.Rule + ": guideline_draft_executor is not configured"})
	if err != nil {
		return "", err
	}
	var it struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &it); err != nil || it.ID == "" {
		return "", fmt.Errorf("item.create returned no id: %v", err)
	}
	return it.ID, nil
}

func (o Options) delegate(c rules.Call, m missing) (string, error) {
	if o.Executor == "" {
		return o.noExecutor(c, m)
	}
	res, err := c.Do("item.create", map[string]any{"kind": domain.KindTask, "title": title(m.project), "phase": domain.PhaseOpen,
		"project": m.project.ID, "reason": "rule " + c.Rule})
	if err != nil {
		return "", err
	}
	var parent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &parent); err != nil || parent.ID == "" {
		return "", fmt.Errorf("item.create returned no id: %v", err)
	}
	_, err = c.Do("delegate.create", map[string]any{"parent": parent.ID, "assignee": o.Executor, "title": title(m.project),
		"body": brief(m), "reason": "rule " + c.Rule})
	return parent.ID, err
}
