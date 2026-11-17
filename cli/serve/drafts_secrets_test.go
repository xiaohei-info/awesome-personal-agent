package serve

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// barkSink 是本地的假推送服务器：只记收到几次请求、其中几次带了指定的旧凭据，不保存凭据本身。
type barkSink struct {
	*httptest.Server
	mu             sync.Mutex
	hits, withOld  int
	oldKey, oldPwd string
}

func newBarkSink(t *testing.T, oldKey, oldPwd string) *barkSink {
	s := &barkSink{oldKey: oldKey, oldPwd: oldPwd}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, pwd, _ := r.BasicAuth()
		s.mu.Lock()
		s.hits++
		if pwd == s.oldPwd || strings.Contains(string(b), s.oldKey) {
			s.withOld++
		}
		s.mu.Unlock()
		io.WriteString(w, `{"code":200,"message":"success"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *barkSink) seen() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.withOld
}

// 安全路径（§7.6）：改已有连接的草稿沿用了生效凭据，又改了去处或接收目标时，Agent 不能借草稿测试把旧凭据发出去；
// 新建草稿用自己给的凭据照常可测；本人确认后连接按新值可用。用真实推送插件与本地假服务器，凭据都是合成值。
func TestAgentDraftTestCannotCarryKeptSecretsToNewTarget(t *testing.T) {
	const oldKey, oldPwd, agentKey = "OLDdevKEY-synthetic-4711", "OLDpwd-synthetic-0815", "AGENTkey-synthetic-2024"
	live, other := newBarkSink(t, oldKey, oldPwd), newBarkSink(t, oldKey, oldPwd)

	home := initOnFreePort(t)
	addr, stop := start(t, home)
	defer stop()
	s := webLogin(t, addr)
	as := tokenCaller(t, s, addr)
	worker, worker2 := as.issue("worker"), as.issue("worker")

	code, b := s.do("POST", "/connections", `{"type":"bark","name":"我的手机","config":{"server":"`+live.URL+`","auth_user":"me"},"secrets":{"device_key":"`+oldKey+`","auth_secret":"`+oldPwd+`"},"enabled":true}`)
	var c struct{ ID string }
	if code != 201 || json.Unmarshal([]byte(b), &c) != nil {
		t.Fatalf("create: %d %s", code, b)
	}
	propose := func(secret, body string) string {
		t.Helper()
		code, b := as.do(secret, "POST", "/connections/drafts", body)
		var d struct{ ID string }
		if code != 201 || json.Unmarshal([]byte(b), &d) != nil {
			t.Fatalf("propose: %d %s", code, b)
		}
		return d.ID
	}
	refused := func(name, id string) {
		t.Helper()
		lh, _ := live.seen()
		oh, _ := other.seen()
		code, b := as.do(worker, "POST", "/connections/drafts/"+id+"/test", `{}`)
		lh2, lold := live.seen()
		oh2, oold := other.seen()
		if lh2 != lh || oh2 != oh {
			t.Fatalf("%s: request sent (live +%d, other +%d; carrying old credentials: live %d, other %d); response %d", name, lh2-lh, oh2-oh, lold, oold, code)
		}
		if code != 403 || !strings.Contains(b, "draft_test_kept_secrets") {
			t.Fatalf("%s: agent test of a draft that keeps live secrets: %d %s", name, code, b)
		}
	}

	// 1. 沿用旧凭据 + 服务器改到别处：不发请求；Agent 拿到的草稿与列表里没有现用服务器、登录名与配置
	code, b = as.do(worker, "POST", "/connections/drafts", `{"connection":"`+c.ID+`","config":{"server":"`+other.URL+`"}}`)
	var mv struct{ ID string }
	if code != 201 || json.Unmarshal([]byte(b), &mv) != nil || strings.Contains(b, live.URL) || strings.Contains(b, other.URL) || strings.Contains(b, `"config"`) || strings.Contains(b, `"secrets"`) {
		t.Fatalf("agent propose view: %d %s", code, b)
	}
	moved := mv.ID
	if code, b := as.do(worker, "GET", "/connections/drafts", ""); code != 200 || strings.Contains(b, live.URL) || strings.Contains(b, `"config"`) || strings.Contains(b, `"routes`) {
		t.Fatalf("agent draft list: %d %s", code, b)
	}
	refused("server moved", moved)
	// 2. 他人的草稿：按不存在处理，不发
	if code, b := as.do(worker2, "POST", "/connections/drafts/"+moved+"/test", `{}`); code != 404 {
		t.Fatalf("other agent: %d %s", code, b)
	}
	// 3. 服务器不变、接收设备换成 Agent 的，登录密码沿用：同样不发
	refused("device replaced", propose(worker, `{"connection":"`+c.ID+`","secrets":{"device_key":"`+agentKey+`"}}`))
	// 4. 只改路由（去处与凭据都同现用值）：可测，只到原服务器
	routesOnly := propose(worker, `{"connection":"`+c.ID+`","routes":["decision"]}`)
	if code, b := as.do(worker, "POST", "/connections/drafts/"+routesOnly+"/test", `{}`); code != 200 || !strings.Contains(b, `"ok":true`) {
		t.Fatalf("routes-only draft test: %d %s", code, b)
	}
	if h, _ := other.seen(); h != 0 {
		t.Fatalf("other server got %d requests", h)
	}
	// 5. 凭据全部重新给出（不沿用），改到别处：可测，别处收到的是 Agent 给的凭据，不是旧的
	fresh := propose(worker, `{"connection":"`+c.ID+`","config":{"server":"`+other.URL+`","auth_user":""},"secrets":{"device_key":"`+agentKey+`","auth_secret":""}}`)
	if code, b := as.do(worker, "POST", "/connections/drafts/"+fresh+"/test", `{}`); code != 200 || !strings.Contains(b, `"ok":true`) {
		t.Fatalf("fresh-secrets draft test: %d %s", code, b)
	}
	// 6. 新建草稿自带凭据：照常可测
	created := propose(worker2, `{"type":"bark","name":"Agent 的 iPad","config":{"server":"`+other.URL+`"},"secrets":{"device_key":"`+agentKey+`"}}`)
	if code, b := as.do(worker2, "POST", "/connections/drafts/"+created+"/test", `{}`); code != 200 || !strings.Contains(b, `"ok":true`) {
		t.Fatalf("new draft test: %d %s", code, b)
	}
	if h, old := other.seen(); h == 0 || old != 0 {
		t.Fatalf("other server: %d requests, %d with old credentials", h, old)
	}
	// 对方服务器的回话只给 Web 本人：Agent 只拿问题码
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"code":400,"message":"remote-body-marker"}`)
	}))
	defer bad.Close()
	rej := propose(worker2, `{"type":"bark","name":"拒收的","config":{"server":"`+bad.URL+`"},"secrets":{"device_key":"`+agentKey+`"}}`)
	if code, b := as.do(worker2, "POST", "/connections/drafts/"+rej+"/test", `{}`); code != 200 || !strings.Contains(b, `"problem":"rejected"`) || strings.Contains(b, "remote-body-marker") || strings.Contains(b, "detail") {
		t.Fatalf("agent test result: %d %s", code, b)
	}
	if code, b := as.do(worker2, "POST", "/connections/drafts/"+rej+"/test", `{}`); code != 409 || !strings.Contains(b, "draft_test_cooldown") {
		t.Fatalf("agent test again within a minute: %d %s", code, b)
	}
	if code, b := s.do("POST", "/connections/drafts/"+rej+"/test", `{}`); code != 200 || !strings.Contains(b, "remote-body-marker") {
		t.Fatalf("owner test result keeps the detail: %d %s", code, b)
	}
	// 原连接保持原值
	if code, b := s.do("GET", "/connections?direction=channel", ""); code != 200 || !strings.Contains(b, live.URL) {
		t.Fatalf("live connection changed: %d %s", code, b)
	}
	// 本人确认第 1 份之前它已被第 5 份取代；重提一份沿用凭据改服务器的，本人确认后连接按新值可用
	moved = propose(worker, `{"connection":"`+c.ID+`","config":{"server":"`+other.URL+`"}}`)
	if code, b := s.do("POST", "/connections/drafts/"+moved+"/confirm", `{}`); code != 200 {
		t.Fatalf("confirm: %d %s", code, b)
	}
	if code, b := s.do("POST", "/connections/"+c.ID+"/test", `{}`); code != 200 || !strings.Contains(b, `"ok":true`) {
		t.Fatalf("test after confirm: %d %s", code, b)
	}
	if _, old := other.seen(); old == 0 {
		t.Fatal("after confirmation the kept credentials should go to the confirmed server")
	}
}

// tokenCaller 签发 token 并以它调用 HTTP API。
type tokenAPI struct {
	t    *testing.T
	s    *session
	addr net.Addr
}

func tokenCaller(t *testing.T, s *session, addr net.Addr) tokenAPI { return tokenAPI{t, s, addr} }

func (a tokenAPI) issue(kind string) string {
	a.t.Helper()
	code, b := a.s.do("POST", "/settings/tokens", `{"kind":"`+kind+`"}`)
	var out struct{ Secret string }
	if code != 201 || json.Unmarshal([]byte(b), &out) != nil {
		a.t.Fatalf("issue %s: %d %s", kind, code, b)
	}
	return out.Secret
}

func (a tokenAPI) do(secret, method, path, body string) (int, string) {
	a.t.Helper()
	r, _ := http.NewRequest(method, "http://"+a.addr.String()+"/v1"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(rb)
}
