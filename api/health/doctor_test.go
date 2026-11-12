package health

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
)

func (e *env) doctor(kind auth.Kind, mutate func(*http.Request)) (int, kdoctor.Report) {
	e.t.Helper()
	req := e.request(kind)
	req.URL.Path = "/v1/doctor"
	if mutate != nil {
		mutate(req)
	}
	rec := e.serve(req)
	var rep kdoctor.Report
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			e.t.Fatal(err)
		}
	}
	return rec.Code, rep
}

// ACT-DOC-12：不带凭据被拒；observer 读到内核一侧五项检查，不含只在 CLI 做的本机文件与宿主检查；ext 与 webhook 凭据不能读。
func TestDoctorRouteNeedsCredentialAndReturnsKernelSideChecks(t *testing.T) {
	e := setup(t, nil)
	req := e.request(auth.Observer)
	req.URL.Path = "/v1/doctor"
	req.Header.Del("Authorization")
	if rec := e.serve(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	code, rep := e.doctor(auth.Observer, nil)
	if code != 200 || !rep.OK {
		t.Fatalf("observer: %d %+v", code, rep)
	}
	var names []string
	for _, c := range rep.Checks {
		names = append(names, c.Name)
		if c.Why == "" || c.Status == "" {
			t.Errorf("incomplete check: %+v", c)
		}
	}
	if strings.Join(names, ",") != "tier,model_endpoint,sources,packs,channels" {
		t.Fatalf("kernel-side checks expected, got %v", names)
	}
	for _, k := range []auth.Kind{auth.Ext, auth.Webhook} {
		if code, _ := e.doctor(k, nil); code != http.StatusForbidden {
			t.Errorf("%s credential: %d", k, code)
		}
	}
}

// 经认证的 ext 密钥与带 X-APA-Harness 的请求被记下"最近一次见到"，自检据此报告领取时间与插件最近调用。
func TestAuthenticatedRequestsAreNotedForTheDoctor(t *testing.T) {
	e := setup(t, nil)
	var f kdoctor.Facts
	e.doctor(auth.Ext, nil)                                                                      // 被拒也算见到：密钥有效
	e.doctor(auth.Observer, func(r *http.Request) { r.Header.Set("X-APA-Harness", "acc-stub") }) // 宿主插件
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		if f, err = kdoctor.ReadFacts(bg, e.st); err != nil {
			t.Fatal(err)
		}
		if len(f.Seen) == 2 || time.Now().After(deadline) {
			break
		}
	}
	if _, ok := f.Seen["ext:x"]; !ok {
		t.Fatalf("ext key not noted: %v", f.Seen)
	}
	if _, ok := f.Seen["harness:acc-stub"]; !ok {
		t.Fatalf("harness not noted: %v", f.Seen)
	}
}
