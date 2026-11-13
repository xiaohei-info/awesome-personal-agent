//go:build e2e

package authz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const injSrc = "src_inj" // 验收用脚本来源（source_type=acc，没有任何内置规则命中）

// injScript 把队列目录里待输出的事件文件逐个输出，末行给游标（Source 执行契约，§9.2）。
const injScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// injSource 登记脚本来源，并返回事件队列目录。
func (k *kit) injSource() string {
	k.t.Helper()
	q := filepath.Join(k.Root, "inj-source")
	for _, d := range []string{"in", "done"} {
		if err := os.MkdirAll(filepath.Join(q, d), 0o755); err != nil {
			k.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(q, "inj.sh"), []byte(injScript), 0o755); err != nil {
		k.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": injSrc, "type": "script", "source_type": "acc",
		"command": filepath.Join(q, "inj.sh") + " " + q, "schedule": "1s", "timeout": 30})
	k.webOK("POST", "/v1/sources", string(body), 201)
	return q
}

// emitEvent 向队列追加一条标准事件（下一个周期被脚本输出）。
func (k *kit) emitEvent(queue, id, subject, detail string) {
	k.t.Helper()
	b, _ := json.Marshal(map[string]any{"id": id, "type": "acc.msg", "subject": subject, "summary": "摘要 " + subject, "detail": detail,
		"sender": id + "@example.invalid", "sender_verified": true, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
	tmp := filepath.Join(queue, "tmp-"+id)
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		k.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(queue, "in", id+".jsonl")); err != nil {
		k.t.Fatal(err)
	}
}

// waitDisposed 等来源事件 id 有处置（不再 pending）。
func (k *kit) waitDisposed(id string) {
	k.t.Helper()
	ok := k.WaitFor(90*time.Second, func() bool {
		var out struct {
			Events []struct {
				Disposition string `json:"disposition"`
				DedupeKey   struct {
					EventID string `json:"event_id"`
				} `json:"dedupe_key"`
			} `json:"events"`
		}
		r := k.APIRaw("GET", "/v1/sources/"+injSrc+"/events?limit=500", "", k.OBS)
		if json.Unmarshal([]byte(r.Body), &out) != nil {
			return false
		}
		for _, e := range out.Events {
			if e.DedupeKey.EventID == id && e.Disposition != "" && e.Disposition != "pending" {
				return true
			}
		}
		return false
	})
	if !ok {
		k.t.Fatalf("来源事件 %s 在 90 秒内没有被处置\n%s", id, k.ServiceLog())
	}
}

// plain 把请求体里的 JSON 转义还原成可查找的纯文本。
func plain(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(t + "\n")
			var inner any
			if len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Unmarshal([]byte(t), &inner) == nil {
				walk(inner)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				sb.WriteString(k + "\n")
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}

// run 是 `apa run list` 里的一条运行。
type runView struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Target   string `json:"target"`
	State    string `json:"state"`
	Harness  string `json:"harness"`
	Access   string `json:"access"`
}

func (k *kit) runs(category string) []runView {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "run", "list", "--category", category)
	k.expect("run list", r, 0)
	var out struct {
		Runs []runView `json:"runs"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Runs
}

// newRun 等 category 里出现一条 seen 之外的运行并返回它。
func (k *kit) newRun(category string, seen map[string]bool) runView {
	k.t.Helper()
	var found runView
	if !k.WaitFor(60*time.Second, func() bool {
		for _, r := range k.runs(category) {
			if !seen[r.ID] {
				found = r
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("60 秒内没有出现新的 %s 运行\n%s", category, k.ServiceLog())
	}
	return found
}

func (k *kit) seenRuns(category string) map[string]bool {
	m := map[string]bool{}
	for _, r := range k.runs(category) {
		m[r.ID] = true
	}
	return m
}

func (k *kit) readFile(path string) string {
	k.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		k.t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

// writePack 把用户能力包写进 APA_HOME/packs（内核热加载），等包和动作类规则出现。
func (k *kit) writePack(name string, files map[string]string, ruleKeys ...string) {
	k.t.Helper()
	for rel, body := range files {
		p := filepath.Join(k.Home, "packs", name, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			k.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			k.t.Fatal(err)
		}
	}
	if !k.WaitFor(30*time.Second, func() bool { return strings.Contains(k.APIRaw("GET", "/v1/packs", "", k.OBS).Body, `"`+name+`"`) }) {
		k.t.Fatalf("能力包 %s 没有被加载\n%s", name, k.ServiceLog())
	}
	for _, key := range ruleKeys {
		if !k.WaitFor(30*time.Second, func() bool { return strings.Contains(k.webOK("GET", "/v1/rules", "", 200), key) }) {
			k.t.Fatalf("规则 %s 没有被加载\n%s", key, k.ServiceLog())
		}
	}
}
