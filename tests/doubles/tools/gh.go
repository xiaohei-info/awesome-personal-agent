package tools

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ghRepo = "acc-org/acc-repo"

type ghCase struct {
	AgentsSHA string            `json:"agents_sha"`
	Issues    []json.RawMessage `json:"issues"`
}

// ghCall 回放 `gh api <path>`：issues 列表按 since/page/per_page 过滤分页，与 GitHub REST 一致；
// 其余子命令（含 auth login）一律拒绝，保证没有写操作。
func ghCall(c string, args []string, root string) out {
	if len(args) < 2 || args[0] != "api" {
		return failf(ExitRefused, "tool-stub: gh %v 不是录制过的只读命令", args)
	}
	if c == "gh-auth-401" {
		return failf(1, "gh: Bad credentials (HTTP 401)")
	}
	b, err := readFixture(root, "tests", "doubles", "tools", "testdata", "gh", c+".json")
	if err != nil {
		return failf(ExitNoCase, "tool-stub: 未知代码托管 case %q", c)
	}
	var cs ghCase
	if err := json.Unmarshal(b, &cs); err != nil {
		return failf(ExitNoCase, "tool-stub: %s.json: %v", c, err)
	}
	u, err := url.Parse(args[len(args)-1])
	if err != nil {
		return failf(ExitRefused, "tool-stub: 路径无法解析 %q", args[len(args)-1])
	}
	q := u.Query()
	switch p := strings.TrimPrefix(u.Path, "/"); {
	case p == "repos/"+ghRepo+"/issues":
		since, _ := time.Parse(time.RFC3339, q.Get("since"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		page, _ := strconv.Atoi(q.Get("page"))
		if per <= 0 {
			per = 30
		}
		if page <= 0 {
			page = 1
		}
		var hit []json.RawMessage
		for _, raw := range cs.Issues { // 样本已按 updated_at 升序
			var it struct {
				UpdatedAt time.Time `json:"updated_at"`
			}
			json.Unmarshal(raw, &it)
			if !it.UpdatedAt.Before(since) {
				hit = append(hit, raw)
			}
		}
		lo := min((page-1)*per, len(hit))
		hi := min(lo+per, len(hit))
		o := marshal(append([]json.RawMessage{}, hit[lo:hi]...))
		return ok(string(o) + "\n")
	case p == "repos/"+ghRepo+"/contents/AGENTS.md":
		if cs.AgentsSHA == "" {
			return failf(1, "gh: Not Found (HTTP 404)")
		}
		o := marshal(map[string]any{"type": "file", "sha": cs.AgentsSHA, "html_url": "https://github.com/" + ghRepo + "/blob/main/AGENTS.md"})
		return ok(string(o) + "\n")
	case p == "notifications":
		return ok("[]\n")
	}
	return failf(1, "gh: Not Found (HTTP 404)")
}
