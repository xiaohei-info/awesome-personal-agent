package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 自测约定（写给第三方 ext 作者）。apa ext check 在本机起一个模拟内核，以环境变量启动 ext：
//
//	APA_URL=<模拟内核>  APA_EXT_KEY=<模拟密钥>  APA_EXT_CHECK=1
//
// 模拟内核按顺序投递四条消息并逐项检查（对应设计 §10.3 的投递协议与回答回流）：
//
//	claim       GET /v1/deliveries/next?wait=n 带 Bearer 领取
//	ack         POST /v1/deliveries/{id}/ack，带领取到的 version 与非空 external_ref
//	idempotent  回执丢失（首个 ack 收到 500）后，同一投递重发必须得到同一个 external_ref（以投递 id 为幂等键）
//	stale       领取后版本被作废：ack 收到 409 后，ext 不得再对该投递报告 ack 或 fail
//	answer      APA_EXT_CHECK=1 时，ext 把标题为 AnswerTitle 的投递当作收件人选了第一个选项（发送人 SimSender）：
//	            送达后调 POST /v1/deliveries/{id}/answer，带 ack 时的 ref、sender、choice
const (
	AnswerTitle = "apa-ext-check:answer"
	SimSender   = "check-user"
	leaseTime   = 3 * time.Second
	maxWait     = 2 * time.Second // 模拟内核的长轮询上限
)

const (
	dAck, dIdem, dStale, dAnswer = "chk-ack", "chk-idem", "chk-stale", "chk-answer"
)

// Result 是一项检查的结果。
type Result struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Report 是一个 ext 的自测报告。
type Report struct {
	Ext    string   `json:"ext"`
	OK     bool     `json:"ok"`
	Checks []Result `json:"checks"`
}

type obs struct {
	deliv      domain.Delivery // 当前（最新）版本
	state      string          // queued | leased | acked
	leasedAt   time.Time
	claimed    bool
	acks       []string // 每次 ack 带的 ref
	ackBad     string   // 非法 ack 的原因
	lostFirst  bool     // d-idem：首个 ack 已按"回执丢失"处理
	stale409   bool
	afterStale int // 409 之后又收到的 ack/fail 数
	fails      int
	answered   bool
	answerBad  string
	ackRef     string
}

type sim struct {
	mu  sync.Mutex
	key string
	ds  []*obs
}

func newSim() *sim {
	s := &sim{key: "sim-" + strconv.FormatInt(clock.Now().UnixNano(), 36)}
	add := func(id, title string, opts []string) {
		s.ds = append(s.ds, &obs{state: "queued", deliv: domain.Delivery{ID: id, Item: "item-" + id, Class: "notify",
			Route: "channel:check", Version: 1, Content: domain.DeliveryContent{Title: title, Options: opts}}})
	}
	add(dAck, "check: ack", nil)
	add(dIdem, "check: idempotent", nil)
	add(dStale, "check: stale", nil)
	add(dAnswer, AnswerTitle, []string{"yes", "no"})
	return s
}

func (s *sim) get(id string) *obs {
	for _, o := range s.ds {
		if o.deliv.ID == id {
			return o
		}
	}
	return nil
}

func writeErr(w http.ResponseWriter, e *domain.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *sim) authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+s.key {
		writeErr(w, domain.NewError(domain.CodeUnauthenticated, "missing or wrong ext key", "send the ext key as a Bearer token"))
		return false
	}
	return true
}

func (s *sim) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/deliveries/next", s.next)
	mux.HandleFunc("POST /v1/deliveries/{id}/ack", s.report("ack"))
	mux.HandleFunc("POST /v1/deliveries/{id}/fail", s.report("fail"))
	mux.HandleFunc("POST /v1/deliveries/{id}/answer", s.answer)
	return mux
}

func (s *sim) next(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	wait := 0
	if v := r.URL.Query().Get("wait"); v != "" {
		var err error
		if wait, err = strconv.Atoi(v); err != nil || wait < 0 {
			writeErr(w, domain.NewError(domain.CodeInvalidRequest, "wait must be a non-negative number of seconds", "send wait=50 or omit it"))
			return
		}
	}
	deadline := clock.Now().Add(min(time.Duration(wait)*time.Second, maxWait))
	for {
		s.mu.Lock()
		for _, o := range s.ds {
			if o.state == "leased" && clock.Now().Sub(o.leasedAt) > leaseTime { // 租约超时重新可领
				o.state = "queued"
			}
		}
		for _, o := range s.ds {
			if o.state != "queued" {
				continue
			}
			o.state, o.leasedAt, o.claimed = "leased", clock.Now(), true
			claimed := o.deliv
			if claimed.ID == dStale {
				o.deliv.Version++ // 领取后事项变化，旧版本作废
			}
			s.mu.Unlock()
			writeJSON(w, claimed)
			return
		}
		s.mu.Unlock()
		if deadline.Sub(clock.Now()) <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *sim) report(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(w, r) {
			return
		}
		var in struct {
			Version     int64  `json:"version"`
			ExternalRef string `json:"external_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Version < 1 {
			writeErr(w, domain.NewError(domain.CodeInvalidRequest, "version is required", "send the version from the claimed delivery"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		o := s.get(r.PathValue("id"))
		if o == nil {
			writeErr(w, domain.NewError(domain.CodeNotFound, "delivery not found", "check the delivery id"))
			return
		}
		if in.Version != o.deliv.Version {
			if o.stale409 {
				o.afterStale++
			}
			o.stale409 = true
			writeErr(w, domain.NewError(domain.CodeConflict, "delivery version is no longer current", "do not send it; claim the next delivery"))
			return
		}
		if kind == "fail" {
			o.fails++
			o.state = "queued"
			writeJSON(w, o.deliv)
			return
		}
		if in.ExternalRef == "" {
			o.ackBad = "ack without external_ref"
			writeErr(w, domain.NewError(domain.CodeInvalidRequest, "external_ref is required", "send the platform message ref"))
			return
		}
		o.acks = append(o.acks, in.ExternalRef)
		if o.deliv.ID == dIdem && !o.lostFirst { // 回执丢失：内核已记下，但响应没到 ext
			o.lostFirst, o.state = true, "queued"
			writeErr(w, domain.NewError(domain.CodeInternal, "simulated lost receipt", "retry"))
			return
		}
		o.state, o.ackRef = "acked", in.ExternalRef
		writeJSON(w, o.deliv)
	}
}

func (s *sim) answer(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	var in struct {
		Ref    string `json:"ref"`
		Sender string `json:"sender"`
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, domain.NewError(domain.CodeInvalidRequest, "bad answer body", "send ref, sender and choice"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.get(r.PathValue("id"))
	if o == nil || o.deliv.ID != dAnswer {
		writeErr(w, domain.NewError(domain.CodeNotFound, "delivery not found or takes no answer", "check the delivery id"))
		return
	}
	bad := func(why string, e *domain.Error) {
		o.answerBad = why
		writeErr(w, e)
	}
	switch {
	case o.answered:
		writeErr(w, domain.NewError(domain.CodeConflict, "already answered", "only the first answer counts"))
	case o.state != "acked" || in.Ref != o.ackRef:
		bad("answer ref does not match the acked external_ref", domain.NewError(domain.CodeInvalidRequest, "ref does not match the delivery", "send the ref given at ack"))
	case in.Sender != SimSender:
		bad("answer sender is not the simulated recipient "+SimSender, domain.NewError(domain.CodePermissionDenied, "sender is not an allowed answerer", "forward the real answerer"))
	case !slices.Contains(o.deliv.Content.Options, in.Choice):
		bad("answer choice is not one of the options", domain.NewError(domain.CodeInvalidRequest, "choice is not an option", "send one of the options"))
	default:
		o.answered = true
		writeJSON(w, map[string]any{"id": o.deliv.Item, "replayed": false})
	}
}

func (s *sim) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(dAnswer).answered
}

// results 在 ext 停止后汇总。exited 非空表示 ext 在完成前自行退出。
func (s *sim) results(exited string) []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := func(name string, ok bool, bad string) Result {
		r := Result{Name: name, OK: ok}
		if !ok {
			r.Detail = bad
			if exited != "" {
				r.Detail += " (" + exited + ")"
			}
		}
		return r
	}
	a, i, st, an := s.get(dAck), s.get(dIdem), s.get(dStale), s.get(dAnswer)
	ackBad := "no valid ack for the first delivery"
	if a.ackBad != "" {
		ackBad = a.ackBad
	}
	idemOK := len(i.acks) >= 2 && i.acks[0] == i.acks[1]
	idemBad := "no resend after the lost receipt"
	if len(i.acks) >= 2 {
		idemBad = fmt.Sprintf("resend produced a different external_ref (%q then %q); use the delivery id as the idempotency key", i.acks[0], i.acks[1])
	}
	staleBad := "ext never reported the claimed stale delivery"
	if st.stale409 {
		staleBad = fmt.Sprintf("ext kept reporting the stale delivery %d time(s) after the 409", st.afterStale)
	}
	answerBad := "no answer was forwarded"
	if an.answerBad != "" {
		answerBad = an.answerBad
	}
	return []Result{
		res("claim", a.claimed, "no delivery was claimed with GET /deliveries/next"),
		res("ack", len(a.acks) > 0 && a.ackBad == "" && a.state == "acked", ackBad),
		res("idempotent", idemOK && i.state == "acked", idemBad),
		res("stale-version", st.stale409 && st.afterStale == 0 && st.fails == 0, staleBad),
		res("answer", an.answered, answerBad),
	}
}

// Check 起模拟内核并以其启动 ext，等到回答转达完成、ext 退出或超时，返回逐项结果。
func Check(ctx context.Context, x Info, timeout time.Duration, stderr io.Writer) Report {
	s := newSim()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p := exec.CommandContext(ctx, x.Command[0], x.Command[1:]...)
	p.Dir = x.dir
	p.Env = append(os.Environ(), "APA_URL="+srv.URL, "APA_EXT_KEY="+s.key, "APA_EXT_CHECK=1")
	p.Stderr = stderr
	p.Cancel = func() error { return p.Process.Signal(syscall.SIGTERM) }
	p.WaitDelay = 3 * time.Second
	rep := Report{Ext: x.Name}
	if err := p.Start(); err != nil {
		rep.Checks = []Result{{Name: "start", Detail: "ext could not be started: check command in ext.yaml"}}
		return rep
	}
	exit := make(chan error, 1)
	go func() { exit <- p.Wait() }()

	var exited string
	for stop := false; !stop; {
		select {
		case err := <-exit:
			exited = "ext exited early"
			if err != nil {
				exited += ": " + err.Error()
			}
			exit <- err
			stop = true
		case <-ctx.Done():
			exited = "timed out after " + timeout.String()
			stop = true
		case <-time.After(20 * time.Millisecond):
			if s.done() {
				time.Sleep(200 * time.Millisecond) // 宽限：看 ext 在完成后是否还对作废投递乱报
				stop = true
			}
		}
	}
	if s.done() {
		exited = ""
	}
	cancel()
	<-exit
	rep.Checks = s.results(exited)
	rep.OK = !slices.ContainsFunc(rep.Checks, func(r Result) bool { return !r.OK })
	return rep
}
