//go:build e2e

package rawingest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 验收用能力包（数据目录包，不属于发行）：通用共享 token 方案和一份只用字段映射、不写 detail 的映射。
const hookPack = `name: acc-rawhook
version: 1.0.0
description: 验收用 webhook 校验方案与映射（数据目录包，不属于发行）
webhook_schemes:
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
`

type ingestRow struct {
	ID          string  `json:"id"`
	DeliveryKey string  `json:"delivery_key"`
	State       string  `json:"state"`
	Events      int     `json:"events"`
	Body        *string `json:"body"`
	BodySize    int     `json:"body_size"`
}

func (k *kit) ingests(src string) []ingestRow {
	k.t.Helper()
	var out struct {
		Ingests []ingestRow `json:"ingests"`
	}
	r := k.apa(k.OBS, 0, "source", "ingests", src)
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatalf("apa source ingests 输出不是 JSON: %v\n%.300s", err, r.Stdout)
	}
	return out.Ingests
}

func (k *kit) post(src string, body []byte, hdr map[string]string) (int, string) {
	k.t.Helper()
	req, err := http.NewRequest("POST", k.URL+"/ingest/"+src, bytes.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for h, v := range hdr {
		req.Header.Set(h, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		k.t.Fatalf("POST /ingest/%s: %v", src, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestFlowRawWebhook 是验收 flow-raw-webhook（ACT-WHK-01、02）：只用字段映射的 webhook 来源把请求体作为原文 original 永久保存，
// 不随接收记录的 30 天保留期清除；请求体上限缺省 1 MiB 且可配。
func TestFlowRawWebhook(t *testing.T) {
	e := baseline.New(t)
	k := newKit(t, e)
	e.Init()
	dir := filepath.Join(e.Home, "packs", "acc-rawhook")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(hookPack), 0o644); err != nil { // 能力包只在内核启动时加载
		t.Fatal(err)
	}
	e.Serve()
	e.IssueCredentials()

	const src = "src_acc_hook"
	var secret string
	hdr := func() map[string]string { return map[string]string{"X-Acc-Token": secret, "X-Acc-Event": "created"} }

	k.step("登记只用字段映射（映射不写 detail）的 webhook 来源并取接收密钥", func(t *testing.T) {
		k.OwnerApaOK("", "source", "add", "--id", src, "--name", src, "--type", "webhook", "--source-type", "acc-hook",
			"--config", `{"scheme":"acc-token","mappings":["acc-generic"]}`)
		var out struct {
			Secret string `json:"secret"`
		}
		r := k.OwnerApaOK("", "source", "webhook", "create", src)
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Secret == "" {
			t.Fatalf("webhook create 没有返回密钥: %q", r.Stdout)
		}
		secret = out.Secret
	})

	var eventID string
	payload := []byte(`{"id":"hook-1","title":"字段映射的事件","note":"请求体逐字节保留 \u00e9  \t 缩进","n":[1,2,3]}`)

	k.step("ACT-WHK-01 字段映射 webhook 的请求体进原文 original，不随 Ingest 清除", func(t *testing.T) {
		code, body := k.post(src, payload, hdr())
		if code/100 != 2 {
			t.Fatalf("POST /ingest/%s: %d %s", src, code, body)
		}
		k.until("请求体转换出事件", func() bool {
			for _, ev := range k.events(src) {
				if ev.DedupeKey.EventID == "hook-1" {
					eventID = ev.ID
					return true
				}
			}
			return false
		})
		check := func(when string) {
			t.Helper()
			got := k.readEvent("", eventID, "original", 200)
			if got.Original == nil || got.Original.Text != string(payload) {
				t.Fatalf("%s：original.text 与请求体不是逐字节相同: %+v", when, got.Original)
			}
			eq(t, when+" original.media_type", got.Original.MediaType, "application/json")
			whole := k.readEvent("", eventID, "", 200)
			if b, h, ok := whole.Content.part("original"); !ok || b != len(payload) || h != sha(payload) {
				t.Errorf("%s：content.parts 的 original = (%d, %s, %v)", when, b, h, ok)
			}
			if whole.Body != nil && *whole.Body != "" {
				t.Errorf("%s：映射没写 detail，事件不应带正文: %.80s", when, *whole.Body)
			}
		}
		check("接收后")

		if len(k.ingests(src)) == 0 || k.ingests(src)[0].Body == nil {
			t.Fatal("接收后 Ingest 应保留请求体")
		}
		k.TCAdvance(31 * 86400)
		k.WebLogin()                                 // 测试时钟推进后原会话已过期
		if !k.WaitFor(100*time.Second, func() bool { // 内核每分钟清理一次
			for _, g := range k.ingests(src) {
				if g.Body != nil {
					return false
				}
			}
			return true
		}) {
			t.Fatal("100s 内 Ingest 请求体没有按 30 天被清掉（只留元数据）")
		}
		k.until("清理完成后元数据仍在", func() bool {
			for _, g := range k.ingests(src) {
				if g.Body != nil {
					return false
				}
			}
			return len(k.ingests(src)) > 0
		})
		if g := k.ingests(src)[0]; g.BodySize != len(payload) || g.Events != 1 {
			t.Errorf("清理后应留元数据: %+v", g)
		}
		check("31 天后")
	})

	k.step("ACT-WHK-02 max_webhook_body_mb 缺省 1 且可改", func(t *testing.T) {
		big, _ := json.Marshal(map[string]string{"id": "hook-big", "title": "大请求体", "pad": strings.Repeat("x", 1258291)}) // 约 1.2 MiB
		if len(big) < 1<<20+100<<10 {
			t.Fatalf("测试请求体只有 %d 字节", len(big))
		}
		events, ingests := len(k.events(src)), len(k.ingests(src))
		code, _ := k.post(src, big, hdr())
		eq(t, "缺省上限下 1.2 MiB 的状态码", code, 413)
		time.Sleep(1500 * time.Millisecond) // 异步入队的转换有时间发生之后再断言"没有新行"
		eq(t, "来源事件数（不落盘）", len(k.events(src)), events)
		eq(t, "Ingest 数（不落盘）", len(k.ingests(src)), ingests)

		k.setConfig("max_webhook_body_mb: 2\n")
		k.WebLogin()
		code, resp := k.post(src, big, hdr())
		if code/100 != 2 {
			t.Fatalf("上限 2 MiB 时同一请求应被接收: %d %s", code, resp)
		}
		k.until("同一请求被转换", func() bool {
			for _, ev := range k.events(src) {
				if ev.DedupeKey.EventID == "hook-big" {
					got := k.readEvent("", ev.ID, "original", 200)
					return got.Original != nil && got.Original.Text == string(big)
				}
			}
			return false
		})

		k.setConfig("") // 删掉该配置项：缺省值恢复
		k.WebLogin()
		big2, _ := json.Marshal(map[string]string{"id": "hook-big-2", "title": "大请求体", "pad": strings.Repeat("y", 1258291)})
		code, _ = k.post(src, big2, hdr())
		eq(t, "删掉配置项后的状态码", code, 413)
	})
}
