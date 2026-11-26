//go:build e2e

package rawlifecycle

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestFlowAttachmentFetch 是验收 flow-attachment-fetch（ACT-FET-01…04）：来源程序声明支持取回时本人可按需取回一个附件，
// 取回后哈希一致、不产生来源事件、游标不变；不支持是 409；失败与超限是 502 且清单不变；delegate 须附原话，运行凭据与 observer 不可。
// ACT-FET-02 的页面部分（没有"取回"按钮）由 web/src/features/trace 的组件测试覆盖，这里断言页面据以决定的接口事实：
// 不支持取回的来源，读接口不给清单项的 ref。
func TestFlowAttachmentFetch(t *testing.T) {
	k := setup(t)
	k.EnableStubRuns() // ACT-FET-04 要一个运行凭据
	var ev listed
	var mail map[string]any
	var before srcShow
	var beforeIDs []string

	ids := func(src string) (out []string) {
		for _, e := range k.events(src) {
			out = append(out, e.ID)
		}
		return
	}

	k.step("准备 acc-raw（声明支持取回）与 acc-raw-nofetch（没有声明）", func(t *testing.T) {
		mail = k.stubOut("mail-like")[0]
		k.addStub("src_acc_raw", "mail", "full", "full")
		k.addProgram("src_acc_raw_nofetch", "src-stub-nofetch", "mail", "full", "full")
		ev = k.eventByStubID("src_acc_raw", "mail-like-1")
	})

	k.step("ACT-FET-01 取回第 0 个附件：哈希一致，记审计事件，没有新来源事件，游标不变", func(t *testing.T) {
		before, beforeIDs = k.show("src_acc_raw"), ids("src_acc_raw")
		for _, a := range k.attachments(ev.ID) {
			if a.Blob != "" {
				t.Fatalf("取回前清单不应带 blob: %+v", a)
			}
		}
		code, body := k.fetch(ev.ID, 0)
		if code != 200 {
			t.Fatalf("取回: HTTP %d %s", code, body)
		}
		var item manifestItem
		if err := json.Unmarshal([]byte(body), &item); err != nil || item.Blob == "" {
			t.Fatalf("响应应是带 blob 的清单项: %s", body)
		}
		want := k.stubBytes(attachRef(mail, 0))
		got, err := os.ReadFile(k.blobFile(item.Blob))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || sha(got) != sha(want) {
			t.Errorf("blob 内容（%d 字节）与 SRC_STUB 的固定字节（%d 字节）不同", len(got), len(want))
		}
		if m := k.attachments(ev.ID); m[0].Blob != item.Blob || m[1].Blob != "" {
			t.Errorf("清单只有第 0 项带 blob: %+v", m)
		}
		if a := k.audit("source.attachment_fetched", ev.ID); len(a) != 1 {
			t.Errorf("应记一条 source.attachment_fetched 审计事件，实际 %d", len(a))
		}
		k.settle("src_acc_raw", 2) // 取回之后来源又拉了两轮：仍没有新事件、游标不动
		after := k.show("src_acc_raw")
		eq(t, "游标", after.Cursor, before.Cursor)
		eq(t, "来源事件数", after.Stats.Events, before.Stats.Events)
		if now := ids("src_acc_raw"); strings.Join(now, ",") != strings.Join(beforeIDs, ",") {
			t.Errorf("来源事件列表变了: %v → %v", beforeIDs, now)
		}
	})

	k.step("ACT-FET-02 不支持取回的来源返回 409，清单不给取回句柄", func(t *testing.T) {
		ne := k.eventByStubID("src_acc_raw_nofetch", "mail-like-1")
		if code, body := k.fetch(ne.ID, 0); code != 409 {
			t.Errorf("不支持取回应为 409: HTTP %d %s", code, body)
		}
		// 页面据此只显示名字、类型与大小：清单项没有 ref（页面组件测试以此隐藏"取回"）。
		for _, a := range k.attachments(ne.ID) {
			if a.Ref != "" || a.Blob != "" || a.Name == "" || a.Size == 0 {
				t.Errorf("不支持取回的来源，清单项只应有元数据: %+v", a)
			}
		}
		for _, a := range k.attachments(ev.ID) {
			if a.Ref == "" {
				t.Errorf("支持取回的来源，清单项应带取回句柄: %+v", a)
			}
		}
	})

	k.step("ACT-FET-03 取回失败与超限返回 502，清单不变，不产生来源事件", func(t *testing.T) {
		for _, c := range []struct{ mode, what string }{{"fail:1", "退出码 1"}, {"big", "输出 33 MiB"}} {
			k.setFetch("src_acc_raw", c.mode)
			manifest := string(k.readEvent("", ev.ID, "", 200).Attachments)
			code, body := k.fetch(ev.ID, 1)
			if code != 502 || !strings.Contains(body, `"why"`) || strings.Contains(body, "blob") {
				t.Errorf("%s: 应为 502 并带原因: HTTP %d %s", c.what, code, body)
			}
			eq(t, c.what+" 之后的清单", string(k.readEvent("", ev.ID, "", 200).Attachments), manifest)
			if m := k.attachments(ev.ID); m[1].Blob != "" {
				t.Errorf("%s: 清单该项不应有 blob: %+v", c.what, m[1])
			}
		}
		k.settle("src_acc_raw", 2)
		eq(t, "来源事件数", k.show("src_acc_raw").Stats.Events, before.Stats.Events)
		eq(t, "来源事件", strings.Join(ids("src_acc_raw"), ","), strings.Join(beforeIDs, ","))
		if a := k.audit("source.attachment_fetched", ev.ID); len(a) != 1 {
			t.Errorf("失败的取回不应再记审计事件，实际共 %d 条", len(a))
		}
		k.setFetch("src_acc_raw", "ok")
	})

	// 2026-11-26 改写（权限卡）：取回附件属"处理来源事件"，delegate 与运行凭据（与所属 Agent 同权）可取回；自报的原话与私聊头被忽略；observer 在范围外。
	k.step("ACT-FET-04 delegate 可取回，observer 不可", func(t *testing.T) {
		path := "/v1/source-events/" + ev.ID + "/attachments/1/fetch"
		direct := map[string]string{"X-APA-Evidence": "本人：把第二个附件取回来", "X-APA-Session": "fet-session", "X-APA-Session-Kind": "direct"}
		for name, tok := range map[string]string{"OBS": k.OBS} {
			if r := k.APIRawHeaders("POST", path, "", tok, direct); r.Code != 403 {
				t.Errorf("%s 应为 403: %d %s", name, r.Code, r.Body)
			}
		}
		if m := k.attachments(ev.ID); m[1].Blob != "" {
			t.Fatalf("被拒的调用不应取回: %+v", m[1])
		}
		r := k.APIRaw("POST", path, "", k.DELO)
		if r.Code != 200 {
			t.Fatalf("只整理的 delegate 不带原话应成功: %d %s", r.Code, r.Body)
		}
		if m := k.attachments(ev.ID); m[1].Blob == "" || !exists(k.blobFile(m[1].Blob)) {
			t.Errorf("取回后清单第 1 项应带 blob 且文件在: %+v", m[1])
		}
	})
}
