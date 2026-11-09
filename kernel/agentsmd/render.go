// Package agentsmd 是连接模块里的 AGENTS.md 渲染与漂移判定（设计 §12.5、§12.7）。
//
// 唯一源是 Guideline 材料（全局一份加每项目一份）；仓库根目录的 AGENTS.md 是整个文件归 APA 的渲染产物。
// 本包只做四件事：
//
//   - 渲染：按仓库用 agents-md 包的模板渲染，hash 不含头部标记行，每次新渲染的 hash 记入渲染记录（agents.render）；
//   - 判定：工作区文件 hash 与渲染记录比对，得到一致、过期、漂移三态；漂移时生成带 diff 的 needs_input，
//     同一仓库同一渲染 hash 的过期只提示一次（agents.check）；
//   - 取舍：合回（diff 变成 Guideline 的 proposed 修订，确认后重新渲染）、覆盖（之后 sync 才许写入）、本次忽略（agents.resolve、Service.Resolve）；
//   - 触发：仓库默认分支上 AGENTS.md 相关提交与 check 走同一条判定（Service.Trigger）。
//
// 内核不触达各机器的文件系统：文件内容由该机 CLI 随 check 上报，写入也由 CLI 做（agents.sync 只回应该写什么）。
// 规范只是渲染进文件的文字，不进入授权判定：web_only、白名单等内核规则不读 Guideline，所以规范放宽不了它们。
// 材料修订、决定与事项都经各自模块的命令或导出入口，本包只写自己的三张表。
package agentsmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// EvRendered 记一次新的渲染结果。它不在对外稳定类型表内（§10.1），只供审计与回放。
const EvRendered = "agents_md.rendered"

// FileName 是仓库根目录下被渲染的唯一文件。
const FileName = "AGENTS.md"

// 头部标记行都以此开头；hash 不含这些行。
const headerPrefix = "<!-- apa:"

// 模板占位符（附录 C.9），渲染时恰好各替换一次。
var placeholders = []string{"managed_header", "global_summary", "project_sections", "design_doc_index", "fixed_guidance"}

const fixedGuidance = "## 协作约定\n\n设计文档随代码改，走分支与 PR；需求、纪要等项目材料经 `apa context` 查阅。"

// Module 是本模块在存储层登记的迁移。
//   - agentsmd_render：渲染记录，(仓库, hash) 唯一；seq 最大的是最新渲染，规范改回旧内容时旧记录重新成为最新。
//   - agentsmd_check：每个仓库最近一次检查，以及已经提示过的过期 hash（同一 hash 只提示一次）。
//   - agentsmd_drift：每个 (仓库, 文件 hash, 渲染 hash) 至多一条漂移登记，对应一个 needs_input 事项。
var Module = store.Module{Name: "agentsmd", Migrations: []store.Migration{{
	Version: 1, Name: "create-agentsmd",
	SQL: `CREATE TABLE agentsmd_render (
	repo TEXT NOT NULL,
	hash TEXT NOT NULL,
	project TEXT NOT NULL,
	content TEXT NOT NULL,
	body TEXT NOT NULL,
	head TEXT NOT NULL,
	tail TEXT NOT NULL,
	sources TEXT NOT NULL,
	seq INTEGER NOT NULL,
	rendered_at INTEGER NOT NULL,
	PRIMARY KEY (repo, hash)
) WITHOUT ROWID;
CREATE TABLE agentsmd_check (
	repo TEXT PRIMARY KEY,
	state TEXT NOT NULL,
	file_hash TEXT NOT NULL,
	render_hash TEXT NOT NULL,
	host TEXT NOT NULL,
	checked_at INTEGER NOT NULL,
	prompted_hash TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE TABLE agentsmd_drift (
	item TEXT PRIMARY KEY,
	repo TEXT NOT NULL,
	file_hash TEXT NOT NULL,
	render_hash TEXT NOT NULL,
	content TEXT NOT NULL,
	state TEXT NOT NULL,
	UNIQUE (repo, file_hash, render_hash)
) WITHOUT ROWID;`,
}}}

// Render 是一条渲染记录。Content 是完整文件（含头部标记），Body 是 hash 的输入。
type Render struct {
	Repo       string `json:"repo"`
	Hash       string `json:"hash"`
	Project    string `json:"project"`
	Content    string `json:"content"`
	Sources    string `json:"sources"` // 来源修订 id，逗号分隔
	RenderedAt int64  `json:"rendered_at"`
	body       string
	head, tail string
}

// normalize 让渲染与文件比对不受行尾空白、首尾空行和连续空行数量影响。
func normalize(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// stripHeader 去掉头部标记行。
func stripHeader(file string) string {
	var keep []string
	for _, l := range strings.Split(strings.ReplaceAll(file, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), headerPrefix) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// FileBody 与 FileHash 是工作区文件参与比对的部分：不含头部标记行，所以只改头部不算漂移。
func FileBody(file string) string { return normalize(stripHeader(file)) }
func FileHash(file string) string { return sum(FileBody(file)) }

// dropEmptySections 去掉没有内容的 "## " 小节，保留其余文字原样（合回时能无损还原）。
func dropEmptySections(s string) string {
	var secs [][]string // 第 0 段是第一个小节标题之前的前言
	secs = append(secs, nil)
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "## ") {
			secs = append(secs, nil)
		}
		secs[len(secs)-1] = append(secs[len(secs)-1], l)
	}
	var out []string
	for i, sec := range secs {
		body := sec
		if i > 0 {
			body = sec[1:] // 标题行不算内容
		}
		if strings.TrimSpace(strings.Join(body, "")) != "" {
			out = append(out, sec...)
		}
	}
	return normalize(strings.Join(out, "\n"))
}

// guideline 读一个范围（project 为空是全局）的规范当前修订；没有规范或尚未生效返回空。
func guideline(tx *store.Tx, project string) (content, rev string, err error) {
	ms, err := materials.List(tx, project)
	if err != nil {
		return "", "", err
	}
	for _, m := range ms {
		if m.Kind != materials.KindGuideline || m.Project != project || m.Location != "" || m.CurrentRev == "" {
			continue
		}
		_, v, err := materials.Show(tx, m.ID, "")
		return v.Content, v.ID, err
	}
	return "", "", nil
}

// designIndex 只列本仓库托管的、路径仍在默认分支上的文档。
func designIndex(tx *store.Tx, project, repo string) (string, error) {
	es, err := hosted.List(tx, project)
	if err != nil {
		return "", err
	}
	var rows []string
	for _, e := range es {
		if projects.SameRepo(e.Repo, repo) && !e.Missing {
			rows = append(rows, fmt.Sprintf("- %s：`%s`", e.Material.Title, e.Path))
		}
	}
	if len(rows) == 0 {
		return "", nil
	}
	sort.Strings(rows)
	return "## 设计文档索引\n\n" + strings.Join(rows, "\n"), nil
}

// compose 按模板渲染。头部带 hash，所以先在头部为空时算 hash，再填头部。
func compose(tpl string, tx *store.Tx, project, repo string) (Render, error) {
	gc, grev, err := guideline(tx, "")
	if err != nil {
		return Render{}, err
	}
	pc, prev, err := guideline(tx, project)
	if err != nil {
		return Render{}, err
	}
	idx, err := designIndex(tx, project, repo)
	if err != nil {
		return Render{}, err
	}
	global := dropEmptySections(gc)
	if global != "" {
		global = "## 全局规范\n\n" + global
	}
	proj := dropEmptySections(pc)
	tail := normalize(idx + "\n\n" + fixedGuidance)
	body := normalize(strings.NewReplacer("{managed_header}", "", "{global_summary}", global, "{project_sections}", proj,
		"{design_doc_index}", idx, "{fixed_guidance}", fixedGuidance).Replace(tpl))
	var srcs []string
	for _, r := range []string{grev, prev} {
		if r != "" {
			srcs = append(srcs, r)
		}
	}
	r := Render{Repo: repo, Project: project, Sources: strings.Join(srcs, ","), body: body, head: normalize(global), tail: tail}
	r.Hash = sum(body)
	src := r.Sources
	if src == "" {
		src = "none"
	}
	header := headerPrefix + "managed rev=" + src + " hash=" + r.Hash + " -->\n" + headerPrefix + "notice 本文件由 APA 渲染，不要手改；请修改项目规范 -->"
	r.Content = header + "\n\n" + body + "\n"
	return r, nil
}

// renderRepo 渲染一个仓库并记账：结果与最新记录相同时不动；与旧记录相同时让旧记录重新成为最新；否则新增并记事件。
func renderRepo(c *command.Ctx, tpl, project, repo string) (Render, bool, error) {
	r, err := compose(tpl, c.Tx, project, repo)
	if err != nil {
		return r, false, err
	}
	latest, ok, err := latestRender(c.Tx, repo)
	if err != nil {
		return r, false, err
	}
	if ok && latest.Hash == r.Hash {
		return latest, false, nil
	}
	now := c.Now().UnixNano()
	n, err := c.Tx.Exec("UPDATE agentsmd_render SET seq = (SELECT COALESCE(MAX(seq), 0) + 1 FROM agentsmd_render WHERE repo = ?), project = ?, rendered_at = ? WHERE repo = ? AND hash = ?",
		repo, project, now, repo, r.Hash)
	if err != nil {
		return r, false, err
	}
	if n == 0 {
		if _, err := c.Tx.Exec("INSERT INTO agentsmd_render (repo, hash, project, content, body, head, tail, sources, seq, rendered_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM agentsmd_render WHERE repo = ?), ?)",
			repo, r.Hash, project, r.Content, r.body, r.head, r.tail, r.Sources, repo, now); err != nil {
			return r, false, err
		}
	}
	r.RenderedAt = now
	p := fmt.Sprintf(`{"repo":%q,"hash":%q,"project":%q,"sources":%q}`, repo, r.Hash, project, r.Sources)
	_, err = c.Emit(domain.Event{Type: EvRendered, Object: project, Reason: "AGENTS.md rendered", Payload: []byte(p)})
	return r, true, err
}

const renderCols = "repo, hash, project, content, body, head, tail, sources, rendered_at"

func scanRender(r *store.Rows) (Render, error) {
	var x Render
	return x, r.Scan(&x.Repo, &x.Hash, &x.Project, &x.Content, &x.body, &x.head, &x.tail, &x.Sources, &x.RenderedAt)
}

func queryRender(tx *store.Tx, where string, args ...any) (Render, bool, error) {
	r, err := tx.Query("SELECT "+renderCols+" FROM agentsmd_render WHERE "+where, args...)
	if err != nil {
		return Render{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return Render{}, false, r.Err()
	}
	x, err := scanRender(r)
	return x, err == nil, err
}

func latestRender(tx *store.Tx, repo string) (Render, bool, error) {
	return queryRender(tx, "repo = ? ORDER BY seq DESC LIMIT 1", repo)
}

func renderByHash(tx *store.Tx, repo, hash string) (Render, bool, error) {
	return queryRender(tx, "repo = ? AND hash = ?", repo, hash)
}

// mergedProject 把漂移文件还原成项目规范的新正文：去掉渲染里属于全局规范的开头和索引、固定指引的结尾，余下就是项目部分。
// 带 managed 头部的文件是从渲染改出来的：开头或结尾对不上说明手改落在了全局规范或生成段，合回不到项目规范，返回错误。
// 没有头部的手写文件整份当作项目规范，由本人在 diff 里取舍；它只会成为 proposed 修订，不会直接生效。
func mergedProject(rec Render, file string) (string, error) {
	b := FileBody(file)
	if strings.Contains(file, headerPrefix+"managed") {
		rest, okHead := strings.CutPrefix(b, rec.head)
		rest, okTail := strings.CutSuffix(rest, rec.tail)
		if !okHead || !okTail {
			return "", invalid("the hand edit is outside the project guideline (global rules or generated sections): change it in the guideline it comes from")
		}
		return strings.TrimSpace(rest), nil
	}
	return strings.TrimSpace(b), nil
}

// diffText 只列增删行，供事项正文展示：`-` 是渲染里有而文件里没有的，`+` 是文件里新增的。
func diffText(from, to string) string {
	var b strings.Builder
	b.WriteString("```diff\n")
	for _, d := range materials.Diff(from, to) {
		if d.Op != " " {
			b.WriteString(d.Op + " " + d.Text + "\n")
		}
	}
	b.WriteString("```")
	return b.String()
}

// isFile 判断 p 是否仓库根目录的 AGENTS.md。
func isFile(p string) bool { return path.Clean(strings.TrimPrefix(p, "/")) == FileName }
