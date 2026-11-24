// Package readscope 是来源内容的读取闸（设计 §14.2、§14.14）：外部钥匙读来源事件、事项、材料时给多少，只看钥匙自己的逐来源档位。
// Web 本人、内部钥匙、运行凭据与内核自己的入口不受档位限制；外部钥匙（audience=external，含全部 ext 钥匙）
// 只读 source_scopes 里列出的来源，不在表里即不给。一条内容出自多个来源时取最严。
// 本包只裁剪读出的副本，不回写账本（§6.14）。
package readscope

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 档位；None（空串）表示不给。
const (
	Full    = registry.ScopeFull
	Excerpt = registry.ScopeExcerpt
	Meta    = registry.ScopeMeta
	None    = ""
)

// ExcerptChars 与送外部端点的摘录同一长度。
const ExcerptChars = registry.ExcerptChars

var rank = map[string]int{None: 0, Meta: 1, Excerpt: 2, Full: 3}

// Strictest 取最严的档位；没有参数时为全文（内容不出自任何来源）。
func Strictest(ts ...string) string {
	out := Full
	for _, t := range ts {
		if rank[t] < rank[out] {
			out = t
		}
	}
	return out
}

// Denied 是外部钥匙够不到这条内容时的统一拒绝（退出码 4）。
func Denied() error {
	e := domain.NewError(domain.CodePermissionDenied, "this key is not given this source", "ask the owner to give this key the source when issuing or editing it in Web")
	e.Reason = domain.ReasonOutOfScope
	return e
}

// Reader 在一个事务里判定档位，缓存来源是否已登记。
type Reader struct {
	tx         *store.Tx
	c          command.Caller
	registered map[string]bool
}

// New 返回 c 的读取闸。
func New(tx *store.Tx, c command.Caller) *Reader {
	return &Reader{tx: tx, c: c, registered: map[string]bool{}}
}

// All 报告调用者是否不受档位限制（Web、运行、内部钥匙、内核入口）。
func (r *Reader) All() bool { return !r.c.External() }

// Tier 是调用者读来源 source 的档位。对话捕获只给 Web 本人，由调用方另行判定；外部钥匙在这里得到不给。
func (r *Reader) Tier(source string) string {
	if r.All() {
		return Full
	}
	if strings.HasPrefix(source, "capture:") {
		return None
	}
	return r.c.SourceScopes[source]
}

// source 报告 id 是否算一个来源：已登记的来源与对话捕获算；例行任务实例与没登记的名字（本人手建事项的旧伪来源）不算。
func (r *Reader) source(id string) (bool, error) {
	if id == "" || strings.HasPrefix(id, domain.OriginInstancePrefix) {
		return false, nil
	}
	if strings.HasPrefix(id, "capture:") {
		return true, nil
	}
	if ok, seen := r.registered[id]; seen {
		return ok, nil
	}
	_, err := registry.Get(r.tx, id)
	var de *domain.Error
	switch {
	case err == nil:
		r.registered[id] = true
	case errors.As(err, &de) && de.Code == domain.CodeNotFound:
		r.registered[id] = false
	default:
		return false, err
	}
	return r.registered[id], nil
}

// ItemSources 是事项内容的全部来源：origin、挂在它上面的来源事件、并入它的合并留痕。
func (r *Reader) ItemSources(it domain.Item) ([]string, error) {
	cands := []string{}
	if it.Origin != nil {
		cands = append(cands, it.Origin.Source)
	}
	evs, err := runner.SourcesOfItem(r.tx, it.ID)
	if err != nil {
		return nil, err
	}
	cands = append(cands, evs...)
	if it.Proposal != nil {
		for _, m := range it.Proposal.Merges {
			cands = append(cands, m.Source)
		}
		for _, a := range it.Proposal.Absorbed { // 折叠进来的建议的标题与 ref 也是内容
			cands = append(cands, a.Source)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range cands {
		if seen[id] {
			continue
		}
		seen[id] = true
		ok, err := r.source(id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// ItemTier 是调用者读事项的档位：全部来源里最严的一个；不出自任何来源按全文。
func (r *Reader) ItemTier(it domain.Item) (string, error) {
	if r.All() {
		return Full, nil
	}
	srcs, err := r.ItemSources(it)
	if err != nil {
		return None, err
	}
	ts := make([]string, len(srcs))
	for i, s := range srcs {
		ts[i] = r.Tier(s)
	}
	return Strictest(ts...), nil
}

// MaterialTier 是调用者读材料的档位：与材料有关系边的全部事项的来源里最严的一个；没有连任何出自来源的事项按全文。
// shortcut: 材料本身不记来源出处，Agent 把来源内容抄进一份没连事项的材料时不受限；要严格时给修订记出处。
func (r *Reader) MaterialTier(id string) (string, error) {
	if r.All() {
		return Full, nil
	}
	rels, err := items.Relations(r.tx, id)
	if err != nil {
		return None, err
	}
	ts := []string{}
	for _, rel := range rels {
		other := rel.From
		if other == id {
			other = rel.To
		}
		it, err := items.Get(r.tx, other)
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeNotFound {
			continue // 另一端不是事项（材料之间的边）
		}
		if err != nil {
			return None, err
		}
		t, err := r.ItemTier(it)
		if err != nil {
			return None, err
		}
		ts = append(ts, t)
	}
	return Strictest(ts...), nil
}

// Items 按档位过滤并裁剪一组事项：不给的去掉，其余按档位裁；scopes 只列被裁的（摘录、元信息）。
func (r *Reader) Items(its []domain.Item) (kept []domain.Item, scopes map[string]string, err error) {
	if r.All() {
		return its, nil, nil
	}
	kept = []domain.Item{}
	scopes = map[string]string{}
	for _, it := range its {
		t, err := r.ItemTier(it)
		if err != nil {
			return nil, nil, err
		}
		if t == None {
			continue
		}
		if t != Full {
			scopes[it.ID] = t
		}
		kept = append(kept, TrimItem(it, t))
	}
	return kept, scopes, nil
}

// Item 判定并裁剪单个事项；不给时返回 Denied。
func (r *Reader) Item(it domain.Item) (domain.Item, string, error) {
	t, err := r.ItemTier(it)
	if err != nil {
		return it, None, err
	}
	if t == None {
		return it, None, Denied()
	}
	return TrimItem(it, t), t, nil
}

// TrimItem 按档位裁事项：摘录留正文前 ExcerptChars 字；元信息去掉正文、摘要、建议理由与合并留痕的摘要，留标题与结构化字段。
func TrimItem(it domain.Item, tier string) domain.Item {
	switch tier {
	case Full:
		return it
	case Excerpt:
		it.Body = First(it.Body, ExcerptChars)
		return it
	}
	it.Body, it.Summary = "", ""
	if it.Proposal != nil {
		p := *it.Proposal
		p.Reason = ""
		p.Merges = append([]domain.ProposalMerge(nil), p.Merges...)
		for i := range p.Merges {
			p.Merges[i].Summary, p.Merges[i].Why = "", ""
		}
		it.Proposal = &p
	}
	return it
}

// TrimText 按档位裁一段正文（进展、修订内容）：摘录留前 ExcerptChars 字，元信息不留。
func TrimText(s, tier string) string {
	switch tier {
	case Full:
		return s
	case Excerpt:
		return First(s, ExcerptChars)
	}
	return ""
}

// TrimProgress 按档位裁事项的进展正文。
func TrimProgress(ps []domain.Progress, tier string) []domain.Progress {
	if tier == Full {
		return ps
	}
	out := make([]domain.Progress, len(ps))
	for i, p := range ps {
		p.Text = TrimText(p.Text, tier)
		out[i] = p
	}
	return out
}

// Messages 按各自来源的档位裁一组挂靠消息：不给的去掉；摘录留摘要与正文前 ExcerptChars 字；元信息只留标题、发件人与时间；
// 非全文时链接只留域名（与送外部端点同一裁法，registry.Trim）。
func (r *Reader) Messages(ms []followmsg.Message) []followmsg.Message {
	if r.All() {
		return ms
	}
	out := []followmsg.Message{}
	for _, m := range ms {
		if m, ok := r.Message(m); ok {
			out = append(out, m)
		}
	}
	return out
}

// Message 按来源档位裁一条挂靠消息；不给时返回 false。
func (r *Reader) Message(m followmsg.Message) (followmsg.Message, bool) {
	t := r.Tier(m.Source)
	if t == None {
		return m, false
	}
	if t == Full {
		return m, true
	}
	tr, _ := registry.Trim(domain.SourceEvent{Subject: m.Subject, Sender: m.Sender, Link: m.Link, Summary: m.Summary, Detail: m.Detail}, t)
	m.Subject, m.Sender, m.Link, m.Summary, m.Detail = tr.Subject, tr.Sender, tr.Link, tr.Summary, tr.Detail
	return m, true
}

// First 是 s 的前 n 个字符（按字符，不按字节）。
func First(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Trimmed 是单条读取时正文被截的说明。
type Trimmed struct {
	TotalChars    int `json:"total_chars"`
	ReturnedChars int `json:"returned_chars"`
}

// TrimmedOf 比较裁剪前后的正文；没有截短时为 nil。
func TrimmedOf(before, after string) *Trimmed {
	total, got := utf8.RuneCountInString(before), utf8.RuneCountInString(after)
	if total == got {
		return nil
	}
	return &Trimmed{TotalChars: total, ReturnedChars: got}
}
