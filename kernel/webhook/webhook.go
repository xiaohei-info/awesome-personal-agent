// Package webhook 是 webhook 接收处理（设计 §9.5）：按能力包里的校验方案验签，原始 payload 落盘之后才回 2xx，
// 按投递 id 幂等，再异步转成标准事件交给与拉取型同一条流水线。
//
//   - 校验方案、字段映射都是能力包数据，本包只执行，不含产品分支；特殊格式走来源 config 里的转换脚本
//     （命令 + stdin JSON → stdout 标准事件 JSONL，经外呼调用器，退出码语义同拉取型）。
//   - 校验失败回 401，只记元数据，不存请求体；超限回 413、429。
//   - 落盘的记录有状态：received → transforming → processed|failed。启动时 Recover 把没处理完的重新处理。
//   - Replay 绕过投递 id 幂等，但事件仍经 (去重域, 事件 id) 去重，所以修好映射后重放不产生重复事项。
//   - payload 是外部不可信输入，全程只作资料：只被解析、映射、传给转换脚本的 stdin，不作命令、不拼命令行。
//
// 本包不碰 HTTP：传输层把请求转成 Request、把 Response 写回，只需暴露 /ingest/ 路径。
// 事件经命令模型（runner.CmdIngest）写入，不直写来源事件表。
package webhook

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 接收记录的状态。
const (
	StateReceived     = "received"     // 已落盘，等转换
	StateTransforming = "transforming" // 转换脚本已入队
	StateProcessed    = "processed"
	StateFailed       = "failed"   // 映射或脚本出错；修好后 Replay
	StateRejected     = "rejected" // 校验失败，只留元数据
)

const (
	defaultMaxBody   = 1 << 20
	defaultRatePerMn = 600
	defaultRetention = 30 * 24 * time.Hour
	transformTimeout = 30
	idPrefix         = "ing_"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "webhook", Migrations: []store.Migration{{
	Version: 1, Name: "create-ingest",
	SQL: `CREATE TABLE webhook_ingest (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	source_id TEXT NOT NULL,
	delivery_key TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	state TEXT NOT NULL,
	signed INTEGER NOT NULL,
	gen INTEGER NOT NULL,
	run_id TEXT NOT NULL,
	events INTEGER NOT NULL,
	reason TEXT NOT NULL,
	headers TEXT NOT NULL,
	body BLOB NOT NULL,
	body_size INTEGER NOT NULL,
	purged INTEGER NOT NULL,
	UNIQUE (source_id, delivery_key)
);
CREATE INDEX webhook_ingest_state ON webhook_ingest(state);`,
}}}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface{ List() []*packs.Pack }

// Dispatcher 是外呼调用器的调度入口（*proc.Dispatcher 满足它）。
type Dispatcher interface {
	Dispatch(ctx context.Context) error
}

// Options 配置 Service。Store、Exec、Packs、Auth、Secrets 必须给出。
type Options struct {
	Store   *store.Store
	Exec    *command.Executor
	Packs   PackSource
	Auth    *auth.Auth
	Secrets SecretSource
	Now     func() time.Time
	// MaxBody 是单个请求体上限（字节），RatePerMin 是每个来源每分钟的请求数上限。
	MaxBody    int
	RatePerMin int
	// Retention 是原始 payload 的保留期，默认 30 天；过期只留元数据（Expire）。
	Retention time.Duration
	// ScriptRoot 是转换脚本允许所在的目录（宿主放脚本的地方）。webhook 来源可由 delegate 登记，
	// 它写进 config 的命令不能凭此获得主机代码执行能力：可执行文件必须是 ScriptRoot 之下的绝对路径；为空则不执行转换脚本。
	ScriptRoot string
	// OnError 接收异步处理的错误；可为空。
	OnError func(error)
}

// Service 接收并处理 webhook 请求。
type Service struct {
	o    Options
	disp Dispatcher

	mu    sync.Mutex // 串行化 Process
	again atomic.Bool
	wg    sync.WaitGroup

	rmu  sync.Mutex
	rate map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func New(o Options) (*Service, error) {
	if o.Store == nil || o.Exec == nil || o.Packs == nil || o.Auth == nil || o.Secrets == nil {
		return nil, errors.New("webhook: Store, Exec, Packs, Auth and Secrets are required")
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	if o.MaxBody <= 0 {
		o.MaxBody = defaultMaxBody
	}
	if o.RatePerMin <= 0 {
		o.RatePerMin = defaultRatePerMn
	}
	if o.Retention <= 0 {
		o.Retention = defaultRetention
	}
	return &Service{o: o, rate: map[string]*window{}}, nil
}

// Attach 接上外呼调用器（转换脚本入队后靠它拉起）。
func (s *Service) Attach(d Dispatcher) { s.disp = d }

// Wait 等待已触发的异步处理结束（关停与测试用）。
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) fail(err error) {
	if err != nil && s.o.OnError != nil {
		s.o.OnError(err)
	}
}

// Request 是传输层交来的一次接收；Header 的键大小写不敏感。
type Request struct {
	SourceID string
	Header   map[string]string
	Body     []byte
}

// Response 是要写回对方的应答。
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

func reply(status int, v any) Response {
	b, _ := json.Marshal(v)
	return Response{Status: status, ContentType: "application/json", Body: b}
}

// sourceConfig 是 webhook 来源登记的 config：用哪个校验方案、哪些映射，或转换脚本。
type sourceConfig struct {
	Scheme    string   `json:"scheme"`
	Mappings  []string `json:"mappings"`
	Transform *struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	} `json:"transform"`
}

func parseConfig(src domain.Source) (c sourceConfig, err error) {
	if len(src.Config) == 0 {
		return c, nil
	}
	if err = json.Unmarshal(src.Config, &c); err != nil {
		return c, fmt.Errorf("source %s config: %w", src.ID, err)
	}
	return c, nil
}

func (s *Service) scheme(id string) (packs.WebhookScheme, bool) {
	for _, p := range s.o.Packs.List() {
		for _, sc := range p.WebhookSchemes {
			if sc.ID == id {
				return sc, true
			}
		}
	}
	return packs.WebhookScheme{}, false
}

func (s *Service) mapping(id string) (packs.WebhookMapping, bool) {
	for _, p := range s.o.Packs.List() {
		for _, m := range p.WebhookMaps {
			if m.ID == id {
				return m, true
			}
		}
	}
	return packs.WebhookMapping{}, false
}

func (s *Service) allow(source string, now time.Time) bool {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	w := s.rate[source]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.rate[source] = w
	}
	w.n++
	return w.n <= s.o.RatePerMin
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return idPrefix + hex.EncodeToString(b[:]), nil
}

// dropped 是不落盘的请求头：凭据不进账本。
var dropped = map[string]bool{"authorization": true, "cookie": true, "proxy-authorization": true}

func lower(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = v
	}
	return out
}

// Receive 处理一次 webhook 请求。落盘成功之后才返回 2xx；其余情况对方会（或应该）重试。
func (s *Service) Receive(ctx context.Context, req Request) Response {
	now := s.o.Now()
	var src domain.Source
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { src, err = registry.Get(tx, req.SourceID); return })
	if err != nil || src.Type != domain.SourceWebhook || !src.Enabled {
		return reply(404, map[string]string{"error": "unknown source"})
	}
	if !s.allow(src.ID, now) {
		return reply(429, map[string]string{"error": "rate limit exceeded"})
	}
	if len(req.Body) > s.o.MaxBody {
		return reply(413, map[string]string{"error": "request body too large"})
	}
	cfg, err := parseConfig(src)
	if err != nil {
		return reply(500, map[string]string{"error": "source is misconfigured"})
	}
	sc, ok := s.scheme(cfg.Scheme)
	if !ok {
		return reply(500, map[string]string{"error": "source is misconfigured"})
	}
	h := lower(req.Header)

	var signed bool
	var why string
	err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) { signed, why, err = s.verify(tx, sc, src.ID, h, req.Body, now); return })
	if errors.Is(err, errUnauthenticated) {
		s.reject(ctx, src.ID, h, len(req.Body), why, now)
		return reply(401, map[string]string{"error": "unauthenticated"})
	}
	if err != nil {
		s.fail(err)
		return reply(500, map[string]string{"error": "internal error"})
	}
	// 平台的 URL 验证握手由方案同步应答，不落盘。
	if hs := sc.Handshake; hs != nil {
		c := newExprCtx(h, req.Body)
		if ok, _ := c.matches(map[string]any{"json_path": hs.When["json_path"], "equals": hs.When["equals"]}); ok && hs.When["json_path"] != nil {
			v, _ := c.lookup(fmt.Sprint(hs.Respond["json_path"]))
			return Response{Status: 200, ContentType: "text/plain", Body: []byte(v)}
		}
	}
	key := h[strings.ToLower(sc.DeliveryIDHeader)]
	if sc.DeliveryIDHeader == "" || key == "" {
		sum := sha256.Sum256(req.Body) // 对方不提供投递 id：用 payload 哈希
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	id, err := newID()
	if err != nil {
		s.fail(err)
		return reply(500, map[string]string{"error": "internal error"})
	}
	keep := map[string]string{}
	for k, v := range h {
		if !dropped[k] {
			keep[k] = v
		}
	}
	hj, _ := json.Marshal(keep)
	var dup bool
	err = s.o.Store.Update(ctx, func(tx *store.Tx) error {
		n, err := tx.Exec(`INSERT OR IGNORE INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
			VALUES (?,?,?,?,?,?,1,'',0,'',?,?,?,0)`, id, src.ID, key, now.UnixNano(), StateReceived, b2i(signed), string(hj), req.Body, int64(len(req.Body)))
		if err != nil {
			return err
		}
		if dup = n == 0; dup {
			r, err := tx.Query(`SELECT id FROM webhook_ingest WHERE source_id = ? AND delivery_key = ?`, src.ID, key)
			if err != nil {
				return err
			}
			defer r.Close()
			if r.Next() {
				return r.Scan(&id)
			}
			return r.Err()
		}
		return nil
	})
	if err != nil { // 没落盘就不回 2xx：对方会重试
		s.fail(err)
		return reply(500, map[string]string{"error": "not persisted"})
	}
	s.kick()
	return reply(200, map[string]any{"status": "accepted", "id": id, "duplicate": dup})
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// reject 只记元数据：不存请求体，也不占投递键。
func (s *Service) reject(ctx context.Context, source string, h map[string]string, size int, why string, now time.Time) {
	id, err := newID()
	if err == nil {
		err = s.o.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
				VALUES (?,?,?,?,?,0,1,'',0,?,'{}',x'',?,1)`, id, source, "rejected:"+id, now.UnixNano(), StateRejected, why, int64(size))
			return err
		})
	}
	s.fail(err)
}

// kick 在回包之后异步处理已落盘的记录。
func (s *Service) kick() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.fail(s.Process(context.Background()))
	}()
}

// Ingest 是一条接收记录（排查用：原始 payload、校验结果与转换出的事件数）。
type Ingest struct {
	ID          string
	SourceID    string
	DeliveryKey string
	ReceivedAt  time.Time
	State       string
	Signed      bool
	Gen         int
	RunID       string
	Events      int
	Reason      string
	Headers     map[string]string
	Body        []byte
	BodySize    int
	Purged      bool
}

const ingestCols = `id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged`

func queryIngests(tx *store.Tx, where string, args ...any) ([]Ingest, error) {
	rows, err := tx.Query(`SELECT `+ingestCols+` FROM webhook_ingest `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ingest
	for rows.Next() {
		var (
			g                                   Ingest
			at, signed, gen, events, size, purg int64
			hj                                  string
		)
		if err := rows.Scan(&g.ID, &g.SourceID, &g.DeliveryKey, &at, &g.State, &signed, &gen, &g.RunID, &events, &g.Reason, &hj, &g.Body, &size, &purg); err != nil {
			return nil, err
		}
		g.ReceivedAt, g.Signed, g.Gen, g.Events, g.BodySize, g.Purged = time.Unix(0, at).UTC(), signed != 0, int(gen), int(events), int(size), purg != 0
		if err := json.Unmarshal([]byte(hj), &g.Headers); err != nil {
			return nil, fmt.Errorf("webhook: ingest %s headers: %w", g.ID, err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Ingests 按接收顺序列出某来源的接收记录（空 sourceID 为全部）。
func Ingests(tx *store.Tx, sourceID string) ([]Ingest, error) {
	if sourceID == "" {
		return queryIngests(tx, "")
	}
	return queryIngests(tx, "WHERE source_id = ?", sourceID)
}

// IngestByID 读取一条接收记录；不存在返回 not_found。
func IngestByID(tx *store.Tx, id string) (Ingest, error) {
	gs, err := queryIngests(tx, "WHERE id = ?", id)
	if err != nil {
		return Ingest{}, err
	}
	if len(gs) == 0 {
		return Ingest{}, domain.NewError(domain.CodeNotFound, "ingest "+id+" not found", "list ingests and retry")
	}
	return gs[0], nil
}

// Recover 启动时调用：转换到一半丢失的记录退回 received，并重新处理所有已落盘但未处理的记录。
func (s *Service) Recover(ctx context.Context) error {
	if err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE webhook_ingest SET state = ?, run_id = '' WHERE state = ?`, StateReceived, StateTransforming)
		return err
	}); err != nil {
		return err
	}
	return s.Process(ctx)
}

// Process 处理所有 received 的记录。单条失败不影响其他条，错误合并返回。
// 并发调用只留下"还要再扫一遍"的标记，由正在运行的那次接着处理；标记必须先于抢锁写下，
// 持锁者释放锁之后再检查它，这样每次调用之后至少有一次完整扫描，落盘的记录不会停在 received。
func (s *Service) Process(ctx context.Context) error {
	var errs []error
	for {
		s.again.Store(true)
		if !s.mu.TryLock() {
			return errors.Join(errs...)
		}
		errs = append(errs, s.drain(ctx)...)
		s.mu.Unlock()
		if !s.again.Load() {
			return errors.Join(errs...)
		}
	}
}

func (s *Service) drain(ctx context.Context) (errs []error) {
	for {
		s.again.Store(false)
		var todo []Ingest
		if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { todo, err = queryIngests(tx, "WHERE state = ?", StateReceived); return }); err != nil {
			return append(errs, err)
		}
		for _, g := range todo {
			if err := s.one(ctx, g); err != nil {
				errs = append(errs, fmt.Errorf("webhook: ingest %s: %w", g.ID, err))
			}
		}
		// 失败的记录保持 received，等下次触发；只有处理期间又有新的触发才再扫一轮。
		if !s.again.Load() {
			return errs
		}
	}
}

func (s *Service) settle(ctx context.Context, g Ingest, state, reason string, events int) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE webhook_ingest SET state = ?, reason = ?, events = ?, run_id = '' WHERE id = ? AND gen = ?`, state, reason, int64(events), g.ID, int64(g.Gen))
		return err
	})
}

// one 处理一条记录：映射同步完成，转换脚本入队后等 Handle。无法处理的原因记在记录上（failed），等修好后 Replay。
func (s *Service) one(ctx context.Context, g Ingest) error {
	var src domain.Source
	if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { src, err = registry.Get(tx, g.SourceID); return }); err != nil {
		return s.settle(ctx, g, StateFailed, "source: "+err.Error(), 0)
	}
	cfg, err := parseConfig(src)
	if err != nil {
		return s.settle(ctx, g, StateFailed, err.Error(), 0)
	}
	if cfg.Transform != nil {
		return s.enqueueTransform(ctx, g, src, cfg)
	}
	evs, err := s.mapAll(cfg, g)
	if err != nil {
		return s.settle(ctx, g, StateFailed, err.Error(), 0)
	}
	return s.ingest(ctx, g, evs)
}

func (s *Service) mapAll(cfg sourceConfig, g Ingest) ([]runner.IngestEvent, error) {
	c := newExprCtx(g.Headers, g.Body)
	for _, id := range cfg.Mappings {
		m, ok := s.mapping(id)
		if !ok {
			return nil, fmt.Errorf("mapping %q is not in any pack", id)
		}
		hit, err := c.matches(m.Match)
		if err != nil {
			return nil, fmt.Errorf("mapping %s: %w", id, err)
		}
		if !hit {
			continue
		}
		ev, err := mapEvent(m, c, g.Body, g.DeliveryKey, g.ReceivedAt, g.Signed)
		if err != nil {
			return nil, fmt.Errorf("mapping %s: %w", id, err)
		}
		return []runner.IngestEvent{ev}, nil
	}
	return nil, errors.New("no mapping matched this payload")
}

// ingest 经命令模型把事件写进来源事件表（与拉取型同一流水线），再标记 processed。幂等键含代数，重放换新键。
func (s *Service) ingest(ctx context.Context, g Ingest, evs []runner.IngestEvent) error {
	in, err := json.Marshal(runner.IngestInput{SourceID: g.SourceID, RunID: g.ID, Events: evs})
	if err != nil {
		return err
	}
	if _, err := s.o.Exec.Do(ctx, command.Request{Identity: runner.Internal, Command: runner.CmdIngest, Input: in,
		IdemKey: fmt.Sprintf("webhook:%s:%d", g.ID, g.Gen), Evidence: "webhook " + g.ID, RuleID: ""}); err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeInvalidRequest { // 事件不合标准：数据问题，不是临时故障
			return s.settle(ctx, g, StateFailed, err.Error(), 0)
		}
		return err // 临时错误：保持 received，下次处理或启动恢复时重试
	}
	return s.settle(ctx, g, StateProcessed, "", len(evs))
}

type transformStdin struct {
	SourceID   string            `json:"source_id"`
	IngestID   string            `json:"ingest_id"`
	Config     json.RawMessage   `json:"config"`
	Header     map[string]string `json:"header"`
	Body       string            `json:"body"`
	Payload    json.RawMessage   `json:"payload,omitempty"`
	ReceivedAt string            `json:"received_at"`
}

func (s *Service) enqueueTransform(ctx context.Context, g Ingest, src domain.Source, cfg sourceConfig) error {
	argv := strings.Fields(cfg.Transform.Command)
	if len(argv) == 0 {
		return s.settle(ctx, g, StateFailed, "transform command is empty", 0)
	}
	if exe := filepath.Clean(argv[0]); s.o.ScriptRoot == "" || !filepath.IsAbs(exe) || !strings.HasPrefix(exe, filepath.Clean(s.o.ScriptRoot)+string(filepath.Separator)) {
		return s.settle(ctx, g, StateFailed, "transform command must be an absolute path under the host script directory", 0)
	}
	timeout := cfg.Transform.Timeout
	if timeout <= 0 {
		timeout = transformTimeout
	}
	doc := transformStdin{SourceID: src.ID, IngestID: g.ID, Config: src.Config, Header: g.Headers, Body: string(g.Body), ReceivedAt: g.ReceivedAt.Format(time.RFC3339)}
	if json.Valid(g.Body) {
		doc.Payload = g.Body
	}
	if len(doc.Config) == 0 {
		doc.Config = json.RawMessage(`{}`)
	}
	stdin, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	// 每次入队用新的 run id：重试与崩溃恢复后的再入队不会撞上旧 Run；Handle 只认记录上当前的 run_id。
	runID, err := newID()
	if err != nil {
		return err
	}
	runID = "run_" + strings.TrimPrefix(runID, idPrefix)
	if err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{ID: runID, Category: domain.RunTransform, Target: g.ID, TimeoutSec: timeout},
			Spec: proc.Spec{Command: argv, Stdin: stdin}, Explicit: g.Gen > 1}, s.o.Now()); err != nil { // Gen 首次为 1，Replay 才会加；本人重放在短任务队列内优先（§6.9）
			return err
		}
		_, err := tx.Exec(`UPDATE webhook_ingest SET state = ?, run_id = ? WHERE id = ? AND gen = ?`, StateTransforming, runID, g.ID, int64(g.Gen))
		return err
	}); err != nil {
		return err
	}
	if s.disp != nil {
		return s.disp.Dispatch(ctx)
	}
	return nil
}

// Handle 处置转换脚本 Run 的终态，作为外呼调用器 OnResult 的一环；不是转换 Run 的结果直接忽略。
// 退出码语义同拉取型：0 成功；1、超时等临时错误保持 received 待下次重试；2、3 记 failed 等人修好后 Replay。
func (s *Service) Handle(ctx context.Context, res proc.Result) error {
	run := res.Run
	if run.Category != domain.RunTransform || !strings.HasPrefix(run.Target, idPrefix) {
		return nil
	}
	var g Ingest
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		gs, err := queryIngests(tx, "WHERE id = ? AND run_id = ? AND state = ?", run.Target, run.ID, StateTransforming)
		if len(gs) == 1 {
			g = gs[0]
		}
		return err
	}); err != nil || g.ID == "" { // 过期的 Run（记录已被恢复或重放）：忽略
		return err
	}
	exit := -1
	if run.ExitCode != nil {
		exit = *run.ExitCode
	}
	switch {
	case run.State == domain.RunSucceeded:
		if res.Overflow {
			return s.settle(ctx, g, StateFailed, "script output exceeds max_source_output_mb", 0)
		}
		evs, rejects := runner.ParseEvents(res.Stdout)
		if len(rejects) > 0 && len(evs) == 0 {
			return s.settle(ctx, g, StateFailed, fmt.Sprintf("script output line %d: %s", rejects[0].Line, rejects[0].Reason), 0)
		}
		return s.ingest(ctx, g, evs)
	case exit == 2 || exit == 3:
		return s.settle(ctx, g, StateFailed, fmt.Sprintf("transform exit status %d", exit), 0)
	default:
		return s.settleRetry(ctx, g, "transform failed: "+orElse(res.Reason, fmt.Sprintf("exit status %d", exit)))
	}
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// settleRetry 退回 received 等重试，原因留在记录上；不立即重跑，避免坏脚本空转。
func (s *Service) settleRetry(ctx context.Context, g Ingest, reason string) error {
	return s.settle(ctx, g, StateReceived, reason, 0)
}

// Replay 重新处理一条已落盘的记录（apa source replay）：绕过投递 id 幂等，不绕过事件去重。
func (s *Service) Replay(ctx context.Context, ingestID string) error {
	err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		gs, err := queryIngests(tx, "WHERE id = ?", ingestID)
		if err != nil {
			return err
		}
		switch {
		case len(gs) == 0:
			return domain.NewError(domain.CodeNotFound, "ingest "+ingestID+" not found", "list ingests and retry")
		case gs[0].State == StateRejected || gs[0].Purged:
			return domain.NewError(domain.CodeConflict, "ingest "+ingestID+" has no payload to replay", "the request was rejected or its payload expired")
		case gs[0].State == StateTransforming:
			return domain.NewError(domain.CodeConflict, "ingest "+ingestID+" is being transformed", "wait for it to finish")
		}
		_, err = tx.Exec(`UPDATE webhook_ingest SET state = ?, gen = gen + 1, reason = '', run_id = '' WHERE id = ?`, StateReceived, ingestID)
		return err
	})
	if err != nil {
		return err
	}
	return s.Process(ctx)
}

// Expire 清掉超过保留期的原始 payload 与请求头，只留元数据。
func (s *Service) Expire(ctx context.Context) error {
	cut := s.o.Now().Add(-s.o.Retention).UnixNano()
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE webhook_ingest SET body = x'', headers = '{}', purged = 1
			WHERE purged = 0 AND received_at < ? AND state IN (?,?,?)`, cut, StateProcessed, StateFailed, StateRejected)
		return err
	})
}
