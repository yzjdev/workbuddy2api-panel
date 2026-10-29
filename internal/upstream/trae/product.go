// Package trae TRAE SOLO 上游链路。
//
// 重写依据：`connectedGraph/trae2api-web`（Go 参考实现）的
// `internal/upstream/{constants,headers,payload,client,solosse}.go` 与
// `internal/server/{login,callback}.go`，以及其 `login.sh` 的第一手登录流程。
//
// 链路：
//
//	OpenAI chat.completions 入站
//	  → payload.go 转 SOLO 请求体（content 分块、config_name、tools 序列化）
//	  → client.go POST {AgentHost}/api/agent/v3/llm_utils_chat
//	  → sse.go 把 SOLO 事件翻回标准 OpenAI SSE
//
// 与旧版（从 TS 插件移植）的关键差异，均为参考实现实测口径：
//   - 设备形态是 **Windows**（X-Device-Type: windows / X-OS-Version: Windows 11 Pro /
//     X-Device-Brand: 83DG），不是 macOS；
//   - 模型目录用 **get_detail_param（单通道）**，不是 batch_get_detail_param；
//   - ExchangeToken / GetUserInfo 的响应包在 **Result** 里；
//   - 回调 query 的 trace 参数名是 **loginTraceID**（驼峰），不是 login_trace_id；
//   - function 固定 solo_work_lite，不做按模型的通道路由。
package trae

import (
	"net/http"
	"time"
)

// 上游常量（SPEC 实测值，禁止随意改动）。
const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"

	ClientID       = "en1oxy7wnw8j9n"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IDEVersion     = "0.1.52"
	IDEVersionCode = "20260811"
	// DeviceBrand / OSVersion 伪装的是 **Windows** 客户端。
	DeviceBrand = "83DG"
	OSVersion   = "Windows 11 Pro"
	// Function 对话通道（固定）。
	Function = "solo_work_lite"
	// DefaultConfigName 默认模型（实测可用）。
	DefaultConfigName = "glm-5.2"
	// PluginVersion 登录 URL 的 plugin_version（不是 IDE 版本号）。
	PluginVersion = "2.3.62834"

	// CallbackPort 登录回调的默认端口（与官方客户端/login.sh 一致）。
	CallbackPort = "18080"
	// CallbackPath 回调路径（TRAE 登录页只认 /authorize）。
	CallbackPath = "/authorize"
)

// 上游端点。
const (
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// clientUA 出站 UA（三类请求共用）。
const clientUA = "Trae/" + IDEVersion

// Account 出站所需账号视图（来自 auth.Auth 的锁定快照）。
type Account struct {
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	// MachineID / DeviceID 均为 hex32，登录时生成并与凭据一起持久化。
	MachineID string
	DeviceID  string
	// APIHost ExchangeToken / GetUserInfo 的基址覆盖（缺省 OAuthHost）。
	APIHost string
	// ExpiresAt access_token 过期时刻（Unix 秒，0 = 未知）。
	ExpiresAt int64
}

// soloHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头（实测必须）。
func soloHeaders(acct Account, stream bool) map[string]string {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	h := map[string]string{
		"Content-Type":         "application/json",
		"Accept":               accept,
		"User-Agent":           clientUA,
		"Authorization":        "Cloud-IDE-JWT " + acct.AccessToken,
		"X-Cloudide-Token":     acct.AccessToken,
		"X-Ide-Token":          acct.AccessToken,
		"X-App-Id":             AppID,
		"X-App-Version":        "default",
		"X-Ide-Version":        IDEVersion,
		"X-Ide-Version-Code":   IDEVersionCode,
		"X-App-Version-Code":   IDEVersionCode,
		"X-Ide-Version-Type":   "stable",
		"X-Device-Type":        "windows",
		"X-OS-Version":         OSVersion,
		"X-Device-Brand":       DeviceBrand,
		"Request-Traffic-Type": "prod",
	}
	if acct.UID != "" {
		h["X-Uid"] = acct.UID
	}
	if acct.MachineID != "" {
		h["X-Machine-Id"] = acct.MachineID
	}
	if acct.DeviceID != "" {
		h["X-Device-Id"] = acct.DeviceID
	}
	return h
}

// ugHeaders 设置签到/积分（api.trae.cn）所需头。
//
// ⚠️ 与 SOLO 头不同：只带 Authorization + **X-User-Region: CN** + X-Device-Id，
// 不带 X-Uid / X-App-Id 系列。
func ugHeaders(acct Account) map[string]string {
	h := map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"User-Agent":    clientUA,
		"Authorization": "Cloud-IDE-JWT " + acct.AccessToken,
		"X-User-Region": "CN",
	}
	if acct.DeviceID != "" {
		h["X-Device-Id"] = acct.DeviceID
	}
	return h
}

// oauthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func oauthHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   clientUA,
	}
}

// defaultHTTPClient 短请求客户端（有总超时兜底）。
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}
