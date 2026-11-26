//go:build e2e

package baseline

import (
	"encoding/json"
	"fmt"
	"time"
)

// DiagBriefSoon 临时诊断（合并前撤掉）：把默认简报挪到 lead 之后到点，用例失败时打印运行、简报事项与模型请求。
func (e *Env) DiagBriefSoon(lead time.Duration) {
	at := time.Now().Add(lead)
	var l struct {
		Instances []struct {
			ID      string `json:"id"`
			Pack    string `json:"pack"`
			Enabled bool   `json:"enabled"`
		} `json:"instances"`
	}
	if err := json.Unmarshal([]byte(e.webOK("GET", "/v1/instances", "", 200)), &l); err != nil {
		e.fatal("%v", err)
	}
	for _, in := range l.Instances {
		if in.Pack == "brief" {
			e.webOK("PATCH", "/v1/instances/"+in.ID, fmt.Sprintf(`{"params":{"schedule":"%d %d * * *"}}`, at.Minute(), at.Hour()), 200)
			e.T.Logf("DIAG brief %s enabled=%v due %s", in.ID, in.Enabled, at.Format("15:04"))
		}
	}
	t := e.T
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("DIAG runs: %s", e.apaAt(e.URL, e.OBS, "", "run", "list").Stdout)
		t.Logf("DIAG briefs: %s", e.apaAt(e.URL, e.OBS, "", "item", "list", "--kind", "brief").Stdout)
		for _, r := range e.ModelStub.Requests() {
			t.Logf("DIAG model: %.300s", r.BodyText)
		}
	})
}
