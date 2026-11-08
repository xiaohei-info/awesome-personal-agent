// Package materials 是自有型材料与修订（设计 §6.6、§7.1、§7.2、§12.4）：Material 登记、追加不改的 Revision、
// 按材料 kind 的写入策略直接生效（direct）或进入确认（proposed）、proposed 修订的确认与拒绝、回退（新增一条修订）。
// 所有写入经命令模型登记的命令，授权与事件由管线负责；本包只写自己的两张表，不碰其他模块的数据。
//
// 规则要点：
//   - direct 修订的 based_on 不等于当前修订时返回冲突（退出码 6）；proposed 修订可带旧基线入队，
//     确认时必须明示取舍：on_stale=adopt（仍然采用）或 rebase（基于当前重提）。
//   - guideline（规范）的写入策略恒为 proposed，不受策略配置放宽；确认它属 web_only（spec_activation），delegate 得到退出码 20。
//   - 回退只在 Web：新增一条内容等于旧快照的修订，历史不改。
//   - 托管型（location 非空）不能 commit，返回退出码 6 并提示其仓库路径；托管型的登记与读取不在本包。
//
// 本包不做全文检索、不渲染 AGENTS.md、不创建事项（决策待办流经 Pending 与事件读取）。
package materials

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 修订状态（§6.6）。被拒绝或被取代的修订都记为 superseded，内容保留。
const (
	StateCurrent    = "current"
	StateProposed   = "proposed"
	StateSuperseded = "superseded"
)

// KindGuideline 是特殊 kind：规范。每个项目至多一份，project 为空是全局规范。
const KindGuideline = "guideline"

// EvRevisionRejected 记 proposed 修订被拒绝。它不在对外稳定类型表内（§10.1），只供审计与回放。
const EvRevisionRejected = "material.revision_rejected"

// EvRevisionSummarized 记一条修订写入了变更摘要。同样不在对外稳定类型表内，只供审计。
const EvRevisionSummarized = "material.revision_summarized"

// builtinKinds 是内置 kind；能力包的 material_kinds 经 RegisterWith 的 PackSource 扩展。
var builtinKinds = []string{"minutes", "reference", "worknote", "requirement", "design", KindGuideline}

// 确认 proposed 修订时对"基线已变化"的取舍。
const (
	OnStaleAdopt  = "adopt"  // 仍然采用
	OnStaleRebase = "rebase" // 基于当前重提：旧提议作废，写一条基于当前的新 proposed 修订
)

// Module 是本模块在存储层登记的迁移。revisions 的内容列由触发器保证追加不改，只有 state 可变。
var Module = store.Module{Name: "materials", Migrations: []store.Migration{{
	Version: 1, Name: "create-materials-and-revisions",
	SQL: `CREATE TABLE materials (
	id TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	kind TEXT NOT NULL,
	title TEXT NOT NULL,
	tags TEXT NOT NULL,
	location TEXT NOT NULL DEFAULT '',
	current_rev TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX materials_project ON materials (project, kind);
CREATE TABLE revisions (
	id TEXT PRIMARY KEY,
	material TEXT NOT NULL,
	seq INTEGER NOT NULL,
	content TEXT NOT NULL,
	actor TEXT NOT NULL,
	reason TEXT NOT NULL,
	based_on TEXT NOT NULL,
	state TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (material, seq)
) WITHOUT ROWID;
CREATE TRIGGER revisions_append_only BEFORE UPDATE OF id, material, seq, content, actor, reason, based_on, created_at ON revisions
BEGIN SELECT RAISE(ABORT, 'revision content is append-only'); END;`,
}, {
	Version: 2, Name: "add-revision-summary",
	// 变更摘要由生成型在修订写入后起草，写入一次；不在追加不改的列里。
	SQL: `ALTER TABLE revisions ADD COLUMN summary TEXT NOT NULL DEFAULT '';`,
}}}

// Material 是材料登记。Location 非空表示托管型（"仓库:路径"），无 Revision。
type Material struct {
	ID         string   `json:"id"`
	Project    string   `json:"project"` // 空：全局规范
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Tags       []string `json:"tags,omitempty"`
	Location   string   `json:"location,omitempty"`
	CurrentRev string   `json:"current_rev,omitempty"`
}

// Revision 是一条追加不改的修订：完整 Markdown 快照。
type Revision struct {
	ID       string       `json:"id"`
	Material string       `json:"material"`
	Seq      int          `json:"seq"`
	Content  string       `json:"content"`
	Actor    domain.Actor `json:"actor"`
	Reason   string       `json:"reason"`
	BasedOn  string       `json:"based_on"`
	State    string       `json:"state"`
	At       time.Time    `json:"at"`
	Summary  string       `json:"summary,omitempty"` // 生成型起草的变更摘要；未起草或起草失败为空（§12.4）
}

// Pending 是一条待确认的 proposed 修订；BaselineChanged 为真时确认界面要提示"基线已变化"。
type Pending struct {
	Revision        Revision `json:"revision"`
	Material        Material `json:"material"`
	BaselineChanged bool     `json:"baseline_changed"`
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func notFound(what, id string) error {
	return domain.NewError(domain.CodeNotFound, what+" "+id+" does not exist", "list materials and retry")
}

func conflict(why, next string) error { return domain.NewError(domain.CodeConflict, why, next) }

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// ---- 读 ----

const matCols = "id, project, kind, title, tags, location, current_rev"
const revCols = "id, material, seq, content, actor, reason, based_on, state, created_at, summary"

func scanMaterial(r *store.Rows) (Material, error) {
	var m Material
	var tags string
	if err := r.Scan(&m.ID, &m.Project, &m.Kind, &m.Title, &tags, &m.Location, &m.CurrentRev); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(tags), &m.Tags); err != nil {
		return m, fmt.Errorf("materials: corrupt row %s: %w", m.ID, err)
	}
	return m, nil
}

func scanRevision(r *store.Rows) (Revision, error) {
	var v Revision
	var actor string
	var seq, at int64
	if err := r.Scan(&v.ID, &v.Material, &seq, &v.Content, &actor, &v.Reason, &v.BasedOn, &v.State, &at, &v.Summary); err != nil {
		return v, err
	}
	v.Seq = int(seq)
	if err := json.Unmarshal([]byte(actor), &v.Actor); err != nil {
		return v, fmt.Errorf("materials: corrupt revision %s: %w", v.ID, err)
	}
	v.At = time.Unix(0, at).UTC()
	return v, nil
}

func queryMaterials(tx *store.Tx, where string, args ...any) ([]Material, error) {
	r, err := tx.Query("SELECT "+matCols+" FROM materials "+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Material
	for r.Next() {
		m, err := scanMaterial(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, r.Err()
}

func queryRevisions(tx *store.Tx, where string, args ...any) ([]Revision, error) {
	r, err := tx.Query("SELECT "+revCols+" FROM revisions "+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Revision
	for r.Next() {
		v, err := scanRevision(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, r.Err()
}

// Get 读取材料登记；不存在返回 not_found。
func Get(tx *store.Tx, id string) (Material, error) {
	ms, err := queryMaterials(tx, "WHERE id = ?", id)
	if err != nil {
		return Material{}, err
	}
	if len(ms) == 0 {
		return Material{}, notFound("material", id)
	}
	return ms[0], nil
}

// List 返回材料登记，project 非空时只取该项目；按 kind、标题、id 排序。
func List(tx *store.Tx, project string) ([]Material, error) {
	if project == "" {
		return queryMaterials(tx, "ORDER BY kind, title, id")
	}
	return queryMaterials(tx, "WHERE project = ? ORDER BY kind, title, id", project)
}

// GetRevision 读取一条修订；不存在返回 not_found。
func GetRevision(tx *store.Tx, id string) (Revision, error) {
	vs, err := queryRevisions(tx, "WHERE id = ?", id)
	if err != nil {
		return Revision{}, err
	}
	if len(vs) == 0 {
		return Revision{}, notFound("revision", id)
	}
	return vs[0], nil
}

// History 返回材料的全部修订，旧的在前。
func History(tx *store.Tx, material string) ([]Revision, error) {
	return queryRevisions(tx, "WHERE material = ? ORDER BY seq", material)
}

// Show 读取材料与一条修订：rev 为空取当前修订；托管型与尚无当前修订的材料返回 not_found。
func Show(tx *store.Tx, id, rev string) (Material, Revision, error) {
	m, err := Get(tx, id)
	if err != nil {
		return m, Revision{}, err
	}
	if rev == "" {
		rev = m.CurrentRev
	}
	if rev == "" {
		return m, Revision{}, notFound("current revision of material", id)
	}
	v, err := GetRevision(tx, rev)
	if err != nil {
		return m, v, err
	}
	if v.Material != id {
		return m, Revision{}, notFound("revision", rev)
	}
	return m, v, nil
}

// PendingRevisions 返回待确认的 proposed 修订，先提交的在前；决策待办流据此展示确认项。
func PendingRevisions(tx *store.Tx) ([]Pending, error) {
	vs, err := queryRevisions(tx, "WHERE state = ? ORDER BY created_at, id", StateProposed)
	if err != nil {
		return nil, err
	}
	out := make([]Pending, 0, len(vs))
	for _, v := range vs {
		m, err := Get(tx, v.Material)
		if err != nil {
			return nil, err
		}
		out = append(out, Pending{Revision: v, Material: m, BaselineChanged: v.BasedOn != m.CurrentRev})
	}
	return out, nil
}

// ---- diff（读取时计算，§6.6） ----

// DiffLine 是一行差异：Op 为 " "（不变）、"-"（删除）、"+"（新增）。
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// Diff 按行计算 from 到 to 的差异（最长公共子序列）。
func Diff(from, to string) []DiffLine {
	a, b := strings.Split(from, "\n"), strings.Split(to, "\n")
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{" ", a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{"-", a[i]})
			i++
		default:
			out = append(out, DiffLine{"+", b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, DiffLine{"-", a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, DiffLine{"+", b[j]})
	}
	return out
}

// PackSource 取已加载能力包声明的材料 kind 及其默认写入策略（packs.Loader 满足它）；为 nil 表示只有内置 kind。
type PackSource interface {
	MaterialKinds() map[string]string
}

// packKinds 是已加载能力包声明的材料 kind 及其默认写入策略（C.2 material_kinds）。
func packKinds(src PackSource) map[string]string {
	if src == nil {
		return map[string]string{}
	}
	return src.MaterialKinds()
}

// Kinds 返回内置 kind 加 src 里能力包声明的 kind；每次调用从加载器读，热加载后立即生效。托管型登记（kernel/hosted）也用它。
func Kinds(src PackSource) []string {
	out := slices.Clone(builtinKinds)
	for k := range packKinds(src) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// AddHosted 登记一条托管型材料（Location 非空、无 Revision），只供 kernel/hosted 的命令在事务内调用；
// 授权、事件由调用方的命令管线负责。返回带新 id 的登记。
func AddHosted(tx *store.Tx, project, kind, title string, tags []string, location string) (Material, error) {
	if blank(location) {
		return Material{}, invalid("location is required for a hosted material")
	}
	id, err := newID("mat_")
	if err != nil {
		return Material{}, err
	}
	m := Material{ID: id, Project: project, Kind: kind, Title: strings.TrimSpace(title), Tags: tags, Location: location}
	return m, insertMaterial(tx, m)
}

// SetLocation 改托管型材料登记的"仓库:路径"，只供 kernel/hosted 的命令在事务内调用。
func SetLocation(tx *store.Tx, id, location string) error {
	n, err := tx.Exec("UPDATE materials SET location = ? WHERE id = ? AND location <> ''", location, id)
	if err == nil && n == 0 {
		return conflict("material "+id+" is not hosted", "only a hosted material has a location")
	}
	return err
}

// ---- 写（仅限命令内部） ----

func insertMaterial(tx *store.Tx, m Material) error {
	tags, _ := json.Marshal(m.Tags)
	_, err := tx.Exec("INSERT INTO materials ("+matCols+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		m.ID, m.Project, m.Kind, m.Title, string(tags), m.Location, m.CurrentRev)
	return err
}

func setCurrent(tx *store.Tx, material, rev string) error {
	_, err := tx.Exec("UPDATE materials SET current_rev = ? WHERE id = ?", rev, material)
	return err
}

func setState(tx *store.Tx, rev, state string) error {
	_, err := tx.Exec("UPDATE revisions SET state = ? WHERE id = ?", state, rev)
	return err
}

// addRevision 追加一条修订，序号为该材料现有最大序号加一。
func addRevision(c *command.Ctx, material, content, reason, basedOn, state string) (Revision, error) {
	id, err := newID("rev_")
	if err != nil {
		return Revision{}, err
	}
	var seq int64
	r, err := c.Tx.Query("SELECT COALESCE(MAX(seq), 0) + 1 FROM revisions WHERE material = ?", material)
	if err != nil {
		return Revision{}, err
	}
	if r.Next() {
		err = r.Scan(&seq)
	}
	r.Close()
	if err != nil {
		return Revision{}, err
	}
	v := Revision{ID: id, Material: material, Seq: int(seq), Content: content, Actor: c.Caller.Actor, Reason: reason,
		BasedOn: basedOn, State: state, At: c.Now().UTC()}
	actor, _ := json.Marshal(v.Actor)
	_, err = c.Tx.Exec("INSERT INTO revisions ("+revCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		v.ID, v.Material, v.Seq, v.Content, string(actor), v.Reason, v.BasedOn, v.State, v.At.UnixNano(), "")
	return v, err
}

// snapshot 是事件 payload：足以重放这次变更（材料登记加完整修订）。
func snapshot(m Material, v Revision, extra map[string]any) json.RawMessage {
	p := map[string]any{"material": m, "revision": v}
	for k, x := range extra {
		p[k] = x
	}
	b, _ := json.Marshal(p)
	return b
}

// promote 让 v 成为当前修订：旧当前修订转 superseded，记 material.revised；规范另记 guideline.activated。
func promote(c *command.Ctx, m Material, v Revision, reason string, extra map[string]any) error {
	if m.CurrentRev != "" {
		if err := setState(c.Tx, m.CurrentRev, StateSuperseded); err != nil {
			return err
		}
	}
	if err := setState(c.Tx, v.ID, StateCurrent); err != nil {
		return err
	}
	if err := setCurrent(c.Tx, m.ID, v.ID); err != nil {
		return err
	}
	m.CurrentRev, v.State = v.ID, StateCurrent
	p := snapshot(m, v, extra)
	if _, err := c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: m.ID, Reason: reason, Payload: p}); err != nil {
		return err
	}
	if m.Kind == KindGuideline {
		_, err := c.Emit(domain.Event{Type: domain.EvGuidelineActivated, Object: m.ID, Reason: reason, Payload: p})
		return err
	}
	return nil
}

// ---- 命令 ----

type commitIn struct {
	ID      string   `json:"id"` // 空：新建材料
	Project string   `json:"project"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
	Content string   `json:"content"`
	Reason  string   `json:"reason"`
	BasedOn string   `json:"based_on"` // 提交时看到的当前修订；新材料为空
}

type confirmIn struct {
	ID      string `json:"id"` // 修订 id
	OnStale string `json:"on_stale"`
	Reason  string `json:"reason"`
}

type rejectIn struct {
	ID     string `json:"id"` // 修订 id
	Reason string `json:"reason"`
}

type revertIn struct {
	ID     string `json:"id"` // 材料 id
	To     string `json:"to"` // 要恢复内容的修订 id
	Reason string `json:"reason"`
}

// Result 是写命令的结果。
type Result struct {
	Material string `json:"material"`
	Revision string `json:"revision"`
	State    string `json:"state"`
}

// Register 登记 material.commit、revision.confirm、revision.reject、material.revert、revision.summarize，kind 只接受内置集合。
func Register(x *command.Executor) error { return RegisterWith(x, nil) }

// RegisterWith 同 Register；src 里能力包声明的 material_kinds 也接受，默认写入策略取包声明（Web 按 kind 设置的优先）。
//
// 授权（§14.3）：commit 按"修订材料"（observer 得到 4）；confirm 与 reject 按代答（Web 本人可；delegate 必须附本人原话；
// 规范属 spec_activation，delegate 得到 20；observer、worker、运行凭据得到 4）；revert 只在 Web（delegate 得到 20，其余得到 4）。
func RegisterWith(x *command.Executor, src PackSource) error {
	for _, sp := range []command.Spec{commit(src), confirm(), reject(), revert(), summarize()} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

// writeMode 返回 kind 的写入策略：规范恒为 proposed；其次 Web 按 kind 的设置、能力包声明的默认、Web 的总默认。
func writeMode(p policy.Policy, kind, packDefault string) string {
	if kind == KindGuideline {
		return policy.WriteProposed
	}
	if m, ok := p.MaterialWrite.Kinds[kind]; ok {
		return m
	}
	if packDefault != "" {
		return packDefault
	}
	return p.MaterialWrite.Default
}

func objectOf(m Material) authz.Object {
	o := authz.Object{ID: m.ID, Kind: m.Kind, Project: m.Project, Tags: m.Tags}
	if m.Kind == KindGuideline {
		o.Category = policy.CategorySpecActivation
	}
	return o
}

// revisionObject 为 confirm 与 reject 解析授权对象：全部字段来自账本。
func revisionObject(tx *store.Tx, id string) (authz.Object, Material, Revision, error) {
	if blank(id) {
		return authz.Object{}, Material{}, Revision{}, invalid("revision id is required")
	}
	v, err := GetRevision(tx, id)
	if err != nil {
		return authz.Object{}, Material{}, v, err
	}
	m, err := Get(tx, v.Material)
	return objectOf(m), m, v, err
}

func hostedConflict(m Material) error {
	return conflict("material is hosted in a repository and has no revisions: "+m.Location,
		"change it through a pull request in that repository")
}

func commit(src PackSource) command.Spec {
	return command.Spec{
		Name: "material.commit", Action: authz.ActionReviseMaterial, Tier: authz.TierWork,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v commitIn
			if err := decode(in, &v); err != nil {
				return authz.Object{}, err
			}
			if v.ID == "" {
				return authz.Object{Kind: v.Kind, Project: v.Project, Tags: v.Tags}, nil
			}
			m, err := Get(tx, v.ID)
			return objectOf(m), err
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			var v commitIn
			if err := decode(in, &v); err != nil {
				return err
			}
			if blank(v.Reason) {
				return invalid("reason is required")
			}
			if v.Content == "" {
				return invalid("content is required")
			}
			if v.ID != "" {
				m, err := Get(tx, v.ID)
				if err != nil {
					return err
				}
				if m.Location != "" {
					return hostedConflict(m)
				}
				if (v.Kind != "" && v.Kind != m.Kind) || (v.Project != "" && v.Project != m.Project) {
					return invalid("kind and project of an existing material cannot change")
				}
				return nil
			}
			if !slices.Contains(Kinds(src), v.Kind) {
				return invalid("unknown material kind " + v.Kind)
			}
			if blank(v.Title) {
				return invalid("title is required")
			}
			if v.BasedOn != "" {
				return invalid("a new material has no base revision")
			}
			if v.Project == "" && v.Kind != KindGuideline {
				return invalid("only a guideline can be global; project is required")
			}
			if v.Project != "" {
				if _, err := projects.Get(tx, v.Project); err != nil {
					return err
				}
			}
			if v.Kind == KindGuideline {
				if ms, err := queryMaterials(tx, "WHERE kind = ? AND project = ?", KindGuideline, v.Project); err != nil {
					return err
				} else if len(ms) > 0 {
					return conflict("this scope already has a guideline: "+ms[0].ID, "commit a new revision to the existing guideline")
				}
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var in commitIn
			if err := decode(c.Input, &in); err != nil {
				return nil, err
			}
			var m Material
			if in.ID == "" {
				id, err := newID("mat_")
				if err != nil {
					return nil, err
				}
				m = Material{ID: id, Project: in.Project, Kind: in.Kind, Title: strings.TrimSpace(in.Title), Tags: in.Tags}
				if err := insertMaterial(c.Tx, m); err != nil {
					return nil, err
				}
			} else {
				var err error
				if m, err = Get(c.Tx, in.ID); err != nil {
					return nil, err
				}
			}
			pv, pol, err := policy.Current(c.Tx)
			if err != nil {
				return nil, err
			}
			mode := writeMode(pol, m.Kind, packKinds(src)[m.Kind])
			extra := map[string]any{"write_policy": mode, "policy_version": pv}
			if mode == policy.WriteDirect {
				if in.BasedOn != m.CurrentRev {
					return nil, conflict("material is at revision "+orNone(m.CurrentRev)+", not "+orNone(in.BasedOn),
						"re-read the current revision and commit again on top of it")
				}
				v, err := addRevision(c, m.ID, in.Content, in.Reason, in.BasedOn, StateSuperseded)
				if err != nil {
					return nil, err
				}
				if err := promote(c, m, v, in.Reason, extra); err != nil {
					return nil, err
				}
				return Result{m.ID, v.ID, StateCurrent}, nil
			}
			if in.BasedOn != "" {
				if base, err := GetRevision(c.Tx, in.BasedOn); err != nil || base.Material != m.ID {
					return nil, invalid("based_on is not a revision of this material")
				}
			}
			v, err := addRevision(c, m.ID, in.Content, in.Reason, in.BasedOn, StateProposed)
			if err != nil {
				return nil, err
			}
			if _, err := c.Emit(domain.Event{Type: domain.EvMaterialRevisionProposed, Object: m.ID, Reason: in.Reason, Payload: snapshot(m, v, extra)}); err != nil {
				return nil, err
			}
			return Result{m.ID, v.ID, StateProposed}, nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func confirm() command.Spec {
	return command.Spec{
		Name: "revision.confirm", Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v confirmIn
			if err := decode(in, &v); err != nil {
				return authz.Object{}, err
			}
			o, _, _, err := revisionObject(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			var in2 confirmIn
			if err := decode(in, &in2); err != nil {
				return err
			}
			if !slices.Contains([]string{"", OnStaleAdopt, OnStaleRebase}, in2.OnStale) {
				return invalid("on_stale must be adopt or rebase")
			}
			_, m, v, err := revisionObject(tx, in2.ID)
			if err != nil {
				return err
			}
			if v.State != StateProposed { // 只认第一次
				return conflict("revision is already "+v.State, "re-read the material to see its current state")
			}
			if v.BasedOn != m.CurrentRev && in2.OnStale == "" {
				return conflict("baseline changed: the current revision is no longer the one this proposal was based on",
					"review the diff, then confirm with on_stale=adopt to keep it or on_stale=rebase to re-propose on the current revision")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var in confirmIn
			if err := decode(c.Input, &in); err != nil {
				return nil, err
			}
			_, m, v, err := revisionObject(c.Tx, in.ID)
			if err != nil {
				return nil, err
			}
			reason := in.Reason
			if blank(reason) {
				reason = v.Reason
			}
			stale := v.BasedOn != m.CurrentRev
			if stale && in.OnStale == OnStaleRebase {
				if err := setState(c.Tx, v.ID, StateSuperseded); err != nil {
					return nil, err
				}
				nv, err := addRevision(c, m.ID, v.Content, reason, m.CurrentRev, StateProposed)
				if err != nil {
					return nil, err
				}
				if _, err := c.Emit(domain.Event{Type: domain.EvMaterialRevisionProposed, Object: m.ID, Reason: reason,
					Payload: snapshot(m, nv, map[string]any{"rebased_from": v.ID})}); err != nil {
					return nil, err
				}
				return Result{m.ID, nv.ID, StateProposed}, nil
			}
			extra := map[string]any{"confirmed": v.ID, "baseline_changed": stale}
			if err := promote(c, m, v, reason, extra); err != nil {
				return nil, err
			}
			return Result{m.ID, v.ID, StateCurrent}, nil
		},
	}
}

func reject() command.Spec {
	return command.Spec{
		Name: "revision.reject", Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v rejectIn
			if err := decode(in, &v); err != nil {
				return authz.Object{}, err
			}
			o, _, _, err := revisionObject(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			var in2 rejectIn
			if err := decode(in, &in2); err != nil {
				return err
			}
			_, _, v, err := revisionObject(tx, in2.ID)
			if err != nil {
				return err
			}
			if v.State != StateProposed {
				return conflict("revision is already "+v.State, "re-read the material to see its current state")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var in rejectIn
			if err := decode(c.Input, &in); err != nil {
				return nil, err
			}
			_, m, v, err := revisionObject(c.Tx, in.ID)
			if err != nil {
				return nil, err
			}
			if err := setState(c.Tx, v.ID, StateSuperseded); err != nil {
				return nil, err
			}
			v.State = StateSuperseded
			if _, err := c.Emit(domain.Event{Type: EvRevisionRejected, Object: m.ID, Reason: in.Reason, Payload: snapshot(m, v, nil)}); err != nil {
				return nil, err
			}
			return Result{m.ID, v.ID, StateSuperseded}, nil
		},
	}
}

func revert() command.Spec {
	return command.Spec{
		Name: "material.revert", Action: authz.ActionChangeSettings, // Web 本人专属：delegate 得到 20，其余得到 4
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v revertIn
			if err := decode(in, &v); err != nil {
				return authz.Object{}, err
			}
			m, err := Get(tx, v.ID)
			return objectOf(m), err
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			var v revertIn
			if err := decode(in, &v); err != nil {
				return err
			}
			if blank(v.Reason) {
				return invalid("reason is required")
			}
			m, err := Get(tx, v.ID)
			if err != nil {
				return err
			}
			if m.Location != "" {
				return hostedConflict(m)
			}
			to, err := GetRevision(tx, v.To)
			if err != nil || to.Material != m.ID {
				return notFound("revision", v.To)
			}
			if to.State == StateProposed {
				return conflict("revision is still proposed; confirm or reject it instead", "use revision.confirm")
			}
			if to.ID == m.CurrentRev {
				return conflict("revision is already current", "pick an older revision")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var in revertIn
			if err := decode(c.Input, &in); err != nil {
				return nil, err
			}
			m, err := Get(c.Tx, in.ID)
			if err != nil {
				return nil, err
			}
			to, err := GetRevision(c.Tx, in.To)
			if err != nil {
				return nil, err
			}
			nv, err := addRevision(c, m.ID, to.Content, in.Reason, m.CurrentRev, StateSuperseded)
			if err != nil {
				return nil, err
			}
			if err := promote(c, m, nv, in.Reason, map[string]any{"reverted_to": to.ID}); err != nil {
				return nil, err
			}
			return Result{m.ID, nv.ID, StateCurrent}, nil
		},
	}
}

type summarizeIn struct {
	ID      string `json:"id"` // 修订 id
	Summary string `json:"summary"`
}

// summarize 写入修订的变更摘要（内核起草后以 system 身份调用，没有 HTTP 入口）。只写一次：已有摘要返回冲突。
// 摘要不改修订内容与状态，也不触发生效。
func summarize() command.Spec {
	parse := func(in json.RawMessage) (summarizeIn, error) {
		var v summarizeIn
		return v, decode(in, &v)
	}
	return command.Spec{
		Name: "revision.summarize", Action: authz.ActionReviseMaterial,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			o, _, _, err := revisionObject(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := parse(in)
			if err != nil {
				return err
			}
			if blank(v.Summary) {
				return invalid("summary is required")
			}
			_, _, r, err := revisionObject(tx, v.ID)
			if err != nil {
				return err
			}
			if r.Summary != "" {
				return conflict("revision already has a summary", "nothing to do")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			_, m, r, err := revisionObject(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			r.Summary = strings.TrimSpace(v.Summary)
			if _, err := c.Tx.Exec("UPDATE revisions SET summary = ? WHERE id = ?", r.Summary, r.ID); err != nil {
				return nil, err
			}
			if _, err := c.Emit(domain.Event{Type: EvRevisionSummarized, Object: m.ID, Reason: "revision summary drafted",
				Payload: snapshot(m, r, nil)}); err != nil {
				return nil, err
			}
			return Result{m.ID, r.ID, r.State}, nil
		},
	}
}

// Purge 物理删除材料及其全部修订（设计 §6.15），返回这些修订正文引用过的 blob 引用串（apa://blob/<hash>，已去重）。
// 材料不存在返回 not_found。事件墓碑与 blob 清理由 kernel/purge 在同一事务内完成。
func Purge(tx *store.Tx, id string) ([]string, error) {
	if _, err := Get(tx, id); err != nil {
		return nil, err
	}
	revs, err := History(tx, id)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, v := range revs {
		for rest := v.Content; ; {
			i := strings.Index(rest, blobRef)
			if i < 0 {
				break
			}
			rest = rest[i:]
			if len(rest) < len(blobRef)+64 {
				break
			}
			// 只收录后 64 位全为小写十六进制的串；其余是普通文本，跳过。
			if ref := rest[:len(blobRef)+64]; lowerHex(ref[len(blobRef):]) && !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
			rest = rest[len(blobRef):]
		}
	}
	if _, err := tx.Exec(`DELETE FROM revisions WHERE material = ?`, id); err != nil {
		return nil, err
	}
	_, err = tx.Exec(`DELETE FROM materials WHERE id = ?`, id)
	return refs, err
}

// blobRef 与 kernel/blobs.RefPrefix 一致；本包不依赖 blobs，引用格式是正文里的稳定约定。
const blobRef = "apa://blob/"

// BlobReferenced 报告是否仍有修订正文引用 ref（apa://blob/<hash>）。
func BlobReferenced(tx *store.Tx, ref string) (bool, error) {
	r, err := tx.Query(`SELECT 1 FROM revisions WHERE instr(content, ?) > 0 LIMIT 1`, ref)
	if err != nil {
		return false, err
	}
	defer r.Close()
	found := r.Next()
	return found, r.Err()
}

func lowerHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
