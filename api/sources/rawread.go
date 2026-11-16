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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/allowlist"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文的读取（附录 B.2、B.6，设计 §7.6、§14.14）。原文由 kernel/sources/runner 存取，本文件只决定谁能读到多少：
// Web 登录读全文；token 按该来源的 outbound.external 裁剪；运行凭据、ext 与 webhook 密钥读不到；对话捕获只对 Web 登录。
// 没有任何写原文的路由：事项正文是事项自己的字段，改它不碰来源事件（§6.14 第 19 条）。

// eventView 是读接口给出的来源事件：detail 只是预览（至多 runner.PreviewChars 个字符），content 是原文清单，全文经 GET /source-events/{id}。
type eventView struct {
	domain.SourceEvent
	Content runner.Manifest `json:"content"`
}

// viewEvent 把账本里读出的事件（Detail 是全文）改成列表口径。
func viewEvent(tx *store.Tx, ev domain.SourceEvent) (eventView, error) {
	mf, err := runner.ContentOf(tx, ev.ID)
	if err != nil {
		return eventView{}, err
	}
	ev.Detail = runner.Preview(ev.Detail)
	return eventView{SourceEvent: ev, Content: mf}, nil
}

func viewEvents(tx *store.Tx, evs []domain.SourceEvent) ([]eventView, error) {
	out := make([]eventView, len(evs))
	for i, ev := range evs {
		v, err := viewEvent(tx, ev)
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

func viewScored(tx *store.Tx, ss []inbound.Scored) ([]scoredView, error) {
	out := make([]scoredView, len(ss))
	for i, s := range ss {
		v, err := viewEvent(tx, s.SourceEvent)
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

// readScope 决定 p 读 ev 的原文时的范围。ext 与 webhook 密钥已被 readable 挡在外面。
func readScope(tx *store.Tx, p *core.Principal, ev domain.SourceEvent) (string, error) {
	if inbound.IsCaptureSource(ev.SourceID) { // 对话内容只给本人
		if !p.Web {
			return "", denied("conversation capture can only be read by the owner in the Web UI", "open it in the Web UI")
		}
		return scopeFull, nil
	}
	if p.Web {
		return scopeFull, nil
	}
	// 挂在已接受事项上的挂靠消息是工作现场，按 GET /items/{id}/messages 的权限读，不按外发范围裁剪（§14.14）。
	if ev.Disposition == domain.DispositionAttached && ev.Item != "" {
		msgs, err := allowlist.Messages(tx, ev.Item, p.Caller.Level)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(msgs, func(m allowlist.Attached) bool { return m.ID == ev.ID }) {
			return scopeFull, nil
		}
	}
	if p.Caller.Level == authz.LevelRun { // 运行凭据只经挂靠消息的权限读，不读来源原文（§7.6）
		return "", denied("run credential cannot read source event content", "read forwarded messages from the context instead")
	}
	src, err := registry.Get(tx, ev.SourceID)
	if err != nil {
		return "", err
	}
	// token 读到的内容进它自己的模型，内核无从知道在哪，一律按外部端点的范围；未选择与 none 只回元信息。
	switch registry.ScopeFor(src.Outbound, "external") {
	case registry.ScopeFull:
		return scopeFull, nil
	case registry.ScopeExcerpt:
		return scopeExcerpt, nil
	}
	return scopeMeta, nil
}

// scoped 按范围裁剪事件的元信息；全文范围保持列表口径（detail 为预览）。
func scoped(ev domain.SourceEvent, scope string) domain.SourceEvent {
	if scope == scopeFull {
		return ev
	}
	t, _ := registry.Trim(ev, scope)
	out := domain.SourceEvent{ID: ev.ID, SourceID: ev.SourceID, Type: t.Type, Subject: t.Subject, Sender: t.Sender, SenderVerified: ev.SenderVerified,
		OccurredAt: ev.OccurredAt, Link: t.Link, Disposition: ev.Disposition, Item: ev.Item, Summary: t.Summary, Detail: t.Detail}
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
