// Package version 是 GET /version 路由与 `apa version` 共用的版本信息（附录 B.5）：
// 两处调用同一个 Current，所以内容天然相同；路由不需要凭据。
package version

import (
	"net/http"
	"runtime/debug"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// Release 是本发行的语义化版本号。
const Release = "0.2.0"

// Info 是 `apa version` 与 GET /version 的响应。
type Info struct {
	Version   string    `json:"version"`
	Commit    string    `json:"commit"`
	TestClock bool      `json:"test_clock"` // apa_testclock 构建为 true；发布构建恒为 false
	Schema    Schema    `json:"schema"`
	Protocols Protocols `json:"protocols"`
}

// Schema 是各存储模块的数据结构版本。
type Schema struct {
	Ledger int `json:"ledger"`
}

// Protocols 是各协议版本。
type Protocols struct {
	HTTPAPI             string `json:"http_api"`
	SourceContract      int    `json:"source_contract"`
	ChannelExt          int    `json:"channel_ext"`
	ProbabilityJudgment string `json:"probability_judgment"`
}

// Current 返回当前二进制的版本信息；ledger 取自已登记迁移里最高的账本结构版本（store.Registry.SchemaLedger）。
func Current(ledger int) Info {
	return Info{
		Version: Release, Commit: commit(), TestClock: clock.TestClockBuild,
		Schema:    Schema{Ledger: ledger},
		Protocols: Protocols{HTTPAPI: "v1", SourceContract: 1, ChannelExt: 1, ProbabilityJudgment: policy.ProtocolProbability},
	}
}

// commit 是构建所在的版本控制提交；不是从提交构建（如 go test）时为 "unknown"。
func commit() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// Routes 返回 GET /version 路由分组，交给 core.Deps.Groups。
func Routes(ledger int) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/version", Auth: core.AuthNone,
			Handler: func(w http.ResponseWriter, _ *http.Request, _ *core.Principal) error {
				return core.WriteJSON(w, http.StatusOK, Current(ledger))
			}})
	}
}
