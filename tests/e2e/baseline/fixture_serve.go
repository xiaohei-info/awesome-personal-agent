package baseline

import (
	"os"
	"path/filepath"
	"strings"
)

// ServeFixture 把本环境的 APA_HOME 换成账本夹具副本并在其上启动 `apa serve`（测试构建），用夹具自己的 Web 口令登录；
// OBS、DEL 取夹具 manifest 里的 observer、delegate 凭据。config 的键值覆盖夹具 config.yaml 里的同名键（监听地址总改为本机空闲端口）。
// 夹具已含全部账本数据，不运行 apa init；之后的 Apa、Web、APIRaw、TCAdvance、StopKernel、StartKernelAgain 都作用在这份副本上。
func (e *Env) ServeFixture(fx *Fixture, config map[string]string) {
	e.T.Helper()
	port, err := freePort()
	if err != nil {
		e.fatal("%v", err)
	}
	cfgPath := filepath.Join(fx.Home, "config.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		e.fatal("%v", err)
	}
	want := map[string]string{"listen": loopback + ":" + port}
	for k, v := range config {
		want[k] = v
	}
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if k, _, ok := strings.Cut(ln, ":"); ok {
			if _, over := want[strings.TrimSpace(k)]; over && !strings.HasPrefix(ln, " ") {
				continue
			}
		}
		lines = append(lines, ln)
	}
	for k, v := range want {
		lines = append(lines, k+": "+v)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		e.fatal("%v", err)
	}
	e.StopKernel()
	e.Home, e.WebSecret = fx.Home, fx.M.WebSecret
	e.OBS, e.DEL = fx.M.Tokens["observer"], fx.M.Tokens["delegate"]
	e.startServe(port)
}

// KillKernel 以 SIGKILL 结束 apa serve（不给任何收尾机会），等进程退出。
func (e *Env) KillKernel() {
	e.T.Helper()
	if e.srv == nil || e.srv.Process == nil {
		return
	}
	_ = e.srv.Process.Kill()
	<-e.srvDone
	e.srv = nil
}

// APIRawHeaders 同 APIRaw，并附加请求头（会话引用、本人原话、幂等键等）。
func (e *Env) APIRawHeaders(method, path, body, token string, hdr map[string]string) Resp {
	e.T.Helper()
	h := map[string]string{"Authorization": "Bearer " + token}
	for k, v := range hdr {
		h[k] = v
	}
	return e.do(method, path, body, h)
}
