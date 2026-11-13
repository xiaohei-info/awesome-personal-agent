//go:build e2e

package judge

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("不是 JSON 对象: %v\n%s", err, s)
	}
	return m
}

func sub(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestFlowEndpointConfig 对应 flow-endpoint-config（ACT-EPC-01..10）。
func TestFlowEndpointConfig(t *testing.T) {
	// ACT-EPC-01 要在一个新的空数据目录上跑 `apa init`，所以自己建环境，不用共用的 setup。
	e := baseline.New(t)
	e.Init()
	cfg, _ := os.ReadFile(filepath.Join(e.Home, "config.yaml"))
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	cls := modelstub.NewClsAt("127.0.0.1:0")
	t.Cleanup(cls.Close)
	e.Cls = cls
	e.ModelStub = modelstub.NewAt("127.0.0.1:0")
	f := &fx{Env: e, t: t, src: "src_acctext", events: filepath.Join(e.Root, "acc_text_events.jsonl")}
	f.registerText()

	f.step(t, "ACT-EPC-01 新配置键带默认值", func(t *testing.T) {
		f.webOK("PUT", "/settings/model-endpoints", mustJSON(map[string]any{
			"judge_endpoint": map[string]any{"url": cls.URL()}, "generate_endpoint": map[string]any{"url": e.ModelStub.URL()}}), 200)
		got := jsonMap(t, f.webOK("GET", "/settings/model-endpoints", "", 200))
		j := sub(got, "judge_endpoint")
		for k, want := range map[string]any{"timeout_ms": 10000.0, "max_batch": 32.0, "max_material_chars": 8000.0, "protocol": "probability-judgment/1", "locality": "external"} {
			eq(t, "judge_endpoint."+k, j[k], want)
		}
		eq(t, "generate_endpoint.locality", sub(got, "generate_endpoint")["locality"], "external")
		eq(t, "screening_timeout_ms（顶层）", got["screening_timeout_ms"], 30000.0)
		if _, nested := sub(got, "generate_endpoint")["screening_timeout_ms"]; nested {
			t.Error("screening_timeout_ms 应在顶层，不在端点里")
		}
		jd := jsonMap(t, f.webOK("GET", "/settings/judgment", "", 200))
		eq(t, "spot_check.per_brief", sub(jd, "spot_check")["per_brief"], 5.0)
		// config.yaml 里的键：未写出视为取默认值，写出了就必须等于默认值。
		for k, want := range map[string]string{"backlog_release_per_day": "40", "judgment_retention_days": "180", "delegation_stall_after": "24h"} {
			for _, line := range strings.Split(string(cfg), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), k+":") && strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), k+":")) != want {
					t.Errorf("config.yaml 的 %s 与默认值 %s 不符: %q", k, want, line)
				}
			}
		}
		pack := repoFile(t, "packs/follow-up/pack.yaml")
		if !strings.Contains(pack, "remind_rounds: 2") || !strings.Contains(pack, "stale_after: 14d") {
			t.Errorf("follow-up 包的 remind_rounds/stale_after 默认值不符:\n%s", pack)
		}
		if !strings.Contains(f.APIRaw("GET", "/v1/packs", "", e.OBS).Body, "follow-up") {
			t.Error("GET /packs 没有列出 follow-up 包")
		}
	})

	f.step(t, "ACT-EPC-02 在 Web 分别配置两个端点", func(t *testing.T) {
		f.webOK("PUT", "/settings/model-endpoints", mustJSON(map[string]any{
			"judge_endpoint":    map[string]any{"url": cls.URL(), "api_key": "acc-cls-key", "locality": "local", "timeout_ms": 8000},
			"generate_endpoint": map[string]any{"url": e.ModelStub.URL(), "model": "acc-model", "api_key": "acc-gen-key", "locality": "local"}}), 200)
		body := f.webOK("GET", "/settings/model-endpoints", "", 200)
		if strings.Contains(body, "acc-cls-key") || strings.Contains(body, "acc-gen-key") {
			t.Fatalf("api_key 不应读回明文: %s", body)
		}
		got := jsonMap(t, body)
		j, g := sub(got, "judge_endpoint"), sub(got, "generate_endpoint")
		eq(t, "judge.url", j["url"], cls.URL())
		eq(t, "judge.api_key_set", j["api_key_set"], true)
		eq(t, "judge.locality", j["locality"], "local")
		eq(t, "judge.timeout_ms", j["timeout_ms"], 8000.0)
		eq(t, "generate.url", g["url"], e.ModelStub.URL())
		eq(t, "generate.model", g["model"], "acc-model")
		eq(t, "generate.api_key_set", g["api_key_set"], true)
		if !strings.Contains(f.webOK("GET", "/audit/events?limit=20&type=policy.changed", "", 200), "policy.changed") {
			t.Error("审计页没有 policy.changed")
		}
		cls.Judge(modelstub.ClsJudge{Key: key("epc-auth"), PYes: pf(0.05)})
		id := f.write(ev{Subject: "epc-auth", ID: "epc-auth"})[0]
		f.settled(id, 60*time.Second)
		bs := cls.Batches()
		if len(bs) == 0 || bs[0].Header.Get("Authorization") != "Bearer acc-cls-key" {
			t.Errorf("CLS_STUB 的请求应带 Authorization: Bearer acc-cls-key: %+v", bs)
		}
	})

	f.step(t, "ACT-EPC-03 非 Web 凭据不能改端点配置", func(t *testing.T) {
		before := f.webOK("GET", "/settings/model-endpoints", "", 200)
		for name, tok := range map[string]string{"DEL": e.DEL, "WRK": e.WRK, "OBS": e.OBS} {
			r := e.APIRaw("PUT", "/v1/settings/model-endpoints", `{"judge_endpoint": null}`, tok)
			if r.Code != 403 {
				t.Errorf("%s 改端点 HTTP %d，期望 403: %s", name, r.Code, r.Body)
			}
		}
		eq(t, "配置不变", f.webOK("GET", "/settings/model-endpoints", "", 200), before)
		if !strings.Contains(f.webOK("GET", "/audit/events?limit=50&type=command.rejected", "", 200), "command.rejected") {
			t.Error("审计页没有 command.rejected")
		}
	})

	f.step(t, "ACT-EPC-07 只配生成端点时判断题全部由对话端点回答", func(t *testing.T) {
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint": null}`, 200)
		f.reset()
		f.ModelStub.Judge(modelstub.ChatJudge{Key: key("epc-gen-only"), Answer: "yes"})
		f.ModelStub.Judge(modelstub.ChatJudge{Key: key("epc-gen-only"), Answer: "yes"})
		id := f.write(ev{Subject: "epc-gen-only", Detail: "只有生成端点", ID: "epc-gen-only"})[0]
		se, j := f.judgmentOf(id)
		if n := len(cls.Batches()); n != 0 {
			t.Errorf("CLS_LOG 应无新请求，实际 %d", n)
		}
		eq(t, "levels[0].outcome", j.Levels[0].Outcome, "skipped")
		eq(t, "levels[0].backend", j.Levels[0].Backend, "classifier")
		eq(t, "decided_by", j.DecidedBy, "chat")
		eq(t, "处置", se.Disposition, "proposed")
	})

	f.step(t, "ACT-EPC-08 只配判断端点时是非题照常定案，起草退化为来源标题", func(t *testing.T) {
		f.reset()
		f.webOK("PUT", "/settings/model-endpoints", mustJSON(map[string]any{
			"judge_endpoint":    map[string]any{"url": cls.URL(), "api_key": "acc-cls-key", "locality": "local"},
			"generate_endpoint": nil}), 200)
		cls.Judge(modelstub.ClsJudge{Key: key("epc-judge-low"), PYes: pf(0.05)})
		cls.Judge(modelstub.ClsJudge{Key: key("epc-judge-high"), PYes: pf(0.9)})
		ids := f.write(ev{Subject: "epc-judge-low", ID: "epc-judge-low"}, ev{Subject: "epc-judge-high", Detail: "需要决定", ID: "epc-judge-high"})
		low := f.settled(ids[0], 60*time.Second)
		high := f.settled(ids[1], 60*time.Second)
		if n := len(f.ModelStub.Requests()); n != 0 {
			t.Errorf("MODEL_LOG 应无请求，实际 %d", n)
		}
		eq(t, "第一条处置", low.Disposition, "ignored")
		eq(t, "第二条处置", high.Disposition, "proposed")
		item := jsonMap(t, f.apa(f.OBS, 0, "item", "show", high.Item).Stdout)
		it := sub(item, "item")
		if it == nil {
			it = item
		}
		eq(t, "建议标题", it["title"], "epc-judge-high")
	})

	f.step(t, "ACT-EPC-09 两个端点都不配时只走规则", func(t *testing.T) {
		f.reset()
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint": null, "generate_endpoint": null}`, 200)
		id := f.write(ev{Subject: "epc-none", Detail: "没有模型", ID: "epc-none"})[0]
		se := f.settled(id, 60*time.Second)
		eq(t, "处置", se.Disposition, "unjudged")
		eq(t, "原因", se.Unjudged, "model_unavailable")
		if len(cls.Batches()) != 0 || len(f.ModelStub.Requests()) != 0 {
			t.Errorf("两份日志都应无请求: cls=%d model=%d", len(cls.Batches()), len(f.ModelStub.Requests()))
		}
		// 决策请求不做初筛，立即按规则进入决策流。
		task := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "epc-ask", "--phase", "open", "--assignee", "work-host.cli",
			"--project", e.Projects["acc-work"], "--evidence", "建任务").Stdout
		taskID := jsonMap(t, task)["id"].(string)
		f.apa(e.WRK, 0, "ask", taskID, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
		item := f.Web("GET", "/v1/items/"+taskID, "")
		if strings.Contains(item.Body, `"screening_pending":true`) {
			t.Errorf("没有端点时决策不应停在初筛中: %s", item.Body)
		}
		_, d := f.modelHealth()
		eq(t, "model_unavailable.detail.judge", d["judge"], "not_configured")
		eq(t, "model_unavailable.detail.generate", d["generate"], "not_configured")
	})

	f.step(t, "ACT-EPC-10 恢复缺省端点接线", func(t *testing.T) {
		e.WireEndpoints()
		got := jsonMap(t, f.webOK("GET", "/settings/model-endpoints", "", 200))
		eq(t, "judge.url", sub(got, "judge_endpoint")["url"], cls.URL())
		eq(t, "generate.url", sub(got, "generate_endpoint")["url"], e.ModelStub.URL())
	})

	f.step(t, "ACT-EPC-04..06 旧账本里的 model_endpoint 读作生成端点", func(t *testing.T) {
		legacyFixture(t, e)
	})
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func hashOf(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

// registerText 登记 ACC_TEXT（同 setup，但用在已有环境上）。
func (f *fx) registerText() {
	script := filepath.Join(f.Root, "acc_text.sh")
	if err := os.WriteFile(script, []byte(accScript), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.webOK("POST", "/sources", mustJSON(map[string]any{"id": f.src, "type": "script", "source_type": "acc",
		"command": "/bin/sh " + script + " " + f.events, "schedule": "1s", "timeout": 10}), 201)
	f.webOK("PUT", "/settings/sources/"+f.src+"/outbound", `{"local":"full"}`, 200)
}
