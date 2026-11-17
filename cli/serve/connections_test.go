package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
)

// testPlugin 是测试里的假渠道插件：记下收到的消息。
type testPlugin struct {
	mu  sync.Mutex
	got []conn.Message
}

type testInst struct{ p *testPlugin }

func (i testInst) Send(_ context.Context, m conn.Message) (conn.Receipt, error) {
	i.p.mu.Lock()
	defer i.p.mu.Unlock()
	i.p.got = append(i.p.got, m)
	return conn.Receipt{Ref: "r-" + m.Key}, nil
}
func (testInst) Test(context.Context) error { return nil }

func (p *testPlugin) plugins() []conn.Plugin {
	t := conn.Text{Zh: "测试渠道", En: "Test channel"}
	return []conn.Plugin{{Decl: conn.Decl{Type: "sample", Direction: conn.DirectionChannel, Title: t, Tagline: t, Default: t, Via: t,
		Fields: []conn.Field{{Key: "token", Kind: conn.KindSecret, Label: t, Required: true}}},
		New: func(conn.Config) (conn.Channel, error) { return testInst{p}, nil }}}
}

const plainSecret = "SECRET-VALUE-9f3a"

// 经 HTTP：添加连接、测草稿、列出（凭据只回是否已设置）；主密钥丢失后照常启动，连接显示要重新填写。
func TestConnectionsOverHTTPAndMissingMasterKey(t *testing.T) {
	p := &testPlugin{}
	prev := builtinConnectors
	builtinConnectors = p.plugins
	t.Cleanup(func() { builtinConnectors = prev })

	home := initOnFreePort(t)
	addr, stop := start(t, home)
	s := webLogin(t, addr)
	if code, b := s.do("GET", "/connections/types?direction=channel", ""); code != 200 || !strings.Contains(b, `"type":"sample"`) {
		t.Fatalf("types: %d %s", code, b)
	}
	code, b := s.do("POST", "/connections", `{"type":"sample","name":"我的手机","secrets":{"token":"`+plainSecret+`"},"enabled":true}`)
	if code != 201 || strings.Contains(b, plainSecret) || !strings.Contains(b, `"token":true`) {
		t.Fatalf("create: %d %s", code, b)
	}
	if code, b := s.do("POST", "/connections/test", `{"type":"sample","secrets":{"token":"x"},"send":true}`); code != 200 || !strings.Contains(b, `"ok":true`) {
		t.Fatalf("test draft: %d %s", code, b)
	}
	if len(p.got) != 1 || !p.got[0].Test {
		t.Fatalf("test message not sent: %+v", p.got)
	}
	// 没有凭据直接 401
	resp, err := http.Get("http://" + addr.String() + "/v1/connections")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous list: %v %v", resp, err)
	}
	resp.Body.Close()
	// token 只读现状（§7.6）：observer 读到名字、类型、状态，读不到配置值与凭据；不能写；ext 密钥读不到。
	bearer := func(kind, scope, method, path, body string) (int, string) {
		t.Helper()
		req := `{"kind":"` + kind + `"}`
		group := "/settings/tokens"
		if kind == "ext" {
			req, group = `{"scope":"`+scope+`"}`, "/settings/ext-keys"
		}
		code, b := s.do("POST", group, req)
		var out struct{ Secret string }
		if code != 201 || json.Unmarshal([]byte(b), &out) != nil {
			t.Fatalf("issue %s: %d %s", kind, code, b)
		}
		r, _ := http.NewRequest(method, "http://"+addr.String()+"/v1"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+out.Secret)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		rb, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(rb)
	}
	code, b = bearer("observer", "", "GET", "/connections?direction=channel", "")
	if code != 200 || !strings.Contains(b, `"name":"我的手机"`) || !strings.Contains(b, `"state":"ok"`) || !strings.Contains(b, `"type":"sample"`) {
		t.Fatalf("observer list: %d %s", code, b)
	}
	for _, leak := range []string{`"config"`, `"secrets"`, `"token"`, `"detail"`, plainSecret} {
		if strings.Contains(b, leak) {
			t.Fatalf("observer list leaks %s: %s", leak, b)
		}
	}
	if code, b := bearer("delegate", "", "POST", "/connections", `{"type":"sample","name":"x"}`); code != 403 || !strings.Contains(b, "web_only") {
		t.Fatalf("delegate create must stay web_only: %d %s", code, b)
	}
	if code, b := bearer("ext", "channel.mine", "GET", "/connections", ""); code != 403 {
		t.Fatalf("ext key list: %d %s", code, b)
	}
	// Agent 草稿（§7.6）：worker 提交并测试，确认前不建连接；delegate 确认得 web_only；Web 确认后启用并写路由。
	const draftSecret = "DRAFT-plain-secret-77"
	issue := func(kind string) string {
		code, b := s.do("POST", "/settings/tokens", `{"kind":"`+kind+`"}`)
		var out struct{ Secret string }
		if code != 201 || json.Unmarshal([]byte(b), &out) != nil {
			t.Fatalf("issue %s: %d %s", kind, code, b)
		}
		return out.Secret
	}
	as := func(secret, method, path, body string) (int, string) {
		r, _ := http.NewRequest(method, "http://"+addr.String()+"/v1"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		rb, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(rb)
	}
	worker, delegate, observer := issue("worker"), issue("delegate"), issue("observer")
	if code, b := as(observer, "POST", "/connections/drafts", `{"type":"sample","name":"x","secrets":{"token":"t"}}`); code != 403 {
		t.Fatalf("observer propose: %d %s", code, b)
	}
	code, b = as(worker, "POST", "/connections/drafts", `{"type":"sample","name":"Agent 的手机","secrets":{"token":"`+draftSecret+`"},"routes":["decision"]}`)
	var dr struct {
		ID      string
		WebPath string `json:"web_path"`
	}
	if code != 201 || strings.Contains(b, draftSecret) || json.Unmarshal([]byte(b), &dr) != nil || !strings.HasPrefix(dr.ID, "dr-") || dr.WebPath != "/settings/channels#draft-"+dr.ID {
		t.Fatalf("propose: %d %s", code, b)
	}
	sent := len(p.got)
	if code, b := as(worker, "POST", "/connections/drafts/"+dr.ID+"/test", `{}`); code != 200 || !strings.Contains(b, `"ok":true`) || len(p.got) != sent+1 || !p.got[sent].Test {
		t.Fatalf("draft test: %d %s (%d sent)", code, b, len(p.got)-sent)
	}
	if code, b := as(delegate, "POST", "/connections/drafts/"+dr.ID+"/test", `{}`); code != 404 || len(p.got) != sent+1 {
		t.Fatalf("another agent tested the draft: %d %s", code, b)
	}
	if code, b := s.do("GET", "/connections?direction=channel", ""); code != 200 || strings.Contains(b, "Agent 的手机") {
		t.Fatalf("a draft must not be a connection yet: %d %s", code, b)
	}
	if code, b := as(delegate, "POST", "/connections/drafts/"+dr.ID+"/confirm", `{}`); code != 403 || !strings.Contains(b, "web_only") {
		t.Fatalf("delegate confirm: %d %s", code, b)
	}
	if code, b := s.do("GET", "/connections/drafts", ""); code != 200 || strings.Contains(b, draftSecret) || !strings.Contains(b, `"state":"pending"`) {
		t.Fatalf("web drafts: %d %s", code, b)
	}
	code, b = s.do("POST", "/connections/drafts/"+dr.ID+"/confirm", `{}`)
	var cc struct {
		ID      string
		Enabled bool
	}
	if code != 200 || json.Unmarshal([]byte(b), &cc) != nil || !cc.Enabled || strings.Contains(b, draftSecret) {
		t.Fatalf("confirm: %d %s", code, b)
	}
	if code, b := s.do("GET", "/settings/policy", ""); code != 200 || !strings.Contains(b, `"decision":"channel:`+cc.ID+`"`) {
		t.Fatalf("route after confirm: %d %s", code, b)
	}
	if code, b := s.do("POST", "/connections/drafts/"+dr.ID+"/confirm", `{}`); code != 409 || !strings.Contains(b, "draft_confirmed") {
		t.Fatalf("second confirm: %d %s", code, b)
	}
	stop()

	if err := os.Remove(filepath.Join(home, "master.key")); err != nil {
		t.Fatal(err)
	}
	addr, stop = start(t, home)
	defer stop()
	s = webLogin(t, addr)
	code, b = s.do("GET", "/connections?direction=channel", "")
	if code != 200 || !strings.Contains(b, `"state":"problem"`) || !strings.Contains(b, `"problem":"credentials_missing"`) || !strings.Contains(b, `"token":false`) {
		t.Fatalf("after losing the master key: %d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(home, "master.key")); err != nil {
		t.Fatalf("a new master key was not generated: %v", err)
	}
}
