package baseline

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 测试进程异常结束（-timeout 超时 panic、Ctrl-C、被 kill -9）时，t.Cleanup 与 TestMain 的 defer 都不会运行：
// 它起的 apa serve 被系统收养后一直跑下去，APA_HOME 与二进制目录也留在 TMPDIR。看管进程是这一漏洞的兜底。
// 它是本测试二进制的另一份（Run 识别 reaperEnv），在自己的进程组里、忽略终端信号，从 stdin 逐行读登记；
// stdin 写端只在测试进程手里，测试进程无论怎样结束写端都会关闭。读到 EOF 后先按登记的进程组 SIGKILL、
// 等它们消失，再删登记的目录。正常结束时各进程已由 Stop/Kill 收掉并注销，目录已由 Cleanup 删掉，EOF 后无事可做。
const reaperEnv = "APA_E2E_REAPER"

var reaper struct {
	once sync.Once
	mu   sync.Mutex
	w    io.WriteCloser
	err  error
}

// note 向看管进程登记一行（pg <pid> | unpg <pid> | dir <quoted path>），首次调用时拉起看管进程。
func note(op, arg string) error {
	reaper.once.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			reaper.err = err
			return
		}
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), reaperEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 不随终端的 Ctrl-C 一起被杀
		if reaper.w, reaper.err = cmd.StdinPipe(); reaper.err == nil {
			reaper.err = cmd.Start()
		}
		// 不 Wait：它与测试进程同寿，写端关闭后自行退出，由系统收养回收。
	})
	if reaper.err != nil {
		return fmt.Errorf("e2e 看管进程: %w", reaper.err)
	}
	reaper.mu.Lock()
	defer reaper.mu.Unlock()
	_, err := fmt.Fprintf(reaper.w, "%s %s\n", op, arg)
	return err
}

// removeOnAbort 登记一个目录：测试进程异常结束时，看管进程在杀完登记的进程之后删掉它。
func removeOnAbort(dir string) error { return note("dir", strconv.Quote(dir)) }

// reap 是看管进程的主体，见文件开头。
func reap() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	pgs := map[int]bool{}
	var dirs []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		op, arg, _ := strings.Cut(sc.Text(), " ")
		switch op {
		case "pg", "unpg":
			if n, err := strconv.Atoi(arg); err == nil && n > 1 { // n>1：kill(-1)/kill(0) 会波及无关进程
				pgs[n] = op == "pg"
			}
		case "dir":
			if p, err := strconv.Unquote(arg); err == nil && p != "" {
				dirs = append(dirs, p)
			}
		}
	}
	for pg, live := range pgs {
		if live {
			_ = syscall.Kill(-pg, syscall.SIGKILL)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for pg, live := range pgs {
		for live && syscall.Kill(-pg, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// Proc 是一个受看管的 apa serve：自成进程组（pgid = pid），登记在看管进程里，回收后注销。
type Proc struct {
	Cmd  *exec.Cmd
	Done chan struct{} // 进程已退出并回收；之后 Cmd.ProcessState 可读
}

// StartServe 以独立进程组启动 cmd（一个 apa serve），启动成功后立即登记看管、注册 t.Cleanup(Stop)。
// dirs 是测试进程异常结束时要在杀掉进程之后删除的目录（Env 的 Root 已由 New 登记，不必重复）。
// 用例不再自己 Wait：等 Done，或调 Stop / Kill。
func StartServe(t testing.TB, cmd *exec.Cmd, dirs ...string) (*Proc, error) {
	t.Helper()
	p, err := startProc(cmd, dirs...)
	if err != nil {
		return nil, err
	}
	t.Cleanup(p.Stop)
	return p, nil
}

// startProc 同 StartServe 但不注册 t.Cleanup：Env 的内核由 New 在顶层 t 上注册的 StopKernel 收掉，
// 不能挂在 Env.T 上——各包会把 Env.T 临时换成步骤子测试的 t，挂上去内核会在该步骤结束时被停掉。
func startProc(cmd *exec.Cmd, dirs ...string) (*Proc, error) {
	for _, d := range dirs {
		if err := removeOnAbort(d); err != nil {
			return nil, err
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	if err := note("pg", strconv.Itoa(pid)); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return nil, err
	}
	p := &Proc{Cmd: cmd, Done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		_ = note("unpg", strconv.Itoa(pid)) // 回收后立即注销：之后这个进程组号可能被系统复用
		close(p.Done)
	}()
	return p, nil
}

// Stop 先 SIGINT 给 apa serve 优雅退出的机会，15 秒不退就 SIGKILL 整个进程组；等到回收。可重复调用。
func (p *Proc) Stop() {
	select {
	case <-p.Done:
		return
	default:
	}
	_ = p.Cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.Done:
	case <-time.After(15 * time.Second):
		p.Kill()
	}
}

// Kill 以 SIGKILL 结束整个进程组（按记录的 pid，不按名字），等到回收。可重复调用。
func (p *Proc) Kill() {
	select {
	case <-p.Done:
		return
	default:
	}
	_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL) // 领头进程尚未回收，组号不会被复用
	<-p.Done
}
