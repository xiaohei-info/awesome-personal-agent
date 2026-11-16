package packs_test

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/packs"
)

// 附录 C.13：内置简报包的 schedule_days 取 1 到 14。经加载内置包得到的真实 schema（YAML 整数）校验，0 与 15 被拒。
func TestBuiltinBriefScheduleDaysBounds(t *testing.T) {
	ld := kpacks.New(kpacks.Options{Builtin: packs.FS, Kinds: items.NewKinds()})
	if rej, err := ld.Load(); err != nil {
		t.Fatalf("load: %v", err)
	} else {
		for _, r := range rej {
			if r.Pack == "brief" {
				t.Fatalf("brief pack rejected: %+v", r)
			}
		}
	}
	p, ok := ld.Get("brief")
	if !ok {
		t.Fatal("brief pack not loaded")
	}
	check := func(days string) error {
		var v any
		if err := json.Unmarshal([]byte(`{"schedule":"24h","schedule_days":`+days+`}`), &v); err != nil {
			t.Fatal(err)
		}
		return items.ValidateAgainst(p.InstanceParams, v)
	}
	for _, bad := range []string{"0", "15", "-1", `"3"`} {
		if check(bad) == nil {
			t.Errorf("schedule_days %s must be rejected", bad)
		}
	}
	for _, ok := range []string{"1", "3", "14"} {
		if err := check(ok); err != nil {
			t.Errorf("schedule_days %s must pass: %v", ok, err)
		}
	}
}
