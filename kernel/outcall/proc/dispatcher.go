package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// tailLimit：事件流以外的 stdout 与所有 stderr 交回调用模块的只是末段——任务型的结构化摘要与回执兜底在 stdout 末段（§8.4），
// 判定只需要末段。但末段不是全文：简报这类长正文任务的正文可能在前面，只留末段等于把正文丢掉，而且以前丢得不留痕。
// 所以有运行目录的运行，stdout 另按 segmentLimit 整份分段落盘（output.001.txt、output.002.txt……，按字节切，依次拼接即全文），
// 交回的末段带总字节与丢掉的字节数，由调用模块在给人看的那份里写明截断与全文位置。
const (
	tailLimit    = 64 << 10
	segmentLimit = 64 << 10
)

// SegmentName 是运行目录里 stdout 全文第 n 段（从 1 起）的文件名。
func SegmentName(n int) string { return fmt.Sprintf("output.%03d.txt", n) }

// envWhitelist 是子进程能从内核环境继承的全部变量（§7.7）：PATH、HOME 和 XDG 目录变量及基本区域设置。
var envWhitelist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
	"XDG_RUNTIME_DIR": true, "XDG_CONFIG_DIRS": true, "XDG_DATA_DIRS": true,
}

// EnvRunContext 是运行上下文文件路径的环境变量（§7.7）。路径不是凭据；名字里不含凭据字样，
// 否则按名称过滤环境的 harness 不会把它带到工具侧（附录 A.5）。
const EnvRunContext = "APA_RUN_CONTEXT"

// RunMark 是追加在运行 PATH 末尾的标记条目的文件名，只用来判定"这是 APA 拉起的运行"（§7.7）：harness 不放行
// APA_RUN_CONTEXT 时路径到不了工具侧，PATH 却总会带到，CLI 和插件据此拒绝，不退回常驻进程自己的凭据。
// 目录不存在，放在末尾，也不会影响命令查找。
const RunMark = ".apa-run"

// Result 是一次运行结束后交回调用模块的结果；本包不据此发任何通知或事件。
type Result struct {
	Run    domain.Run // 已落账的终态
	Stdout []byte     // 事件流类别（eventStream）是上限内的全部完整行；其余类别是末段
	Stderr []byte     // 末段
	Reason string     // 失败、超时、关停的原因；成功为空
	// Overflow 为真表示事件流类别的 stdout 超过单次总上限（max_source_output_mb），超出部分已丢弃。
	Overflow bool
	// StdoutDropped 是只留末段的 stdout 丢掉的开头字节数；StdoutTotal 是子进程写出的 stdout 总字节数（只留末段的类别才有）。
	StdoutDropped, StdoutTotal int64
	// StdoutFiles 是 stdout 全文在运行目录里的分段文件名（有丢掉的开头且有运行目录时才有）；StdoutSpoolErr 是落盘失败的原因。
	StdoutFiles    []string
	StdoutSpoolErr string
}

// Options 是调度器的依赖与扩展点；零值字段取默认。
type Options struct {
	CheckAgent func(*store.Tx, domain.Run) error
	// Environ 返回内核环境，默认 os.Environ；只有白名单变量会传给子进程。
	Environ func() []string
	// Env 返回某次运行额外注入的变量（APA_URL、APA_TOKEN、APA_PROJECT 等）。
	// 在启动时调用而不入账本；运行凭据只落在运行上下文文件里（ContextDir），随运行删除。
	Env func(domain.Run) map[string]string
	// ContextDir 是运行上下文文件的目录（权限 0700，§7.7）；空表示不写。
	ContextDir string
	// OnResult 在终态提交后被调用（可并发）。
	OnResult func(Result)
	// Now 默认 clock.Now。
	Now func() time.Time
}

// Dispatcher 按两个队列的额度拉起子进程。额度从账本里 running 的 Run 数得出，
// 重启后仍存活的进程继续占额度。
type Dispatcher struct {
	st          *store.Store
	limits      map[Queue]int
	maxOutput   int // 事件流类别单次 stdout 的总上限（字节）
	opt         Options
	customClock bool // 仅在测试注入时钟或 apa_testclock 构建为真，生产构建恒为假

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex // 串行化选取，保证不超额
}

// New 用 config 里的两个额度创建调度器。
func New(st *store.Store, cfg config.Config, opt Options) *Dispatcher {
	customClock := opt.Now != nil || clock.TestClockBuild
	if opt.Environ == nil {
		opt.Environ = os.Environ
	}
	if opt.Now == nil {
		opt.Now = clock.Now
	}
	if cfg.MaxSourceOutput == 0 {
		cfg.MaxSourceOutput = config.Default().MaxSourceOutput
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{st: st, opt: opt, customClock: customClock, ctx: ctx, cancel: cancel, maxOutput: cfg.MaxSourceOutput,
		limits: map[Queue]int{QueueLong: cfg.MaxSubprocesses, QueueShort: cfg.MaxShortSubprocesses}}
}

// Recover 在启动时调用一次：进程组已不存在的 running 按失败处理；仍存活的保持 running 并返回，
// 由调用方接管（它们继续占额度）。queued 的 Run 不动，随后 Dispatch 会重新执行。
// 外部 Run 的进程不在本机，内核重启与它无关，不在此判定。
func (d *Dispatcher) Recover(ctx context.Context) (failed []domain.Run, alive []domain.Run, err error) {
	var gone []Record
	err = d.st.Update(ctx, func(tx *store.Tx) error {
		failed, alive, gone = nil, nil, nil
		recs, err := query(tx, "WHERE state = 'running' AND queue != ? ORDER BY seq", string(QueueExternal))
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.PGID > 0 && groupAlive(rec.PGID) {
				alive = append(alive, rec.Run)
				continue
			}
			if err := Settle(tx, rec.Run.ID, domain.RunFailed, nil, reasonRestarted, d.opt.Now()); err != nil {
				return err
			}
			got, err := Get(tx, rec.Run.ID)
			if err != nil {
				return err
			}
			failed = append(failed, got.Run)
			gone = append(gone, rec)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range gone {
		d.removeContext(rec)
	}
	return failed, alive, nil
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Dispatch 在额度内把 queued 的 Run 拉起：本人显式触发的先于其他，同级按入队顺序。
// 入队事务提交后、以及每个运行结束后都要调用（结束时调度器自己会调用）。
// 顺带把到期的外部 Run 按超时落账。
func (d *Dispatcher) Dispatch(ctx context.Context) error {
	if err := d.expire(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, q := range []Queue{QueueLong, QueueShort} {
		for d.ctx.Err() == nil {
			rec, ok, err := d.claim(ctx, q)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			d.wg.Add(1)
			go d.run(rec)
		}
	}
	return nil
}

// expire 把超过运行超时的外部 Run 按超时落账并交回调用模块；没有进程可杀。在锁外调用 OnResult，
// 调用模块收尾时可以再入队、再 Dispatch。
// shortcut: 只在 Dispatch 时检查，落账最多晚一个维护间隔；租约与运行凭据的到期自己按时判定，不受影响。
func (d *Dispatcher) expire(ctx context.Context) error {
	var done []Result
	err := d.st.Update(ctx, func(tx *store.Tx) error {
		done = nil
		now := d.opt.Now()
		recs, err := query(tx, "WHERE queue = ? AND state = 'running' AND started_at + timeout * 1000000000 <= ? ORDER BY seq",
			string(QueueExternal), now.UnixNano())
		if err != nil {
			return err
		}
		for _, rec := range recs {
			why := fmt.Sprintf("timeout after %ds", rec.Run.TimeoutSec)
			if err := Settle(tx, rec.Run.ID, domain.RunTimeout, nil, why, now); err != nil {
				return err
			}
			got, err := Get(tx, rec.Run.ID)
			if err != nil {
				return err
			}
			done = append(done, Result{Run: got.Run, Reason: why})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if d.opt.OnResult != nil {
		for _, res := range done {
			d.opt.OnResult(res)
		}
	}
	return nil
}

// claim 在一个事务内检查额度并把下一条 queued 置为 running。
func (d *Dispatcher) claim(ctx context.Context, q Queue) (rec Record, ok bool, err error) {
	err = d.st.Update(ctx, func(tx *store.Tx) error {
		ok = false
		running, err := query(tx, "WHERE queue = ? AND state = 'running'", string(q))
		if err != nil || len(running) >= d.limits[q] {
			return err
		}
		next, err := query(tx, "WHERE queue = ? AND state = 'queued' ORDER BY explicit DESC, seq LIMIT 1", string(q))
		if err != nil || len(next) == 0 {
			return err
		}
		rec = next[0]
		now := d.opt.Now()
		if _, err := tx.Exec("UPDATE outcall_run SET state = 'running', started_at = ? WHERE id = ?",
			now.UnixNano(), rec.Run.ID); err != nil {
			return err
		}
		rec.Run.StartedAt = &now
		ok = true
		return nil
	})
	return rec, ok, err
}

// Close 关停：杀掉所有运行中的进程组并按失败落账（原因 shutdown），等待它们结束。queued 的 Run 保持原样。
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
}

func (d *Dispatcher) environ(rec Record) ([]string, error) {
	var env []string
	for _, kv := range d.opt.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && envWhitelist[k] {
			env = append(env, kv)
		}
	}
	env = append(env, "APA_RUN="+rec.Run.ID)
	for key, value := range rec.Spec.OptionEnv {
		env = append(env, key+"="+value)
	}
	var extra map[string]string
	if d.opt.Env != nil {
		extra = d.opt.Env(rec.Run)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	if rec.Spec.RunDir != "" && d.opt.ContextDir != "" && extra["APA_TOKEN"] != "" { // 没签出凭据就没有运行身份，工具侧按取不到拒绝
		path, err := d.writeContext(rec, extra)
		if err != nil {
			return nil, err
		}
		env = append(env, EnvRunContext+"="+path)
		env = markRun(env, filepath.Join(d.opt.ContextDir, RunMark))
	}
	return env, nil
}

func markRun(env []string, mark string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = kv + string(os.PathListSeparator) + mark
			return env
		}
	}
	return append(env, "PATH="+mark)
}

// runContext 是运行上下文文件的内容（§7.7）：harness 的工具若不在内核拉起的进程里执行，拿不到这个进程的环境和
// 工作目录，靠它知道本次运行的身份与目录。这两个目录只有这一个来源。
type runContext struct {
	Run     string `json:"run"`
	URL     string `json:"url"`
	Token   string `json:"token"`
	Project string `json:"project,omitempty"`
	RunDir  string `json:"run_dir"`
	WorkDir string `json:"work_dir"`
}

// writeContext 原子写入（续接覆盖时读方看不到半个文件），CreateTemp 已是 0600；返回路径。
func (d *Dispatcher) writeContext(rec Record, env map[string]string) (string, error) {
	b, err := json.Marshal(runContext{Run: rec.Run.ID, URL: env["APA_URL"], Token: env["APA_TOKEN"], Project: env["APA_PROJECT"],
		RunDir: rec.Spec.RunDir, WorkDir: rec.Spec.Dir})
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(d.opt.ContextDir, ".context-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(d.opt.ContextDir, rec.ContextName())
	return path, os.Rename(tmp.Name(), path)
}

// removeContext 在运行结束时删文件；文件里的凭据此时已随运行失效，删不掉也只是留下一份废凭据。
func (d *Dispatcher) removeContext(rec Record) {
	if rec.Spec.RunDir != "" && d.opt.ContextDir != "" {
		os.Remove(filepath.Join(d.opt.ContextDir, rec.ContextName()))
	}
}

// run 执行一条已置为 running 的 Run 并落账终态。
func (d *Dispatcher) run(rec Record) {
	defer d.wg.Done()
	errb := tail{limit: tailLimit}
	var out stdoutSink = &tail{limit: tailLimit, dir: rec.Spec.RunDir}
	if rec.Spec.StdoutLimit > 0 {
		out = &rawBytes{limit: rec.Spec.StdoutLimit}
	} else if eventStream(rec.Run.Category) {
		out = &whole{limit: d.maxOutput}
	}
	state, exit, reason := d.exec(rec, out, &errb)
	d.removeContext(rec)
	stdout, overflow := out.result()
	res := Result{Stdout: stdout, Stderr: errb.bytes(), Reason: reason, Overflow: overflow}
	if t, ok := out.(*tail); ok {
		res.StdoutTotal, res.StdoutDropped, res.StdoutFiles, res.StdoutSpoolErr = t.finish()
	}
	// 落账不能被关停取消，否则 Run 会卡在 running。
	err := d.st.Update(context.Background(), func(tx *store.Tx) error {
		if err := Settle(tx, rec.Run.ID, state, exit, reason, d.opt.Now()); err != nil {
			return err
		}
		got, err := Get(tx, rec.Run.ID)
		res.Run = got.Run
		return err
	})
	if err != nil {
		res.Run, res.Reason = rec.Run, fmt.Sprintf("settle run: %v", err)
	}
	if d.opt.OnResult != nil {
		d.opt.OnResult(res)
	}
	if d.ctx.Err() == nil {
		d.Dispatch(context.Background()) // 额度释放，补位；失败时下次入队或恢复会再试
	}
}

func (d *Dispatcher) exec(rec Record, stdout, stderr io.Writer) (domain.RunState, *int, string) {
	if d.opt.CheckAgent != nil && rec.Run.AgentSnapshot != nil {
		if err := d.st.View(d.ctx, func(tx *store.Tx) error { return d.opt.CheckAgent(tx, rec.Run) }); err != nil {
			return domain.RunFailed, nil, err.Error()
		}
	}
	spec := rec.Spec
	if spec.Dir != "" { // 工作目录不存在时 Start 只报命令 no such file，原因要指向目录
		if _, err := os.Stat(spec.Dir); err != nil {
			return domain.RunFailed, nil, "start: work dir: " + err.Error()
		}
	}
	env, err := d.environ(rec)
	if err != nil {
		return domain.RunFailed, nil, "start: run context: " + err.Error()
	}
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 自成进程组，超时才能连子孙一起杀
	cmd.WaitDelay = 2 * time.Second                       // 孙进程继承管道时不让 Wait 无限挂住
	if err := cmd.Start(); err != nil {
		return domain.RunFailed, nil, "start: " + err.Error()
	}
	pgid := cmd.Process.Pid
	if err := d.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE outcall_run SET pgid = ? WHERE id = ?", int64(pgid), rec.Run.ID)
		return err
	}); err != nil {
		syscall.Kill(-pgid, syscall.SIGKILL)
		cmd.Wait()
		return domain.RunFailed, nil, "record process group: " + err.Error()
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timeout := time.Duration(rec.Run.TimeoutSec) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var killed domain.RunState // 非空表示是我们杀的
	var why string
	var werr error
	// 任务型与后台运行共用同一个时限（§13.6），测试时钟下也按同一个口径判到期；其余类别按真实计时。
	if !d.customClock || (rec.Run.Category != domain.RunBackground && rec.Run.Category != domain.RunTask && rec.Run.Category != domain.RunSourceBatch) {
		select {
		case werr = <-done:
		case <-timer.C:
			killed, why = domain.RunTimeout, fmt.Sprintf("timeout after %ds", rec.Run.TimeoutSec)
		case <-d.ctx.Done():
			killed, why = domain.RunFailed, reasonShutdown
		}
	} else {
		start := d.opt.Now()
		if rec.Run.StartedAt != nil {
			start = *rec.Run.StartedAt
		}
		deadline := start.Add(timeout)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			if !d.opt.Now().Before(deadline) {
				killed, why = domain.RunTimeout, fmt.Sprintf("timeout after %ds", rec.Run.TimeoutSec)
				goto waited
			}
			select {
			case werr = <-done:
				goto waited
			case <-timer.C:
				killed, why = domain.RunTimeout, fmt.Sprintf("timeout after %ds", rec.Run.TimeoutSec)
				goto waited
			case <-tick.C:
			case <-d.ctx.Done():
				killed, why = domain.RunFailed, reasonShutdown
				goto waited
			}
		}
	}
waited:
	if killed != "" {
		syscall.Kill(-pgid, syscall.SIGKILL)
		werr = <-done
		code := exitCode(werr)
		return killed, &code, why
	}
	code := exitCode(werr)
	var ee *exec.ExitError
	switch {
	case werr == nil:
		return domain.RunSucceeded, &code, ""
	case errors.As(werr, &ee):
		// stderr 的最后一行随失败附注落账（不进事件载荷与 /runs），否则"为什么没跑起来"只剩一个退出码。
		return domain.RunFailed, &code, withStderr(fmt.Sprintf("exit status %d", code), stderr)
	default: // 管道错误或 WaitDelay 到期等
		return domain.RunFailed, &code, "wait: " + werr.Error()
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() // 被信号杀死时为 -1
	}
	if err != nil {
		return -1
	}
	return 0
}

// stdoutSink 按流接收子进程的 stdout，运行结束后交出调用模块要的内容。
type stdoutSink interface {
	io.Writer
	result() (out []byte, overflow bool)
}

// eventStream 报告该类别的 stdout 是否是逐行的标准事件（§9.2、§9.5）：每一行都要交回调用模块，不能只看末段。
func eventStream(c domain.RunCategory) bool {
	return c == domain.RunSource || c == domain.RunTransform
}

// whole 保留 limit 字节内的全部输出；超过后只丢不存，子进程不会因管道写满而阻塞，由超时兜底它的运行时长。
type whole struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	over  bool
}

func (w *whole) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := p
	if room := w.limit - len(w.buf); len(p) > room {
		keep, w.over = p[:max(room, 0)], true
	}
	w.buf = append(w.buf, keep...)
	return len(p), nil
}

// result 超限时切掉末尾的半行，调用模块拿到的都是完整行。
func (w *whole) result() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.over {
		return append([]byte(nil), w.buf...), false
	}
	return append([]byte(nil), w.buf[:bytes.LastIndexByte(w.buf, '\n')+1]...), true
}

// rawBytes 保留 limit 字节内的全部输出，超过后只丢不存并标记溢出；不做任何行处理，二进制内容原样交回。
type rawBytes struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	over  bool
}

func (w *rawBytes) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := p
	if room := w.limit - len(w.buf); len(p) > room {
		keep, w.over = p[:max(room, 0)], true
	}
	w.buf = append(w.buf, keep...)
	return len(p), nil
}

func (w *rawBytes) result() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...), w.over
}

// tail 只保留最后 limit 字节，并记下总共写了多少、丢了多少；dir 非空时另把全文按 segmentLimit 分段写进 dir。
// 落盘失败不阻塞子进程（管道照常读完），只记下原因、停止落盘。
type tail struct {
	mu             sync.Mutex
	buf            []byte
	limit          int
	total, dropped int64
	dir            string
	seg            *os.File
	segs           []string
	segBytes       int
	spoolErr       error
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spool(p)
	t.buf = append(t.buf, p...)
	t.total += int64(len(p))
	if n := len(t.buf) - t.limit; n > 0 {
		t.buf = append([]byte(nil), t.buf[n:]...)
		t.dropped += int64(n)
	}
	return len(p), nil
}

func (t *tail) spool(p []byte) {
	for len(p) > 0 && t.dir != "" && t.spoolErr == nil {
		if t.seg == nil || t.segBytes == segmentLimit {
			if t.seg != nil {
				t.spoolErr = t.seg.Close()
			}
			name := SegmentName(len(t.segs) + 1)
			if t.seg, t.spoolErr = os.OpenFile(filepath.Join(t.dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); t.spoolErr != nil {
				t.seg = nil
				return
			}
			t.segs, t.segBytes = append(t.segs, name), 0
		}
		n := min(len(p), segmentLimit-t.segBytes)
		if _, t.spoolErr = t.seg.Write(p[:n]); t.spoolErr != nil {
			return
		}
		t.segBytes += n
		p = p[n:]
	}
}

// finish 关掉分段文件；没有丢掉开头时末段就是全文，分段文件删掉，运行目录里不留两份。
func (t *tail) finish() (total, dropped int64, files []string, spoolErr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seg != nil {
		if err := t.seg.Close(); err != nil && t.spoolErr == nil {
			t.spoolErr = err
		}
		t.seg = nil
	}
	if t.dropped == 0 {
		for _, n := range t.segs {
			os.Remove(filepath.Join(t.dir, n))
		}
		return t.total, 0, nil, ""
	}
	if t.spoolErr != nil {
		spoolErr = t.spoolErr.Error()
	}
	return t.total, t.dropped, append([]string(nil), t.segs...), spoolErr
}

func (t *tail) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

func (t *tail) result() ([]byte, bool) { return t.bytes(), false }

// withStderr 在失败附注后接上 stderr 的最后一个非空行，最多 300 字节。
func withStderr(reason string, stderr io.Writer) string {
	t, ok := stderr.(*tail)
	if !ok {
		return reason
	}
	lines := strings.Split(strings.TrimSpace(string(t.bytes())), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return reason
	}
	if len(last) > 300 {
		last = last[len(last)-300:]
	}
	return reason + ": " + last
}
