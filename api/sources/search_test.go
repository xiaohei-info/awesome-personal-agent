package sources

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
)

// 外部钥匙检索（§14.14）：搜得到的就是读得到的。元信息档来源的事项只按标题命中、不给正文片段；
// 只在被裁掉的正文里命中的不出现；没给的来源的事项不出现，计数里也不体现。内部钥匙照常读全文。
func TestSearchFollowsTheKeysSourceScopes(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_meta", "src_hidden")
	mk := func(title, body, src string) string {
		in, _ := json.Marshal(map[string]any{"kind": "task", "phase": "open", "title": title, "body": body, "origin": map[string]string{"source": src, "ref": title}})
		res, err := e.x.Do(bg, command.Request{Identity: webIdentity, Command: "item.create", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		var it struct{ ID string }
		json.Unmarshal(res.Value, &it)
		return it.ID
	}
	a := mk("周报标题", "正文里的机密词", "src_meta")
	mk("机密词清单", "", "src_hidden")
	find := func(key, q string) search.Result {
		t.Helper()
		var r search.Result
		resp := e.do(key, "GET", "/v1/search?q="+url.QueryEscape(q), "")
		if resp.Code != 200 {
			t.Fatalf("%s: %d %s", q, resp.Code, resp.Body)
		}
		resp.into(&r)
		return r
	}
	meta := e.scoped(auth.Observer, map[string]string{"src_meta": "meta"})
	if r := find(meta, "机密词"); r.Total != 0 || len(r.Hits) != 0 || r.Withheld != nil {
		t.Fatalf("trimmed or withheld content leaked through search: %+v", r)
	}
	if r := find(meta, "周报"); len(r.Hits) != 1 || r.Hits[0].ID != a || r.Hits[0].Snippet != "" {
		t.Fatalf("meta tier: title only, no snippet: %+v", r)
	}
	if r := find(e.token(auth.Observer), "机密词"); r.Total != 2 {
		t.Fatalf("internal key: %+v", r)
	}
	if r := e.do(e.token(auth.Webhook), "GET", "/v1/search?q=x", ""); r.Code != 403 {
		t.Fatalf("webhook credential: %d", r.Code)
	}
}
