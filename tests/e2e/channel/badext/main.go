//go:build e2e

// badext 是故意不做发送幂等的渠道 ext：每次发送都生成新的 external_ref（不以投递 id 为幂等键）。
// 只用于验收 `apa ext check` 能指出坏 ext（ACT-CHP-11），不属于发行。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	base, key := os.Getenv("APA_URL"), os.Getenv("APA_EXT_KEY")
	c := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path string, body any) (*http.Response, error) {
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, base+path, r)
		req.Header.Set("Authorization", "Bearer "+key)
		return c.Do(req)
	}
	for n := 0; ; n++ {
		resp, err := do("GET", "/v1/deliveries/next?wait=1", nil)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var d struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		}
		ok := resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&d) == nil
		resp.Body.Close()
		if !ok {
			continue
		}
		ref := fmt.Sprintf("bad-%d-%d", time.Now().UnixNano(), n) // 每次都是新 ref
		if r, err := do("POST", "/v1/deliveries/"+d.ID+"/ack", map[string]any{"version": d.Version, "external_ref": ref}); err == nil {
			r.Body.Close()
		}
	}
}
