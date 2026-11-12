// Package summaries 是 decision-card 包的两个生成型摘要（设计 §6.3、§8.1、§12.4、§7.6）：
//
//   - 一句话摘要：事项缺 summary 时起草，经 item.update 只写 summary 字段。触发是事项新建、标题或正文变化、阶段变化，
//     只给 open 与 waiting 的事项起草：proposed 多为来源批量产生，被接受转 open 时再起草；needs_input 的投递内容由
//     决策卡片负责，改 summary 会让待决决策按新版本重发；已结束的不再需要。
//   - 材料修订摘要：自有型材料每条新修订（direct 生效、proposed 入队、回退）起草变更摘要，经 revision.summarize 写回；
//     apa context 的材料变更摘要取它，没有时仍取修订理由。
//
// 本包按游标读 Event：装配处在命令提交后唤醒 Drain，后台维护循环兜底并在重启后补做。游标初值是迁移时的日志末尾，不回填历史。
// 调用记 system 身份（kernel/agents/direct 的留痕）；写回经命令模型，以 system 身份代表本人、带 evidence，
// Basis 记策略版本与 decision-card/<模板>。
//
// 退化：模型未配置时不调用、不写；调用失败（direct 已退避重试）或输出不合 schema 时记 kind=decision-card 的
// health.alert（只进健康视图），摘要留空，其余照常；信息不足时不写。写回前事项已变（乐观版本冲突）时放弃这次写回。
// 本包不起子进程、不发网络请求，只写自己的游标与 health.alert，其余写入都经命令。
package summaries

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 生成型提示词由 decision-card 内置包声明（附录 C.14）。
const (
	Pack             = "decision-card"
	TemplateItem     = "one-line-summary"
	TemplateRevision = "revision-summary"
	// AlertKind 是起草失败 health.alert 的 payload.kind（与触达的卡片起草同一类）。
	AlertKind = "decision-card"
)

// Module 登记事件游标；初值取迁移时的日志末尾。
var Module = store.Module{Name: "summaries", Migrations: []store.Migration{{
	Version: 1, Name: "create-summaries-cursor",
	SQL: `CREATE TABLE summaries_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);
INSERT INTO summaries_cursor (id, seq) SELECT 1, COALESCE(MAX(seq), 0) FROM eventlog_event;`,
}}}

// Generator 是生成型调用（*direct.Service 满足它）。
type Generator interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// Options 配置 Service；Store、Exec、Gen 必填。
type Options struct {
	Store *store.Store
	Exec  *command.Executor
	Gen   Generator
	Owner string // 写回命令的 on_behalf_of，默认 "owner"
	Now   func() time.Time
}

// Service 的状态在账本里；mu 只让 Drain 串行。
type Service struct {
	o  Options
	mu sync.Mutex
}

// New 返回 Service。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Exec == nil || o.Gen == nil {
		return nil, errors.New("summaries: Store, Exec and Gen are required")
	}
	if o.Owner == "" {
		o.Owner = "owner"
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o: o}, nil
}

const principal = "system:decision-card"

func (s *Service) actor() domain.Actor {
	return domain.Actor{Type: domain.ActorSystem, Principal: principal, Via: domain.ViaClock, OnBehalfOf: s.o.Owner}
}

// Drain 处理游标之后的全部事件。只有存储错误与停机才返回错误（游标停在出错的批次之前，下次重做）。
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		var (
			batch []eventlog.Entry
			after int64
		)
		if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
			r, err := tx.Query(`SELECT seq FROM summaries_cursor WHERE id = 1`)
			if err != nil {
				return err
			}
			if r.Next() {
				err = r.Scan(&after)
			}
			if cerr := r.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			batch, err = eventlog.Read(tx, after, 100)
			return err
		}); err != nil || len(batch) == 0 {
			return err
		}
		itemIDs, revIDs := candidates(batch)
		for _, id := range itemIDs {
			if err := s.item(ctx, id); err != nil {
				return err
			}
		}
		for _, id := range revIDs {
			if err := s.revision(ctx, id); err != nil {
				return err
			}
		}
		last := batch[len(batch)-1].Event.Seq
		if err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE summaries_cursor SET seq = ? WHERE id = 1`, last)
			return err
		}); err != nil {
			return err
		}
	}
}

// candidates 从一批事件里挑出要起草的事项与修订（去重，按首次出现排序）。
func candidates(batch []eventlog.Entry) (itemIDs, revIDs []string) {
	seen := map[string]bool{}
	add := func(list *[]string, id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			*list = append(*list, id)
		}
	}
	for _, en := range batch {
		if en.Tombstone {
			continue
		}
		e := en.Event
		switch e.Type {
		case domain.EvItemCreated, domain.EvItemPhaseChanged:
			add(&itemIDs, e.Object)
		case domain.EvItemUpdated:
			var p struct {
				Before *struct{ Title, Body string } `json:"before"`
				Item   struct{ Title, Body string }  `json:"item"`
			}
			// 只在标题或正文变化时起草：信息不足的事项不会因无关修改反复调用。
			if json.Unmarshal(e.Payload, &p) == nil && (p.Before == nil || p.Before.Title != p.Item.Title || p.Before.Body != p.Item.Body) {
				add(&itemIDs, e.Object)
			}
		case domain.EvMaterialRevised, domain.EvMaterialRevisionProposed:
			var p struct {
				Revision struct {
					ID string `json:"id"`
				} `json:"revision"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				add(&revIDs, p.Revision.ID) // 托管型刷新没有修订
			}
		}
	}
	return
}

// item 给缺 summary 的事项起草一句话摘要并写回。
func (s *Service) item(ctx context.Context, id string) error {
	var (
		call direct.Call
		ver  int64
		skip bool
	)
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, id)
		if isNotFound(err) {
			skip = true
			return nil
		}
		if err != nil {
			return err
		}
		if skip = it.Summary != "" || (it.Phase != domain.PhaseOpen && it.Phase != domain.PhaseWaiting); skip {
			return nil
		}
		if ver, err = command.Version(tx, id); err != nil {
			return err
		}
		in, _ := json.Marshal(map[string]any{"item": it.ID, "kind": it.Kind, "title": it.Title, "project": it.Project, "tags": it.Tags})
		call, err = s.call(tx, TemplateItem, it.ID, it.Project, in, it.Body)
		return err
	}); err != nil || skip {
		return err
	}
	var out struct {
		Summary string `json:"summary"`
	}
	if ok, err := s.generate(ctx, call, &out, &out.Summary); !ok || err != nil {
		return err
	}
	in, _ := json.Marshal(map[string]any{"id": id, "summary": out.Summary, "reason": "one-line summary drafted"})
	return s.write(ctx, call, command.Request{Command: "item.update", Input: in, ExpectVersion: &ver})
}

// revision 给还没有变更摘要的修订起草摘要并写回。差异按读取时计算（§6.6），新材料与空内容比。
func (s *Service) revision(ctx context.Context, id string) error {
	var (
		call direct.Call
		skip bool
	)
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		v, err := materials.GetRevision(tx, id)
		if isNotFound(err) {
			skip = true
			return nil
		}
		if err != nil {
			return err
		}
		if skip = v.Summary != ""; skip { // 确认 proposed 修订时摘要已在入队时起草
			return nil
		}
		m, err := materials.Get(tx, v.Material)
		if err != nil {
			return err
		}
		var base string
		if v.BasedOn != "" {
			b, err := materials.GetRevision(tx, v.BasedOn)
			if err != nil {
				return err
			}
			base = b.Content
		}
		var diff strings.Builder
		// shortcut: 整份逐行差异进资料段；材料大到超出模型上下文时只带变化行及其前后几行。
		for _, l := range materials.Diff(base, v.Content) {
			diff.WriteString(l.Op + " " + l.Text + "\n")
		}
		in, _ := json.Marshal(map[string]any{"material": m.ID, "title": m.Title, "kind": m.Kind, "revision": v.ID,
			"state": v.State, "reason": v.Reason})
		call, err = s.call(tx, TemplateRevision, m.ID, m.Project, in, diff.String())
		return err
	}); err != nil || skip {
		return err
	}
	var out struct {
		Summary       string `json:"summary"`
		EffectOnRules string `json:"effect_on_rules"`
	}
	if ok, err := s.generate(ctx, call, &out, &out.Summary); !ok || err != nil {
		return err
	}
	sum := out.Summary
	if e := strings.TrimSpace(out.EffectOnRules); e != "" {
		sum += "（对 agent 行为的影响：" + e + "）"
	}
	in, _ := json.Marshal(map[string]any{"id": id, "summary": sum})
	return s.write(ctx, call, command.Request{Command: "revision.summarize", Input: in})
}

// call 组织一次生成型调用：system 身份、Basis 记策略版本与模板；外部正文只进资料段。
func (s *Service) call(tx *store.Tx, tmpl, target, project string, in json.RawMessage, material string) (direct.Call, error) {
	pv, _, err := policy.Current(tx)
	if err != nil {
		return direct.Call{}, err
	}
	depth, err := items.ChainDepth(tx, target) // 材料没有事项事件时为 0
	if err != nil {
		return direct.Call{}, err
	}
	return direct.Call{Pack: Pack, Template: tmpl, Input: in, Material: material, Target: target, Project: project, NoStats: true,
		Actor: s.actor(), ChainDepth: depth, Basis: &domain.Basis{PolicyVersion: pv, RuleID: Pack + "/" + tmpl}}, nil
}

// generate 调用生成型并把结果解析进 out。ok 为假表示不写回：未配置、信息不足、摘要（out 里的 *summary）为空，
// 或失败（已记 health.alert）。
func (s *Service) generate(ctx context.Context, c direct.Call, out any, summary *string) (ok bool, err error) {
	res, derr := s.o.Gen.Do(ctx, c)
	switch {
	case errors.Is(derr, model.ErrNotConfigured):
		return false, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case derr != nil:
		return false, s.alert(ctx, c, derr)
	case !res.Settled:
		return false, nil
	}
	if err := json.Unmarshal(res.Final().Result, out); err != nil { // 已按 schema 校验，不会发生
		return false, err
	}
	*summary = strings.TrimSpace(*summary)
	return *summary != "", nil
}

// write 以 system 身份经命令模型写回。写回前对象已变（冲突）或已被删就放弃；被拒绝等其余命令错误记 health.alert。
func (s *Service) write(ctx context.Context, c direct.Call, req command.Request) error {
	req.Identity = command.Trusted{Actor: s.actor(), Level: authz.LevelDelegate}
	req.Owner, req.RuleID = s.o.Owner, c.Basis.RuleID
	req.Evidence = "kernel drafted " + c.Basis.RuleID
	req.Chain = command.Chain{Mode: command.ChainKeep, From: c.Target}
	_, err := s.o.Exec.Do(ctx, req)
	var de *domain.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &de) && (de.Code == domain.CodeConflict || de.Code == domain.CodeNotFound):
		return nil
	case errors.As(err, &de):
		return s.alert(ctx, c, err)
	}
	return err
}

// alert 记一条只进健康视图的 health.alert（kind=decision-card）。
func (s *Service) alert(ctx context.Context, c direct.Call, cause error) error {
	payload, _ := json.Marshal(map[string]string{"kind": AlertKind, "template": c.Template, "reason": cause.Error()})
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: c.Target, Actor: s.actor(),
			Reason: c.Template + " draft failed; left empty: " + cause.Error(), Payload: payload, ChainDepth: c.ChainDepth, Basis: c.Basis, At: s.o.Now()})
		return err
	})
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}
