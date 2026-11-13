//go:build e2e

package workbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// 脚本来源：把收到的 stdin 存盘，输出队列目录里的事件，末行给游标（§9.2 执行契约）。
const sourceScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// scriptSource 是一个 Web 登记的自定义脚本来源，周期缩短为 1 秒。
type scriptSource struct {
	id, queue string
	seq       int
}

func (k *kit) registerSource(id, sourceType string) *scriptSource {
	k.t.Helper()
	q := filepath.Join(k.Root, "src-"+id)
	k.must(os.MkdirAll(filepath.Join(q, "in"), 0o755))
	k.must(os.MkdirAll(filepath.Join(q, "done"), 0o755))
	script := filepath.Join(q, "source.sh")
	k.must(os.WriteFile(script, []byte(sourceScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": sourceType,
		"command": script + " " + q, "schedule": "1s", "timeout": 30, "default_project": k.Projects["acc-open"]})
	k.webOK("POST", "/sources", string(body), 201)
	return &scriptSource{id: id, queue: q}
}

// emit 让来源在下一个周期输出这些标准事件（一次一个文件，原子改名）。
func (s *scriptSource) emit(k *kit, events ...map[string]any) {
	k.t.Helper()
	s.seq++
	var buf []byte
	for _, e := range events {
		e["sender_verified"] = true
		e["occurred_at"] = time.Now().UTC().Format(time.RFC3339)
		b, _ := json.Marshal(e)
		buf = append(buf, append(b, '\n')...)
	}
	tmp := filepath.Join(s.queue, fmt.Sprintf("tmp-%d", s.seq))
	k.must(os.WriteFile(tmp, buf, 0o644))
	k.must(os.Rename(tmp, filepath.Join(s.queue, "in", fmt.Sprintf("%04d.jsonl", s.seq))))
}

// writePack 把能力包写进数据目录；能力包只在内核启动时加载，调用方随后重启内核。
func (k *kit) writePack(name, yaml string) {
	k.t.Helper()
	dir := filepath.Join(k.Home, "packs", name)
	k.must(os.MkdirAll(dir, 0o755))
	k.must(os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(yaml), 0o644))
}

func (k *kit) restartKernel() {
	k.t.Helper()
	k.StopKernel()
	k.StartKernelAgain()
	k.WebLogin()
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events，附录 B.2）。
type SrcEvent struct {
	ID             string `json:"id"`
	Subject        string `json:"subject"`
	Disposition    string `json:"disposition"`
	UnjudgedReason string `json:"unjudged_reason"`
	Item           string `json:"item"`
	Type           string `json:"type"`
}

func (k *kit) srcEvents(src string, disposition string) []SrcEvent {
	k.t.Helper()
	var out struct {
		Events []SrcEvent `json:"events"`
	}
	path := "/sources/" + src + "/events?limit=500"
	if disposition != "" {
		path += "&disposition=" + disposition
	}
	jsonOf(k.t, k.webOK("GET", path, "", 200), &out)
	return out.Events
}

// waitEvents 等来源的事件数达到 n 且都已被处置（disposition 不再是 pending）。
func (k *kit) waitEvents(src string, n int) []SrcEvent {
	k.t.Helper()
	var evs []SrcEvent
	if !k.WaitFor(60*time.Second, func() bool {
		evs = k.srcEvents(src, "")
		if len(evs) < n {
			return false
		}
		for _, e := range evs {
			if e.Disposition == "" || e.Disposition == "pending" {
				return false
			}
		}
		return true
	}) {
		k.t.Fatalf("来源 %s 在 60 秒内没有处置到 %d 条事件: %+v\n%s", src, n, evs, k.ServiceLog())
	}
	return evs
}

// draft 配置 triage-draft：请求里的来源标题为 subject 时返回给定起草结果（请求体里的输入 JSON 经转义，键用转义形态）。
func (k *kit) draft(subject string, d map[string]any) {
	b, _ := json.Marshal(d)
	k.ModelStub.When(`subject\":\"`+subject+`\"`, modelstub.Reply{Content: string(b)})
}

func ptr[T any](v T) *T { return &v }

// healthSignal 读 apa health 里某个信号（用 token 指定的实例）。
type signal struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
	Detail   any    `json:"detail"`
}

func (k *kit) health(token string) (healthy bool, sigs map[string]signal) {
	k.t.Helper()
	var out struct {
		Healthy bool     `json:"healthy"`
		Signals []signal `json:"signals"`
	}
	jsonOf(k.t, k.apa(token, 0, "health").Stdout, &out)
	sigs = map[string]signal{}
	for _, s := range out.Signals {
		sigs[s.ID] = s
	}
	return out.Healthy, sigs
}

var _ = testing.Short
