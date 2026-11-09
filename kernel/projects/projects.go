// Package projects 是项目与 repos 属性（设计 §6.5、§12.1、§14.7）：持久化、命令、以及
// "当前目录到项目"的映射（§7.6）。所有写入经命令模型登记的命令，授权与事件由管线负责。
//
// repos 不是独立实体，作为项目行内的 JSON 列表存储；allowed_executors 三态
// （未设置 / 空列表 / 名单）以 JSON 文本存储，"null" 即未设置，保证读回与写入一致。
package projects

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "projects", Migrations: []store.Migration{{
	Version: 1, Name: "create-projects",
	SQL: `CREATE TABLE projects (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	intro TEXT NOT NULL,
	tags TEXT NOT NULL,
	status TEXT NOT NULL,
	repos TEXT NOT NULL,
	allowed_executors TEXT NOT NULL,
	capture INTEGER NOT NULL
) WITHOUT ROWID;`,
}}}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func notFound(id string) error {
	return domain.NewError(domain.CodeNotFound, "project "+id+" does not exist", "list projects and retry")
}

func conflict(why string) error {
	return domain.NewError(domain.CodeConflict, why, "re-read the project and retry")
}

// ---- 读 ----

const cols = "id, name, intro, tags, status, repos, allowed_executors, capture"

func scan(r *store.Rows) (domain.Project, error) {
	var p domain.Project
	var tags, status, repos, allowed string
	if err := r.Scan(&p.ID, &p.Name, &p.Intro, &tags, &status, &repos, &allowed, &p.Capture); err != nil {
		return p, err
	}
	p.Status = domain.ProjectStatus(status)
	for _, f := range []struct {
		raw string
		dst any
	}{{tags, &p.Tags}, {repos, &p.Repos}, {allowed, &p.AllowedExecutors}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return p, fmt.Errorf("projects: corrupt row %s: %w", p.ID, err)
		}
	}
	return p, nil
}

func query(tx *store.Tx, where string, args ...any) ([]domain.Project, error) {
	r, err := tx.Query("SELECT "+cols+" FROM projects "+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []domain.Project
	for r.Next() {
		p, err := scan(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, r.Err()
}

// Get 读取一个项目；不存在返回 not_found。
func Get(tx *store.Tx, id string) (domain.Project, error) {
	ps, err := query(tx, "WHERE id = ?", id)
	if err != nil {
		return domain.Project{}, err
	}
	if len(ps) == 0 {
		return domain.Project{}, notFound(id)
	}
	return ps[0], nil
}

// List 按名称（再按 id）排序返回全部项目。
func List(tx *store.Tx) ([]domain.Project, error) {
	return query(tx, "ORDER BY name, id")
}

// ByRemote 按仓库身份找到所属项目（来源事件归属，§6.5）。仓库全局唯一，所以至多一个。
func ByRemote(tx *store.Tx, remote string) (domain.Project, bool, error) {
	p, _, ok, err := RepoOf(tx, remote)
	return p, ok, err
}

// RepoOf 按仓库身份找到所属项目与登记项。登记项的 Remote 是这个仓库在账本里的唯一写法。
func RepoOf(tx *store.Tx, remote string) (domain.Project, domain.Repo, bool, error) {
	ps, err := List(tx)
	if err != nil {
		return domain.Project{}, domain.Repo{}, false, err
	}
	p, r, ok := Find(ps, remote)
	return p, r, ok, nil
}

func save(tx *store.Tx, p domain.Project, insert bool) error {
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	verb := "INSERT"
	if !insert {
		verb = "INSERT OR REPLACE"
	}
	_, err := tx.Exec(verb+" INTO projects ("+cols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		p.ID, p.Name, p.Intro, enc(p.Tags), string(p.Status), enc(p.Repos), enc(p.AllowedExecutors), p.Capture)
	return err
}

// ---- 运行的工作目录（附录 C.7 的 cwd） ----

// WorkDir 是运行 cwd 取值的唯一解析，任务型与后台运行共用。cwd=project_checkout 时返回项目在 host 上的检出路径，
// 没有时 refuse 给出不拉起的原因；其他取值 dir 为空，运行在自己的运行目录。
func WorkDir(cwd string, p *domain.Project, host string) (dir, refuse string) {
	if cwd != "project_checkout" {
		return "", ""
	}
	if p != nil {
		if c := p.LocalCheckout(host); c != nil {
			return c.Path, ""
		}
	}
	return "", "项目在本机（" + host + "）没有可用的检出目录"
}

// ---- 当前目录到项目的映射（§7.6） ----

// Scope 是映射的输入。Host 是本机主机名，由调用方提供。
type Scope struct {
	Flag string // --project
	Env  string // APA_PROJECT
	Cwd  string
	Host string
}

// Resolve 返回作用域项目 id，空串表示无项目作用域。顺序：显式指定（Flag，再 Env）→
// 本机检出的最长前缀 → 不猜。显式指定的项目不存在是错误，不回退；同一最长前缀命中多个项目也是错误。
func Resolve(tx *store.Tx, s Scope) (string, error) {
	for _, id := range []string{s.Flag, s.Env} {
		if id != "" {
			p, err := Get(tx, id)
			return p.ID, err
		}
	}
	if s.Cwd == "" {
		return "", nil
	}
	if !filepath.IsAbs(s.Cwd) {
		return "", invalid("current directory must be an absolute path")
	}
	cwd := filepath.Clean(s.Cwd)
	ps, err := List(tx)
	if err != nil {
		return "", err
	}
	best, owners := -1, map[string]bool{}
	for _, p := range ps {
		for _, r := range p.Repos {
			for _, c := range r.Checkouts {
				if c.Host != s.Host || !inside(cwd, c.Path) {
					continue
				}
				switch n := len(c.Path); {
				case n > best:
					best, owners = n, map[string]bool{p.ID: true}
				case n == best:
					owners[p.ID] = true
				}
			}
		}
	}
	switch len(owners) {
	case 0:
		return "", nil
	case 1:
		for id := range owners {
			return id, nil
		}
	}
	return "", conflict("current directory matches checkouts of more than one project; pass --project")
}

func inside(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// ---- 命令 ----

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "prj_" + hex.EncodeToString(b), nil
}

type ref struct {
	ID string `json:"id"`
}

type createIn struct {
	Name    string               `json:"name"`
	Intro   string               `json:"intro"`
	Tags    []string             `json:"tags"`
	Status  domain.ProjectStatus `json:"status"`
	Capture bool                 `json:"capture"`
}

type updateIn struct {
	ID      string                `json:"id"`
	Name    *string               `json:"name"`
	Intro   *string               `json:"intro"`
	Tags    *[]string             `json:"tags"`
	Status  *domain.ProjectStatus `json:"status"`
	Capture *bool                 `json:"capture"`
}

type repoIn struct {
	ID     string `json:"id"`
	Remote string `json:"remote"`
}

type checkoutIn struct {
	ID     string `json:"id"`
	Remote string `json:"remote"`
	Host   string `json:"host"`
	Path   string `json:"path"`
}

type executorsIn struct {
	ID        string          `json:"id"`
	Executors json.RawMessage `json:"executors"` // 必须出现：null 为不限制，[] 为只能指派给本人
}

func validStatus(s domain.ProjectStatus) bool {
	return s == domain.ProjectActive || s == domain.ProjectPaused || s == domain.ProjectArchived
}

func validName(s string) error {
	if strings.TrimSpace(s) == "" {
		return invalid("name is required")
	}
	return nil
}

func validTags(ts []string) error {
	for _, t := range ts {
		if strings.TrimSpace(t) == "" {
			return invalid("tags must not be blank")
		}
	}
	return nil
}

func validRemote(s string) error {
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return invalid("remote must be a non-empty address without whitespace")
	}
	return nil
}

func validCheckout(c domain.Checkout) error {
	if strings.TrimSpace(c.Host) == "" {
		return invalid("host is required")
	}
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path {
		return invalid("path must be an absolute, clean path")
	}
	return nil
}

// objectOf 为已存在的项目命令解析授权对象；id 必须存在。
func objectOf[T any](id func(T) string) func(*store.Tx, json.RawMessage) (authz.Object, error) {
	return func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
		var v T
		if err := decode(in, &v); err != nil {
			return authz.Object{}, err
		}
		if id(v) == "" {
			return authz.Object{}, invalid("project id is required")
		}
		if _, err := Get(tx, id(v)); err != nil {
			return authz.Object{}, err
		}
		return authz.Object{ID: id(v)}, nil
	}
}

func snapshot(op string, p domain.Project) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"op": op, "project": p})
	return b
}

// edit 读出项目、交给 fn 修改、写回并记 project.updated。
func edit(c *command.Ctx, op string, fn func(*domain.Project) error) (any, error) {
	p, err := Get(c.Tx, c.Object.ID)
	if err != nil {
		return nil, err
	}
	if err := fn(&p); err != nil {
		return nil, err
	}
	if err := save(c.Tx, p, false); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: domain.EvProjectUpdated, Object: p.ID, Payload: snapshot(op, p)}); err != nil {
		return nil, err
	}
	return ref{p.ID}, nil
}

func repoIndex(p *domain.Project, remote string) int {
	_, r, ok := Find([]domain.Project{*p}, remote)
	if !ok {
		return -1
	}
	return slices.IndexFunc(p.Repos, func(x domain.Repo) bool { return x.Remote == r.Remote })
}

// repoOwner 返回已登记了同一仓库的项目。比 Find 严：简写同时对上多个站点时，新增也算冲突。
func repoOwner(tx *store.Tx, remote string) (string, error) {
	ps, err := List(tx)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if slices.ContainsFunc(p.Repos, func(r domain.Repo) bool { return SameRepo(r.Remote, remote) }) {
			return p.ID, nil
		}
	}
	return "", nil
}

// Register 在命令执行器上登记本模块的全部命令。
//
// allowed_executors 只有 project.allowed_executors.set 一条写入路径，它要求 change_settings
// （Web 本人）；delegate 得到退出码 20 并留拒绝事件，其余级别得到退出码 4（§14.7）。
func Register(x *command.Executor) error {
	specs := []command.Spec{
		{Name: "project.create", Action: authz.ActionManageProject, Tier: authz.TierWords,
			Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
				var v createIn
				return authz.Object{}, decode(in, &v)
			},
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				var v createIn
				_ = decode(in, &v)
				if v.Status != "" && !validStatus(v.Status) {
					return invalid("unknown status " + string(v.Status))
				}
				if err := validName(v.Name); err != nil {
					return err
				}
				return validTags(v.Tags)
			},
			Apply: func(c *command.Ctx) (any, error) {
				var v createIn
				_ = decode(c.Input, &v)
				id, err := newID()
				if err != nil {
					return nil, err
				}
				if v.Status == "" {
					v.Status = domain.ProjectActive
				}
				p := domain.Project{ID: id, Name: v.Name, Intro: v.Intro, Tags: v.Tags, Status: v.Status, Capture: v.Capture}
				if err := save(c.Tx, p, true); err != nil {
					return nil, err
				}
				if _, err := c.Emit(domain.Event{Type: domain.EvProjectUpdated, Object: id, Payload: snapshot("create", p)}); err != nil {
					return nil, err
				}
				return ref{id}, nil
			}},
		{Name: "project.update", Action: authz.ActionManageProject, Tier: authz.TierWords,
			Object: objectOf(func(v updateIn) string { return v.ID }),
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				var v updateIn
				_ = decode(in, &v)
				if v.Name == nil && v.Intro == nil && v.Tags == nil && v.Status == nil && v.Capture == nil {
					return invalid("nothing to update")
				}
				if v.Name != nil {
					if err := validName(*v.Name); err != nil {
						return err
					}
				}
				if v.Status != nil && !validStatus(*v.Status) {
					return invalid("unknown status " + string(*v.Status))
				}
				if v.Tags != nil {
					return validTags(*v.Tags)
				}
				return nil
			},
			Apply: func(c *command.Ctx) (any, error) {
				var v updateIn
				_ = decode(c.Input, &v)
				return edit(c, "update", func(p *domain.Project) error {
					if v.Name != nil {
						p.Name = *v.Name
					}
					if v.Intro != nil {
						p.Intro = *v.Intro
					}
					if v.Tags != nil {
						p.Tags = *v.Tags
					}
					if v.Status != nil {
						p.Status = *v.Status
					}
					if v.Capture != nil {
						p.Capture = *v.Capture
					}
					return nil
				})
			}},
		{Name: "project.repo.add", Action: authz.ActionManageProject, Tier: authz.TierWords,
			Object: objectOf(func(v repoIn) string { return v.ID }),
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				var v repoIn
				_ = decode(in, &v)
				return validRemote(v.Remote)
			},
			Apply: func(c *command.Ctx) (any, error) {
				var v repoIn
				_ = decode(c.Input, &v)
				if owner, err := repoOwner(c.Tx, v.Remote); err != nil {
					return nil, err
				} else if owner != "" {
					return nil, conflict("remote is already registered in project " + owner)
				}
				return edit(c, "repo.add", func(p *domain.Project) error {
					p.Repos = append(p.Repos, domain.Repo{Remote: v.Remote})
					return nil
				})
			}},
		{Name: "project.repo.remove", Action: authz.ActionManageProject, Tier: authz.TierWords,
			Object: objectOf(func(v repoIn) string { return v.ID }),
			Apply: func(c *command.Ctx) (any, error) {
				var v repoIn
				_ = decode(c.Input, &v)
				return edit(c, "repo.remove", func(p *domain.Project) error {
					i := repoIndex(p, v.Remote)
					if i < 0 {
						return domain.NewError(domain.CodeNotFound, "remote is not registered in this project", "list the project's repos")
					}
					p.Repos = slices.Delete(p.Repos, i, i+1)
					return nil
				})
			}},
		{Name: "project.checkout.set", Action: authz.ActionEditCheckout, Tier: authz.TierWork,
			Object: objectOf(func(v checkoutIn) string { return v.ID }),
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				var v checkoutIn
				_ = decode(in, &v)
				return validCheckout(domain.Checkout{Host: v.Host, Path: v.Path})
			},
			Apply: func(c *command.Ctx) (any, error) {
				var v checkoutIn
				_ = decode(c.Input, &v)
				return edit(c, "checkout.set", func(p *domain.Project) error {
					i := repoIndex(p, v.Remote)
					if i < 0 {
						return domain.NewError(domain.CodeNotFound, "remote is not registered in this project", "add the remote first")
					}
					co := domain.Checkout{Host: v.Host, Path: v.Path}
					if !slices.Contains(p.Repos[i].Checkouts, co) {
						p.Repos[i].Checkouts = append(p.Repos[i].Checkouts, co)
					}
					return nil
				})
			}},
		{Name: "project.checkout.remove", Action: authz.ActionEditCheckout, Tier: authz.TierWork,
			Object: objectOf(func(v checkoutIn) string { return v.ID }),
			Apply: func(c *command.Ctx) (any, error) {
				var v checkoutIn
				_ = decode(c.Input, &v)
				return edit(c, "checkout.remove", func(p *domain.Project) error {
					i := repoIndex(p, v.Remote)
					if i >= 0 {
						co := domain.Checkout{Host: v.Host, Path: v.Path}
						if j := slices.Index(p.Repos[i].Checkouts, co); j >= 0 {
							p.Repos[i].Checkouts = slices.Delete(p.Repos[i].Checkouts, j, j+1)
							return nil
						}
					}
					return domain.NewError(domain.CodeNotFound, "checkout is not registered in this project", "list the project's repos")
				})
			}},
		{Name: "project.allowed_executors.set", Action: authz.ActionChangeSettings,
			Object: objectOf(func(v executorsIn) string { return v.ID }),
			Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
				_, err := parseExecutors(in)
				return err
			},
			Apply: func(c *command.Ctx) (any, error) {
				next, _ := parseExecutors(c.Input)
				p, err := Get(c.Tx, c.Object.ID)
				if err != nil {
					return nil, err
				}
				before := p.AllowedExecutors
				p.AllowedExecutors = next
				if err := save(c.Tx, p, false); err != nil {
					return nil, err
				}
				payload, _ := json.Marshal(map[string]any{"before": before, "after": next})
				if _, err := c.Emit(domain.Event{Type: domain.EvProjectAllowedExecutorsChanged, Object: p.ID, Payload: payload}); err != nil {
					return nil, err
				}
				return ref{p.ID}, nil
			}},
	}
	for _, s := range specs {
		if err := x.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// parseExecutors 把 executors 字段落成三态：null → nil（不限制），[] → 空非 nil，名单 → 去重后的名单。
func parseExecutors(in json.RawMessage) ([]string, error) {
	var v executorsIn
	if err := decode(in, &v); err != nil {
		return nil, err
	}
	if len(v.Executors) == 0 {
		return nil, invalid("executors is required: null for unrestricted, [] for owner only")
	}
	var ids []string
	if err := json.Unmarshal(v.Executors, &ids); err != nil {
		return nil, invalid("executors must be null or a list of strings")
	}
	if ids == nil {
		return nil, nil
	}
	out := []string{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, invalid("executor ids must not be blank")
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}
