package materials

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"golang.org/x/text/unicode/norm"
)

// 材料的形态（§6.6）：托管型、自有型、链接型。表里的 form 为空的是升级前的材料，读取时按原规则推定，不改写。
const (
	FormHosted = "hosted"
	FormOwned  = "owned"
	FormLink   = "link"
)

// KindReference 是链接型材料缺省的 kind。
const KindReference = "reference"

// VaultPrefix 是笔记库内位置的写法：vault:<库名>/<库内相对路径>。
const VaultPrefix = "vault:"

// NormalizeLocation 校验并规整链接型材料的位置（§12.8）：只接受 vault:<已登记库名>/<库内相对路径> 或 http(s) 网址，
// 其余（本机绝对路径、file://）一律拒绝。库内路径用正斜杠、不含空段与 . 、..，按 Unicode 规范化形式（NFC）比较；
// 网址的协议与主机名转小写。内核不检查位置是否仍然有效。
func NormalizeLocation(loc string, vaults []policy.NoteVault) (string, error) {
	loc = strings.TrimSpace(loc)
	if loc == "" || strings.IndexFunc(loc, func(r rune) bool { return unicode.IsSpace(r) && r != ' ' || unicode.IsControl(r) }) >= 0 {
		return "", invalid("link is required and must not contain control characters")
	}
	if rest, ok := strings.CutPrefix(loc, VaultPrefix); ok {
		name, path, found := strings.Cut(rest, "/")
		name, path = norm.NFC.String(name), norm.NFC.String(path)
		if !found || name == "" || path == "" {
			return "", invalid("a vault link is written vault:<vault>/<path inside the vault>")
		}
		if !slices.ContainsFunc(vaults, func(v policy.NoteVault) bool { return v.Name == name }) {
			return "", invalid("note vault " + name + " is not registered; the owner registers vaults in Web settings")
		}
		for _, seg := range strings.Split(path, "/") {
			if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "\\") {
				return "", invalid("a path inside a vault uses forward slashes and no empty, . or .. segments")
			}
		}
		return VaultPrefix + name + "/" + path, nil
	}
	lower := strings.ToLower(loc)
	for _, scheme := range []string{"http://", "https://"} {
		if !strings.HasPrefix(lower, scheme) {
			continue
		}
		u, err := url.Parse(loc)
		if err != nil || u.Hostname() == "" {
			return "", invalid("link is not a valid http(s) URL")
		}
		rest := loc[len(scheme):]
		auth, tail := rest, ""
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			auth, tail = rest[:i], rest[i:]
		}
		userinfo := ""
		if i := strings.LastIndex(auth, "@"); i >= 0 {
			userinfo, auth = auth[:i+1], auth[i+1:]
		}
		return scheme + userinfo + strings.ToLower(auth) + tail, nil
	}
	return "", invalid("a link is either vault:<vault>/<path> or an http(s) URL; local paths and file:// are not accepted")
}

// OpenURL 返回链接型材料的"在笔记应用打开"地址；库已登记打开方式时才有，网址型与已删除登记的库返回空串。
func OpenURL(p policy.Policy, m Material) string {
	rest, ok := strings.CutPrefix(m.Link, VaultPrefix)
	if m.Form != FormLink || !ok {
		return ""
	}
	name, path, _ := strings.Cut(rest, "/")
	return p.OpenURL(name, path)
}

// LinkIn 是 material.link 的输入。
type LinkIn struct {
	Link    string   `json:"link"`
	Title   string   `json:"title"`
	Project string   `json:"project"`
	Kind    string   `json:"kind"`
	Tags    []string `json:"tags"`
	Reason  string   `json:"reason"`
}

// LinkResult 是 material.link 的结果：同一项目内同一位置已登记时 Created 为假，返回已有的那条，不改它的标题与 kind。
type LinkResult struct {
	Material string `json:"material"`
	Created  bool   `json:"created"`
}

// resolveLink 校验登记请求并补缺省：project 必填且存在，kind 缺省 reference、不能是规范，位置按策略里的笔记库规整。
func resolveLink(tx *store.Tx, src PackSource, in LinkIn) (LinkIn, error) {
	if blank(in.Title) {
		return in, invalid("title is required")
	}
	if blank(in.Project) {
		return in, invalid("project is required for a linked material")
	}
	if _, err := projects.Get(tx, in.Project); err != nil {
		return in, err
	}
	if in.Kind == "" {
		in.Kind = KindReference
	}
	if in.Kind == KindGuideline {
		return in, invalid("a linked material cannot be a guideline")
	}
	if !slices.Contains(Kinds(src), in.Kind) {
		return in, invalid("unknown material kind " + in.Kind)
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return in, err
	}
	in.Link, err = NormalizeLocation(in.Link, pol.NoteVaults)
	return in, err
}

// LinkTx 在事务内登记一条链接型材料（位置须已规整）：同项目同位置已有则返回那一条（created 为假）。
// 授权与事件由调用方负责：material.link 命令与回执里库内链接的自动登记（kernel/delegation）共用它。
func LinkTx(tx *store.Tx, project, kind, title string, tags []string, loc string) (m Material, created bool, err error) {
	ms, err := queryMaterials(tx, "WHERE project = ? AND form = ? AND link_location = ?", project, FormLink, loc)
	if err != nil || len(ms) > 0 {
		if len(ms) > 0 {
			m = ms[0]
		}
		return m, false, err
	}
	id, err := newID("mat_")
	if err != nil {
		return m, false, err
	}
	if tags == nil {
		tags = []string{}
	}
	m = Material{ID: id, Project: project, Kind: kind, Title: strings.TrimSpace(title), Tags: tags, Form: FormLink, Link: loc}
	return m, true, insertMaterial(tx, m)
}

// LinkedPayload 是登记链接型材料时 material.revised 事件的载荷（链接型没有修订，所以只有登记本身）。
func LinkedPayload(m Material) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"material": m, "registered": true})
	return b
}

func link(src PackSource) command.Spec {
	parse := func(in json.RawMessage) (LinkIn, error) {
		var v LinkIn
		return v, decode(in, &v)
	}
	return command.Spec{
		Name: "material.link", Action: authz.ActionReviseMaterial, Tier: authz.TierWork,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{Kind: v.Kind, Project: v.Project, Tags: v.Tags}, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := parse(in)
			if err != nil {
				return err
			}
			_, err = resolveLink(tx, src, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			if v, err = resolveLink(c.Tx, src, v); err != nil {
				return nil, err
			}
			m, created, err := LinkTx(c.Tx, v.Project, v.Kind, v.Title, v.Tags, v.Link)
			if err != nil {
				return nil, err
			}
			if created {
				reason := v.Reason
				if blank(reason) {
					reason = "linked material " + m.Title
				}
				if _, err := c.Emit(domain.Event{Type: domain.EvMaterialRevised, Object: m.ID, Reason: reason, Payload: LinkedPayload(m)}); err != nil {
					return nil, err
				}
			}
			return LinkResult{Material: m.ID, Created: created}, nil
		},
	}
}
