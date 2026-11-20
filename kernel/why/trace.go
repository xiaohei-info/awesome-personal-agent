package why

// 处理经过（§15.1）：一条来源消息从进来到后来的经过，或一个事项连同它来自的消息、派出去的子事项的经过。
// 每行是"谁 · 做了什么 · 结果"的种类（Kind）与参数（Args），句子由 Web 的词表生成；合成这一行的原始 Event 在 Steps，
// 按凭据级别裁剪（与 Replay 同一份 StepOf）。只读，不写 Event。账本里没记的环节如实给出（例如判断记录已过保留期），不猜。

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Trace 是一份经过。消息经过有 Message；事项经过有 Origins（它来自的消息，至多 MaxOrigins 条，其余计入 MoreOrigins）。
type Trace struct {
	Message     *Head   `json:"message,omitempty"`
	Origins     []Trace `json:"origins,omitempty"`
	MoreOrigins int     `json:"more_origins,omitempty"`
	Entries     []Entry `json:"entries"`
}

// MaxOrigins 是事项经过里展开的来源消息条数上限。
const MaxOrigins = 5

// Head 是消息本身：从哪进来、谁发的、发件人是否已核实。ArrivedAt 是 APA 第一次处理它的时刻（读时推算：最早的处置、挂靠或判断，
// 都没有时为空）；OccurredAt 是来源自报的时间。
type Head struct {
	ID             string     `json:"id"`
	Source         string     `json:"source"`
	Type           string     `json:"type"`
	Subject        string     `json:"subject,omitempty"`
	Sender         string     `json:"sender,omitempty"`
	SenderVerified bool       `json:"sender_verified"`
	Link           string     `json:"link,omitempty"`
	Disposition    string     `json:"disposition"`
	OccurredAt     time.Time  `json:"occurred_at"`
	ArrivedAt      *time.Time `json:"arrived_at,omitempty"`
}

// Who 是一行的主语。Name：agent 是凭据 id，rule 是"包/规则 id"，model 是模型名（可空），source 是来源 id。Role 只用于 model。
type Who struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"`
}

// 主语的种类。
const (
	WhoOwner  = "owner"
	WhoAgent  = "agent"
	WhoRule   = "rule"
	WhoModel  = "model"
	WhoSystem = "system"
	WhoSource = "source"

	RoleJudge    = "judge"
	RoleGenerate = "generate"
)

// ItemRef 是一行涉及的事项。Gone 为真表示事项已被永久删除。
type ItemRef struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Gone  bool   `json:"gone,omitempty"`
}

// Entry 是经过里的一行。Mech 为真是机械记录，相邻同类可以折叠。
type Entry struct {
	At    time.Time      `json:"at"`
	Who   Who            `json:"who"`
	Kind  string         `json:"kind"`
	Args  map[string]any `json:"args,omitempty"`
	Item  *ItemRef       `json:"item,omitempty"`
	Mech  bool           `json:"mech,omitempty"`
	Steps []Step         `json:"steps"`
}

// 行的种类（附录 B.2 的 trace 词表）。
const (
	KArrived   = "arrived"   // args: type, sender, verified
	KCaptured  = "captured"  // 从对话里记下
	KRule      = "rule"      // args: then（ignore、judge、proposed、digest、alert、extract）
	KJudged    = "judged"    // args: levels[], low, high, decided_by, answer, low_confidence, shadow, expired
	KBacklog   = "backlog"   // args: reason（unjudged_reason）
	KExpired   = "expired"   // 积压过期，不再自动判断
	KRequeued  = "requeued"  // 放回积压等重判
	KIgnored   = "ignored"   // 没有规则、没有判断的忽略（本人从积压里忽略）
	KProposed  = "proposed"  // item；args: low_confidence, count（extract 一次建多条）
	KMerged    = "merged"    // item：并入已有建议
	KDigest    = "digest"    // 放进简报的知会一节
	KAlerted   = "alerted"   // 直接提醒
	KAttached  = "attached"  // item；args: via（follow、proposal）、fields（后续消息改写过的字段，旧数据）
	KDrafted   = "drafted"   // args: outcome（ok、failed、skipped、unknown）、code
	KSpot      = "spot"      // args: verdict（should_see、correct）；item（该看建出的建议）
	KCreated   = "created"   // item
	KAccepted  = "accepted"  // 接受建议
	KDismissed = "dismissed" // 驳回建议
	KPhase     = "phase"     // args: from, to, waiting_kind, waiting_ref
	KEdited    = "edited"    // args: fields[], tags_added[], tags_removed[]
	KSynced    = "synced"    // 系统同步来源对象的更新；args: fields[]
	KAbsorbed  = "absorbed"  // 另一条建议补到这里；item 是那条建议
	KAttachTo  = "attach_to" // 这条建议补到了 item
	KProgress  = "progress"  // args: text
	KDeadline  = "deadline_passed"
	KDecision  = "decision"  // args: state（requested、resolved）
	KFollow    = "follow"    // 后续消息；args: message, subject, state（attached、processed、handed_back、released）
	KObserved  = "observed"  // 规则试运行；args: rule
	KRun       = "run"       // Agent 后台运行；args: state（launched、started、finished）；launched 另有 runner、launch、from?、fallback?
	KGenerated = "generated" // 写作模型的一次生成（如一句话摘要）；args: state（succeeded、failed）、template
	KAlert     = "alert"     // 系统告警（详情在健康视图）
	KDelegated = "delegated"
	KDispatch  = "dispatched"       // args: executor, harness, state
	KDispUpd   = "dispatch_updated" // args: state
	KReported  = "reported"         // args: phase
	KRejected  = "rejected"         // 命令被挡下；args: command
	KRelated   = "related"          // 登记或移除关系、关联键
	KEvent     = "event"            // 词表没有专门句子的事件；args: type
)

// 本包不依赖入站与抽检，事件类型的字面值在此对齐（装配处的测试守住一致）。
const (
	evRequeued      = "inbound.requeued"
	evCaptured      = "inbound.captured"
	evSpotReviewed  = "inbound.spot_reviewed"
	evFollowAttach  = "followmsg.attached"
	evFollowDone    = "followmsg.processed"
	evFollowBack    = "followmsg.handed_back"
	evFollowRelease = "followmsg.released"
	evObserved      = "rule.observed"
	evDispatched    = "delegation.dispatched"
	evDispatchUpd   = "delegation.dispatch_updated"
	evBgLaunched    = "background.launched"
)

// kernelInbound、kernelFollow 是入站与跟进消息归属代写事项时的操作者。
const (
	kernelInbound = "kernel:inbound"
	kernelFollow  = "kernel:followmsg"
)

// MessageTrace 返回一条来源消息的经过；它建出的建议，后来的经过一并接上。
func MessageTrace(tx *store.Tx, p policy.Policy, r Request) (Trace, error) {
	if err := readTrace(p, r); err != nil {
		return Trace{}, err
	}
	b := builder{tx: tx, level: r.Level}
	ev, err := runner.Event(tx, r.ID)
	if err != nil {
		return Trace{}, err
	}
	head, entries, made, err := b.message(ev)
	if err != nil {
		return Trace{}, err
	}
	for _, id := range made {
		more, err := b.item(id, true)
		if err != nil {
			return Trace{}, err
		}
		entries = append(entries, more...)
	}
	return Trace{Message: &head, Entries: ordered(entries)}, nil
}

// ItemTrace 返回一个事项的经过：开头是它来自的消息，之后是事项本身与派出去的子事项。
func ItemTrace(tx *store.Tx, p policy.Policy, r Request) (Trace, error) {
	if err := readTrace(p, r); err != nil {
		return Trace{}, err
	}
	if _, err := items.Get(tx, r.ID); err != nil {
		return Trace{}, err
	}
	b := builder{tx: tx, level: r.Level}
	disposed, err := eventlog.ReadDisposedFor(tx, r.ID)
	if err != nil {
		return Trace{}, err
	}
	t := Trace{Entries: []Entry{}}
	seen := map[string]bool{}
	for _, en := range disposed {
		id := en.Event.Object
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(t.Origins) == MaxOrigins {
			t.MoreOrigins++
			continue
		}
		ev, err := runner.Event(tx, id)
		if isNotFound(err) {
			continue // 来源消息已被永久删除
		}
		if err != nil {
			return Trace{}, err
		}
		head, entries, _, err := b.message(ev)
		if err != nil {
			return Trace{}, err
		}
		t.Origins = append(t.Origins, Trace{Message: &head, Entries: ordered(entries)})
	}
	if t.Entries, err = b.item(r.ID, len(t.Origins) > 0); err != nil {
		return Trace{}, err
	}
	t.Entries = ordered(t.Entries)
	return t, nil
}

func readTrace(p policy.Policy, r Request) error {
	if r.ID == "" {
		return domain.NewError(domain.CodeInvalidRequest, "id is empty", "pass a source event or item id")
	}
	if d := authz.Decide(p, authz.Request{Actor: r.Actor, Level: r.Level, Action: authz.ActionRead, Tier: authz.TierRead, Object: authz.Object{ID: r.ID}}); d.Err != nil {
		return d.Err
	}
	return nil
}

// ordered 按时间排序；同一时刻按落账序号（事项与子事项的记录交错时以账本为准），没有原始记录的行沿用前一行的序号，
// 保持组装顺序（规则在判断前、结论在起草前）。
func ordered(es []Entry) []Entry {
	if es == nil {
		return []Entry{}
	}
	seq := make([]int64, len(es))
	for i, e := range es {
		switch {
		case len(e.Steps) > 0:
			seq[i] = e.Steps[0].Seq
		case i > 0:
			seq[i] = seq[i-1]
		}
	}
	idx := make([]int, len(es))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := idx[a], idx[b]
		if !es[x].At.Equal(es[y].At) {
			return es[x].At.Before(es[y].At)
		}
		return seq[x] < seq[y]
	})
	out := make([]Entry, len(es))
	for i, j := range idx {
		out[i] = es[j]
	}
	return out
}

type builder struct {
	tx    *store.Tx
	level authz.Level
}

func (b builder) steps(ens ...eventlog.Entry) []Step {
	out := make([]Step, 0, len(ens))
	for _, en := range ens {
		out = append(out, StepOf(en, b.level))
	}
	return out
}

func (b builder) ref(id string) (*ItemRef, error) {
	if id == "" {
		return nil, nil
	}
	it, err := items.Get(b.tx, id)
	if isNotFound(err) {
		return &ItemRef{ID: id, Gone: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ItemRef{ID: id, Title: it.Title}, nil
}

// disposedPayload 是入站处置的载荷（kernel/inbound 的 Apply 与抽检"该看"写下的字段）。
type disposedPayload struct {
	Disposition    string   `json:"disposition"`
	Item           string   `json:"item"`
	Items          []string `json:"items"`
	Rule           string   `json:"rule"`
	Judgment       string   `json:"judgment"`
	Merged         bool     `json:"merged"`
	UnjudgedReason string   `json:"unjudged_reason"`
	Answer         string   `json:"answer"`
	DecidedBy      string   `json:"decided_by"`
	Review         string   `json:"review"`
	Draft          *struct {
		Outcome string `json:"outcome"`
		Code    string `json:"code"`
	} `json:"draft"`
}

// message 组装消息本身的经过，返回它建出的事项（不含并入已有建议的）。
func (b builder) message(ev domain.SourceEvent) (Head, []Entry, []string, error) {
	own, err := eventlog.ReadObject(b.tx, ev.ID)
	if err != nil {
		return Head{}, nil, nil, err
	}
	mentions, err := eventlog.ReadMentioning(b.tx, ev.ID)
	if err != nil {
		return Head{}, nil, nil, err
	}
	recs, _, err := record.List(b.tx, record.Filter{Subject: ev.ID, Limit: record.MaxLimit})
	if err != nil {
		return Head{}, nil, nil, err
	}
	head := Head{ID: ev.ID, Source: ev.SourceID, Type: ev.Type, Subject: ev.Subject, Sender: ev.Sender, SenderVerified: ev.SenderVerified,
		Link: ev.Link, Disposition: string(ev.Disposition), OccurredAt: ev.OccurredAt}
	for _, en := range append(slices.Clone(own), mentions...) {
		if at := en.Event.At; head.ArrivedAt == nil || at.Before(*head.ArrivedAt) {
			head.ArrivedAt = &at
		}
	}
	for _, r := range recs { // 判断在处置落账之前：最早的判断也是 APA 已经收到它的证据
		if at := r.CreatedAt; head.ArrivedAt == nil || at.Before(*head.ArrivedAt) {
			head.ArrivedAt = &at
		}
	}
	arrived := ev.OccurredAt
	if head.ArrivedAt != nil {
		arrived = *head.ArrivedAt
	}
	out := []Entry{{At: arrived, Who: Who{Kind: WhoSource, Name: ev.SourceID}, Kind: KArrived, Steps: []Step{},
		Args: map[string]any{"type": ev.Type, "sender": ev.Sender, "verified": ev.SenderVerified}}}
	byID := map[string]record.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	used, reviewed := map[string]bool{}, map[string]bool{}
	var made []string
	spotAt := map[string]int{} // 判断记录 id → 已有"该看"一行的下标
	for _, en := range own {
		if en.Tombstone {
			out = append(out, Entry{At: en.Event.At, Who: b.who(en), Kind: KEvent, Args: map[string]any{"type": en.Event.Type}, Steps: b.steps(en)})
			continue
		}
		switch en.Event.Type {
		case evDisposed:
			var d disposedPayload
			_ = json.Unmarshal(en.Event.Payload, &d)
			if d.Review == "should_see" {
				ref, err := b.ref(d.Item)
				if err != nil {
					return Head{}, nil, nil, err
				}
				spotAt[d.Judgment] = len(out)
				out = append(out, Entry{At: en.Event.At, Who: b.who(en), Kind: KSpot, Args: map[string]any{"verdict": "should_see"}, Item: ref, Steps: b.steps(en)})
				made = append(made, d.Item)
				continue
			}
			group, item, err := b.disposed(en, d, byID)
			if err != nil {
				return Head{}, nil, nil, err
			}
			used[d.Judgment] = true
			out = append(out, group...)
			if item != "" {
				made = append(made, item)
			}
		case evRequeued:
			out = append(out, Entry{At: en.Event.At, Who: b.who(en), Kind: KRequeued, Steps: b.steps(en)})
		case evCaptured:
			out = append(out, Entry{At: en.Event.At, Who: Who{Kind: WhoSystem}, Kind: KCaptured, Steps: b.steps(en)})
		case evSpotReviewed:
			var p struct{ Verdict, Judgment string }
			_ = json.Unmarshal(en.Event.Payload, &p)
			reviewed[p.Judgment] = true
			if i, ok := spotAt[p.Judgment]; ok && p.Verdict == "should_see" {
				out[i].Steps = append(out[i].Steps, b.steps(en)...)
				continue
			}
			out = append(out, Entry{At: en.Event.At, Who: b.who(en), Kind: KSpot, Args: map[string]any{"verdict": p.Verdict}, Steps: b.steps(en)})
		default:
			out = append(out, Entry{At: en.Event.At, Who: b.who(en), Kind: KEvent, Args: map[string]any{"type": en.Event.Type}, Steps: b.steps(en)})
		}
	}
	for _, r := range recs {
		if !used[r.ID] { // 影子打分，或处置之外的判断
			out = append(out, judged(r))
		}
		if r.Review != nil && !reviewed[r.ID] { // 旧数据：抽检结论只在判断记录上
			if _, ok := spotAt[r.ID]; !ok {
				out = append(out, Entry{At: r.Review.At, Who: Who{Kind: WhoOwner}, Kind: KSpot, Args: map[string]any{"verdict": r.Review.Result}, Steps: []Step{}})
			}
		}
	}
	for _, en := range mentions {
		e, err := b.mention(ev, en)
		if err != nil {
			return Head{}, nil, nil, err
		}
		out = append(out, e)
	}
	return head, out, made, nil
}

// disposed 把一次入站处置拆成：命中的规则、判断、结论、起草。返回它建出的事项（并入已有建议时为空）。
func (b builder) disposed(en eventlog.Entry, d disposedPayload, recs map[string]record.Record) ([]Entry, string, error) {
	at := en.Event.At
	var out []Entry
	var j *Entry
	if d.Judgment != "" {
		if r, ok := recs[d.Judgment]; ok {
			e := judged(r)
			j = &e
		} else { // 判断记录已过保留期：只剩处置载荷里的结论
			j = &Entry{At: at, Who: Who{Kind: WhoModel, Role: RoleJudge}, Kind: KJudged,
				Args: map[string]any{"expired": true, "answer": d.Answer, "decided_by": d.DecidedBy}}
		}
		if j.At.After(at) {
			j.At = at
		}
	}
	if d.Rule != "" {
		then := map[string]string{"ignored": "ignore", "proposed": "proposed", "digest": "digest", "alerted": "alert"}[d.Disposition]
		if j != nil || d.Disposition == string(domain.DispositionUnjudged) {
			then = "judge"
		}
		if len(d.Items) > 1 {
			then = "extract"
		}
		ruleAt := at
		if j != nil {
			ruleAt = j.At
		}
		out = append(out, Entry{At: ruleAt, Who: Who{Kind: WhoRule, Name: ruleName(d.Rule)}, Kind: KRule, Args: map[string]any{"then": then}})
	}
	if j != nil {
		out = append(out, *j)
	}
	var made string
	who := Who{Kind: WhoSystem}
	if en.Event.Actor.Type == domain.ActorHuman {
		who = Who{Kind: WhoOwner}
	}
	switch domain.Disposition(d.Disposition) {
	case domain.DispositionIgnored:
		if d.Rule == "" && d.Judgment == "" {
			out = append(out, Entry{At: at, Who: who, Kind: KIgnored})
		}
	case domain.DispositionProposed:
		ref, err := b.ref(d.Item)
		if err != nil {
			return nil, "", err
		}
		kind := KProposed
		if d.Merged {
			kind = KMerged
		} else {
			made = d.Item
		}
		args := map[string]any{}
		if len(d.Items) > 1 {
			args["count"] = len(d.Items)
		}
		if j != nil && j.Args["low_confidence"] == true {
			args["low_confidence"] = true
		}
		out = append(out, Entry{At: at, Who: who, Kind: kind, Item: ref, Args: args})
		if d.Judgment != "" && !d.Merged { // 初筛后进建议流的才起草（§9.3 第 5 步）
			out = append(out, drafted(at, d, en.Event.Reason))
		}
	case domain.DispositionUnjudged:
		if d.UnjudgedReason == domain.UnjudgedRestored {
			out = append(out, Entry{At: at, Who: who, Kind: KRequeued, Args: map[string]any{"reason": d.UnjudgedReason}})
		} else {
			out = append(out, Entry{At: at, Who: Who{Kind: WhoSystem}, Kind: KBacklog, Args: map[string]any{"reason": d.UnjudgedReason}})
		}
	case domain.DispositionExpired:
		out = append(out, Entry{At: at, Who: Who{Kind: WhoSystem}, Kind: KExpired})
	case domain.DispositionDigest:
		out = append(out, Entry{At: at, Who: who, Kind: KDigest})
	case domain.DispositionAlerted:
		out = append(out, Entry{At: at, Who: who, Kind: KAlerted})
	default:
		if len(out) == 0 {
			out = append(out, Entry{At: at, Who: b.who(en), Kind: KEvent, Args: map[string]any{"type": en.Event.Type}})
		}
	}
	// 这次处置的原始记录挂在结论那一行（起草之前的最后一行）；其余行与它同出一条。
	at0 := len(out) - 1
	if out[at0].Kind == KDrafted {
		at0--
	}
	for i := range out {
		out[i].Steps = []Step{}
	}
	out[at0].Steps = b.steps(en)
	return out, made, nil
}

// drafted 取起草结果：新数据读载荷的 draft，旧数据退回内核自己写的理由前缀，都没有时如实写"不清楚"。
func drafted(at time.Time, d disposedPayload, reason string) Entry {
	e := Entry{At: at, Who: Who{Kind: WhoModel, Role: RoleGenerate}, Kind: KDrafted}
	switch {
	case d.Draft != nil:
		e.Args = map[string]any{"outcome": d.Draft.Outcome, "code": d.Draft.Code}
	case strings.Contains(reason, "draft skipped"):
		e.Args = map[string]any{"outcome": "skipped", "code": "outbound"}
	case strings.Contains(reason, "draft failed"):
		code := "model"
		switch {
		case strings.Contains(reason, "schema"):
			code = "schema"
		case strings.Contains(reason, "unavailable"):
			code = "unavailable"
		case strings.Contains(reason, "unsettled"):
			code = "unsettled"
		}
		e.Args = map[string]any{"outcome": "failed", "code": code}
	default:
		e.Args = map[string]any{"outcome": "unknown"}
	}
	if e.Args["outcome"] != "ok" {
		e.Who = Who{Kind: WhoSystem}
		if e.Args["outcome"] == "failed" {
			e.Who = Who{Kind: WhoModel, Role: RoleGenerate}
		}
	}
	return e
}

// judged 把一条判断记录写成一行：逐级的后端、模型、概率与落在门槛的哪一段（按记录的 band 推算，与 direct 的判定一致）。
func judged(r record.Record) Entry {
	levels := make([]map[string]any, 0, len(r.Levels))
	model := ""
	for _, lv := range r.Levels {
		m := map[string]any{"backend": lv.Backend, "model": lv.Model, "outcome": lv.Outcome}
		if lv.PYes != nil {
			m["p"] = *lv.PYes
			switch {
			case *lv.PYes < r.Band.Low:
				m["seg"] = "below"
			case *lv.PYes > r.Band.High:
				m["seg"] = "above"
			default:
				m["seg"] = "between"
			}
		}
		if lv.Answer != "" {
			m["answer"] = lv.Answer
		}
		if lv.ErrorCode != "" {
			m["error"] = lv.ErrorCode
		}
		if lv.Outcome == record.OutcomeDecided || model == "" && lv.Model != "" {
			model = lv.Model
		}
		levels = append(levels, m)
	}
	args := map[string]any{"levels": levels, "low": r.Band.Low, "high": r.Band.High, "decided_by": r.DecidedBy, "answer": r.Answer, "use": r.Use}
	if r.LowConfidence {
		args["low_confidence"] = true
	}
	if r.Shadow {
		args["shadow"] = true
	}
	return Entry{At: r.CreatedAt, Who: Who{Kind: WhoModel, Name: model, Role: RoleJudge}, Kind: KJudged, Args: args, Steps: []Step{}}
}

// mention 是事项一侧点名这条消息的事件：跟进消息挂上、交回、退回，或建议补到已有事项时随之改挂。
func (b builder) mention(ev domain.SourceEvent, en eventlog.Entry) (Entry, error) {
	ref, err := b.ref(en.Event.Object)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{At: en.Event.At, Item: ref, Steps: b.steps(en)}
	switch en.Event.Type {
	case domain.EvProposalAttached:
		e.Who, e.Kind, e.Args = b.who(en), KAttached, map[string]any{"via": "proposal"}
	case evFollowAttach:
		e.Who, e.Kind, e.Args = Who{Kind: WhoSource, Name: ev.SourceID}, KAttached, map[string]any{"via": "follow"}
		// 旧数据：评论曾按来源对象更新改写过事项（同一时刻、操作者是跟进消息归属的 item.updated）。
		own, err := eventlog.ReadObject(b.tx, en.Event.Object)
		if err != nil {
			return Entry{}, err
		}
		for _, x := range own {
			if x.Event.Type == domain.EvItemUpdated && x.Event.Actor.Principal == kernelFollow && x.Event.At.Equal(en.Event.At) && strings.Contains(x.Event.Reason, ev.ID) {
				e.Args["fields"] = changed(x.Event.Payload)
				e.Steps = append(b.steps(x), e.Steps...)
			}
		}
	default:
		e.Who, e.Kind = b.who(en), KFollow
		e.Args = map[string]any{"state": strings.TrimPrefix(en.Event.Type, "followmsg."), "message": ev.ID}
	}
	return e, nil
}

// itemPayload 是事项事件的快照（kernel/items 的 payload）。
type itemPayload struct {
	Op       string          `json:"op"`
	Before   json.RawMessage `json:"before"`
	Item     json.RawMessage `json:"item"`
	To       string          `json:"to"`
	Proposal string          `json:"proposal"`
	Message  string          `json:"message"`
	Progress *struct {
		Text string `json:"text"`
	} `json:"progress"`
	Dispatch *struct {
		Executor string `json:"executor"`
		Harness  string `json:"harness"`
		State    string `json:"state"`
	} `json:"dispatch"`
}

type snapshot struct {
	Phase     string            `json:"phase"`
	Assignee  string            `json:"assignee"`
	WaitingOn *domain.WaitingOn `json:"waiting_on"`
}

func snap(raw json.RawMessage) snapshot {
	var s snapshot
	_ = json.Unmarshal(raw, &s)
	return s
}

// item 组装事项本身与派出去的子事项的经过。fromMessage 为真时略去"新建"一行：前面消息经过里的"提成建议"就是它。
func (b builder) item(id string, fromMessage bool) ([]Entry, error) {
	own, err := eventlog.ReadObject(b.tx, id)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i := 0; i < len(own); i++ {
		en := own[i]
		var p itemPayload
		_ = json.Unmarshal(en.Event.Payload, &p)
		e := Entry{At: en.Event.At, Who: b.who(en), Steps: b.steps(en)}
		switch t := en.Event.Type; {
		case en.Tombstone:
			e.Kind, e.Args = KEvent, map[string]any{"type": t}
		case t == domain.EvItemCreated:
			if fromMessage {
				continue
			}
			e.Kind = KCreated
		case t == domain.EvItemUpdated && en.Event.Actor.Principal == kernelInbound:
			// 入站并入来源事件时的同步记录：并进上一行（新建或提成建议），没有上一行就作为一条同步记录。
			if n := len(out); n > 0 {
				out[n-1].Steps = append(out[n-1].Steps, e.Steps...)
				continue
			}
			if fromMessage {
				continue
			}
			e.Kind, e.Mech, e.Args = KSynced, true, map[string]any{"fields": changed(en.Event.Payload)}
		case t == domain.EvItemUpdated && en.Event.Actor.Principal == kernelFollow:
			e.Who, e.Kind, e.Mech, e.Args = Who{Kind: WhoSystem}, KSynced, true, map[string]any{"fields": changed(en.Event.Payload)}
			if i+1 < len(own) && own[i+1].Event.Type == evFollowAttach && own[i+1].Event.At.Equal(en.Event.At) { // 与紧随的挂靠是同一件事
				next, err := b.follow(own[i+1], "attached")
				if err != nil {
					return nil, err
				}
				next.Args["fields"] = e.Args["fields"]
				next.Steps = append(e.Steps, next.Steps...)
				out = append(out, next)
				i++
				continue
			}
		case t == domain.EvItemUpdated && p.Op == "relate":
			e.Kind = KRelated
		case t == domain.EvItemUpdated:
			e.Kind, e.Args = KEdited, edits(p.Before, p.Item)
			if w := snap(p.Item).WaitingOn; p.Op == "delegate.create" && w != nil {
				e.Kind, e.Args = KPhase, map[string]any{"from": snap(p.Before).Phase, "to": snap(p.Item).Phase, "waiting_kind": w.Kind, "waiting_ref": w.Ref}
			}
		case t == domain.EvItemPhaseChanged:
			from, to := snap(p.Before).Phase, snap(p.Item)
			switch {
			case p.Op == "proposal.attach":
				ref, err := b.ref(p.To)
				if err != nil {
					return nil, err
				}
				e.Kind, e.Item = KAttachTo, ref
			case from == string(domain.PhaseProposed) && to.Phase == string(domain.PhaseOpen):
				e.Kind = KAccepted
			case from == string(domain.PhaseProposed) && to.Phase == string(domain.PhaseDropped):
				e.Kind = KDismissed
			default:
				e.Kind, e.Args = KPhase, map[string]any{"from": from, "to": to.Phase}
				if to.WaitingOn != nil {
					e.Args["waiting_kind"], e.Args["waiting_ref"] = to.WaitingOn.Kind, to.WaitingOn.Ref
				}
			}
		case t == domain.EvItemProgressAdded:
			e.Kind = KProgress
			if p.Progress != nil {
				e.Args = map[string]any{"text": p.Progress.Text}
			}
		case t == domain.EvItemDeadlinePassed:
			e.Kind = KDeadline
		case t == domain.EvDecisionRequested || t == domain.EvDecisionResolved:
			e.Kind, e.Args = KDecision, map[string]any{"state": strings.TrimPrefix(t, "decision.")}
		case t == domain.EvProposalAttached:
			ref, err := b.ref(p.Proposal)
			if err != nil {
				return nil, err
			}
			e.Kind, e.Item = KAbsorbed, ref
		case strings.HasPrefix(t, "followmsg."):
			f, err := b.follow(en, strings.TrimPrefix(t, "followmsg."))
			if err != nil {
				return nil, err
			}
			out = append(out, f)
			continue
		case t == evObserved:
			e.Kind, e.Mech = KObserved, true
			if e.Who.Kind != WhoRule {
				e.Who = Who{Kind: WhoRule, Name: ruleName(observedRule(en.Event.Payload))}
			}
		case t == evBgLaunched: // 这次怎么拉起、来自哪一级设置（§13.6），选了唤醒却改按无头运行时带 fallback
			var lp struct {
				Runner, Launch string
				From           string `json:"launch_from"`
				Fallback       bool   `json:"launch_fallback"`
			}
			_ = json.Unmarshal(en.Event.Payload, &lp)
			e.Kind, e.Args = KRun, map[string]any{"state": "launched", "runner": lp.Runner, "launch": lp.Launch}
			if lp.From != "" {
				e.Args["from"] = lp.From
			}
			if lp.Fallback {
				e.Args["fallback"] = true
			}
		case t == domain.EvRunStarted || t == domain.EvRunFinished:
			var rp struct{ Category, State, Template string }
			_ = json.Unmarshal(en.Event.Payload, &rp)
			e.Kind, e.Mech, e.Args = KRun, true, map[string]any{"state": strings.TrimPrefix(t, "run.")}
			if rp.Category != string(domain.RunBackground) && rp.Category != string(domain.RunTask) { // 写作模型的调用不是 Agent 运行
				e.Who, e.Kind, e.Args = Who{Kind: WhoModel, Role: RoleGenerate}, KGenerated, map[string]any{"state": rp.State, "template": rp.Template}
			}
		case t == domain.EvHealthAlert:
			e.Who, e.Kind, e.Mech = Who{Kind: WhoSystem}, KAlert, true
		case t == domain.EvCommandRejected:
			e.Kind = KRejected
		case t == domain.EvDelegationCreated || t == domain.EvDelegationReported || t == evDispatched || t == evDispatchUpd:
			e = delegationEntry(e, t, p) // 子事项自己的经过：与它在父事项经过里的几行同一写法
		default:
			e.Kind, e.Args = KEvent, map[string]any{"type": t}
		}
		out = append(out, e)
	}
	kids, err := items.Children(b.tx, id)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		more, err := b.child(k)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// child 是派出去的子事项在父事项经过里的几行：派出、开工、进度、交回。
func (b builder) child(k domain.Item) ([]Entry, error) {
	own, err := eventlog.ReadObject(b.tx, k.ID)
	if err != nil {
		return nil, err
	}
	ref := &ItemRef{ID: k.ID, Title: k.Title}
	var out []Entry
	for _, en := range own {
		if en.Tombstone {
			continue
		}
		var p itemPayload
		_ = json.Unmarshal(en.Event.Payload, &p)
		e := Entry{At: en.Event.At, Who: b.who(en), Item: ref, Steps: b.steps(en)}
		switch t := en.Event.Type; t {
		case domain.EvDelegationCreated, evDispatched, evDispatchUpd, domain.EvDelegationReported:
			e = delegationEntry(e, t, p)
		case domain.EvCommandRejected:
			e.Kind = KRejected
		default:
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// delegationEntry 写委派的一行：派出、开工、进度、交回。
func delegationEntry(e Entry, t string, p itemPayload) Entry {
	switch t {
	case domain.EvDelegationCreated:
		e.Kind, e.Args = KDelegated, map[string]any{"assignee": snap(p.Item).Assignee}
	case domain.EvDelegationReported:
		e.Kind, e.Args = KReported, map[string]any{"phase": snap(p.Item).Phase, "assignee": snap(p.Item).Assignee}
	default:
		e.Kind, e.Args = KDispatch, map[string]any{}
		if t == evDispatchUpd {
			e.Kind, e.Mech = KDispUpd, true
		}
		if p.Dispatch != nil {
			e.Args = map[string]any{"executor": p.Dispatch.Executor, "harness": p.Dispatch.Harness, "state": p.Dispatch.State}
		}
	}
	return e
}

// follow 是事项上的一条后续消息：挂上、已处理、交回或退回。
func (b builder) follow(en eventlog.Entry, state string) (Entry, error) {
	var p struct {
		Message  string   `json:"message"`
		Messages []string `json:"messages"`
	}
	_ = json.Unmarshal(en.Event.Payload, &p)
	e := Entry{At: en.Event.At, Who: b.who(en), Kind: KFollow, Steps: b.steps(en), Args: map[string]any{"state": state}}
	if p.Message == "" && len(p.Messages) == 1 {
		p.Message = p.Messages[0]
	}
	if len(p.Messages) > 1 {
		e.Args["count"] = len(p.Messages)
	}
	if p.Message != "" {
		e.Args["message"] = p.Message
		switch m, err := runner.Event(b.tx, p.Message); {
		case err == nil:
			e.Args["subject"] = m.Subject
			if r := []rune(m.Summary); len(r) > 0 { // 评论等后续消息的标题常是所属对象的标题，正文才区分得开
				e.Args["text"] = string(r[:min(len(r), 60)])
			}
			if state == "attached" {
				e.Who = Who{Kind: WhoSource, Name: m.SourceID}
			}
		case !isNotFound(err):
			return Entry{}, err
		}
	}
	e.Mech = state == "attached" || state == "processed"
	return e, nil
}

// who 由操作者得出主语：本人、Agent、规则（规则写入或依据规则）与系统。
func (b builder) who(en eventlog.Entry) Who {
	a := en.Event.Actor
	switch {
	case a.Type == domain.ActorHuman:
		return Who{Kind: WhoOwner}
	case strings.HasPrefix(a.Principal, "rule:"):
		return Who{Kind: WhoRule, Name: ruleName(strings.TrimPrefix(a.Principal, "rule:"))}
	case a.Type == domain.ActorAgent:
		return Who{Kind: WhoAgent, Name: a.Principal}
	case a.Via == domain.ViaRule && en.Event.Basis != nil && en.Event.Basis.RuleID != "": // 只认规则写入；其他记录的 basis 可能只是模板
		return Who{Kind: WhoRule, Name: ruleName(en.Event.Basis.RuleID)}
	}
	return Who{Kind: WhoSystem}
}

// ruleName 把规则键 <包>/<组>/<id> 写成"包/规则 id"（Web 词表的键）。
func ruleName(key string) string {
	if p := strings.Split(key, "/"); len(p) == 3 {
		return p[0] + "/" + p[2]
	}
	return key
}

func observedRule(payload json.RawMessage) string {
	var p struct {
		Rule string `json:"rule"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Rule
}

// traced 是"改了什么"要比较的事项字段；版本、时间戳、阶段与内核状态不算。
var traced = []string{"title", "summary", "body", "priority", "project", "assignee", "deadline", "planned_for", "planned_slot", "kind", "spec", "status_label", "tags"}

// changed 返回一条事项事件里改过的字段。
func changed(payload json.RawMessage) []string {
	var p itemPayload
	_ = json.Unmarshal(payload, &p)
	f, _ := edits(p.Before, p.Item)["fields"].([]string)
	return f
}

// edits 比较改前改后两份快照：改过的字段，以及标签的增删。
func edits(before, after json.RawMessage) map[string]any {
	var a, z map[string]any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &z)
	fields := []string{}
	for _, k := range traced {
		if !reflect.DeepEqual(a[k], z[k]) {
			fields = append(fields, k)
		}
	}
	out := map[string]any{"fields": fields}
	if slices.Contains(fields, "tags") {
		was, now := strs(a["tags"]), strs(z["tags"])
		var add, del []string
		for _, t := range now {
			if !slices.Contains(was, t) {
				add = append(add, t)
			}
		}
		for _, t := range was {
			if !slices.Contains(now, t) {
				del = append(del, t)
			}
		}
		if add != nil {
			out["tags_added"] = add
		}
		if del != nil {
			out["tags_removed"] = del
		}
	}
	return out
}

func strs(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func isNotFound(err error) bool {
	e, ok := err.(*domain.Error)
	return ok && e.Code == domain.CodeNotFound
}
