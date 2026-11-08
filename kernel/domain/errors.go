package domain

// Code 是错误响应里的稳定机器码（附录 B.3）。
type Code string

const (
	CodeInvalidRequest       Code = "invalid_request"        // 400 → 2
	CodeUnauthenticated      Code = "unauthenticated"        // 401 → 3
	CodePermissionDenied     Code = "permission_denied"      // 403 → 4
	CodeWebOnly              Code = "web_only"               // 403 → 20
	CodeExecutorNotAllowed   Code = "executor_not_allowed"   // 403 → 21
	CodeRiskScreeningPending Code = "risk_screening_pending" // 403 → 22
	CodeBasisRequired        Code = "basis_required"         // 403 → 23：替本人做决定的依据不足（§14.5）
	CodeNotFound             Code = "not_found"              // 404 → 5
	CodeConflict             Code = "conflict"               // 409 → 6
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
	ExitScreeningPending   = 22
	ExitBasisRequired      = 23
)

var codeHTTP = map[Code]int{
	CodeInvalidRequest:       400,
	CodeUnauthenticated:      401,
	CodePermissionDenied:     403,
	CodeWebOnly:              403,
	CodeExecutorNotAllowed:   403,
	CodeRiskScreeningPending: 403,
	CodeBasisRequired:        403,
	CodeNotFound:             404,
	CodeConflict:             409,
	CodeInternal:             500,
}

var codeExit = map[Code]int{
	CodeInvalidRequest:       ExitUsage,
	CodeUnauthenticated:      ExitUnauthenticated,
	CodePermissionDenied:     ExitPermissionDenied,
	CodeWebOnly:              ExitWebOnly,
	CodeExecutorNotAllowed:   ExitExecutorNotAllowed,
	CodeRiskScreeningPending: ExitScreeningPending,
	CodeBasisRequired:        ExitBasisRequired,
	CodeNotFound:             ExitNotFound,
	CodeConflict:             ExitConflict,
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
	WebURL  *string  `json:"web_url"`
}

func NewError(code Code, why, next string) *Error {
	return &Error{Code: code, Why: why, Next: next}
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Why }

func (e *Error) HTTPStatus() int { return e.Code.HTTPStatus() }
func (e *Error) ExitCode() int   { return e.Code.ExitCode() }
