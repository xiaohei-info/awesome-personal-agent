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

// importGrant 是导入下线前签发的导入专用单（只读的历史值，§14.5）。
func importGrant() *policy.Grant {
	return &policy.Grant{ID: "grt_1", Grantee: policy.Grantee{Credential: "cred1"}, Scope: policy.Scope{All: true},
		Actions: []string{"import.apply"}, Import: &policy.ImportScope{Source: "notes", MaxRecords: 5}, RiskCeiling: policy.RiskNormal,
		ExpiresAt: now.Add(time.Hour)}
}

func code(d Decision) domain.Code {
	if d.Err == nil {
		return ""
	}
	return d.Err.Code
}

// 导入下线后，库里旧的导入专用单不放行任何命令。
func TestImportGrantDoesNotWiden(t *testing.T) {
	p := pol()
	if d := Decide(p, grantReq(LevelDelegate, "item.update", importGrant())); code(d) != domain.CodeBasisRequired {
		t.Fatalf("import grant used for item.update: %+v", d)
	}
}

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
