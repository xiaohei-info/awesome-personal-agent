package authz_test

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 列：§14.3 表的五个调用者。
var columns = []authz.Level{authz.LevelObserver, authz.LevelWorker, authz.LevelDelegate, authz.LevelRun, authz.LevelWeb}

// 期望结果：允许 / 各退出码。exitEvidence 是"第二档"：不附本人原话或会话引用被拒（退出码 23），都附了允许。
const (
	ok           = 0
	denied       = domain.ExitPermissionDenied
	web          = domain.ExitWebOnly
	screening    = domain.ExitScreeningPending
	exitEvidence = -1
)

type row struct {
	name   string
	action authz.Action
	obj    authz.Object
	want   [5]int // 与 columns 同序
}

// 对象的 Assignee 与调用者本人一致，"限自己的事项"的行再给出一个不属于自己的对象。
const self = "caller"

func inScope() authz.Object {
	return authz.Object{ID: "x", Assignee: self, InRunScope: true, InRunProject: true}
}
func notMine() authz.Object     { return authz.Object{ID: "x", Assignee: "someone-else"} }
func decisionObj() authz.Object { return authz.Object{ID: "x", Kind: "decision"} }

// §14.3 表的每一行（"把唤醒运行的 id 换成运行凭据"另测，见 TestMatrixRunIDExchange）。
var rows = []row{
	{"读账本、订阅事件", authz.ActionRead, inScope(), [5]int{ok, ok, ok, ok, ok}},
	{"以 proposed 新建", authz.ActionCreateProposed, inScope(), [5]int{ok, ok, ok, ok, ok}},
	{"更新自己的事项（自己的）", authz.ActionUpdateOwn, inScope(), [5]int{denied, ok, ok, ok, ok}},
	{"更新自己的事项（目标事项及其子事项之外）", authz.ActionUpdateOwn, authz.Object{ID: "x", Assignee: self}, [5]int{denied, ok, ok, denied, ok}},
	{"更新自己的事项（别人的）", authz.ActionUpdateOwn, notMine(), [5]int{denied, denied, ok, denied, ok}},
	{"提交材料修订", authz.ActionReviseMaterial, notMine(), [5]int{denied, ok, ok, ok, ok}},
	{"apa delegate（自己的事项）", authz.ActionDelegate, inScope(), [5]int{denied, ok, ok, ok, ok}},
	{"apa delegate（别人的事项）", authz.ActionDelegate, notMine(), [5]int{denied, denied, ok, denied, ok}},
	{"增改 repos 的本机检出（目标事项所属项目内）", authz.ActionEditCheckout, inScope(), [5]int{denied, ok, ok, ok, ok}},
	{"增改 repos 的本机检出（项目外）", authz.ActionEditCheckout, authz.Object{ID: "x"}, [5]int{denied, ok, ok, denied, ok}},
	{"提交插件声明", authz.ActionSubmitPlugin, notMine(), [5]int{denied, ok, ok, denied, ok}},
	{"改任意事项（附原话与会话）", authz.ActionEditAnyItem, notMine(), [5]int{denied, denied, ok, denied, ok}},
	{"改任意事项（缺原话或会话）", authz.ActionEditAnyItem, notMine(), [5]int{denied, denied, exitEvidence, denied, ok}},
	{"代答决策、接受或丢弃建议（附原话与会话）", authz.ActionAnswerDecision, decisionObj(), [5]int{denied, denied, ok, denied, ok}},
	{"代答决策、接受或丢弃建议（缺原话或会话）", authz.ActionAnswerDecision, decisionObj(), [5]int{denied, denied, exitEvidence, denied, ok}},
	{"回答 web_only 决策", authz.ActionAnswerDecision, authz.Object{ID: "x", ScreenedWebOnly: true}, [5]int{denied, denied, web, denied, ok}},
	{"回答 web_only 初筛未返回的决策", authz.ActionAnswerDecision, authz.Object{ID: "x", ScreeningPending: true}, [5]int{denied, denied, screening, denied, ok}},
	{"建项目、改项目信息、增删 repos 的远程地址", authz.ActionManageProject, notMine(), [5]int{denied, denied, ok, denied, ok}},
	{"登记拉取程序、启停来源、重放入站记录、转建议、webhook 密钥、能力包实例", authz.ActionManageSource, notMine(), [5]int{denied, denied, ok, denied, ok}},
	{"改策略、信任级、token、白名单、允许的执行者等（设置类）", authz.ActionChangeSettings, notMine(), [5]int{denied, denied, web, denied, ok}},
}

// 每个格子都有断言：行 × 列的结果都被逐一核对，而且调用者 Actor 与 Level 一致（Web 为 Web 上的本人）。
func TestMatrixEveryRowAndColumn(t *testing.T) {
	pol := policy.Default()
	cells := 0
	for _, r := range rows {
		for i, lv := range columns {
			r, i, lv := r, i, lv
			t.Run(r.name+"/"+string(lv), func(t *testing.T) {
				req := authz.Request{Actor: actorOf(lv), Level: lv, Action: r.action, Object: r.obj,
					Owner: "owner", WebURL: "https://apa.example/items/x"}
				want := r.want[i]
				if want == exitEvidence {
					// 缺原话、缺会话引用：被拒；下面都附了才允许。
					d := authz.Decide(pol, req)
					if d.Verdict != authz.Deny || d.ExitCode() != domain.ExitBasisRequired {
						t.Fatalf("without evidence: %+v", d)
					}
					req.Evidence = "owner said so (chat#1)"
					if d = authz.Decide(pol, req); d.ExitCode() != domain.ExitBasisRequired {
						t.Fatalf("evidence without session: %+v", d)
					}
					want = ok
				}
				req.Evidence = "owner said so (chat#1)"
				req.Actor.OnBehalfOf, req.Actor.Session = "owner", "s1"
				d := authz.Decide(pol, req)
				if d.ExitCode() != want {
					t.Fatalf("exit %d, want %d (%+v)", d.ExitCode(), want, d)
				}
				switch want {
				case ok:
					if d.Verdict != authz.Allow || d.Err != nil {
						t.Fatalf("not allowed: %+v", d)
					}
				case web, screening:
					if want == web && d.Verdict != authz.NeedWeb {
						t.Fatalf("verdict %s", d.Verdict)
					}
					if d.Err.WebURL == nil || *d.Err.WebURL != req.WebURL {
						t.Fatalf("20/22 must carry the web link: %+v", d.Err)
					}
				}
			})
			cells++
		}
	}
	if cells != len(rows)*len(columns) || len(columns) != 5 {
		t.Fatalf("matrix incomplete: %d cells", cells)
	}
}

func actorOf(lv authz.Level) domain.Actor {
	if lv == authz.LevelWeb {
		return domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	}
	return domain.Actor{Type: domain.ActorAgent, Principal: self, Via: domain.ViaCLI}
}

// 表里涉及的授权动作必须被矩阵覆盖：新增动作而没有补行，这里立刻失败。
func TestMatrixCoversEveryTableAction(t *testing.T) {
	seen := map[authz.Action]bool{}
	for _, r := range rows {
		seen[r.action] = true
	}
	for _, a := range []authz.Action{authz.ActionRead, authz.ActionCreateProposed, authz.ActionUpdateOwn, authz.ActionReviseMaterial,
		authz.ActionDelegate, authz.ActionEditCheckout, authz.ActionSubmitPlugin, authz.ActionEditAnyItem,
		authz.ActionAnswerDecision, authz.ActionManageProject, authz.ActionManageSource, authz.ActionChangeSettings} {
		if !seen[a] {
			t.Errorf("action %s has no matrix row", a)
		}
	}
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
