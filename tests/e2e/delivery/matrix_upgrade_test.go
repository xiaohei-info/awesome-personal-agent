//go:build e2e

package delivery

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 升级到投递矩阵（主文档 §7.4、§16.5）：升级前的策略只有旧路由、有一条在途的决策投递；新二进制启动时一次性采用建议值，
// 在途的投递不重复、不补出第二层；旧路由原样冻结（旧二进制忽略 delivery 读到的就是它们），改旧路由被拒。
func TestUpgradeToDeliveryMatrixKeepsInFlightAndFreezesLegacyKeys(t *testing.T) {
	f := setup(t)
	before := route(map[string]string{"decision": "channel:acc-chan", "reminder": "channel:acc-chan"})
	f.setDisturb(disturb{Routes: before, RemindOnDeadline: true})
	id := f.task("升级前的问题")
	f.ask(id)
	base := f.waitBacklog("decision:" + id)

	f.StopKernel()
	f.StartKernelAgain()

	var got struct {
		Disturb struct {
			Routes        map[string]string `json:"routes"`
			DeliverySaved bool              `json:"delivery_saved"`
			Delivery      struct {
				DefaultChannel string                       `json:"default_channel"`
				Rows           map[string]map[string]string `json:"rows"`
			} `json:"delivery"`
		} `json:"disturb"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/policy", "", http.StatusOK)), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Disturb
	if !d.DeliverySaved || d.Delivery.DefaultChannel != "channel:acc-chan" || !reflect.DeepEqual(d.Routes, before) {
		t.Fatalf("after upgrade: saved=%v default=%q routes=%v", d.DeliverySaved, d.Delivery.DefaultChannel, d.Routes)
	}
	if r := d.Delivery.Rows; r["decision"]["user"] != "default" || r["decision"]["agent"] != "off" ||
		r["stale"]["agent"] != "next_chat" || r["stale"]["user"] != "off" || r["task_failed:brief"]["user"] != "default" {
		t.Fatalf("suggested rows = %v", r)
	}

	if c := f.claimWithin(settle); c == nil || c.ID != base {
		t.Fatalf("in-flight decision after upgrade = %+v, want %s once", c, base)
	}
	if c := f.claim(3); c != nil {
		t.Fatalf("upgrade produced another delivery: %+v", c)
	}

	// 旧二进制的读法：解码策略时忽略 delivery，只看旧键。
	out, err := exec.Command("sqlite3", "-readonly", filepath.Join(f.Home, "ledger.db"),
		"SELECT doc FROM policy_version ORDER BY version DESC LIMIT 1").CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3: %v %s", err, out)
	}
	var old struct {
		Disturb struct {
			Routes map[string]string `json:"routes"`
		} `json:"disturb"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &old); err != nil || !reflect.DeepEqual(old.Disturb.Routes, before) {
		t.Fatalf("stored legacy routes = %v (%v), want %v", old.Disturb.Routes, err, before)
	}

	body, _ := json.Marshal(map[string]any{"disturb": map[string]any{"routes": route(map[string]string{"decision": "web"}), "remind_on_deadline": true}})
	if r := f.Web("PUT", "/v1/settings/policy", string(body)); r.Code != http.StatusBadRequest || !strings.Contains(r.Body, "frozen") {
		t.Fatalf("changing a frozen key: HTTP %d %s", r.Code, r.Body)
	}
}
