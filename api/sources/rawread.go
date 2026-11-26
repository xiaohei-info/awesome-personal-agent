package sources

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文的读取（附录 B.2、B.6，设计 §7.6、§14.14）。原文由 kernel/sources/runner 存取，本文件只决定谁能读到多少：
// Web 登录与内部钥匙读全文；外部钥匙按钥匙自己的逐来源档位裁剪，不在表里不给（api/readscope）；
// 运行凭据只经挂靠消息的权限读；webhook 密钥与非导出 ext 密钥读不到；对话捕获只对 Web 登录。
// 没有任何写原文的路由：事项正文是事项自己的字段，改它不碰来源事件（§6.14 第 19 条）。

// eventView 是读接口给出的来源事件：detail 只是预览（至多 runner.PreviewChars 个字符），content 是原文清单，全文经 GET /source-events/{id}。
type eventView struct {
	domain.SourceEvent
	Content runner.Manifest `json:"content"`
}

// viewEvent 把账本里读出的事件（Detail 是全文）改成列表口径，并按档位裁剪：全文给预览，摘录给前 500 字，元信息不给正文。
// 档位不给的由调用方在此之前拒绝。
func viewEvent(tx *store.Tx, ev domain.SourceEvent, scope string) (eventView, error) {
	mf, err := runner.ContentOf(tx, ev.ID)
	if err != nil {
		return eventView{}, err
	}
	ev.Detail = runner.Preview(ev.Detail)
	if scope != scopeFull {
		ev = scoped(ev, scope)
		mf = runner.Manifest{} // 原文清单里有附件名与大小，只随全文给
	}
	return eventView{SourceEvent: ev, Content: mf}, nil
}

func viewEvents(tx *store.Tx, evs []domain.SourceEvent, scope string) ([]eventView, error) {
	out := make([]eventView, len(evs))
	for i, ev := range evs {
		v, err := viewEvent(tx, ev, scope)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// scoredView 是按分数排序的列表项：事件视图加分数。
type scoredView struct {
	eventView
	Score *float64 `json:"score,omitempty"`
}

func viewScored(tx *store.Tx, ss []inbound.Scored, scope string) ([]scoredView, error) {
	out := make([]scoredView, len(ss))
	for i, s := range ss {
		v, err := viewEvent(tx, s.SourceEvent, scope)
		if err != nil {
			return nil, err
		}
		out[i] = scoredView{eventView: v, Score: s.Score}
	}
	return out, nil
}

// ---- GET /source-events/{id}[?part=] ----

const (
	scopeFull    = "full"
	scopeExcerpt = "excerpt"
	scopeMeta    = "meta"
)

type trimmed struct {
	TotalChars    int `json:"total_chars"`
	ReturnedChars int `json:"returned_chars"`
}

type eventRead struct {
	Event       eventView        `json:"event"`
	Content     runner.Manifest  `json:"content"`
	Body        *string          `json:"body,omitempty"`
	Original    *runner.Original `json:"original,omitempty"`
	Headers     json.RawMessage  `json:"headers,omitempty"`
	Attachments json.RawMessage  `json:"attachments,omitempty"`
	Messages    json.RawMessage  `json:"messages,omitempty"`
	Scope       string           `json:"scope"`
	Trimmed     *trimmed         `json:"trimmed,omitempty"`
}

func denied(why, next string) error {
	return domain.NewError(domain.CodePermissionDenied, why, next)
}

// readScope 决定 p 读 ev 的原文时的范围。webhook 与非导出 ext 密钥已被 readable 挡在外面。
// 对话捕获与其他来源一样：Web 本人、运行凭据与内部钥匙全文；外部钥匙只看钥匙自己的逐来源档位（对话捕获不在其中），
// 挂在事项上也不例外（§14.14）。
func readScope(tx *store.Tx, p *core.Principal, ev domain.SourceEvent) (string, error) {
	if p.Web {
		return scopeFull, nil
	}
	return sourceTier(tx, p, ev.SourceID)
}

// sourceTier 是 p 读来源 id 的档位；不给时返回退出码 4 的拒绝。
func sourceTier(tx *store.Tx, p *core.Principal, id string) (string, error) {
	t := readscope.New(tx, p.Caller).Tier(id)
	if t == readscope.None {
		return "", readscope.Denied()
	}
	return t, nil
}

// internalOnly 挡住外部钥匙：原始请求体、被拒行、外发预览是排查用的，不按档位裁剪，只给 Web、运行凭据与内部钥匙。
func internalOnly(p *core.Principal, what string) error {
	if p.Caller.External() {
		return denied("an external key cannot read "+what, "use the source events this key is given")
	}
	return nil
}

// scoped 按范围裁剪事件的元信息；全文范围保持列表口径（detail 为预览）。
func scoped(ev domain.SourceEvent, scope string) domain.SourceEvent {
	if scope == scopeFull {
		return ev
	}
	t, _ := registry.Trim(ev, scope)
	// 只留元信息与处置状态（处置、未判定原因、挂靠与判断的关联）；来源给的数值、关联键与合并键只随全文给。
	out := domain.SourceEvent{ID: ev.ID, SourceID: ev.SourceID, Type: t.Type, Subject: t.Subject, Sender: t.Sender, SenderVerified: ev.SenderVerified,
		OccurredAt: ev.OccurredAt, Link: t.Link, Disposition: ev.Disposition, Item: ev.Item, Summary: t.Summary, Detail: t.Detail,
		DedupeKey: ev.DedupeKey, AttachState: ev.AttachState, UnjudgedReason: ev.UnjudgedReason, Rejudges: ev.Rejudges, Items: ev.Items, Judgment: ev.Judgment}
	return out
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (a *api) sourceEvent(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	part := r.URL.Query().Get("part")
	if part != "" && !slices.Contains(runner.Parts, part) {
		return invalid("part is not valid", "use one of "+join(runner.Parts))
	}
	var out eventRead
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		ev, err := runner.Event(tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		scope, err := readScope(tx, p, ev)
		if err != nil {
			return err
		}
		mf, err := runner.ContentOf(tx, ev.ID)
		if err != nil {
			return err
		}
		ev.Detail = runner.Preview(ev.Detail)
		out = eventRead{Event: eventView{SourceEvent: scoped(ev, scope), Content: mf}, Content: mf, Scope: scope}
		want := []string{part}
		if part == "" {
			want = nil
			for _, info := range mf.Parts {
				want = append(want, info.Part)
			}
		}
		for _, name := range want {
			if !partAllowed(scope, name) {
				continue
			}
			pd, err := runner.ReadPart(tx, ev.ID, name) // 指定的部分不存在是 404
			if err != nil {
				return err
			}
			switch name {
			case runner.PartBody:
				s := string(pd.Content)
				if total := utf8.RuneCountInString(s); scope == scopeExcerpt && total > registry.ExcerptChars {
					s = firstRunes(s, registry.ExcerptChars)
					out.Trimmed = &trimmed{TotalChars: total, ReturnedChars: registry.ExcerptChars}
				}
				out.Body = &s
			case runner.PartOriginal:
				out.Original = &runner.Original{MediaType: pd.MediaType, Text: string(pd.Content)}
			case runner.PartHeaders:
				out.Headers = pd.Content
			case runner.PartAttachments:
				out.Attachments = pd.Content
				if src, err := registry.Get(tx, ev.SourceID); err == nil && (a.dc.CanFetch == nil || !a.dc.CanFetch(src)) {
					if out.Attachments, err = withoutRefs(pd.Content); err != nil { // 来源不支持取回：不给取回句柄，Web 不显示"取回"
						return err
					}
				}
			case runner.PartMessages:
				out.Messages = pd.Content
			}
		}
		if scope == scopeMeta && (part == "" || part == runner.PartBody) { // 正文存在而一字未回：写明裁掉了多少
			for _, info := range mf.Parts {
				if info.Part == runner.PartBody {
					pd, err := runner.ReadPart(tx, ev.ID, runner.PartBody)
					if err != nil {
						return err
					}
					out.Trimmed = &trimmed{TotalChars: utf8.RuneCount(pd.Content)}
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// withoutRefs 去掉附件清单每一项的 ref，其余字段原样保留。
func withoutRefs(raw json.RawMessage) (json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	for _, it := range items {
		delete(it, "ref")
	}
	return json.Marshal(items)
}

// partAllowed 报告 scope 是否允许回这个部分：excerpt 不回 original、headers、messages；meta 一个部分也不回。
func partAllowed(scope, part string) bool {
	switch scope {
	case scopeFull:
		return true
	case scopeExcerpt:
		return part == runner.PartBody || part == runner.PartAttachments
	}
	return false
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ---- GET /storage ----

type sourceUsage struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Events        int64     `json:"events"`
	ContentBytes  int64     `json:"content_bytes"`
	Oldest        time.Time `json:"oldest,omitzero"`
	RetentionDays *int      `json:"retention_days,omitempty"`
}

type storageOut struct {
	LedgerBytes  int64         `json:"ledger_bytes"`
	BlobsBytes   int64         `json:"blobs_bytes"`
	ContentBytes int64         `json:"content_bytes"`
	Sources      []sourceUsage `json:"sources"`
}

// ledgerBytes 是账本文件（含 WAL）占用的字节数。
func ledgerBytes(path string) (n int64, err error) {
	for _, f := range []string{path, path + "-wal"} {
		fi, e := os.Stat(f)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return 0, e
		}
		n += fi.Size()
	}
	return n, nil
}

// storage 与 Web 的"设置 › 存储"同源：账本、内容寻址附件与各来源原文各占多少。
// content_bytes 是全部来源事件原文之和（含对话捕获的内部来源）；sources 只列登记过的来源。
func (a *api) storage(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	out := storageOut{Sources: []sourceUsage{}}
	var err error
	if out.LedgerBytes, err = ledgerBytes(a.st.Path()); err != nil {
		return err
	}
	if out.BlobsBytes, err = blobs.Bytes(a.dc.BlobsDir); err != nil {
		return err
	}
	if err := a.st.View(r.Context(), func(tx *store.Tx) error {
		ss, err := registry.List(tx)
		if err != nil {
			return err
		}
		usage, err := runner.UsageBySource(tx)
		if err != nil {
			return err
		}
		days, err := registry.ContentRetentionDays(tx)
		if err != nil {
			return err
		}
		for _, u := range usage {
			out.ContentBytes += u.ContentBytes
		}
		for _, s := range ss {
			u := usage[s.ID]
			su := sourceUsage{ID: s.ID, Name: s.Name, Events: u.Events, ContentBytes: u.ContentBytes, Oldest: u.Oldest}
			if d, ok := days[s.ID]; ok {
				su.RetentionDays = &d
			}
			out.Sources = append(out.Sources, su)
		}
		sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].ID < out.Sources[j].ID })
		return nil
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// withStats 给来源的 stats 补上事件数与原文字节数（来源页、apa source list 读到的与 /storage 同源）。
func withStats(s domain.Source, u runner.Usage) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(s.Stats) > 0 {
		if err := json.Unmarshal(s.Stats, &m); err != nil {
			return nil, err
		}
	}
	m["events"], _ = json.Marshal(u.Events)
	m["content_bytes"], _ = json.Marshal(u.ContentBytes)
	return json.Marshal(m)
}
