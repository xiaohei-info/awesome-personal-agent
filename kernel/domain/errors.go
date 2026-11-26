package domain

// Code 是错误响应里的稳定机器码（附录 B.3）。
type Code string

const (
	CodeInvalidRequest     Code = "invalid_request"      // 400 → 2
	CodeUnauthenticated    Code = "unauthenticated"      // 401 → 3
	CodePermissionDenied   Code = "permission_denied"    // 403 → 4
	CodeWebOnly            Code = "web_only"             // 403 → 20：硬线，只有本人在 Web 做（reason hard_line）
	CodeReauthRequired     Code = "reauth_required"      // 403 → 20：3b 动作，Web 会话须再输一次口令
	CodeTierClosed         Code = "tier_closed"          // 403 → 23：该 Agent 的档位没有放开这一档
	CodeExecutorNotAllowed Code = "executor_not_allowed" // 403 → 21
	// 以下几个是旧权限模型的码（初筛、本人原话、私聊、授权单、导入）：内核不再产出，取值保留为只读的历史值，旧的拒绝记录仍能读懂。
	CodeRiskScreeningPending Code = "risk_screening_pending" // 403 → 22
	CodeBasisRequired        Code = "basis_required"         // 403 → 23
	CodeSessionNotDirect     Code = "session_not_direct"     // 403 → 23
	CodeGrantRequired        Code = "grant_required"         // 403 → 23：命令只接受授权单（import.apply），没有带单或只带了本人原话
	CodeImportSourceMismatch Code = "import_source_mismatch" // 403 → 23：导入授权单的来源名不符
	CodeImportLimitExceeded  Code = "import_limit_exceeded"  // 403 → 23：累计条数会超过导入授权单的上限
	CodeNotFound             Code = "not_found"              // 404 → 5
	CodeConflict             Code = "conflict"               // 409 → 6
	CodeModelsPaused         Code = "models_paused"          // 409 → 6：本人暂停了模型，需要模型的本人操作被拒（§8.7）
	CodeLoginLocked          Code = "login_locked"           // 429 → 1：登录被限速或锁定（§14.13），带 Retry-After
	CodeFetchFailed          Code = "fetch_failed"           // 502 → 1：来源程序取回附件失败或超限（§7.6），清单不变
	CodeInternal             Code = "internal"               // 其余 → 1
)

// 退出码（§17.3）。Source 程序的退出码是另一套（§9.2），不在此处。
const (
	ExitOK                 = 0
	ExitUnclassified       = 1
	ExitUsage              = 2
	ExitUnauthenticated    = 3
	ExitPermissionDenied   = 4
	ExitNotFound           = 5
	ExitConflict           = 6
	ExitUnreachable        = 7 // 仅 CLI 侧：内核不可达，没有 HTTP 响应
	ExitAgentsStale        = 10
	ExitAgentsDrift        = 11
	ExitWebOnly            = 20
	ExitExecutorNotAllowed = 21
	ExitScreeningPending   = 22             // 旧模型：初筛未返回，不再产出
	ExitTierClosed         = 23             // 档位未放开
	ExitBasisRequired      = ExitTierClosed // 旧名，旧的拒绝记录按它读
)

var codeHTTP = map[Code]int{
	CodeInvalidRequest:       400,
	CodeUnauthenticated:      401,
	CodePermissionDenied:     403,
	CodeWebOnly:              403,
	CodeReauthRequired:       403,
	CodeTierClosed:           403,
	CodeExecutorNotAllowed:   403,
	CodeRiskScreeningPending: 403,
	CodeBasisRequired:        403,
	CodeSessionNotDirect:     403,
	CodeGrantRequired:        403,
	CodeImportSourceMismatch: 403,
	CodeImportLimitExceeded:  403,
	CodeNotFound:             404,
	CodeConflict:             409,
	CodeModelsPaused:         409,
	CodeLoginLocked:          429,
	CodeFetchFailed:          502,
	CodeInternal:             500,
}

var codeExit = map[Code]int{
	CodeInvalidRequest:       ExitUsage,
	CodeUnauthenticated:      ExitUnauthenticated,
	CodePermissionDenied:     ExitPermissionDenied,
	CodeWebOnly:              ExitWebOnly,
	CodeReauthRequired:       ExitWebOnly,
	CodeTierClosed:           ExitTierClosed,
	CodeExecutorNotAllowed:   ExitExecutorNotAllowed,
	CodeRiskScreeningPending: ExitScreeningPending,
	CodeBasisRequired:        ExitBasisRequired,
	CodeSessionNotDirect:     ExitBasisRequired,
	CodeGrantRequired:        ExitBasisRequired,
	CodeImportSourceMismatch: ExitBasisRequired,
	CodeImportLimitExceeded:  ExitBasisRequired,
	CodeNotFound:             ExitNotFound,
	CodeConflict:             ExitConflict,
	CodeModelsPaused:         ExitConflict,
	CodeLoginLocked:          ExitUnclassified,
	CodeInternal:             ExitUnclassified,
}

// HTTPStatus 返回码对应的 HTTP 状态；未登记的码按内部错误处理。
func (c Code) HTTPStatus() int {
	if s, ok := codeHTTP[c]; ok {
		return s
	}
	return 500
}

// ExitCode 返回码对应的 CLI 退出码；未登记的码为 1。
func (c Code) ExitCode() int {
	if e, ok := codeExit[c]; ok {
		return e
	}
	return ExitUnclassified
}

// CodeFromHTTP 由 HTTP 状态反推码类（403 无法区分 4/20/21/22/23，须由 code 字段决定，这里取 4）。
func CodeFromHTTP(status int) Code {
	switch status {
	case 400:
		return CodeInvalidRequest
	case 401:
		return CodeUnauthenticated
	case 403:
		return CodePermissionDenied
	case 404:
		return CodeNotFound
	case 409:
		return CodeConflict
	}
	return CodeInternal
}

// Error 是 API 错误响应体（附录 B.3），同时实现 error。
// Why 与 Next 只写机制，不含账本里的用户内容。
type Error struct {
	Code    Code     `json:"code"`
	Why     string   `json:"why"`
	Next    string   `json:"next"`
	Allowed []string `json:"allowed,omitempty"`
	// AllowedMatch 与 Candidates 只随退出码 21 返回：项目的匹配规则，以及名录里当前命中名单的执行者 id。
	AllowedMatch []ExecutorMatch `json:"allowed_match,omitempty"`
	Candidates   []string        `json:"candidates,omitempty"`
	// Reason 是同一个码之下更细的稳定原因（如授权单不可用的具体原因 GrantReason*），展示层据它说人话；空表示只看 Code。
	Reason string  `json:"reason,omitempty"`
	WebURL *string `json:"web_url"`
	// Group 与 SettingsURL 只随退出码 23 返回：没开的那一组（组键）与这个 Agent 权限卡的页面（§14.3）。
	Group       string  `json:"group,omitempty"`
	SettingsURL *string `json:"settings_url,omitempty"`
}

// 拒绝的三种口径（Error.Reason，§14.3）：只有本人（hard_line）、权限卡没开（tier_closed）、钥匙范围外；
// reauth_required 是 Web 会话做后果较大的事还没再输口令。
const (
	ReasonHardLine       = "hard_line"
	ReasonReauthRequired = "reauth_required"
	ReasonTierClosed     = "tier_closed"
	ReasonOutOfScope     = "out_of_scope"
	// ReasonChainDepthExceeded：连锁触发超过设置里的层数，命令不生效（退出码 6，§14.9）。
	ReasonChainDepthExceeded = "chain_depth_exceeded"
)

// 旧模型授权单不可用的原因：内核不再产出，只供读旧的拒绝记录。
const (
	GrantReasonScreeningPending = "grant_screening_pending" // 事项上有初筛未返回的决策请求：处理那张卡或稍后再试，之后同一张单自动可用
	GrantReasonRiskCeiling      = "grant_risk_ceiling"      // 对象属 web_only，授权单不能覆盖
	GrantReasonNotForBackground = "grant_not_for_background"
	GrantReasonUntrustedOrigin  = "untrusted_origin"
	GrantReasonUntrustedTrigger = "untrusted_trigger"
)

func NewError(code Code, why, next string) *Error {
	return &Error{Code: code, Why: why, Next: next}
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Why }

func (e *Error) HTTPStatus() int { return e.Code.HTTPStatus() }
func (e *Error) ExitCode() int   { return e.Code.ExitCode() }
