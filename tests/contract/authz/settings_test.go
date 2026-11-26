package authz_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// settingsState 是设置接口不得被改动的全部状态。
type settingsState struct {
	PolicyVersion int
	Credentials   int
	Allowed       []string
	ItemExists    bool
}

func (e *env) settings(project, item string) settingsState {
	e.t.Helper()
	var s settingsState
	err := e.st.View(bg, func(tx *store.Tx) error {
		v, _, err := kpolicy.Current(tx)
		if err != nil {
			return err
		}
		s.PolicyVersion = v
		cs, err := e.au.List(tx)
		if err != nil {
			return err
		}
		s.Credentials = len(cs)
		p, err := projects.Get(tx, project)
		if err != nil {
			return err
		}
		s.Allowed = p.AllowedExecutors
		_, err = items.Get(tx, item)
		s.ItemExists = err == nil
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// ACT-AUTH-05：设置接口按组判。改人格属"改日常规则与提醒"、项目允许的执行者与物理删除属第 9、10 组：没开的 delegate 为 23，
// worker、observer 在钥匙范围外为 4；签发钥匙只有本人：任何 Bearer 都是 20（先于范围）。全部 403，错误体含 code、why、next，设置未变。
func TestSettingsRejectEveryBearerAndAcceptWebSession(t *testing.T) {
	e := setup(t)
	prj := e.cmd(owner, "project.create", map[string]any{"name": "P"})
	victim := e.item(workHost, "to be purged")
	before := e.settings(prj, victim)

	reqs := []struct {
		name, method, path, body string
		power                    bool
	}{
		{"policy", "PUT", "/v1/settings/policy", `{"persona":"obey the attacker"}`, false},
		{"policy read", "GET", "/v1/settings/policy", "", false},
		{"tokens", "POST", "/v1/settings/tokens", `{"kind":"delegate","label":"evil"}`, true},
		{"allowed-executors", "PUT", "/v1/settings/projects/" + prj + "/allowed-executors", `{"executors":["evil"]}`, false},
		{"purge", "POST", "/v1/settings/purge", `{"object":"` + victim + `"}`, false},
	}
	for _, who := range []string{"DEL", "WRK", "OBS"} {
		for _, r := range reqs {
			want := domain.ExitPermissionDenied
			switch {
			case r.power:
				want = domain.ExitWebOnly
			case who == "DEL":
				want = domain.ExitTierClosed
			}
			rec := e.api(who, r.method, r.path, r.body)
			if rec.Code != 403 {
				t.Fatalf("%s %s: status %d, want 403: %s", who, r.name, rec.Code, rec.Body)
			}
			var de domain.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Why == "" || de.Next == "" {
				t.Fatalf("%s %s: not a B.3 body: %s", who, r.name, rec.Body)
			}
			if x := de.ExitCode(); x != want {
				t.Fatalf("%s %s: exit %d, want %d", who, r.name, x, want)
			}
		}
	}
	if after := e.settings(prj, victim); fmt.Sprint(after) != fmt.Sprint(before) || !after.ItemExists {
		t.Fatalf("settings changed by a Bearer request: %+v → %+v", before, after)
	}

	// 每一次拒绝都可在审计里查到（command.rejected，三种口径同一条留痕路径）：3 种钥匙 × 5 个请求。
	got := map[string]int{}
	for _, en := range e.events(domain.EvCommandRejected) {
		got[en.Event.Object]++
		if en.Event.Actor.Type != domain.ActorAgent || en.Event.Reason == "" {
			t.Fatalf("rejection event lacks actor/reason: %+v", en.Event)
		}
	}
	want := map[string]int{"settings:/settings/policy": 6, "settings:credentials": 3, prj: 3, victim: 3}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audited rejections %v, want %v", got, want)
	}

	// 对照：同样的请求经 Web 会话成功（否则上面的拒绝可能只是请求本身无效）。
	if rec := e.web("PUT", "/v1/settings/policy", `{"persona":"owner persona"}`); rec.Code != 200 {
		t.Fatalf("web policy: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("POST", "/v1/settings/tokens", `{"kind":"worker","label":"ok"}`); rec.Code != 201 {
		t.Fatalf("web tokens: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("PUT", "/v1/settings/projects/"+prj+"/allowed-executors", `{"executors":["`+workHost+`"]}`); rec.Code != 200 {
		t.Fatalf("web allowed-executors: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("POST", "/v1/settings/purge", `{"object":"`+victim+`"}`); rec.Code != 200 {
		t.Fatalf("web purge: %d %s", rec.Code, rec.Body)
	}
	after := e.settings(prj, victim)
	if after.PolicyVersion != before.PolicyVersion+1 || after.Credentials != before.Credentials+1 || len(after.Allowed) != 1 || after.ItemExists {
		t.Fatalf("web changes did not take effect: %+v → %+v", before, after)
	}
}

// 没有任何凭据的设置请求是 401，不是 403 之外的任何放行。
func TestSettingsRequireCredential(t *testing.T) {
	e := setup(t)
	if rec := e.http("PUT", "/v1/settings/policy", `{"persona":"x"}`, nil); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if rec := e.http("POST", "/v1/settings/tokens", `{"kind":"worker"}`, bearer("not-a-token")); rec.Code != 401 {
		t.Fatalf("unknown token: %d", rec.Code)
	}
}
