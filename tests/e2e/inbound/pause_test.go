//go:build e2e

package inbound

import (
	"encoding/json"
	"testing"
)

// signal 是 `apa health` 里某一项信号。
func (k *kit) signal(id string) (ok bool, detail map[string]any) {
	k.t.Helper()
	var h struct {
		Signals []struct {
			ID     string          `json:"id"`
			OK     bool            `json:"ok"`
			Detail json.RawMessage `json:"detail"`
		} `json:"signals"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "health").Stdout), &h))
	for _, s := range h.Signals {
		if s.ID == id {
			k.must(json.Unmarshal(s.Detail, &detail))
			return s.OK, detail
		}
	}
	k.t.Fatalf("健康视图没有信号 %s", id)
	return false, nil
}

// 暂停模型（设计 §8.7）：配好两个端点 → 暂停 → 新事件零模型请求、记 models_paused，健康视图是"已暂停"而不是故障，
// 端点与密钥仍在设置里 → 恢复 → 新事件立即被判断；暂停期间的积压与其他积压一样按重判窗口自动重判。
func TestFlowModelsPause(t *testing.T) {
	k := newKit(t)
	k.registerInp()
	k.setEndpoints(true, true)
	pause := func(on bool) {
		b, _ := json.Marshal(map[string]any{"models_paused": on})
		k.webOK("PUT", "/v1/settings/model-endpoints", string(b), 200)
	}

	k.step("暂停后来新事件：不调用任何模型，未判定原因为 models_paused", func(t *testing.T) {
		pause(true)
		var got map[string]json.RawMessage
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/model-endpoints", "", 200)), &got))
		if string(got["models_paused"]) != "true" || string(got["judge_endpoint"]) == "null" || string(got["generate_endpoint"]) == "null" {
			t.Fatalf("暂停时设置里应保留两个端点: %s", got)
		}
		k.clear()
		k.emit("pz-1", "pz-paused 暂停期间的事件")
		ev := k.waitEvent(inpSrc, "pz-1", wait)
		eq(t, "disposition", ev.Disposition, "unjudged")
		eq(t, "unjudged_reason", ev.Unjudged, "models_paused")
		quiet()
		eq(t, "CLS_LOG 新请求", len(k.Cls.Batches())-k.clsOff, 0)
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
		ok, d := k.signal("model_unavailable")
		if !ok || d["paused"] != true || d["generate"] != "paused" {
			t.Errorf("模型信号应为正常的已暂停: ok=%v %v", ok, d)
		}
		if _, b := k.signal("unjudged_backlog"); b["models_paused"] != float64(1) {
			t.Errorf("积压应单列 models_paused: %v", b)
		}
	})

	k.step("恢复后新事件立即按原配置判断", func(t *testing.T) {
		k.screenCls("pz-", 0.90)
		k.draft("pz-resumed", map[string]any{"title": "pz 恢复后起草", "kind": "task"})
		k.draft("pz-paused", map[string]any{"title": "pz 暂停积压重判", "kind": "task"})
		pause(false)
		k.clear()
		k.emit("pz-2", "pz-resumed 恢复后的事件")
		ev := k.waitEvent(inpSrc, "pz-2", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		eq(t, "title", k.item(ev.Item).Title, "pz 恢复后起草")
		if len(k.Cls.Batches())-k.clsOff == 0 || len(k.ModelStub.Requests())-k.modelOff == 0 {
			t.Errorf("恢复后应调用两个端点: cls %d model %d", len(k.Cls.Batches())-k.clsOff, len(k.ModelStub.Requests())-k.modelOff)
		}
		if ok, d := k.signal("model_unavailable"); !ok || d["paused"] == true {
			t.Errorf("恢复后模型信号不再是已暂停: ok=%v %v", ok, d)
		}
	})

	k.step("暂停期间的积压与其他积压一样在重判窗口内自动重判", func(t *testing.T) {
		k.TCAdvance(11 * 60)
		if !k.WaitFor(wait, func() bool { return k.byEventID(inpSrc, "pz-1")[0].Disposition == "proposed" }) {
			t.Fatalf("pz-1 一直没有重判: %+v\n%s", k.byEventID(inpSrc, "pz-1")[0], k.ServiceLog())
		}
		eq(t, "title", k.item(k.byEventID(inpSrc, "pz-1")[0].Item).Title, "pz 暂停积压重判")
	})
}
