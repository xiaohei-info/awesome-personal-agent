//go:build e2e

// Package webhook 按验收文档 flow-webhook（ACT-HOOK-01..13）用真实 apa 二进制和真实 HTTP 请求
// 独立执行"外部系统推送的事件落盘后才确认，重放不产生重复"。环境来自 flow-env-baseline 的共用脚本；
// 模型端点用 MODEL_STUB，其余（签名、落盘、强杀恢复、限流、转换脚本、重放、去重域）全部是真实内核行为。
//
// 只改测试，不改产品代码。
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 验收用能力包放在数据目录（<APA_HOME>/packs），内核启动时加载：Slack 类握手方案、通用共享 token 方案和几份映射。
// GitHub 类方案与映射用发行附带的 inbound-triage 包，不在这里重复。
func accPack(typeField string) string {
	return `name: acc-webhook
version: 1.0.0
description: 验收用 webhook 校验方案与映射（数据目录包，不属于发行）
source_rules:
  - id: acc-hook-created
    match: {source_type: acc-hook, event_type: [acc.created]}
    kind: task
    project_by: none
    then: proposed
webhook_schemes:
  - id: acc-slack
    signature_header: X-Slack-Signature
    signature_prefix: "v0="
    algorithm: hmac-sha256
    signed_payload: "v0:{timestamp}:{body}"
    timestamp_header: X-Slack-Request-Timestamp
    handshake:
      when: {json_path: "$.type", equals: url_verification}
      respond: {json_path: "$.challenge"}
  - id: acc-token
    signature_header: X-Acc-Token
    algorithm: token
webhook_mappings:
  - id: acc-generic
    match: {header: {X-Acc-Event: created}}
    fields:
      id: "{$.id}"
      type: "acc.created"
      subject: "{$.title}"
  - id: acc-fix-map
    match: {header: {X-Acc-Event: created}}
    fields:
      id: "{$.id}"
      type: "` + typeField + `"
      subject: "{$.title}"
`
}

const (
	ghSource    = "src_acc_gh"
	ghPayloadID = "d-1"
)

type fx struct {
	*baseline.Env
	t        *testing.T
	secrets  map[string]string // 来源 id → 当前接收密钥
	ledgerDB string
}

type ingest struct {
	ID          string            `json:"id"`
	DeliveryKey string            `json:"delivery_key"`
	State       string            `json:"state"`
	Signed      bool              `json:"signed"`
	Events      int               `json:"events"`
	Reason      string            `json:"reason"`
	Headers     map[string]string `json:"headers"`
	Body        *string           `json:"body"`
	BodySize    int               `json:"body_size"`
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) addSource(id, sourceType, config, dedupe string) {
	f.t.Helper()
	args := []string{"source", "add", "--id", id, "--type", "webhook", "--source-type", sourceType, "--config", config}
	if dedupe != "" {
		args = append(args, "--dedupe-domain", dedupe)
	}
	f.apa(f.DEL, 0, args...)
}

type keyOut struct {
	Credential json.RawMessage `json:"credential"`
	Secret     string          `json:"secret"`
	URL        string          `json:"url"`
	Rotated    bool            `json:"rotated"`
}

func (f *fx) webhookKey(op, id string) keyOut {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "source", "webhook", op, id)
	var k keyOut
	if err := json.Unmarshal([]byte(r.Stdout), &k); err != nil || k.Secret == "" || k.URL != "/ingest/"+id {
		f.t.Fatalf("webhook %s 输出不对: %q %v", op, r.Stdout, err)
	}
	f.secrets[id] = k.Secret
	return k
}

// credentialID 取 webhook create/rotate 输出里的凭据 id。
func credentialID(t *testing.T, k keyOut) string {
	t.Helper()
	var s string
	if json.Unmarshal(k.Credential, &s) == nil && s != "" {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(k.Credential, &o); err != nil || o.ID == "" {
		t.Fatalf("没有凭据 id: %s", k.Credential)
	}
	return o.ID
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// post 向 /ingest/{id} 发请求，返回状态码与响应体。
func (f *fx) post(id string, body []byte, hdr map[string]string) (int, string) {
	f.t.Helper()
	code, out, err := f.postErr(id, body, hdr)
	if err != nil {
		f.t.Fatalf("POST /ingest/%s: %v", id, err)
	}
	return code, out
}

var httpc = &http.Client{Timeout: 30 * time.Second}

func (f *fx) postErr(id string, body []byte, hdr map[string]string) (int, string, error) {
	req, err := http.NewRequest("POST", f.URL+"/ingest/"+id, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func (f *fx) ghIssue(number int, title string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "opened",
		"issue": map[string]any{"number": number, "title": title, "body": "点击登录后 500",
			"html_url":   strings.TrimSuffix(f.TestRepo, ".git") + "/issues/" + strconv.Itoa(number),
			"updated_at": "2026-11-01T08:00:00Z"},
		"repository": map[string]any{"full_name": "acc/test-repo"},
		"sender":     map[string]any{"login": "alice"},
	})
	return b
}

func (f *fx) ghHeaders(source string, body []byte, delivery string) map[string]string {
	h := map[string]string{"X-Hub-Signature-256": "sha256=" + sign(f.secrets[source], body), "X-GitHub-Event": "issues"}
	if delivery != "" {
		h["X-GitHub-Delivery"] = delivery
	}
	return h
}

func (f *fx) ingests(source string) []ingest {
	f.t.Helper()
	r := f.apa(f.OBS, 0, "source", "ingests", source)
	var out struct {
		Ingests []ingest `json:"ingests"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		f.t.Fatalf("apa source ingests 输出不是 JSON: %v\n%s", err, r.Stdout)
	}
	return out.Ingests
}

func (f *fx) ingestBy(source, delivery string) (ingest, bool) {
	for _, g := range f.ingests(source) {
		if g.DeliveryKey == delivery {
			return g, true
		}
	}
	return ingest{}, false
}

type sourceEvent struct {
	Dedupe struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Type        string `json:"type"`
	Subject     string `json:"subject"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
}

func (f *fx) events(source string) []sourceEvent {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/sources/"+source+"/events?limit=500", "", f.OBS)
	if r.Code != 200 {
		f.t.Fatalf("读来源事件 %s: %d %s", source, r.Code, r.Body)
	}
	var out struct {
		Events []sourceEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

type itemRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Phase string `json:"phase"`
	Kind  string `json:"kind"`
}

func (f *fx) items(phase string) []itemRow {
	f.t.Helper()
	r := f.apa(f.OBS, 0, "item", "list", "--phase", phase)
	var out struct {
		Items []itemRow `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		f.t.Fatalf("apa item list 输出不是 JSON: %v\n%s", err, r.Stdout)
	}
	return out.Items
}

// ledgerEvents 直接读账本事件表（Event 的权威记录），返回某类型事件的 object 与 payload。
func (f *fx) ledgerEvents(typ string) (rows [][2]string) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.ledgerDB+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rs, err := db.Query(`SELECT object, payload FROM eventlog_event WHERE type = ? ORDER BY seq`, typ)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var o, p string
		if err := rs.Scan(&o, &p); err != nil {
			f.t.Fatal(err)
		}
		rows = append(rows, [2]string{o, p})
	}
	return
}

func (f *fx) waitFor(what string, timeout time.Duration, ok func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%s）", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v，期望 %v", what, got, want)
	}
}

func (f *fx) sqlExec(query string, args ...any) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.ledgerDB+"?_pragma=busy_timeout(10000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		f.t.Fatalf("修改账本 %q: %v", query, err)
	}
}

func TestFlowWebhook(t *testing.T) {
	e := baseline.New(t)
	f := &fx{Env: e, t: t, secrets: map[string]string{}, ledgerDB: filepath.Join(e.Home, "ledger.db")}
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		f.t = t
		defer func() { f.t = t }()
		if !t.Run(name, func(t *testing.T) { f.t = t; fn(t) }) {
			t.Fatalf("%s 未通过", name)
		}
	}

	// 环境：先 init，把验收能力包放进数据目录，再 serve（能力包只在内核启动时加载）。
	e.Init()
	packDir := filepath.Join(e.Home, "packs", "acc-webhook")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePack := func(typeField string) {
		if err := os.WriteFile(filepath.Join(packDir, "pack.yaml"), []byte(accPack(typeField)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePack("{$.kinds}") // 故意写错字段名：ACT-HOOK-10 先让映射失败，再修好
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.ModelStub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.95}`})

	step("ACT-HOOK-01 创建接收地址和密钥", func(t *testing.T) {
		f.addSource(ghSource, "github", `{"scheme":"hmac-sha256-header","mappings":["github-issues"]}`, "")
		k := f.webhookKey("create", ghSource)
		if !strings.HasPrefix(k.Secret, "apa_whk_") || k.Rotated {
			t.Fatalf("create 应返回新密钥: %+v", k)
		}
		evs := f.ledgerEvents("source.webhook_key_created")
		if len(evs) != 1 || evs[0][0] != ghSource || strings.Contains(evs[0][1], k.Secret) {
			t.Fatalf("创建密钥应记一条不含明文的 Event: %v", evs)
		}
		// 密钥只显示一次：之后读来源、读事件流、读入站记录都不含明文。
		for _, out := range []string{f.apa(f.OBS, 0, "source", "show", ghSource).Stdout, fmt.Sprint(f.ledgerEvents("source.webhook_key_created"))} {
			if strings.Contains(out, k.Secret) {
				t.Fatal("接收密钥明文可被再次读到")
			}
		}
		// HMAC 方案的明文另存密钥文件（0600，目录 0700），账本里只有哈希。
		kf := filepath.Join(e.Home, "webhook-keys", ghSource)
		if st, err := os.Stat(kf); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("HMAC 密钥文件 %s: %v %v", kf, st, err)
		}
		// OBS 不能创建。
		r := f.Apa(f.OBS, "", "source", "webhook", "create", ghSource)
		eq(t, "OBS 创建接收密钥的退出码", r.Code, 4)
	})

	step("ACT-HOOK-02 签名正确的请求落盘后回 2xx", func(t *testing.T) {
		body := f.ghIssue(7, "登录页 500")
		code, resp := f.post(ghSource, body, f.ghHeaders(ghSource, body, ghPayloadID))
		if code/100 != 2 {
			t.Fatalf("签名正确应回 2xx，实际 %d %s", code, resp)
		}
		// 回 2xx 时原始 payload 已经落盘。
		g, ok := f.ingestBy(ghSource, ghPayloadID)
		if !ok || g.Body == nil || *g.Body != string(body) || !g.Signed {
			t.Fatalf("回 2xx 后入站记录应含原始 payload 与验签结果: %+v", g)
		}
		f.waitFor("入站记录处理完成", 30*time.Second, func() bool { g, _ := f.ingestBy(ghSource, ghPayloadID); return g.State == "processed" })
		g, _ = f.ingestBy(ghSource, ghPayloadID)
		eq(t, "转换出的事件数", g.Events, 1)
		f.waitFor("来源事件进入建议流", 30*time.Second, func() bool {
			evs := f.events(ghSource)
			return len(evs) == 1 && evs[0].Disposition == "proposed" && evs[0].Item != ""
		})
		evs := f.events(ghSource)
		eq(t, "转换出的事件 id", evs[0].Dedupe.EventID, ghPayloadID)
		eq(t, "事件类型", evs[0].Type, "issue.opened")
		var found bool
		for _, it := range f.items("proposed") {
			found = found || (it.ID == evs[0].Item && it.Title == "登录页 500")
		}
		if !found {
			t.Fatalf("proposed 里没有该事项: %+v", f.items("proposed"))
		}
		if len(f.ModelStub.Requests()) == 0 {
			t.Fatal("GitHub 事件应经判断型（MODEL_STUB）进入流水线")
		}
	})

	step("ACT-HOOK-03 签名错误回 401 且不存请求体", func(t *testing.T) {
		evBefore, itBefore := len(f.events(ghSource)), len(f.items("proposed"))
		body := f.ghIssue(7, "登录页 500")
		h := f.ghHeaders(ghSource, body, "d-bad")
		h["X-Hub-Signature-256"] = "sha256=" + strings.Repeat("0", 64)
		code, _ := f.post(ghSource, body, h)
		eq(t, "错误签名的状态码", code, 401)
		var rec *ingest
		for _, g := range f.ingests(ghSource) {
			if g.State == "rejected" {
				g := g
				rec = &g
			}
		}
		if rec == nil {
			t.Fatal("被拒绝的请求应留下只含元数据的入站记录")
		}
		if rec.Body != nil || rec.Signed {
			t.Fatalf("被拒绝的记录不应保存请求体: %+v", rec)
		}
		if _, ok := f.ingestBy(ghSource, "d-bad"); ok {
			t.Fatal("被拒绝的投递不得按投递 id 占位")
		}
		time.Sleep(500 * time.Millisecond)
		eq(t, "来源事件数", len(f.events(ghSource)), evBefore)
		eq(t, "proposed 事项数", len(f.items("proposed")), itBefore)
	})

	step("ACT-HOOK-04 投递 id 幂等", func(t *testing.T) {
		evBefore, itBefore, igBefore := len(f.events(ghSource)), len(f.items("proposed")), len(f.ingests(ghSource))
		body := f.ghIssue(7, "登录页 500")
		code, _ := f.post(ghSource, body, f.ghHeaders(ghSource, body, ghPayloadID))
		if code/100 != 2 {
			t.Fatalf("重发同一投递 id 应回 2xx，实际 %d", code)
		}
		time.Sleep(time.Second)
		eq(t, "重发后来源事件数", len(f.events(ghSource)), evBefore)
		eq(t, "重发后 proposed 事项数", len(f.items("proposed")), itBefore)
		eq(t, "重发后入站记录数", len(f.ingests(ghSource)), igBefore)

		// 不带投递 id、payload 相同的两次请求：按 payload 哈希幂等，合起来只新增一条记录和一个事件。
		body2 := f.ghIssue(8, "没有投递 id")
		for i := 0; i < 2; i++ {
			if code, _ := f.post(ghSource, body2, f.ghHeaders(ghSource, body2, "")); code/100 != 2 {
				t.Fatalf("无投递 id 的请求应回 2xx，实际 %d", code)
			}
		}
		sum := sha256.Sum256(body2)
		key := "sha256:" + hex.EncodeToString(sum[:])
		f.waitFor("无投递 id 的请求处理完成", 30*time.Second, func() bool { g, ok := f.ingestBy(ghSource, key); return ok && g.State == "processed" })
		eq(t, "新增入站记录数", len(f.ingests(ghSource)), igBefore+1)
		f.waitFor("新增一个事件", 30*time.Second, func() bool { return len(f.events(ghSource)) == evBefore+1 })
		time.Sleep(time.Second)
		eq(t, "新增来源事件数", len(f.events(ghSource)), evBefore+1)
	})

	step("ACT-HOOK-05 强杀不丢已确认的事件", func(t *testing.T) {
		const id = "src_acc_kill"
		f.addSource(id, "github", `{"scheme":"hmac-sha256-header","mappings":["github-issues"]}`, "")
		f.webhookKey("create", id)
		var mu sync.Mutex
		var acked []string
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 400; i++ { // 低于每分钟 600 的频率上限
				select {
				case <-stop:
					return
				default:
				}
				did := fmt.Sprintf("k-%03d", i)
				body := f.ghIssue(100+i, "强杀 "+did)
				code, _, err := f.postErr(id, body, f.ghHeaders(id, body, did))
				if err != nil {
					return // 内核被杀，连接断开：这条没有收到 2xx，不计入
				}
				if code/100 == 2 {
					mu.Lock()
					acked = append(acked, did)
					mu.Unlock()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		f.waitFor("已确认至少 40 条", 60*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(acked) >= 40 })
		if err := syscall.Kill(f.ServePID(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		close(stop)
		<-done
		f.StopKernel() // 回收已被杀的进程
		mu.Lock()
		ids := append([]string(nil), acked...)
		mu.Unlock()
		t.Logf("强杀前已回 2xx 的投递：%d 条", len(ids))
		f.StartKernelAgain()
		f.waitFor("已确认的投递都有入站记录且处理完成", 120*time.Second, func() bool {
			byKey := map[string]ingest{}
			for _, g := range f.ingests(id) {
				byKey[g.DeliveryKey] = g
			}
			for _, d := range ids {
				if g, ok := byKey[d]; !ok || g.State != "processed" {
					return false
				}
			}
			return true
		})
		have := map[string]bool{}
		for _, ev := range f.events(id) {
			have[ev.Dedupe.EventID] = true
		}
		for _, d := range ids {
			if !have[d] {
				t.Fatalf("已回 2xx 的投递 %s 没有进入来源事件", d)
			}
		}
	})

	step("ACT-HOOK-06 请求体和频率限制", func(t *testing.T) {
		const id = "src_acc_limit"
		f.addSource(id, "acc-limit", `{"scheme":"acc-token","mappings":["acc-generic"]}`, "") // 无来源规则：不生成事项
		tok := f.webhookKey("create", id).Secret
		hdr := map[string]string{"X-Acc-Token": tok, "X-Acc-Event": "created"}
		big := bytes.Repeat([]byte("x"), 1<<20+1024)
		eq(t, "超限请求体的状态码", func() int { c, _ := f.post(id, big, hdr); return c }(), 413)

		got := map[int]int{}
		for i := 0; i < 640; i++ {
			c, _ := f.post(id, []byte(fmt.Sprintf(`{"id":"rate-%d","title":"限流 %d"}`, i, i)), hdr)
			got[c]++
		}
		if got[429] == 0 {
			t.Fatalf("高频请求应出现 429: %v", got)
		}
		if got[200]+got[202]+got[201]+got[204] == 0 {
			t.Fatalf("限额内的请求应被接收: %v", got)
		}
		// 内核不受影响：限额内已落盘的记录最终都处理完，其它来源与 CLI 照常工作。
		f.waitFor("限额内的记录处理完", 300*time.Second, func() bool {
			n := 0
			for _, g := range f.ingests(id) {
				if g.State == "received" || g.State == "transforming" {
					n++
				}
			}
			return n == 0
		})
		f.apa(f.OBS, 0, "inbox")
		body := f.ghIssue(900, "限流后仍可接收")
		code, _ := f.post(ghSource, body, f.ghHeaders(ghSource, body, "after-limit"))
		if code/100 != 2 {
			t.Fatalf("限流不应影响其他来源，实际 %d", code)
		}
	})

	step("ACT-HOOK-07 平台握手", func(t *testing.T) {
		const id = "src_acc_slack"
		f.addSource(id, "acc-hook", `{"scheme":"acc-slack","mappings":["acc-generic"]}`, "")
		secret := f.webhookKey("create", id).Secret
		body := []byte(`{"type":"url_verification","challenge":"challenge-7f3a"}`)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		h := map[string]string{"X-Slack-Request-Timestamp": ts, "X-Slack-Signature": "v0=" + sign(secret, []byte("v0:"+ts+":"+string(body)))}
		code, resp := f.post(id, body, h)
		eq(t, "握手状态码", code, 200)
		eq(t, "握手应同步返回 challenge", resp, "challenge-7f3a")
		if n := len(f.ingests(id)); n != 0 {
			t.Fatalf("握手不落盘，实际有 %d 条入站记录", n)
		}
		h["X-Slack-Signature"] = "v0=" + strings.Repeat("0", 64)
		code, _ = f.post(id, body, h)
		eq(t, "握手请求签名错误的状态码", code, 401)
	})

	step("ACT-HOOK-08 通用共享 token 方案", func(t *testing.T) {
		const id = "src_acc_token"
		f.addSource(id, "acc-hook", `{"scheme":"acc-token","mappings":["acc-generic"]}`, "")
		tok := f.webhookKey("create", id).Secret
		body := []byte(`{"id":"tok-1","title":"共享 token"}`)
		code, _ := f.post(id, body, map[string]string{"X-Acc-Token": tok, "X-Acc-Event": "created"})
		if code/100 != 2 {
			t.Fatalf("正确 token 应回 2xx，实际 %d", code)
		}
		code, _ = f.post(id, body, map[string]string{"X-Acc-Token": "apa_whk_wrong", "X-Acc-Event": "created"})
		eq(t, "错误 token 的状态码", code, 401)
		// 别的来源的 token 同样不能用（密钥只对自己的 source 有效）。
		code, _ = f.post(id, body, map[string]string{"X-Acc-Token": f.secrets["src_acc_limit"], "X-Acc-Event": "created"})
		eq(t, "其他来源 token 的状态码", code, 401)
	})

	step("ACT-HOOK-09 转换脚本处理特殊格式", func(t *testing.T) {
		const id = "src_acc_script"
		script := filepath.Join(e.Home, "sources", "acc-transform.sh")
		mode := filepath.Join(e.Home, "sources", "acc-transform.mode")
		// stdin 是 JSON（含 payload），stdout 是标准事件 JSONL；mode=fail 时退出码 1（临时错误）。
		src := "#!/bin/sh\nin=$(cat)\nif [ \"$(cat " + mode + ")\" = fail ]; then exit 1; fi\n" +
			"title=$(printf '%s' \"$in\" | sed -n 's/.*\"title\":\"\\([^\"]*\\)\".*/\\1/p' | head -n1)\n" +
			"printf '{\"id\":\"s-%s\",\"type\":\"acc.created\",\"subject\":\"%s\",\"occurred_at\":\"2026-11-01T08:00:00Z\"}\\n' \"$$\" \"$title\"\n"
		if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mode, []byte("fail"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.addSource(id, "acc-hook", `{"scheme":"acc-token"}`, "")
		tok := f.webhookKey("create", id).Secret
		// 与 Web 来源页保存转换脚本相同的 HTTP 调用（会话 + CSRF）。ego-browser 不在本主机，见验证说明。
		cfg, _ := json.Marshal(map[string]any{"config": map[string]any{"scheme": "acc-token", "transform": map[string]any{"command": script, "timeout": 20}}})
		if r := f.Web("PATCH", "/v1/sources/"+id, string(cfg)); r.Code != 200 {
			t.Fatalf("Web 登记转换脚本: %d %s", r.Code, r.Body)
		}
		hdr := map[string]string{"X-Acc-Token": tok}

		// 脚本退出码 1：临时错误，原始 payload 不丢，记录保持待处理、原因留在记录上，不算 failed。
		fail := []byte(`{"title":"脚本失败时的 payload"}`)
		if code, _ := f.post(id, fail, hdr); code/100 != 2 {
			t.Fatalf("脚本失败也应先落盘回 2xx，实际 %d", code)
		}
		sum := sha256.Sum256(fail)
		key := "sha256:" + hex.EncodeToString(sum[:])
		defer func() {
			if t.Failed() {
				b, _ := json.Marshal(f.ingests(id))
				t.Logf("入站记录: %s", b)
				db, _ := sql.Open("sqlite", "file:"+f.ledgerDB+"?mode=ro")
				rs, err := db.Query(`SELECT * FROM outcall_run`)
				if err == nil {
					cols, _ := rs.Columns()
					for rs.Next() {
						v := make([]any, len(cols))
						p := make([]any, len(cols))
						for i := range v {
							p[i] = &v[i]
						}
						rs.Scan(p...)
						t.Logf("outcall_run %v %s", cols, fmt.Sprint(v...))
					}
				}
			}
		}()
		f.waitFor("脚本被拉起并记录原因", 30*time.Second, func() bool {
			g, ok := f.ingestBy(id, key)
			return ok && g.State == "received" && strings.Contains(g.Reason, "transform failed")
		})
		g, _ := f.ingestBy(id, key)
		if g.Body == nil || *g.Body != string(fail) {
			t.Fatalf("脚本失败后原始 payload 必须保留: %+v", g)
		}
		eq(t, "失败时的事件数", len(f.events(id)), 0)

		// 脚本恢复后，同一条记录按拉取型语义重试成功（等下一次后台补做）。
		if err := os.WriteFile(mode, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.waitFor("后台重试后按转换结果入流水线", 150*time.Second, func() bool {
			g, _ := f.ingestBy(id, key)
			return g.State == "processed"
		})
		f.waitFor("转换出的事件入建议流", 30*time.Second, func() bool {
			evs := f.events(id)
			return len(evs) == 1 && evs[0].Subject == "脚本失败时的 payload" && evs[0].Disposition == "proposed"
		})

		// 另一条正常的请求：第一次按转换结果入流水线。
		ok := []byte(`{"title":"脚本转换成功"}`)
		if code, _ := f.post(id, ok, hdr); code/100 != 2 {
			t.Fatalf("回 %d", code)
		}
		f.waitFor("第二条按转换结果入流水线", 30*time.Second, func() bool { return len(f.events(id)) == 2 })

		// 脚本必须在 sources/ 之下：登记到别处的绝对路径不执行，记录记 failed。
		outside := filepath.Join(e.Root, "evil.sh")
		_ = os.WriteFile(outside, []byte("#!/bin/sh\necho pwned > "+filepath.Join(e.Root, "pwned")+"\n"), 0o755)
		cfg2, _ := json.Marshal(map[string]any{"config": map[string]any{"scheme": "acc-token", "transform": map[string]any{"command": outside}}})
		if r := f.Web("PATCH", "/v1/sources/"+id, string(cfg2)); r.Code != 200 {
			t.Fatalf("登记越界脚本: %d %s", r.Code, r.Body)
		}
		bad := []byte(`{"title":"越界脚本"}`)
		f.post(id, bad, hdr)
		bs := sha256.Sum256(bad)
		f.waitFor("越界脚本被拒绝", 30*time.Second, func() bool {
			g, ok := f.ingestBy(id, "sha256:"+hex.EncodeToString(bs[:]))
			return ok && g.State == "failed" && strings.Contains(g.Reason, "host script directory")
		})
		if _, err := os.Stat(filepath.Join(e.Root, "pwned")); err == nil {
			t.Fatal("越界脚本被执行了")
		}
	})

	step("ACT-HOOK-10 修好映射后重放不重复", func(t *testing.T) {
		const id = "src_acc_fix"
		f.addSource(id, "acc-hook", `{"scheme":"acc-token","mappings":["acc-fix-map"]}`, "")
		tok := f.webhookKey("create", id).Secret
		hdr := map[string]string{"X-Acc-Token": tok, "X-Acc-Event": "created"}
		if code, _ := f.post(id, []byte(`{"id":"fix-1","kind":"acc.created","title":"映射修好后的事项"}`), hdr); code/100 != 2 {
			t.Fatalf("回 %d", code)
		}
		var rec ingest
		f.waitFor("转换失败的记录", 30*time.Second, func() bool {
			gs := f.ingests(id)
			if len(gs) == 1 && gs[0].State == "failed" {
				rec = gs[0]
				return true
			}
			return false
		})
		if rec.Body == nil || !strings.Contains(*rec.Body, "fix-1") || !strings.Contains(rec.Reason, "type") {
			t.Fatalf("failed 记录应保留原始 payload 并说明缺哪个字段: %+v", rec)
		}
		eq(t, "失败时的来源事件数", len(f.events(id)), 0)

		// 修好映射：能力包只在启动时加载，改数据目录里的包后重启内核。
		writePack("acc.created")
		f.StopKernel()
		f.StartKernelAgain()
		before := len(f.items("proposed"))
		f.apa(f.DEL, 0, "source", "replay", rec.ID)
		f.waitFor("重放后生成事项", 30*time.Second, func() bool {
			evs := f.events(id)
			return len(evs) == 1 && evs[0].Disposition == "proposed" && evs[0].Item != ""
		})
		eq(t, "第一次重放后新增事项数", len(f.items("proposed")), before+1)
		first := f.events(id)[0].Item

		f.apa(f.DEL, 0, "source", "replay", rec.ID)
		f.waitFor("第二次重放处理完成", 30*time.Second, func() bool {
			g, _ := f.ingestBy(id, rec.DeliveryKey)
			return g.State == "processed" && g.Events == 1
		})
		time.Sleep(time.Second)
		eq(t, "第二次重放后来源事件数", len(f.events(id)), 1)
		eq(t, "第二次重放后新增事项数", len(f.items("proposed")), before+1)
		eq(t, "事项仍是同一个", f.events(id)[0].Item, first)
	})

	step("ACT-HOOK-11 轮换密钥，新旧短期并存", func(t *testing.T) {
		const id = "src_acc_rot"
		f.addSource(id, "github", `{"scheme":"hmac-sha256-header","mappings":["github-issues"]}`, "")
		oldK := f.webhookKey("create", id)
		oldSecret := oldK.Secret
		newK := f.webhookKey("rotate", id)
		if !newK.Rotated || newK.Secret == oldSecret {
			t.Fatalf("rotate 应返回新密钥: %+v", newK)
		}
		for i, secret := range []string{newK.Secret, oldSecret} {
			body := f.ghIssue(300+i, fmt.Sprintf("并存期 %d", i))
			h := map[string]string{"X-Hub-Signature-256": "sha256=" + sign(secret, body), "X-GitHub-Event": "issues", "X-GitHub-Delivery": fmt.Sprintf("rot-%d", i)}
			if code, _ := f.post(id, body, h); code/100 != 2 {
				t.Fatalf("并存期内第 %d 个密钥应回 2xx，实际 %d", i, code)
			}
		}
		// 宽限期是一小时：把旧凭据的失效时间拨到过去，等价于等到期满（内核每次验签都读账本）。
		f.sqlExec(`UPDATE auth_credential SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UnixNano(), credentialID(t, oldK))
		body := f.ghIssue(310, "期满后")
		old := map[string]string{"X-Hub-Signature-256": "sha256=" + sign(oldSecret, body), "X-GitHub-Event": "issues", "X-GitHub-Delivery": "rot-old"}
		code, _ := f.post(id, body, old)
		eq(t, "期满后旧密钥的状态码", code, 401)
		neu := map[string]string{"X-Hub-Signature-256": "sha256=" + sign(newK.Secret, body), "X-GitHub-Event": "issues", "X-GitHub-Delivery": "rot-new"}
		if code, _ := f.post(id, body, neu); code/100 != 2 {
			t.Fatalf("期满后新密钥应继续有效，实际 %d", code)
		}
	})

	step("ACT-HOOK-12 接收密钥只能写事件", func(t *testing.T) {
		tok := f.secrets[ghSource]
		for _, c := range []struct{ method, body string }{{"GET", ""}, {"POST", `{"kind":"task","title":"越权","phase":"open"}`}} {
			r := f.APIRaw(c.method, "/v1/items", c.body, tok)
			if r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
				t.Fatalf("%s /v1/items 用接收密钥应被拒绝，实际 %d %s", c.method, r.Code, r.Body)
			}
		}
		if r := f.APIRaw("GET", "/v1/sources/"+ghSource+"/ingests", "", tok); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Fatalf("接收密钥不能读入站记录，实际 %d", r.Code)
		}
		for _, it := range f.items("open") {
			if it.Title == "越权" {
				t.Fatal("接收密钥写进了账本")
			}
		}
	})

	step("ACT-HOOK-13 推拉结合不重复", func(t *testing.T) {
		const (
			push, pull = "src_acc_push", "src_acc_pull"
			domainName = "accdom"
		)
		f.addSource(push, "acc-hook", `{"scheme":"acc-token","mappings":["acc-generic"]}`, domainName)
		tok := f.webhookKey("create", push).Secret
		// 拉取 source：脚本登记只在 Web，输出同一个事件 id。
		script := filepath.Join(e.Home, "sources", "acc-pull.sh")
		src := "#!/bin/sh\nin=$(cat)\ncase \"$in\" in *'\"cursor\":\"done\"'*) printf '{\"cursor\":\"done\"}\\n'; exit 0;; esac\n" +
			"printf '{\"id\":\"shared-1\",\"type\":\"acc.created\",\"subject\":\"同一事件\",\"occurred_at\":\"2026-11-01T08:00:00Z\"}\\n{\"cursor\":\"done\"}\\n'\n"
		if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
		reg, _ := json.Marshal(map[string]any{"id": pull, "type": "script", "source_type": "acc-hook", "command": script, "schedule": "1s", "timeout": 10, "dedupe_domain": domainName})
		if r := f.Web("POST", "/v1/sources", string(reg)); r.Code != 201 {
			t.Fatalf("Web 登记拉取脚本来源: %d %s", r.Code, r.Body)
		}
		f.waitFor("拉取来源产生该事件并生成事项", 60*time.Second, func() bool {
			evs := f.events(pull)
			return len(evs) == 1 && evs[0].Item != ""
		})
		before := len(f.items("proposed"))
		if code, _ := f.post(push, []byte(`{"id":"shared-1","title":"同一事件"}`), map[string]string{"X-Acc-Token": tok, "X-Acc-Event": "created"}); code/100 != 2 {
			t.Fatalf("回 %d", code)
		}
		g := func() ingest { gs := f.ingests(push); return gs[0] }
		f.waitFor("推送记录处理完成", 30*time.Second, func() bool { gs := f.ingests(push); return len(gs) == 1 && g().State == "processed" })
		time.Sleep(2 * time.Second)
		eq(t, "推送后新增事项数", len(f.items("proposed")), before)
		n := 0
		for _, it := range f.items("proposed") {
			if it.Title == "同一事件" {
				n++
			}
		}
		eq(t, "同名事项数", n, 1)
		eq(t, "去重域内事件总数", len(f.events(pull))+len(f.events(push)), 1)
	})
}
