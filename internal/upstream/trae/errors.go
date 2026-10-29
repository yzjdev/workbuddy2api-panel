// 上游错误分类与错误类型。
//
// 分类口径对齐参考实现 `internal/upstream/client.go` 的 Classify：
// 1005 + plan → 权益不足；401 → 会话失效；429 / 404 / 5xx / 其他 4xx 各归一类。
package trae

import (
	"fmt"
	"strings"
)

// Kind 错误分类（pool 据此决定冷却时长）。
type Kind int

const (
	KindNone     Kind = iota // 成功
	KindHardPlan             // 1005 + plan → 权益不足（硬冷却）
	KindSoftRate             // 429 → 短冷却
	KindSessionDead          // 401 → 禁用，需重新登录
	KindQuotaExceeded        // 4008：额度耗尽
	KindNotFound             // 404 → 短冷却
	KindServer               // 5xx
	KindClient               // 其他 4xx
)

func (k Kind) String() string {
	switch k {
	case KindHardPlan:
		return "plan_limit"
	case KindSoftRate:
		return "soft_rate"
	case KindSessionDead:
		return "session_dead"
	case KindQuotaExceeded:
		return "quota_exceeded"
	case KindNotFound:
		return "not_found"
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	default:
		return "none"
	}
}

// sessionDeadMarkers 会话失效的响应体标记。
var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) Kind {
	lower := strings.ToLower(body)
	// 1005 → plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return KindHardPlan
	}
	// 4008 → 额度耗尽（先于 401 判定：网关会把多个错误码拼在一个 msg 里）
	if strings.Contains(body, "4008") || strings.Contains(lower, "exceeded the quota") {
		return KindQuotaExceeded
	}
	if status == 401 {
		return KindSessionDead
	}
	if status == 429 {
		return KindSoftRate
	}
	if status == 404 {
		return KindNotFound
	}
	if status >= 500 {
		return KindServer
	}
	if status >= 400 {
		return KindClient
	}
	return KindNone
}

// ShouldRotate 该类别是否应触发「换下一个账号」。
func ShouldRotate(k Kind) bool { return k != KindNone }

// RecordsRateLimit 该类别的失败是否应记为该模型的限流标记。
func RecordsRateLimit(k Kind) bool {
	switch k {
	case KindHardPlan, KindSoftRate, KindNotFound, KindQuotaExceeded:
		return true
	}
	return false
}

// IsTerminal 该类别是否属于终态（重试无意义，只能重新登录）。
func IsTerminal(k Kind) bool { return k == KindSessionDead }

// SessionDeadBody 响应体是否明确指向会话失效。
func SessionDeadBody(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range sessionDeadMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// HTTPError 上游非 2xx 响应（带分类）。
type HTTPError struct {
	StatusCode int
	Kind       Kind
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.StatusCode, e.Body)
}
