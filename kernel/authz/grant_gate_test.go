package authz

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

var now = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func grantReq(l Level, cmd string, g *policy.Grant) Request {
	r := req(l, ActionUpdateOwn, Object{ID: "i1", Kind: "task", Project: "p1"})
	r.Tier, r.Command, r.Now, r.Credential, r.CredentialHarness = TierDecide, cmd, now, "cred1", "hermes"
	if g != nil {
		r.GrantID, r.Grant = "grt_1", g
		r.Actor.Session, r.Actor.SessionKind = "", "" // 授权单不需要会话
	}
	return r
}

func importGrant() *policy.Grant {
	return &policy.Grant{ID: "grt_1", Grantee: policy.Grantee{Credential: "cred1"}, Scope: policy.Scope{All: true},
		Actions: []string{policy.ImportApply}, Import: &policy.ImportScope{Source: "notes", MaxRecords: 5}, RiskCeiling: policy.RiskNormal,
		ExpiresAt: now.Add(time.Hour)}
}

func code(d Decision) domain.Code {
	if d.Err == nil {
		return ""
	}
	return d.Err.Code
}

// ACT-IGR-01 / WB-20：import.apply 只接受授权单。
func TestImportApplyOnlyAcceptsGrant(t *testing.T) {
	p := pol()
	for _, l := range []Level{LevelDelegate, LevelRun} {
		r := grantReq(l, policy.ImportApply, nil)
		r.RunHarness = "hermes"
		if d := Decide(p, r); code(d) != domain.CodeGrantRequired || d.ExitCode() != 23 {
			t.Errorf("%s without grant: %+v", l, d)
		}
		r.Evidence = "owner said so" // 只带本人原话，同样 23；会话为 direct 也一样
		if d := Decide(p, r); code(d) != domain.CodeGrantRequired || d.ExitCode() != 23 {
			t.Errorf("%s with words: %+v", l, d)
		}
	}
	// 对照：同样只带原话的 item.progress 在 direct 会话里照常放行。
	r := grantReq(LevelDelegate, "item.progress", nil)
	r.Evidence = "owner said so"
	if d := Decide(p, r); d.Err != nil {
		t.Fatalf("words still work for other commands: %+v", d)
	}
	// 持有导入单的 delegate 放行，不要会话引用与私聊。
	r = grantReq(LevelDelegate, policy.ImportApply, importGrant())
	r.Actor.SessionKind = domain.SessionGroup
	if d := Decide(p, r); d.Err != nil {
		t.Fatalf("import grant: %+v", d)
	}
	// 本人在 Web 上传不经这一档。
	if d := Decide(p, grantReq(LevelWeb, policy.ImportApply, nil)); d.Err != nil {
		t.Fatalf("web owner: %+v", d)
	}
}

func TestImportGrantDoesNotWiden(t *testing.T) {
	p := pol()
	g := importGrant()
	// 导入单不放行别的命令。
	if d := Decide(p, grantReq(LevelDelegate, "item.update", g)); code(d) != domain.CodeBasisRequired {
		t.Fatalf("import grant used for item.update: %+v", d)
	}
	// 不是导入专用单（actions 没有 import.apply）不放行 import.apply。
	plain := &policy.Grant{ID: "grt_1", Grantee: policy.Grantee{Credential: "cred1"}, Scope: policy.Scope{All: true},
		Actions: []string{"item.update"}, RiskCeiling: policy.RiskNormal}
	if d := Decide(p, grantReq(LevelDelegate, policy.ImportApply, plain)); code(d) != domain.CodeBasisRequired || d.ExitCode() != 23 {
		t.Fatalf("plain grant for import.apply: %+v", d)
	}
	// 撤销、过期、他人的单、后台运行都不行。
	g.RevokedAt = now.Add(-time.Minute)
	if d := Decide(p, grantReq(LevelDelegate, policy.ImportApply, g)); code(d) != domain.CodeBasisRequired {
		t.Fatalf("revoked: %+v", d)
	}
	g = importGrant()
	g.ExpiresAt = now
	if d := Decide(p, grantReq(LevelDelegate, policy.ImportApply, g)); code(d) != domain.CodeBasisRequired {
		t.Fatalf("expired: %+v", d)
	}
	g = importGrant()
	g.Grantee.Credential = "other"
	if d := Decide(p, grantReq(LevelDelegate, policy.ImportApply, g)); code(d) != domain.CodeBasisRequired {
		t.Fatalf("foreign: %+v", d)
	}
	g = importGrant()
	g.Grantee = policy.Grantee{Agent: "hermes"}
	r := grantReq(LevelRun, policy.ImportApply, g)
	r.RunHarness, r.TriggerTrusted = "hermes", true
	r.Object.InRunScope = true
	if d := Decide(p, r); code(d) != domain.CodeBasisRequired {
		t.Fatalf("background run: %+v", d)
	}
}

// ACT-IGR-08 / WB-19：item.progress 凭单放行；对象越出 scope 返回 23。
func TestProgressGrantScope(t *testing.T) {
	p := pol()
	g := &policy.Grant{ID: "grt_1", Grantee: policy.Grantee{Credential: "cred1"}, Scope: policy.Scope{Projects: []string{"p1"}},
		Actions: []string{"item.progress"}, RiskCeiling: policy.RiskNormal}
	if d := Decide(p, grantReq(LevelDelegate, "item.progress", g)); d.Err != nil {
		t.Fatalf("in scope: %+v", d)
	}
	r := grantReq(LevelDelegate, "item.progress", g)
	r.Object.Project = "p2"
	if d := Decide(p, r); code(d) != domain.CodeBasisRequired || d.ExitCode() != 23 {
		t.Fatalf("out of scope: %+v", d)
	}
}
