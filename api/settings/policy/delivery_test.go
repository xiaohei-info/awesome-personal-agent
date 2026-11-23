package policy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// getDisturb 读 disturb 段原样（map），便于照 GET 的形状改了再 PUT。
func (e *env) getDisturb(t *testing.T) map[string]any {
	t.Helper()
	rec := e.do("GET", "/v1/settings/policy", "", e.web)
	if rec.Code != 200 {
		t.Fatalf("GET %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Disturb map[string]any `json:"disturb"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Disturb
}

func (e *env) putDisturb(t *testing.T, d map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"disturb": d})
	rec := e.do("PUT", "/v1/settings/policy", string(b), e.web)
	return rec.Code, rec.Body.String()
}

// 没保存过：GET 给换算出的矩阵与 delivery_saved=false；照 GET 改旧键再整段 PUT 照旧可用，不写入矩阵、不冻结旧键。
func TestDisturbGetDerivesMatrixAndLegacyRoundTripStillWorks(t *testing.T) {
	e := setup(t)
	d := e.getDisturb(t)
	if d["delivery_saved"] != false {
		t.Fatalf("delivery_saved = %v", d["delivery_saved"])
	}
	raw, _ := json.Marshal(d["delivery"])
	var got kpolicy.Delivery
	if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, kpolicy.FactoryDelivery()) {
		t.Fatalf("derived = %+v (%v)", got, err)
	}
	d["routes"].(map[string]any)["decision"] = "channel:c1"
	if code, body := e.putDisturb(t, d); code != 200 {
		t.Fatalf("legacy round trip: %d %s", code, body)
	}
	if _, p := e.current(); p.Disturb.Delivery != nil || p.Disturb.Routes["decision"] != "channel:c1" {
		t.Fatalf("after legacy PUT: delivery %+v routes %v", p.Disturb.Delivery, p.Disturb.Routes)
	}
	if d := e.getDisturb(t); d["delivery"].(map[string]any)["default_channel"] != "channel:c1" {
		t.Fatalf("derived matrix must follow the new legacy keys: %v", d["delivery"])
	}
}

// 改了矩阵即写入；之后旧键冻结（改了 400，什么都没存）；不带 delivery 的 PUT 沿用已存的矩阵。
func TestSavingTheMatrixFreezesLegacyKeys(t *testing.T) {
	e := setup(t)
	d := e.getDisturb(t)
	rows := d["delivery"].(map[string]any)["rows"].(map[string]any)
	rows["stale"] = map[string]any{"agent": "off", "user": "channel:c1"}
	if code, body := e.putDisturb(t, d); code != 200 {
		t.Fatalf("save matrix: %d %s", code, body)
	}
	v, p := e.current()
	if p.Disturb.Delivery == nil || !reflect.DeepEqual(p.Disturb.Delivery.Rows["stale"], kpolicy.Target{Agent: "off", User: "channel:c1"}) {
		t.Fatalf("saved = %+v", p.Disturb.Delivery)
	}
	if d := e.getDisturb(t); d["delivery_saved"] != true {
		t.Fatalf("delivery_saved after save = %v", d["delivery_saved"])
	}
	for name, mut := range map[string]func(map[string]any){
		"routes":    func(d map[string]any) { d["routes"].(map[string]any)["brief"] = "web" },
		"push":      func(d map[string]any) { d["push"] = map[string]any{"run_failed": map[string]any{"on": true, "route": "channel:x"}} },
		"task_push": func(d map[string]any) { d["proactive"] = map[string]any{"work_start": "09:00", "work_end": "18:00", "workdays": []int{1}, "task_push": map[string]bool{"brief": false}} },
	} {
		d := e.getDisturb(t)
		mut(d)
		code, body := e.putDisturb(t, d)
		if code != 400 {
			t.Fatalf("%s: frozen key change got %d %s", name, code, body)
		}
		var er struct{ Code domain.Code }
		_ = json.Unmarshal([]byte(body), &er)
		if er.Code != domain.CodeInvalidRequest || !strings.Contains(body, "frozen") {
			t.Fatalf("%s: %s", name, body)
		}
		if nv, _ := e.current(); nv != v {
			t.Fatalf("%s: rejected PUT wrote version %d", name, nv)
		}
	}
	d = e.getDisturb(t)
	delete(d, "delivery")
	d["merge_window_sec"] = 60
	if code, body := e.putDisturb(t, d); code != 200 {
		t.Fatalf("PUT without delivery: %d %s", code, body)
	}
	if _, p := e.current(); p.Disturb.Delivery == nil || p.Disturb.MergeWindowSec != 60 {
		t.Fatalf("matrix must survive a PUT without delivery: %+v", p.Disturb)
	}
}
