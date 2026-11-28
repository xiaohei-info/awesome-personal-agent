//go:build e2e

package events

import (
	"strings"
	"testing"
)

// headSeq 是账本里最后一条登记事件对应的游标。
func (f *fx) headSeq() int64 { _, next := f.feed(f.OBS, 0); return next }

// TestOutwardExportKeyReadOnly 对外接口的导出密钥只读：不能写事项，也不能碰投递协议。
func TestOutwardExportKeyReadOnly(t *testing.T) {
	f := setup(t)
	exportKey := f.extKey("EXP", "export")
	count := func() int {
		return strings.Count(f.apa(f.OBS, 0, "item", "list", "--full", "--limit", "200").Stdout, `"itm_`)
	}
	before := count()
	body := `{"kind":"task","title":"越权写入","phase":"open"}`
	if r := f.APIRaw("POST", "/v1/items", body, exportKey); r.Code != 403 && r.Code != 401 {
		t.Errorf("导出密钥 POST /v1/items: HTTP %d（期望被拒）%s", r.Code, r.Body)
	}
	if n := count(); n != before {
		t.Errorf("被拒之后事项数从 %d 变成 %d：写入生效了", before, n)
	}
	// ack：同一个 id 用渠道密钥得到"没有这个投递"，导出密钥必须在鉴权层就被拒，而不是碰巧 404。
	ack := "/v1/deliveries/dlv_nonexistent/ack"
	if r := f.APIRaw("POST", ack, `{}`, exportKey); r.Code != 403 && r.Code != 401 {
		t.Errorf("导出密钥 ack: HTTP %d（期望 401/403）%s", r.Code, r.Body)
	}
	if r := f.APIRaw("POST", ack, `{}`, f.CHAN); r.Code == 403 || r.Code == 401 || r.Code == 200 {
		t.Errorf("对照：渠道密钥 ack 不存在的投递应是 404 类错误，实际 HTTP %d %s", r.Code, r.Body)
	}
}
