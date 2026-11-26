//go:build e2e

package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// BatchHarness 是批处理替身的声明名；它的权限卡取默认卡（查看、整理事项、处理来源事件）。
const BatchHarness = "acc-batch"

// BatchAgent 登记批处理替身并设为来源处理 Agent（§9.3、§13.6）：被拉起后对提示词里的每个事件 id 执行 script
// （环境里有 $APA、$EV 与内核注入的运行凭据）；script 为空时逐条 `apa source propose`。攒够 1 条即起一批。
// 任一命令失败都让运行以非零退出，事件按失败重排。可重复调用以换脚本。
func (e *Env) BatchAgent(script string) {
	e.T.Helper()
	if script == "" {
		script = `"$APA" source propose "$EV" >/dev/null`
	}
	wrapper := filepath.Join(e.Root, "batch-agent.sh")
	body := fmt.Sprintf(`#!/bin/sh
set -e
APA=%q
printf '%%s\n' "$1" >> %q
for EV in $(printf '%%s\n' "$1" | sed -n 's/^[0-9]*\. id=\([^ ]*\) .*/\1/p'); do
%s
done
echo '{"session_id":"batch-'"$APA_RUN"'","text":"本批已处理"}'
`, e.Bin["apa"], filepath.Join(e.Root, "batch-prompts.txt"), script)
	_, statErr := os.Stat(wrapper)
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		e.fatal("%v", err)
	}
	if statErr == nil {
		return // 声明与设置已经在
	}
	h, _ := json.Marshal(map[string]any{"harness": BatchHarness, "headless": map[string]any{
		"command":    []string{wrapper, "{prompt}"},
		"session":    map[string]any{"extract": "$.session_id"},
		"final_text": "$.text",
		"access":     map[string]any{"read": []string{"--access=read"}},
	}})
	if r := e.Apa(e.DEL, string(h), "declare", "-"); r.Code != 0 {
		e.fatal("apa declare %s 退出码 %d: %s%s", BatchHarness, r.Code, r.Stdout, r.Stderr)
	}
	e.webOK("POST", "/v1/settings/declarations/"+BatchHarness+"/1/confirm", "", 200)
	e.webOK("PUT", "/v1/settings/source-pipeline", `{"source_pipeline":{"agent":"legacy:`+BatchHarness+`","batch_size":1,"max_batch":100}}`, 200)
}

// BatchPrompts 返回批处理替身收到过的全部提示词。
func (e *Env) BatchPrompts() string {
	b, _ := os.ReadFile(filepath.Join(e.Root, "batch-prompts.txt"))
	return string(b)
}
