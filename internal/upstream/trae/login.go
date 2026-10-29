// TRAE 登录：授权链接构造 + 回调解析 + JWT 过期推算。
//
// 逐条对齐参考实现 `internal/server/callback.go`（及其 `login.sh`）：
//
//	http://127.0.0.1:18080/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//
// 回调**直接回传 token**（不是 OAuth 的 ?code=）；缺 refreshToken 时回退 userJwt.Token。
package trae

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// loginTraceIDHexLen login_trace_id 的 hex 长度（login.sh 用 token_hex(8) → 16 字符）。
const loginTraceIDHexLen = 16

// CallbackURL 构造本地回调地址（端口为实际监听端口）。
func CallbackURL(port string) string {
	return "http://127.0.0.1:" + port + CallbackPath
}

// MachineTraceID 由 machineID+deviceID 派生稳定的 login_trace_id（hex16）。
//
// 取拼接串尾部 16 字符；不足则左补 0（复刻参考实现 machineTraceID）。
// 回调不回传 machine_id/device_id，但会回传 loginTraceID —— 用它反查登录时生成的那一对 id。
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= loginTraceIDHexLen {
		return h[len(h)-loginTraceIDHexLen:]
	}
	return strings.Repeat("0", loginTraceIDHexLen-len(h)) + h
}

// BuildLoginURL 构造 TRAE 登录 URL（复刻 login.sh 的参数集与顺序）。
//
// callbackURL 是登录成功后的重定向落点（如 http://127.0.0.1:18080/authorize）；
// machineID/deviceID 必须与落盘凭据共用同一对（hex32）。
func BuildLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", ClientID)
	v.Set("redirect", "0")
	return ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(MachineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(IDEVersion) +
		"&x_app_type=stable"
}

// CallbackInfo 回调链接解析结果（原始凭证，仅服务端内部使用）。
type CallbackInfo struct {
	// RefreshToken 优先取 query.refreshToken，缺省回退 userJwt.RefreshToken。
	RefreshToken string
	// AccessToken 无 refreshToken 时回退 userJwt.Token（兜底）。
	AccessToken string
	UID         string
	Nickname    string
	// EnterpriseID 回调字段名是 **TenantID**。
	EnterpriseID string
	// ExpiresAt 仅 userJwt 兜底路径会设（Unix 秒）。
	ExpiresAt int64
	// MachineID / DeviceID 回调可能带上（有则优先用）。
	MachineID string
	DeviceID  string
	// LoginTraceID 回调回传的 trace（驼峰参数名 loginTraceID）。
	LoginTraceID string
}

// ParseCallback 解析 TRAE 登录回调链接，提取凭证字段。
//
// refreshToken 优先；缺失时回退 userJwt.RefreshToken；再缺则用 userJwt.Token 兜底。
// 仅解析，不执行 ExchangeToken（换 token 由 Client.ExchangeToken 完成）。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	// 允许只贴 "/authorize?..." 这种相对形式
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		MachineID:    q.Get("machine_id"),
		DeviceID:     q.Get("device_id"),
		// ⚠️ 回调参数名是**驼峰** loginTraceID（不是 login_trace_id）
		LoginTraceID: firstNonEmpty(q.Get("loginTraceID"), q.Get("login_trace_id")),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = getString(userInfo, "UserID")
	info.Nickname = fixMojibake(getString(userInfo, "ScreenName"), info.UID)
	info.EnterpriseID = getString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := getString(userJwt, "Token")
	jwtRefresh := getString(userJwt, "RefreshToken")

	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		info.AccessToken = jwtToken
		if jwtToken == "" {
			return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
		}
		if exp := getInt64(userJwt, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = normalizeExpire(exp)
		}
	}
	return info, nil
}

// normalizeExpire TokenExpireAt 毫秒 → Unix 秒（>1e12 视为毫秒）。
func normalizeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数（再容错解一层 unquote）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

// getString 从 JSON 对象读字符串（兼容数字）。
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

// getInt64 从 JSON 对象读整数。
func getInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// fixMojibake 修复回调昵称被双重编码导致的乱码（如 'Óû§8847309959'）。
//
// 无法修复且不含 CJK 时回退为「用户+uid 末 4 位」。
func fixMojibake(s, uid string) string {
	if s == "" {
		return s
	}
	// 把每个字符当 latin1 字节重解为 UTF-8
	if bs, ok := latin1Bytes(s); ok {
		if fixed := string(bs); isPrintable(fixed) {
			return fixed
		}
	}
	if !hasCJK(s) {
		suffix := uid
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		return "用户" + suffix
	}
	return s
}

// latin1Bytes 把「每个 rune 都在 0-255 内」的字符串还原成字节串。
func latin1Bytes(s string) ([]byte, bool) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return nil, false
		}
		out = append(out, byte(r))
	}
	return out, true
}

// isPrintable 判定字符串是否可打印（乱码修复后的有效性判据）。
func isPrintable(s string) bool {
	if s == "" || strings.ContainsRune(s, '\uFFFD') {
		return false
	}
	for _, r := range s {
		if r < 32 {
			return false
		}
	}
	return true
}

// hasCJK 是否含中日韩统一表意文字。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// JWTExpiry 解 JWT 的 exp（Unix 秒）；只解码不验签，失败返回 0。
//
// 过期判定依赖它：凭据可能没有 expires_at（手工导入/老数据），只读它会让
// 过期判定恒为 false，表现为「凭据悄悄过期、续期从不触发」。
func JWTExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		return int64(exp)
	}
	return 0
}

// NeedsRefresh token 是否将在 within 内过期（或已过期/无过期信息）。
func NeedsRefresh(acct Account, within time.Duration) bool {
	if acct.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= acct.ExpiresAt
}
