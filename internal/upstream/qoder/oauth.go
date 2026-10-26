// Qoder OAuth 设备授权 / PAT 导入 / token 刷新客户端。
// 协议规格与 qoder2api-hub 逆向结论同源：PKCE S256、404/202 判 pending、
// refresh_token 前缀分族（drt- → device、jrt-/其他 → job，PAT 兜底）。
package qoder

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Realm 双区配置。
type Realm struct {
	OpenAPI     string // openapi 基址
	Website     string // 设备授权网页
	Gateway     string // 推理网关
	ClientID    string
	RedirectURI string // 仅 CN 授权 URL 携带
	UserAgent   string
	// NonceStyle: "uuid"（CN 带横线）/ "hex"（intl 32 位）
	NonceStyle string
}

// RealmCN 国内版配置。
var RealmCN = Realm{
	OpenAPI:     "https://openapi.qoder.com.cn",
	Website:     "https://qoder.com.cn",
	Gateway:     "https://gateway.qoder.com.cn",
	ClientID:    "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
	RedirectURI: "qoder-work-cn://",
	UserAgent:   "QoderWork/1.1.75",
	NonceStyle:  "uuid",
}

// RealmIntl 国际版配置。
var RealmIntl = Realm{
	OpenAPI:     "https://openapi.qoder.sh",
	Website:     "https://qoder.com",
	Gateway:     "https://api3.qoder.sh",
	ClientID:   "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
	RedirectURI: "", // intl 授权页不接受 redirect_uri 参数（spec 明确不随请求发送）
	UserAgent:   "Qoder/1.1.75",
	NonceStyle:  "hex",
}

// RealmFor 名称（"cn"/"global"）→ 配置。
func RealmFor(name string) Realm {
	if name == "global" {
		return RealmIntl
	}
	return RealmCN
}

// pkceAlphabet PKCE verifier 字符表（66 字符）。
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// MakePKCE 生成 (verifier, challenge)。verifier：64 随机字节逐字节 mod 66 映射。
func MakePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 64)
	if _, err = randRead(raw); err != nil {
		return "", "", err
	}
	b := make([]byte, 64)
	for i, v := range raw {
		b[i] = pkceAlphabet[int(v)%len(pkceAlphabet)]
	}
	verifier = string(b)
	challenge = sha256UrlB64(verifier)
	return verifier, challenge, nil
}

// NewNonce 按 realm 风格生成 nonce。
func NewNonce(r Realm) string {
	if r.NonceStyle == "hex" {
		var b [16]byte
		_, _ = randRead(b[:])
		return fmt.Sprintf("%x", b[:])
	}
	return newUUID()
}

// randRead 对 crypto/rand.Read 的薄封装（测试可覆盖）。
var randRead = rand.Read

// LoginSession 一次设备授权登录会话。
type LoginSession struct {
	State     string
	AuthURL   string
	Realm     string
	Verifier  string
	Nonce     string
	CreatedAt time.Time
}

// StartLogin 发起设备授权：返回授权 URL 与会话（轮询凭 state 关联）。
func StartLogin(r Realm, machineID string) (*LoginSession, error) {
	verifier, challenge, err := MakePKCE()
	if err != nil {
		return nil, err
	}
	nonce := NewNonce(r)
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("client_id", r.ClientID)
	q.Set("machine_id", machineID)
	if r.RedirectURI != "" {
		// 仅 CN：intl 授权页不接受 redirect_uri 参数
		q.Set("redirect_uri", r.RedirectURI)
	}
	authURL := r.Website + "/device/selectAccounts?" + q.Encode()
	return &LoginSession{
		State:     "qd-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		AuthURL:   authURL,
		Realm:     realmNameOf(r),
		Verifier:  verifier,
		Nonce:     nonce,
		CreatedAt: time.Now(),
	}, nil
}

func realmNameOf(r Realm) string {
	if r.Website == RealmIntl.Website {
		return "global"
	}
	return "cn"
}

// PollResult 轮询结果状态。
type PollResult struct {
	Status       string // pending | ok | error | expired
	Token        string
	RefreshToken string
	UserID       string
	ExpiresIn    float64 // 毫秒（0 = 未知）
	Message      string
}

// PollToken 轮询设备授权 token 端点。
// GET {openapi}/api/v1/deviceToken/poll?nonce=&verifier=&challenge_method=S256
// 404/202 → pending；200 且有 token → ok。
func PollToken(ctx context.Context, client *http.Client, r Realm, sess *LoginSession) PollResult {
	if time.Since(sess.CreatedAt) > 600*time.Second {
		return PollResult{Status: "expired"}
	}
	u := r.OpenAPI + "/api/v1/deviceToken/poll?" + url.Values{
		"nonce":            {sess.Nonce},
		"verifier":         {sess.Verifier},
		"challenge_method": {"S256"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return PollResult{Status: "error", Message: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	resp, err := client.Do(req)
	if err != nil {
		return PollResult{Status: "pending", Message: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == 404 || resp.StatusCode == 202 {
		return PollResult{Status: "pending"}
	}
	if resp.StatusCode != 200 {
		msg := string(body)
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return PollResult{Status: "error", Message: fmt.Sprintf("poll http %d %s", resp.StatusCode, msg)}
	}
	var tok struct {
		Token        string      `json:"token"`
		DeviceToken  string      `json:"device_token"`
		RefreshToken string      `json:"refresh_token"`
		UserID       string      `json:"user_id"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return PollResult{Status: "pending", Message: "bad json: " + err.Error()}
	}
	token := tok.Token
	if token == "" {
		token = tok.DeviceToken
	}
	if token == "" {
		return PollResult{Status: "pending"}
	}
	exp, _ := tok.ExpiresIn.Float64()
	return PollResult{
		Status:       "ok",
		Token:        token,
		RefreshToken: tok.RefreshToken,
		UserID:       tok.UserID,
		ExpiresIn:    exp,
	}
}

// Userinfo 拉取账号身份（id/name/user_type/organization_*，含驼峰别名）。
func Userinfo(ctx context.Context, client *http.Client, r Realm, accessToken string) (*UserInfo, error) {
	u := r.OpenAPI + "/api/v1/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("userinfo http %d: %s", resp.StatusCode, string(body))
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &UserInfo{
		ID:               strAny(raw["id"]),
		Name:             strAny(raw["name"]),
		UserType:         strAny(raw["user_type"]),
		OrganizationID:   strAny(orAlt(raw["organization_id"], raw["organizationId"])),
		OrganizationName: strAny(orAlt(raw["organization_name"], raw["organizationName"])),
	}, nil
}

// UserInfo 账号身份。
type UserInfo struct {
	ID               string
	Name             string
	UserType         string
	OrganizationID   string
	OrganizationName string
}

func orAlt(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func strAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

// ImportPAT PAT（pt- 前缀）换 jobToken。
// POST {openapi}/api/v1/jobToken/exchange body {"personal_token": ...}
// expires_in 单位毫秒。
func ImportPAT(ctx context.Context, client *http.Client, r Realm, personalToken string) (*TokenPair, error) {
	if !strings.HasPrefix(personalToken, "pt-") {
		return nil, fmt.Errorf("PAT 必须以 pt- 开头")
	}
	var out struct {
		Token        string      `json:"token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := postJSON(ctx, client, r, "/api/v1/jobToken/exchange",
		map[string]string{"personal_token": personalToken}, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("jobToken/exchange: empty token")
	}
	exp, _ := out.ExpiresIn.Float64()
	if exp == 0 {
		exp = 24 * 3600 * 1000 // 无 expires_in 时本地兜底 24h（毫秒）
	}
	return &TokenPair{
		Token:        out.Token,
		RefreshToken: out.RefreshToken,
		ExpiresInMs:  exp,
		Kind:         "job",
	}, nil
}

// TokenPair 一次 token 交换/刷新的结果。
type TokenPair struct {
	Token        string
	RefreshToken string
	ExpiresInMs  float64
	Kind         string // device | job
}

// postJSON openapi POST 公共封装。
func postJSON(ctx context.Context, client *http.Client, r Realm, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.OpenAPI+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &AuthError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s http %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// AuthError 401/403 类上游拒绝（可能意味着会话死亡）。
type AuthError struct {
	StatusCode int
	Body       string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("auth error http %d: %s", e.StatusCode, e.Body)
}

// IsDeadSession 报告该错误是否意味着会话已死（TOKEN_EXPIRE / 12153 / offline session）。
func IsDeadSession(err error) bool {
	ae, ok := err.(*AuthError)
	if !ok {
		return false
	}
	return strings.Contains(ae.Body, "TOKEN_EXPIRE") ||
		strings.Contains(ae.Body, "12153") ||
		strings.Contains(ae.Body, "Offline user session not found")
}

// Refresh 按 refresh_token 前缀分族刷新：
// drt- → deviceToken/refresh；其他非空 → jobToken/refresh，失败回落 PAT 兜底。
func Refresh(ctx context.Context, client *http.Client, r Realm, refreshToken, personalToken string) (*TokenPair, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("no refresh token; sign in again")
	}
	if strings.HasPrefix(refreshToken, "drt-") {
		var out struct {
			Token        string      `json:"token"`
			DeviceToken  string      `json:"device_token"`
			RefreshToken string      `json:"refresh_token"`
			ExpiresIn    json.Number `json:"expires_in"`
		}
		if err := postJSON(ctx, client, r, "/api/v1/deviceToken/refresh",
			map[string]string{"refresh_token": refreshToken}, &out); err != nil {
			return nil, err
		}
		token := out.Token
		if token == "" {
			token = out.DeviceToken
		}
		if token == "" {
			return nil, fmt.Errorf("deviceToken/refresh: empty token")
		}
		exp, _ := out.ExpiresIn.Float64()
		if exp == 0 {
			exp = 30 * 86400 * 1000 // device 分支缺省 30 天（毫秒）
		}
		rt := out.RefreshToken
		if rt == "" {
			rt = refreshToken
		}
		return &TokenPair{Token: token, RefreshToken: rt, ExpiresInMs: exp, Kind: "device"}, nil
	}
	var out struct {
		Token        string      `json:"token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	err := postJSON(ctx, client, r, "/api/v1/jobToken/refresh",
		map[string]string{"refresh_token": refreshToken}, &out)
	if err != nil {
		// PAT 兜底（job 刷新失败且持有 PAT 时）
		if personalToken != "" {
			return ImportPAT(ctx, client, r, personalToken)
		}
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("jobToken/refresh: empty token")
	}
	exp, _ := out.ExpiresIn.Float64()
	rt := out.RefreshToken
	if rt == "" {
		rt = refreshToken
	}
	return &TokenPair{Token: out.Token, RefreshToken: rt, ExpiresInMs: exp, Kind: "job"}, nil
}

// MachineIDFor 生成 machine_id（正式实现落盘 ~/.qoder[-cn]/.auth/machine_id，
// 这里提供生成与读取入口；部署侧决定存储位置）。
func NewMachineID() string {
	return newUUID()
}
