// Package hosted 是仓库托管型材料（设计 §12.2、§12.3）：只登记标题、仓库、路径和最近 commit，不存正文副本，无 Revision。
//
//	material.add       登记（材料行经 materials.AddHosted，本包只写自己的 hosted_material 表）
//	material.refresh   刷新最近 commit；路径在默认分支已不存在时生成 needs_input 修正登记
//	material.relocate  修正登记的路径（needs_input 的落点），清除"路径已消失"状态
//
// 读取与修改见 service.go：读取经外呼调用器入队 reader Run（异步、短期缓存、读不到降级为"暂不可读"），
// 修改不在 APA 内做，生成委派子事项。对托管型 commit 的拒绝（退出码 6）在 kernel/materials。
package hosted

import (
	"bytes"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。fix_item 是还在等本人修正登记的 needs_input 事项，用来保证不重复生成。
var Module = store.Module{Name: "hosted", Migrations: []store.Migration{{
	Version: 1, Name: "create-hosted-material",
	SQL: `CREATE TABLE hosted_material (
	material TEXT PRIMARY KEY,
	repo TEXT NOT NULL,
	path TEXT NOT NULL,
	last_commit TEXT NOT NULL DEFAULT '',
	missing INTEGER NOT NULL DEFAULT 0,
	fix_item TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;`,
}}}

// Entry 是托管型材料的登记信息。
type Entry struct {
	Material materials.Material `json:"material"`
	Repo     string             `json:"repo"`
	Path     string             `json:"path"`
	Commit   string             `json:"commit,omitempty"` // 最近 commit，尚未读到过则为空
	Missing  bool               `json:"missing,omitempty"`
	FixItem  string             `json:"fix_item,omitempty"`
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// Get 读取托管型材料登记；材料不存在返回 not_found，自有型材料返回 conflict。
func Get(tx *store.Tx, id string) (Entry, error) {
	m, err := materials.Get(tx, id)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Material: m}
	r, err := tx.Query("SELECT repo, path, last_commit, missing, fix_item FROM hosted_material WHERE material = ?", id)
	if err != nil {
		return e, err
	}
	defer r.Close()
	if !r.Next() {
		if err := r.Err(); err != nil {
			return e, err
		}
		return e, domain.NewError(domain.CodeConflict, "material "+id+" is not hosted in a repository", "owned materials have revisions: use material show")
	}
	var missing int64
	if err := r.Scan(&e.Repo, &e.Path, &e.Commit, &missing, &e.FixItem); err != nil {
		return e, err
	}
	e.Missing = missing != 0
	return e, r.Err()
}

// List 返回项目内的托管型材料登记（project 为空取全部）。
func List(tx *store.Tx, project string) ([]Entry, error) {
	ms, err := materials.List(tx, project)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, m := range ms {
		if m.Location == "" {
			continue
		}
		e, err := Get(tx, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// cleanPath 校验并规整仓库内相对路径。
func cleanPath(p string) (string, error) {
	c := path.Clean(strings.TrimSpace(p))
	if blank(p) || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") || c == "." {
		return "", invalid("path must be a relative file path inside the repository")
	}
	return c, nil
}

func checkRepo(r string) error {
	if blank(r) || strings.HasPrefix(strings.TrimSpace(r), "-") {
		return invalid("repo is required")
	}
	return nil
}

type addIn struct {
	Project string   `json:"project"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
	Repo    string   `json:"repo"`
	Path    string   `json:"path"`
	Commit  string   `json:"commit"`
	Reason  string   `json:"reason"`
}

type refreshIn struct {
	ID          string `json:"id"`
	Commit      string `json:"commit"`
	PathMissing bool   `json:"path_missing"`
	Path        string `json:"path,omitempty"` // 这次读取读的路径；登记已被修正时结果作废，在事务内比对
}

type relocateIn struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Result 是写命令的结果。
type Result struct {
	Material string `json:"material"`
	FixItem  string `json:"fix_item,omitempty"` // 路径消失时生成的 needs_input 事项
	Changed  bool   `json:"changed"`
}

// Register 登记 material.add、material.refresh、material.relocate，kind 只接受内置集合（规范除外）。
func Register(x *command.Executor) error { return RegisterWith(x, nil) }

// RegisterWith 同 Register；kind 另接受 src 里能力包声明的 material_kinds，每次登记时从加载器读。
// 授权按"修订材料"（§14.3）：observer 得到 4。
func RegisterWith(x *command.Executor, src materials.PackSource) error {
	for _, sp := range []command.Spec{add(src), refresh(), relocate()} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

func objectOf(m materials.Material) authz.Object {
	return authz.Object{ID: m.ID, Kind: m.Kind, Project: m.Project, Tags: m.Tags}
}

func materialObject(tx *store.Tx, id string) (authz.Object, error) {
	e, err := Get(tx, id)
	return objectOf(e.Material), err
}

// event 的 payload：足以让订阅者（AGENTS.md 渲染、导出 ext）重放这次登记变化；不含正文。
func payload(e Entry, extra map[string]any) json.RawMessage {
	p := map[string]any{"material": e.Material.ID, "kind": e.Material.Kind, "title": e.Material.Title, "project": e.Material.Project,
		"hosted": map[string]any{"repo": e.Repo, "path": e.Path, "commit": e.Commit, "path_missing": e.Missing}}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

// kindDefault 是托管型缺省的 kind：留在仓库的是架构设计、模块边界、接口契约、ADR 这类设计文档（§12.2）。
const kindDefault = "design"

// fill 补上可推断的字段（§12.3）：kind 缺省 design；project 缺省取登记了该仓库远程地址的项目（远程地址全局唯一）。
func (v *addIn) fill(tx *store.Tx) error {
	if blank(v.Kind) {
		v.Kind = kindDefault
	}
	if !blank(v.Project) || blank(v.Repo) {
		return nil
	}
	p, ok, err := projects.ByRemote(tx, strings.TrimSpace(v.Repo))
	if ok {
		v.Project = p.ID
	}
	return err
}

func add(src materials.PackSource) command.Spec {
	parse := func(tx *store.Tx, in json.RawMessage) (addIn, error) {
		var v addIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		return v, v.fill(tx)
	}
	return command.Spec{
		Name: "material.add", Action: authz.ActionReviseMaterial, Tier: authz.TierWork,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(tx, in)
			return authz.Object{Kind: v.Kind, Project: v.Project, Tags: v.Tags}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(tx, in)
			if k := v.Kind; k == materials.KindGuideline || !slices.Contains(materials.Kinds(src), k) {
				return invalid("unknown material kind " + v.Kind)
			}
			if blank(v.Project) {
				return invalid("project is required: the repository is not registered in any project")
			}
			if _, err := projects.Get(tx, v.Project); err != nil {
				return err
			}
			if err := checkRepo(v.Repo); err != nil {
				return err
			}
			_, err := cleanPath(v.Path)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Tx, c.Input)
			p, _ := cleanPath(v.Path)
			repo := strings.TrimSpace(v.Repo)
			title := strings.TrimSpace(v.Title)
			if title == "" {
				title = path.Base(p)
			}
			m, err := materials.AddHosted(c.Tx, v.Project, v.Kind, title, v.Tags, repo+":"+p)
			if err != nil {
				return nil, err
			}
			if _, err := c.Tx.Exec("INSERT INTO hosted_material (material, repo, path, last_commit) VALUES (?,?,?,?)",
				m.ID, repo, p, strings.TrimSpace(v.Commit)); err != nil {
				return nil, err
			}
			e := Entry{Material: m, Repo: repo, Path: p, Commit: strings.TrimSpace(v.Commit)}
			reason := v.Reason
			if blank(reason) {
				reason = "hosted material registered"
			}
			_, err = c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: m.ID, Reason: reason, Payload: payload(e, map[string]any{"registered": true})})
			return Result{Material: m.ID, Changed: true}, err
		}}
}

func refresh() command.Spec {
	parse := func(in json.RawMessage) (refreshIn, error) {
		var v refreshIn
		return v, decode(in, &v)
	}
	return command.Spec{
		Name: "material.refresh", Action: authz.ActionReviseMaterial,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			return materialObject(tx, v.ID)
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			if v.PathMissing == !blank(v.Commit) {
				return invalid("give either commit or path_missing")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			e, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			if v.Path != "" && v.Path != e.Path {
				return Result{Material: e.Material.ID}, nil
			}
			if v.PathMissing {
				return markMissing(c, e)
			}
			commit := strings.TrimSpace(v.Commit)
			if commit == e.Commit && !e.Missing {
				return Result{Material: e.Material.ID}, nil
			}
			// 路径又读到了：清除"已消失"，修正事项由本人自己回答，不替他关。
			e.Commit, e.Missing = commit, false
			if _, err := c.Tx.Exec("UPDATE hosted_material SET last_commit = ?, missing = 0 WHERE material = ?", commit, e.Material.ID); err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: e.Material.ID, Reason: "hosted commit refreshed to " + commit, Payload: payload(e, nil)})
			return Result{Material: e.Material.ID, Changed: true}, err
		}}
}

// markMissing 在路径于默认分支消失时登记状态并生成 needs_input；上一条修正事项还在等本人时不重复生成。
func markMissing(c *command.Ctx, e Entry) (any, error) {
	if pending, err := fixPending(c, e); err != nil {
		return nil, err
	} else if pending {
		if e.Missing {
			return Result{Material: e.Material.ID, FixItem: e.FixItem}, nil
		}
		// 路径曾被读到又消失：沿用还在等本人的修正事项，但状态与事件要补上，索引才会重新渲染。
		e.Missing = true
		if _, err := c.Tx.Exec("UPDATE hosted_material SET missing = 1 WHERE material = ?", e.Material.ID); err != nil {
			return nil, err
		}
		_, err := c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: e.Material.ID, Reason: "hosted path missing on default branch",
			Payload: payload(e, map[string]any{"fix_item": e.FixItem})})
		return Result{Material: e.Material.ID, FixItem: e.FixItem, Changed: true}, err
	}
	child := domain.Item{Phase: domain.PhaseNeedsInput, Kind: domain.KindTask, Priority: domain.PriorityNormal,
		Title:   "托管文档路径已不存在：" + e.Material.Title,
		Summary: e.Repo + ":" + e.Path + " 在默认分支上已找不到",
		Project: e.Material.Project,
		Ask: &domain.Ask{AskInput: domain.AskInput{
			Question: "托管文档 " + e.Path + "（" + e.Repo + "）在默认分支上已不存在，请修正登记的路径，或确认已废弃。",
			FreeText: true,
			Options:  []domain.AskOption{{ID: "deprecated", Label: "已废弃，保持现状", Next: domain.PhaseDone}},
		}},
		ItemKernelState: domain.ItemKernelState{ChainDepth: c.ChainDepth}}
	child, snap, err := items.Create(c.Tx, child, "", c.Now())
	if err != nil {
		return nil, err
	}
	if _, err := command.Bump(c.Tx, child.ID); err != nil { // 新对象不是管线的授权对象，版本自己加
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: domain.EvItemCreated, Object: child.ID, Reason: "hosted path missing", Payload: snap}); err != nil {
		return nil, err
	}
	e.Missing, e.FixItem = true, child.ID
	if _, err := c.Tx.Exec("UPDATE hosted_material SET missing = 1, fix_item = ? WHERE material = ?", child.ID, e.Material.ID); err != nil {
		return nil, err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: e.Material.ID, Reason: "hosted path missing on default branch",
		Payload: payload(e, map[string]any{"fix_item": child.ID})})
	return Result{Material: e.Material.ID, FixItem: child.ID, Changed: true}, err
}

// fixPending 报告登记的修正事项是否还在等本人回答。
func fixPending(c *command.Ctx, e Entry) (bool, error) {
	if e.FixItem == "" {
		return false, nil
	}
	it, err := items.Get(c.Tx, e.FixItem)
	if err != nil {
		return false, err
	}
	return it.Phase == domain.PhaseNeedsInput && it.Ask != nil && it.Ask.Resolution == nil, nil
}

// closeFix 在路径已被修正时收掉还在等本人的修正事项（经 items.Revise，同委派回收父事项的做法），并解除登记上的关联。
func closeFix(c *command.Ctx, e Entry) error {
	pending, err := fixPending(c, e)
	if err != nil || e.FixItem == "" {
		return err
	}
	if pending {
		_, _, snap, err := items.Revise(c.Tx, e.FixItem, "material.relocate", c.Now(), func(it *domain.Item) error {
			it.Phase = domain.PhaseDropped
			it.Ask.Resolution = &domain.Resolution{ResolvedVia: "relocate", ResolvedAt: c.Now()}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := command.Bump(c.Tx, e.FixItem); err != nil {
			return err
		}
		if _, err := c.Emit(domain.Event{Type: domain.EvItemPhaseChanged, Object: e.FixItem, Reason: "hosted path corrected", Payload: snap}); err != nil {
			return err
		}
	}
	_, err = c.Tx.Exec("UPDATE hosted_material SET fix_item = '' WHERE material = ?", e.Material.ID)
	return err
}

func relocate() command.Spec {
	parse := func(in json.RawMessage) (relocateIn, error) {
		var v relocateIn
		return v, decode(in, &v)
	}
	return command.Spec{
		Name: "material.relocate", Action: authz.ActionReviseMaterial,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			return materialObject(tx, v.ID)
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			_, err := cleanPath(v.Path)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			e, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			p, _ := cleanPath(v.Path)
			if err := closeFix(c, e); err != nil {
				return nil, err
			}
			e.Path, e.Commit, e.Missing, e.FixItem = p, "", false, ""
			e.Material.Location = e.Repo + ":" + p
			// location 属 materials 的表，经它的导出写入口改；commit 待下次读取刷新。
			if err := materials.SetLocation(c.Tx, e.Material.ID, e.Material.Location); err != nil {
				return nil, err
			}
			if _, err := c.Tx.Exec("UPDATE hosted_material SET path = ?, last_commit = '', missing = 0 WHERE material = ?", p, e.Material.ID); err != nil {
				return nil, err
			}
			reason := v.Reason
			if blank(reason) {
				reason = "hosted path corrected"
			}
			_, err = c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: e.Material.ID, Reason: reason, Payload: payload(e, nil)})
			return Result{Material: e.Material.ID, Changed: true}, err
		}}
}
