package serve

import (
	"context"
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
	// Bearer 凭据不能管理连接：没有会话直接 401
	resp, err := http.Get("http://" + addr.String() + "/v1/connections")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous list: %v %v", resp, err)
	}
	resp.Body.Close()
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
