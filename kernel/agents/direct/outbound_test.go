package direct

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// 生成型调用的统一外发检查（§14.14、§6.14 第 15 条）：事项出自来源、调用方没有自己裁剪时，
// 来源对端点位置的范围未选择或 none 就不发请求；与调用方无关，也不依赖事项当前阶段。
func TestDoBlocksSourceContentWhenOutboundScopeNotChosen(t *testing.T) {
	const (
		unset = ""
		none  = "none"
		full  = "full"
	)
	for _, tc := range []struct {
		name     string
		locality string // 生成端点位置
		external string // 来源外部范围；unset 表示未选择
		origin   string // 事项 origin.source；空表示没有来源
		require  string // 调用方自己裁剪过
		blocked  bool
		noBody   bool // 资料段为空：来源内容只在结构化输入的标题里
	}{
		{"external endpoint, scope unset", policy.LocalityExternal, unset, "src_a", "", true, false},
		{"external endpoint, scope none", policy.LocalityExternal, none, "src_a", "", true, false},
		{"external endpoint, scope full", policy.LocalityExternal, full, "src_a", "", false, false},
		{"local endpoint ignores the unset external scope", policy.LocalityLocal, unset, "src_a", "", false, false},
		{"item without origin", policy.LocalityExternal, unset, "", "", false, false},
		{"caller already trimmed", policy.LocalityExternal, unset, "src_a", policy.LocalityExternal, false, false},
		{"no material, scope unset", policy.LocalityExternal, unset, "src_a", "", true, true},
		{"no material, scope none", policy.LocalityExternal, none, "src_a", "", true, true},
		{"capture project, scope unset", policy.LocalityExternal, unset, "capture:", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, true)
			e.stub.SetDefault(modelstub.Reply{Content: `{"summary":"ok"}`})
			if err := e.st.Update(bg, func(tx *store.Tx) error {
				_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error { p.GenerateEndpoint.Locality = tc.locality; return nil })
				if err != nil {
					return err
				}
				_, err = tx.Exec(`INSERT INTO sources_registry_source
					(id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
					VALUES ('src_a', 'pull', 'k', '', '', 0, '', '', '', 'full', ?, '', '', 1, '')`, tc.external)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			origin := tc.origin
			if origin == "capture:" { // 对话捕获来源：范围在项目上，新建项目的 capture_outbound 未选择
				if err := projects.Register(e.x); err != nil {
					t.Fatal(err)
				}
				pr, err := e.x.Do(bg, command.Request{Identity: owner, Command: "project.create", Input: json.RawMessage(`{"name":"P"}`), Owner: "owner"})
				if err != nil {
					t.Fatal(err)
				}
				var pid struct{ ID string }
				_ = json.Unmarshal(pr.Value, &pid)
				origin += pid.ID
			}
			in := map[string]any{"title": "t", "kind": "task", "phase": "open", "body": "synthetic body"}
			if origin != "" {
				in["origin"] = map[string]string{"source": origin}
			}
			b, _ := json.Marshal(in)
			r, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.create", Input: b, Owner: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			var ref struct{ ID string }
			_ = json.Unmarshal(r.Value, &ref)
			c := e.call("explain")
			c.Target, c.Project, c.RequireLocality = ref.ID, "", tc.require
			if tc.noBody {
				c.Material = ""
			}

			_, err = e.svc.Do(bg, c)
			if got := errors.Is(err, ErrOutboundBlocked); got != tc.blocked || (!tc.blocked && err != nil) {
				t.Fatalf("err = %v, blocked want %v", err, tc.blocked)
			}
			if n := len(e.stub.Requests()); (n == 0) != tc.blocked {
				t.Fatalf("model requests = %d, blocked want %v", n, tc.blocked)
			}
			if tc.blocked && len(e.agentEvents(t, ref.ID)) != 0 {
				t.Fatal("a blocked call must not leave a run record")
			}
		})
	}
}
