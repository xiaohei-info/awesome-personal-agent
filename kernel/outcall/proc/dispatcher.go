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

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const tailLimit = 64 << 10 // 事件流以外的 stdout 与所有 stderr 只保留末段（任务型的结构化摘要在 stdout 末段，§8.4）

// envWhitelist 是子进程能从内核环境继承的全部变量（§7.7）：PATH、HOME 和 XDG 目录变量及基本区域设置。
var envWhitelist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
	"XDG_RUNTIME_DIR": true, "XDG_CONFIG_DIRS": true, "XDG_DATA_DIRS": true,
}

// EnvRunContext 是运行上下文文件路径的环境变量（§7.7）。路径不是凭据；名字里不含凭据字样，
// 否则按名称过滤环境的 harness 不会把它带到工具侧（附录 A.5）。
const EnvRunContext = "APA_RUN_CONTEXT"

// Result 是一次运行结束后交回调用模块的结果；本包不据此发任何通知或事件。
type Result struct {
	Run    domain.Run // 已落账的终态
	Stdout []byte     // 事件流类别（eventStream）是上限内的全部完整行；其余类别是末段
	Stderr []byte     // 末段
	Reason string     // 失败、超时、关停的原因；成功为空
	// Overflow 为真表示事件流类别的 stdout 超过单次总上限（max_source_output_mb），超出部分已丢弃。
	Overflow bool
}

// Options 是调度器的依赖与扩展点；零值字段取默认。
type Options struct {
	// Environ 返回内核环境，默认 os.Environ；只有白名单变量会传给子进程。
	Environ func() []string
	// Env 返回某次运行额外注入的变量（APA_URL、APA_TOKEN、APA_PROJECT 等）。
	// 在启动时调用而不入账本；运行凭据只落在运行上下文文件里（ContextDir），随运行删除。
	Env func(domain.Run) map[string]string
	// ContextDir 是运行上下文文件的目录（权限 0700，§7.7）；空表示不写。
	ContextDir string
	// OnResult 在终态提交后被调用（可并发）。
	OnResult func(Result)
	// Now 默认 time.Now。
	Now func() time.Time
}

// Dispatcher 按两个队列的额度拉起子进程。额度从账本里 running 的 Run 数得出，
// 重启后仍存活的进程继续占额度。
type Dispatcher struct {
	st        *store.Store
	limits    map[Queue]int
	maxOutput int // 事件流类别单次 stdout 的总上限（字节）
	opt       Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex // 串行化选取，保证不超额
}

// New 用 config 里的两个额度创建调度器。
func New(st *store.Store, cfg config.Config, opt Options) *Dispatcher {
	if opt.Environ == nil {
		opt.Environ = os.Environ
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if cfg.MaxSourceOutput == 0 {
		cfg.MaxSourceOutput = config.Default().MaxSourceOutput
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{st: st, opt: opt, ctx: ctx, cancel: cancel, maxOutput: cfg.MaxSourceOutput,
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
			if err := Settle(tx, rec.Run.ID, domain.RunFailed, nil, "no live process after restart", d.opt.Now()); err != nil {
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
		if _, err := tx.Exec("UPDATE outcall_run SET state = 'running', started_at = ? WHERE id = ?",
			d.opt.Now().UnixNano(), rec.Run.ID); err != nil {
			return err
		}
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
	}
	return env, nil
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
	var errb tail
	var out stdoutSink = &tail{}
	if eventStream(rec.Run.Category) {
		out = &whole{limit: d.maxOutput}
	}
	state, exit, reason := d.exec(rec, out, &errb)
	d.removeContext(rec)
	stdout, overflow := out.result()
	res := Result{Stdout: stdout, Stderr: errb.bytes(), Reason: reason, Overflow: overflow}
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
	timer := time.NewTimer(time.Duration(rec.Run.TimeoutSec) * time.Second)
	defer timer.Stop()

	var killed domain.RunState // 非空表示是我们杀的
	var why string
	var werr error
	select {
	case werr = <-done:
	case <-timer.C:
		killed, why = domain.RunTimeout, fmt.Sprintf("timeout after %ds", rec.Run.TimeoutSec)
	case <-d.ctx.Done():
		killed, why = domain.RunFailed, "shutdown"
	}
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
		return domain.RunFailed, &code, fmt.Sprintf("exit status %d", code)
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

// tail 只保留最后 tailLimit 字节。
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if n := len(t.buf) - tailLimit; n > 0 {
		t.buf = append([]byte(nil), t.buf[n:]...)
	}
	return len(p), nil
}

func (t *tail) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

func (t *tail) result() ([]byte, bool) { return t.bytes(), false }
