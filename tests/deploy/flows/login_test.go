//go:build e2e

package flows

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 本流程用过的全部口令；ACT-LGN-13 在 Event 与账本库里逐个搜索它们。
const (
	pwNew     = "second-passphrase-2026"   // ACT-LGN-03 改成的新口令
	pwReset   = "reset-on-host-passphrase" // ACT-LGN-11 主机重设后的口令
	pwShort   = "short-11-ch"              // 11 个字符
	pwWrongA  = "wrong-guess-aaa"
	pwWrongB  = "wrong-current-bbb"
	pwElevenA = "elevenchars"
)

func (f *fx) countItems() int {
	f.t.Helper()
	r := f.Apa(f.OBS, "", "item", "list")
	if r.Code != 0 {
		f.t.Fatalf("apa item list: %d %s", r.Code, r.Stderr)
	}
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		f.t.Fatalf("item list 不是 JSON: %v\n%s", err, r.Stdout)
	}
	return len(out.Items)
}

func TestFlowWebLogin(t *testing.T) {
	f := newFx(t)
	pw := baseline.Password // 当前有效的登录口令
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.FailNow() // 步骤之间有状态依赖：前一步失败后不再继续
		}
	}
	// 失败尝试用各步骤专属来源地址，互不污染；ACT-LGN-06 才使用真实的 127.0.0.1。
	login := func(addr, agent, p string) (*sess, resp) { return tryLogin(t, f.URL, addr, agent, p) }

	step("ACT-LGN-01 设置口令时拒绝少于 12 个字符", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "h")
		r := runBin(t, f.Bin["apa"], home, pwElevenA+"\n"+pwElevenA+"\n", "init")
		if r.Code == 0 || !strings.Contains(r.Stderr, "12") {
			t.Fatalf("11 个字符应被拒并提示长度下限: %+v", r)
		}
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("被拒的口令不得写入任何数据: stat err=%v", err)
		}
		const ok12 = "twelve-chars"
		if r := runBin(t, f.Bin["apa"], home, ok12+"\n"+ok12+"\n", "init"); r.Code != 0 {
			t.Fatalf("12 个字符应被接受，init 退出码 0: %+v", r)
		}
	})

	step("ACT-LGN-02 Web 改口令校验当前口令与长度", func(t *testing.T) {
		a := mustLogin(t, f.URL, "10.0.2.1", "A-browser", pw)
		change := func(cur, nw string) resp {
			b, _ := json.Marshal(map[string]string{"current": cur, "new": nw})
			return a.do("POST", "/v1/auth/password", string(b))
		}
		// 设置页与直接调用 API 走同一个接口，各做一遍。
		for i := 0; i < 2; i++ {
			if r := change(pwWrongB, pwNew); r.Code != 403 {
				t.Fatalf("当前口令错误应被拒: %d %s", r.Code, r.Body)
			}
			if r := change(pw, pwShort); r.Code != 400 {
				t.Fatalf("新口令 11 个字符应返回 400: %d %s", r.Code, r.Body)
			}
		}
		if s, r := login("10.0.2.2", "after-fail", pw); s == nil {
			t.Fatalf("口令未变，旧口令仍应能登录: %d %s", r.Code, r.Body)
		}
		if s, _ := login("10.0.2.3", "must-fail", pwNew); s != nil {
			t.Fatal("新口令不应生效")
		}
	})

	step("ACT-LGN-03 改口令成功后其他会话全部失效", func(t *testing.T) {
		before := countType(ledgerEvents(t, f.Home), "auth.password_changed")
		a := mustLogin(t, f.URL, "10.0.3.1", "A-browser", pw)
		b := mustLogin(t, f.URL, "10.0.3.2", "B-browser", pw)
		body, _ := json.Marshal(map[string]string{"current": pw, "new": pwNew})
		mustCode(t, "A 改口令", a.do("POST", "/v1/auth/password", string(body)), 200)
		b.want(401, "改口令后 B 刷新")
		a.want(200, "改口令后 A 继续操作")
		if s, _ := login("10.0.3.3", "old", pw); s != nil {
			t.Fatal("旧口令登录应失败")
		}
		if s, r := login("10.0.3.4", "new", pwNew); s == nil {
			t.Fatalf("新口令登录应成功: %d %s", r.Code, r.Body)
		}
		evs := ledgerEvents(t, f.Home)
		if got := countType(evs, "auth.password_changed") - before; got != 1 {
			t.Fatalf("应新增恰好一条改口令 Event，实际 %d", got)
		}
		pw = pwNew
		f.WebSecret = pw
	})

	step("ACT-LGN-04 同一来源连续失败 5 次后锁定 15 分钟", func(t *testing.T) {
		const addr = "10.0.4.1"
		before := countType(ledgerEvents(t, f.Home), "auth.login_locked")
		for i := 0; i < 5; i++ {
			if s, r := login(addr, "guesser", pwWrongA); s != nil || r.Code != 401 {
				t.Fatalf("第 %d 次错误口令应得 401: %d", i+1, r.Code)
			}
		}
		s, r := login(addr, "guesser", pw)
		if s != nil || r.Code != 429 || r.errCode() != "login_locked" || r.Header.Get("Retry-After") == "" {
			t.Fatalf("第 6 次（正确口令）应得 429 login_locked 并带 Retry-After: %d %s %v", r.Code, r.Body, r.Header)
		}
		if _, r := login("10.0.4.2", "other-addr", pw); r.Code != 200 {
			t.Fatalf("锁定只针对该来源地址: %d", r.Code)
		}
		f.TCAdvance(900) // 不重启服务
		if s, r := login(addr, "guesser", pw); s == nil {
			t.Fatalf("推进 15 分钟后正确口令应登录成功: %d %s", r.Code, r.Body)
		}
		if got := countType(ledgerEvents(t, f.Home), "auth.login_locked") - before; got != 1 {
			t.Fatalf("锁定应记一条 Event，实际 %d", got)
		}
	})

	step("ACT-LGN-05 全部地址 15 分钟内失败超过 20 次时延迟应答并告警", func(t *testing.T) {
		f.TCAdvance(900) // 让前面步骤的失败移出统计窗口
		alerts := func() int {
			n := 0
			for _, e := range ledgerEvents(t, f.Home) {
				if e.Type == "health.alert" && strings.Contains(e.Payload, "login_flood") {
					n++
				}
			}
			return n
		}
		before := alerts()
		for i, per := range []int{4, 4, 4, 4, 5} { // 21 次，单个地址不超过 4 次即不触发锁定（最后一个地址第 5 次才锁）
			addr := "10.0.5." + strconv.Itoa(i+2)
			for j := 0; j < per; j++ {
				if s, r := login(addr, "flood", pwWrongA); s != nil || (r.Code != 401 && r.Code != 429) {
					t.Fatalf("%s 第 %d 次: %d", addr, j+1, r.Code)
				}
			}
		}
		start := time.Now()
		s, r := login("10.0.5.7", "slow-but-right", pw)
		took := time.Since(start)
		if s == nil {
			t.Fatalf("超过 20 次后正确口令最终应登录成功: %d %s", r.Code, r.Body)
		}
		if took < time.Second {
			t.Fatalf("超过 20 次后登录应被延迟应答，实际只用了 %v", took)
		}
		if got := alerts() - before; got < 1 {
			t.Fatalf("应记 health.alert（login_flood），实际新增 %d", got)
		}
		if r := f.APIRaw("GET", "/v1/health", "", f.OBS); r.Code != 200 {
			t.Fatalf("GET /v1/health: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-LGN-06 失败计数是运行态数据，重启清零", func(t *testing.T) {
		f.TCAdvance(900)
		for i := 0; i < 5; i++ { // 来源是真实的 127.0.0.1
			if s, r := tryLogin(t, f.URL, "", "lock-me", pwWrongA); s != nil || r.Code != 401 {
				t.Fatalf("错误口令第 %d 次: %d", i+1, r.Code)
			}
		}
		if s, r := tryLogin(t, f.URL, "", "lock-me", pw); s != nil || r.Code != 429 {
			t.Fatalf("127.0.0.1 应已被锁定: %d %s", r.Code, r.Body)
		}
		f.StopKernel()
		f.StartKernelAgain() // 内部以 e.WebSecret 登录，成功即说明重启后立即可登录
		if s, r := tryLogin(t, f.URL, "", "after-restart", pw); s == nil {
			t.Fatalf("重启后正确口令应立即登录成功: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-LGN-07 列出在线会话并逐个或全部吊销", func(t *testing.T) {
		revokedBefore := countType(ledgerEvents(t, f.Home), "auth.session_revoked")
		c1 := mustLogin(t, f.URL, "10.0.7.1", "C1-browser", pw)
		c2 := mustLogin(t, f.URL, "10.0.7.2", "C2-browser", pw)
		c3 := mustLogin(t, f.URL, "10.0.7.3", "C3-browser", pw)
		type info struct {
			ID, Addr, Agent string
			LastUsedAt      time.Time `json:"last_used_at"`
			Current         bool
		}
		list := func() map[string]info {
			r := c1.do("GET", "/v1/auth/sessions", "")
			mustCode(t, "列出会话", r, 200)
			var out struct {
				Sessions []info `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
				t.Fatal(err)
			}
			m := map[string]info{}
			for _, s := range out.Sessions {
				m[s.Agent] = s
			}
			return m
		}
		l := list()
		for agent, addr := range map[string]string{"C1-browser": "10.0.7.1", "C2-browser": "10.0.7.2", "C3-browser": "10.0.7.3"} {
			s, ok := l[agent]
			if !ok || s.Addr != addr || s.LastUsedAt.IsZero() || s.ID == "" {
				t.Fatalf("会话 %s 的列表项缺字段（最近使用时间/来源地址/浏览器标识）: %+v", agent, s)
			}
		}
		if !l["C1-browser"].Current {
			t.Fatal("当前会话应标记 current")
		}
		mustCode(t, "吊销 C2", c1.do("DELETE", "/v1/auth/sessions/"+l["C2-browser"].ID, ""), 200)
		c2.want(401, "吊销后 C2 刷新")
		c1.want(200, "吊销 C2 后 C1")
		c3.want(200, "吊销 C2 后 C3")
		mustCode(t, "全部吊销", c1.do("DELETE", "/v1/auth/sessions", ""), 200)
		c1.want(401, "全部吊销后 C1 刷新")
		c3.want(401, "全部吊销后 C3 刷新")
		if got := countType(ledgerEvents(t, f.Home), "auth.session_revoked") - revokedBefore; got != 2 {
			t.Fatalf("两次吊销各记一条 Event，实际新增 %d", got)
		}
		f.WebLogin() // 全部吊销也吊销了基线自己的会话
	})

	step("ACT-LGN-08 会话管理与改口令只接受 Web 登录", func(t *testing.T) {
		hdr := map[string]string{"Authorization": "Bearer " + f.DEL, "X-APA-Evidence": "owner asked", "X-APA-Session-Kind": "direct"}
		body, _ := json.Marshal(map[string]string{"current": pw, "new": "attacker-chosen-passphrase"})
		for _, c := range []struct{ m, p, b string }{
			{"GET", "/v1/auth/sessions", ""}, {"DELETE", "/v1/auth/sessions", ""}, {"POST", "/v1/auth/password", string(body)}} {
			if r := send(t, httpc, c.m, f.URL+c.p, c.b, hdr); r.Code != 403 {
				t.Fatalf("Bearer 请求 %s %s 应被拒绝(403): %d %s", c.m, c.p, r.Code, r.Body)
			}
		}
		probe := mustLogin(t, f.URL, "10.0.8.1", "still-here", pw) // 口令没变
		mustCode(t, "会话仍在", probe.do("GET", "/v1/auth/sessions", ""), 200)
		if s, _ := login("10.0.8.2", "attacker", "attacker-chosen-passphrase"); s != nil {
			t.Fatal("口令不应被 Bearer 请求改掉")
		}
	})

	step("ACT-LGN-09 会话空闲 7 天失效", func(t *testing.T) {
		s1 := mustLogin(t, f.URL, "10.0.9.1", "S1", pw)
		s2 := mustLogin(t, f.URL, "10.0.9.2", "S2", pw)
		f.TCAdvance(518400) // 第 6 天
		s2.want(200, "第 6 天 S2")
		f.TCAdvance(86460) // 第 7 天加 1 分钟
		s1.want(401, "空闲超过 7 天的 S1")
		f.TCAdvance(431940) // 第 12 天
		s2.want(200, "第 12 天 S2（第 6 天用过一次，空闲不足 7 天）")
	})

	step("ACT-LGN-10 会话绝对有效期 30 天", func(t *testing.T) {
		s3 := mustLogin(t, f.URL, "10.0.10.1", "S3", pw)
		for i := 0; i < 4; i++ { // 到第 24 天
			f.TCAdvance(518400)
			s3.want(200, "第 "+strconv.Itoa(6*(i+1))+" 天 S3")
		}
		f.TCAdvance(518340) // 第 29 天 23 小时 59 分
		s3.want(200, "30 天之前 S3")
		f.TCAdvance(120) // 第 30 天加 1 分钟
		s3.want(401, "超过 30 天的 S3（持续使用也不续期）")
		f.WebLogin()
	})

	step("ACT-LGN-11 主机上重设口令，账本不动", func(t *testing.T) {
		items := f.countItems()
		before := ledgerEvents(t, f.Home)
		f.StopKernel()
		if r := runBin(t, f.Bin["apa"], f.Home, pwShort+"\n"+pwShort+"\n", "init", "--reset-password"); r.Code == 0 {
			t.Fatalf("11 个字符应被拒: %+v", r)
		}
		if got := ledgerEvents(t, f.Home); len(got) != len(before) {
			t.Fatalf("被拒的重设不得写账本: %d -> %d", len(before), len(got))
		}
		if r := runBin(t, f.Bin["apa"], f.Home, pwReset+"\n", "init", "--reset-password", "--password-stdin"); r.Code != 0 {
			t.Fatalf("重设应成功: %+v", r)
		}
		old := pw
		pw = pwReset
		f.WebSecret = pw
		f.StartKernelAgain()
		if s, _ := login("10.0.11.1", "old", old); s != nil {
			t.Fatal("重设后旧口令应失败")
		}
		if s, r := login("10.0.11.2", "new", pw); s == nil {
			t.Fatalf("重设后新口令应成功: %d %s", r.Code, r.Body)
		}
		if got := f.countItems(); got != items {
			t.Fatalf("事项数量变了: %d -> %d", items, got)
		}
		after := ledgerEvents(t, f.Home)
		for i, e := range before { // 已有 Event 一条不少、不改，只在末尾追加
			if i >= len(after) || after[i].Seq != e.Seq || after[i].Type != e.Type {
				t.Fatalf("账本前缀被改动于第 %d 条", i)
			}
		}
		for _, e := range after[len(before):] {
			if e.Seq <= maxSeq(before) || strings.Contains(e.Payload, pwReset) {
				t.Fatalf("新增 Event 序号不在末尾或含口令: %+v", e)
			}
		}
	})

	step("ACT-LGN-12 已有短口令升级后继续有效，升级前的会话全部失效", func(t *testing.T) {
		v01 := f.V01Bin()
		home := filepath.Join(t.TempDir(), "h")
		const short = "short-8x" // 8 个字符
		if r := runBin(t, v01, home, short+"\n"+short+"\n", "init"); r.Code != 0 {
			t.Fatalf("旧版本 init: %+v", r)
		}
		port := freePort(t)
		cfg := []byte("listen: 127.0.0.1:" + port + "\n")
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		base := "http://127.0.0.1:" + port
		serve := func(bin string) (stop func()) {
			cmd := exec.Command(bin, "serve")
			cmd.Env = append(os.Environ(), "APA_HOME="+home)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			up := false
			for i := 0; i < 300 && !up; i++ {
				if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
					c.Close()
					up = true
				} else {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if !up {
				_ = cmd.Process.Kill()
				t.Fatalf("%s serve 未启动", bin)
			}
			return func() {
				_ = cmd.Process.Signal(syscall.SIGINT)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
			}
		}
		stop := serve(v01)
		v1, r := tryLogin(t, base, "", "V1-browser", short)
		if v1 == nil {
			stop()
			t.Fatalf("旧版本登录: %d %s", r.Code, r.Body)
		}
		v1.want(200, "旧版本下的 V1")
		stop()
		stop = serve(f.Bin["apa"])
		defer stop()
		v1.want(401, "升级后 V1 刷新")
		if s, r := tryLogin(t, base, "", "after-upgrade", short); s == nil {
			t.Fatalf("长度规则不追溯，8 个字符的口令应登录成功: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-LGN-13 登录相关的审计记录不含口令", func(t *testing.T) {
		raw, types, _ := f.apiEvents()
		for _, typ := range []string{"auth.login", "auth.login_locked", "auth.password_changed", "auth.session_revoked"} {
			if types[typ] == 0 {
				t.Errorf("审计里缺 %s 记录: %v", typ, types)
			}
		}
		stored := ledgerBytes(t, f.Home)
		for _, secret := range []string{baseline.Password, pwNew, pwReset, pwShort, pwWrongA, pwWrongB, "attacker-chosen-passphrase"} {
			if strings.Contains(raw, secret) {
				t.Errorf("GET /v1/events 导出里出现口令明文 %q", secret)
			}
			if strings.Contains(stored, secret) {
				t.Errorf("账本库里出现口令明文 %q", secret)
			}
		}
	})
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}
