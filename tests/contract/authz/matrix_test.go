package authz_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/decisions"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// self 是"自己的事项"的调用者。
const self = "caller"

func actorOf(lv authz.Level) domain.Actor {
	if lv == authz.LevelWeb {
		return domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	}
	return domain.Actor{Type: domain.ActorAgent, Principal: self, Via: domain.ViaCLI}
}

// Web 这一列只授予 Web 上的本人：不是人、或不是经 Web 来的，声称 Web 级也被拒绝。
func TestWebLevelCannotBeClaimedByAgent(t *testing.T) {
	pol := policy.Default()
	for _, a := range []authz.Action{authz.ActionChangeSettings, authz.ActionEditAnyItem, authz.ActionRead} {
		d := authz.Decide(pol, authz.Request{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "x", Via: domain.ViaCLI}, Level: authz.LevelWeb, Action: a})
		if d.ExitCode() != domain.ExitPermissionDenied {
			t.Errorf("%s: agent claiming web level got exit %d", a, d.ExitCode())
		}
	}
}

// "把唤醒运行的 id 换成运行凭据"：observer 否，worker、delegate 可（过了授权，才轮到运行本身是否存在），运行凭据与 Web 否。
func TestMatrixRunIDExchange(t *testing.T) {
	e := setup(t)
	call := func(c command.Caller) error {
		return e.st.Update(bg, func(tx *store.Tx) error {
			_, _, err := runidentity.Exchange(tx, c, "no-such-run", e.now())
			return err
		})
	}
	cases := []struct {
		col    string
		caller command.Caller
		denied bool
	}{
		{"observer", command.Caller{Level: authz.LevelObserver, Kind: auth.Observer}, true},
		{"worker", command.Caller{Level: authz.LevelWorker, Kind: auth.Worker}, false},
		{"delegate", command.Caller{Level: authz.LevelDelegate, Kind: auth.Delegate}, false},
		{"run", command.Caller{Level: authz.LevelRun}, true},
		{"web", command.Caller{Level: authz.LevelWeb, Actor: owner.Actor}, true},
	}
	for _, c := range cases {
		err := call(c.caller)
		var de *domain.Error
		if !asErr(err, &de) {
			t.Fatalf("%s: expected an error for an unknown run, got %v", c.col, err)
		}
		if got := de.ExitCode() == domain.ExitPermissionDenied; got != c.denied {
			t.Errorf("%s: permission-denied=%v, want %v (%v)", c.col, got, c.denied, err)
		}
	}
}

// ---- 对照矩阵（§3.5 验收）：身份 × 每档一条代表命令 × 入口 ----

// tierCols 是每组挑的一条代表命令：查看、整理事项（改事项）、替我拍板（接受建议）、改日常（对话捕获开关）、Agent 与执行者配置（加远程仓库）。
var tierCols = []string{"read", "organize", "decide", "rules", "power"}

// entryOutcome 是一次调用的结果：成功为 0，被拒为错误体的退出码。
type entryOutcome struct {
	exit   int
	reason string
}

// subjects 为一格建好它要的对象：事项与建议都指派给 worker（它的"自己的事项"），项目每格新建，互不干扰。
func (e *env) subject(col string, n int) string {
	e.t.Helper()
	switch col {
	case "read", "organize":
		return e.item(workHost, fmt.Sprintf("m%d", n))
	case "decide":
		return e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": fmt.Sprintf("p%d", n), "assignee": workHost})
	}
	return e.cmd(owner, "project.create", map[string]any{"name": fmt.Sprintf("P%d", n)})
}

func cliArgs(col, id string, n int) []string {
	switch col {
	case "read":
		return []string{"item", "show", id}
	case "organize":
		return []string{"item", "update", id, "--summary", "s", "--reason", "r"}
	case "decide":
		return []string{"accept", id}
	case "rules":
		return []string{"project", "update", id, "--capture=true"}
	}
	return []string{"project", "repos", "add", id, "--remote", fmt.Sprintf("https://example.com/r%d.git", n)}
}

func httpCall(col, id string, n int) (method, path, body string) {
	switch col {
	case "read":
		return "GET", "/v1/items/" + id, ""
	case "organize":
		return "PATCH", "/v1/items/" + id, `{"summary":"s","reason":"r"}`
	case "decide":
		return "POST", "/v1/items/" + id + "/accept", `{}`
	case "rules":
		return "PATCH", "/v1/projects/" + id, `{"capture":true}`
	}
	return "POST", "/v1/projects/" + id + "/repos", fmt.Sprintf(`{"op":"add_remote","remote":"https://example.com/r%d.git"}`, n)
}

// runFor 为目标对象拉一次后台运行（不归属任何 Agent，档位是只整理），返回它的运行凭据。
func (e *env) runFor(target string) string {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, time.Now())
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, time.Now())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

func outcomeOf(t *testing.T, code int, body []byte) entryOutcome {
	t.Helper()
	if code < 300 {
		return entryOutcome{}
	}
	var de domain.Error
	if err := json.Unmarshal(body, &de); err != nil || de.Code == "" {
		t.Fatalf("not a B.3 error body (%d): %s", code, body)
	}
	return entryOutcome{de.ExitCode(), de.Reason}
}

// 同一把钥匙发同一条命令，从命令行与直接 HTTP 进来结果逐格相同（插件走命令行，与它同一入口）；结果只取决于钥匙范围与档位，
// 调用方自报的本人原话、会话类型、授权单（--evidence、APA_SESSION_KIND、X-APA-Grant）一律不改变结果。Web 本人没有命令行入口。
func TestEntryMatrixIdentityByTier(t *testing.T) {
	e := setupWith(t, true)
	e.agent("DEL_DECIDE", "agent_d", policy.DefaultCard().With(policy.GroupDecide))
	e.agent("DEL_RULES", "agent_r", policy.DefaultCard().With(policy.GroupDecide, policy.GroupRules))
	ts := httptest.NewServer(e.srv.Handler())
	t.Cleanup(ts.Close)

	const run = "RUN"
	rows := []struct {
		who  string
		want [5]int
	}{
		{"OBS", [5]int{0, 4, 4, 4, 4}},
		{"WRK", [5]int{0, 0, 23, 4, 4}},
		{"DEL", [5]int{0, 0, 23, 23, 23}},
		{"DEL_DECIDE", [5]int{0, 0, 0, 23, 23}},
		{"DEL_RULES", [5]int{0, 0, 0, 0, 23}},
		{run, [5]int{0, 0, 23, 23, 23}}, // 运行凭据只带身份：与所属 Agent（未设档位，只整理）的钥匙相同
		{"web", [5]int{0, 0, 0, 0, 0}},
	}
	wantReason := map[int]string{domain.ExitPermissionDenied: domain.ReasonOutOfScope, domain.ExitTierClosed: domain.ReasonTierClosed, domain.ExitWebOnly: domain.ReasonHardLine}
	n := 0
	for _, row := range rows {
		for i, col := range tierCols {
			got := map[string]entryOutcome{}
			for _, entry := range []string{"cli", "http"} {
				if row.who == "web" && entry == "cli" {
					continue
				}
				n++
				id := e.subject(col, n)
				secret := e.secret[row.who]
				if row.who == run {
					target := id
					if col == "rules" || col == "power" { // 对象是项目：运行的目标另建一条事项
						target = e.item(workHost, fmt.Sprintf("t%d", n))
					}
					secret = e.runFor(target)
				}
				switch {
				case entry == "cli":
					var out, errb bytes.Buffer
					vars := map[string]string{"APA_URL": ts.URL, "APA_TOKEN": secret, "APA_SESSION": "s1", "APA_SESSION_KIND": "direct", "APA_HOME": t.TempDir()}
					code := client.Main(append([]string{"--evidence", "owner: yes, do it", "--grant", "grt_forged"}, cliArgs(col, id, n)...),
						client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: t.TempDir(), Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
					o := entryOutcome{exit: code}
					if code != 0 {
						o = outcomeOf(t, 403, errb.Bytes())
						if o.exit != code {
							t.Fatalf("%s %s cli: exit %d but body says %d", row.who, col, code, o.exit)
						}
					}
					got[entry] = o
				case row.who == "web":
					m, p, b := httpCall(col, id, n)
					rec := e.web(m, p, b)
					got[entry] = outcomeOf(t, rec.Code, rec.Body.Bytes())
				default:
					m, p, b := httpCall(col, id, n)
					rec := e.http(m, p, b, func(r *http.Request) {
						bearer(secret)(r)
						r.Header.Set("X-APA-Evidence", "owner: yes, do it")
						r.Header.Set("X-APA-Session-Kind", "direct")
						r.Header.Set("X-APA-Grant", "grt_forged")
					})
					got[entry] = outcomeOf(t, rec.Code, rec.Body.Bytes())
				}
			}
			for entry, o := range got {
				if o.exit != row.want[i] || (o.exit != 0 && o.reason != wantReason[o.exit]) {
					t.Errorf("%s × %s via %s: exit %d reason %q, want %d %q", row.who, col, entry, o.exit, o.reason, row.want[i], wantReason[row.want[i]])
				}
			}
			if c, h := got["cli"], got["http"]; row.who != "web" && c != h {
				t.Errorf("%s × %s: cli %+v differs from http %+v", row.who, col, c, h)
			}
		}
	}
}
