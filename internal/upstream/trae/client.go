// SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// GetUserInfo / 签到 / 积分 + 错误分类。
//
// 逐条对齐参考实现 `connectedGraph/trae2api-web` 的 `internal/upstream/client.go`。
package trae

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client SOLO 上游 HTTP 客户端。
type Client struct {
	// HTTP 短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP SSE 流式对话：不设总超时，靠 ResponseHeaderTimeout 兜底首字节。
	StreamHTTP *http.Client

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string
}

// NewClient 构造。传 nil 用默认 Transport 配置。
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		tr := &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		}
		httpClient = &http.Client{Timeout: 120 * time.Second, Transport: tr}
	}
	return &Client{
		HTTP:       httpClient,
		StreamHTTP: &http.Client{Transport: httpClient.Transport},
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return orDefault(c.AgentHost, AgentHost) }
func (c *Client) ugBase() string    { return orDefault(c.UgHost, UgHost) }
func (c *Client) oauthBase() string { return orDefault(c.OAuthHost, OAuthHost) }
func (c *Client) clientID() string  { return orDefault(c.ClientID, ClientID) }

// oauthBaseFor 账号的 OAuth 基址：账号 apiHost 覆盖 > Client.OAuthHost > 常量。
//
// ⚠️ 必须走 Client 字段而不是直接用常量：否则测试无法注入本地 server，
// 且部署时改不了 OAuth 域（参考实现同样以 c.oauthBase() 为默认值）。
func (c *Client) oauthBaseFor(acct Account) string {
	if acct.APIHost != "" {
		return acct.APIHost
	}
	return c.oauthBase()
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// doJSON 发请求并解 JSON；非 2xx 返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTPClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Kind:       Classify(resp.StatusCode, string(raw)),
			Body:       truncate(string(raw), 200),
		}
	}
	return raw, nil
}

// ChatStream 发 llm_utils_chat 请求并返回**原始 SSE body 流**（调用方负责 Close）。
//
// 非 2xx 时返回 *HTTPError（带分类），body 流不返回。
func (c *Client) ChatStream(acct Account, body []byte) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, err
	}
	for k, v := range soloHeaders(acct, true) {
		req.Header.Set(k, v)
	}
	hc := c.StreamHTTP
	if hc == nil {
		hc = c.HTTP
	}
	if hc == nil {
		hc = defaultHTTPClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Kind:       Classify(resp.StatusCode, string(raw)),
			Body:       truncate(string(raw), 300),
		}
	}
	return resp.Body, nil
}

// FetchModels 拉 SOLO 模型表（get_detail_param），解析出上下文档位、
// 思考档位、计费倍率与能力标记（口径见 models.go）。
func (c *Client) FetchModels(acct Account) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range soloHeaders(acct, false) {
		req.Header.Set(k, v)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []map[string]any `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	nowSec := time.Now().Unix()
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		// 硬性过滤（与参考实现一致）：非对话用途、上游停用、官方隐藏的条目不入目录。
		if usage := readString(cfg, "usage", "Usage"); usage != "" && usage != "chat_completion" {
			continue
		}
		if enabled, ok := lookupBool(cfg, "config_switch", "ConfigSwitch"); ok && !enabled {
			continue
		}
		if hidden, ok := lookupBool(cfg, "is_invisible_to_user", "IsInvisibleToUser"); ok && hidden {
			continue
		}
		m, ok := parseConfigEntry(cfg, nowSec)
		if !ok {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// ExchangeResult 一次 ExchangeToken 的结果。
type ExchangeResult struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt Unix 秒（0 = 未知）。
	ExpiresAt int64
}

// ExchangeToken 用 refreshToken 换新 access token（refreshToken 会轮换）。
//
// ⚠️ 响应包在 **Result** 里：`{"Result":{"Token":...,"RefreshToken":...,"TokenExpireAt":毫秒}}`。
// 早期实现在 data/顶层找 token，于是换 token 恒失败（登录看起来「一直不行」的根因之一）。
func (c *Client) ExchangeToken(acct Account) (*ExchangeResult, error) {
	if strings.TrimSpace(acct.RefreshToken) == "" {
		return nil, fmt.Errorf("no refreshToken")
	}
	body := map[string]any{
		"ClientID":     c.clientID(),
		"RefreshToken": acct.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.oauthBaseFor(acct)+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range oauthHeaders() {
		req.Header.Set(k, v)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return nil, ErrRelogin
	}
	out := &ExchangeResult{
		AccessToken:  resp.Result.Token,
		RefreshToken: resp.Result.RefreshToken,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = acct.RefreshToken // 上游没轮换时保留旧值
	}
	out.ExpiresAt = expireAtFromExchange(resp.Result.TokenExpireAt, resp.Result.TokenExpireDuration, time.Now())
	return out, nil
}

// ErrRelogin 凭据不可续期，需要重新登录（终态，不该重试）。
var ErrRelogin = fmt.Errorf("trae: refreshToken 已失效，请重新登录")

// expireAtFromExchange 归一化过期时刻（复刻 login.sh：优先 TokenExpireAt，
// 已过期则用 now+TokenExpireDuration；TokenExpireAt 为毫秒）。
func expireAtFromExchange(tokenExpireAt, tokenExpireDuration int64, now time.Time) int64 {
	if tokenExpireAt > 0 {
		exp := tokenExpireAt
		if exp > 1e12 { // 毫秒 → 秒
			exp /= 1000
		}
		if exp > now.Unix() {
			return exp
		}
	}
	if tokenExpireDuration > 0 {
		return now.Add(time.Duration(tokenExpireDuration) * time.Second).Unix()
	}
	return 0
}

// UserInfo 账号信息（登录补齐用）。
type UserInfo struct {
	UID          string
	Nickname     string
	EnterpriseID string
}

// GetUserInfo 查询账号信息。
//
// ⚠️ 两个要点（参考实现）：请求头是 **X-Cloudide-Token**（不是 Authorization），
// 请求体是 `{"ReqSource":"IDE","IDEVersion":...}`，响应同样包在 **Result** 里。
func (c *Client) GetUserInfo(acct Account) (UserInfo, error) {
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IDEVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.oauthBaseFor(acct)+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return UserInfo{}, err
	}
	for k, v := range oauthHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Cloudide-Token", acct.AccessToken)
	data, err := c.doJSON(req)
	if err != nil {
		return UserInfo{}, err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return UserInfo{}, fmt.Errorf("userinfo parse: %w", err)
	}
	return UserInfo{
		UID:          resp.Result.UserID,
		Nickname:     resp.Result.ScreenName,
		EnterpriseID: resp.Result.EnterpriseID,
	}, nil
}

// CheckinStatus 查询签到状态（响应字段在**顶层**：checked_in / credits / enable）。
func (c *Client) CheckinStatus(acct Account) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	for k, v := range ugHeaders(acct) {
		req.Header.Set(k, v)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
func (c *Client) CheckinClaim(acct Account) error {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	for k, v := range ugHeaders(acct) {
		req.Header.Set(k, v)
	}
	_, err = c.doJSON(req)
	return err
}

// EntUsage 查询额度明细：remain = limit - used（usage.credits_amount 是已用积分）。
func (c *Client) EntUsage(acct Account) (remain, limit, used int64, packs int, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for k, v := range ugHeaders(acct) {
		req.Header.Set(k, v)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	return remain, limit, used, packs, nil
}

// FallbackModels 兜底目录（远端拉取失败时顶替）。
//
// 参考实现不维护兜底表（拉空即报错）；这里保留少量实测可用条目，
// 避免未登录/网络异常时面板模型页完全空白。
func FallbackModels() []ModelInfo {
	return []ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2"},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek V4 Pro"},
		{ID: "DeepSeek-V4-Flash", Name: "DeepSeek V4 Flash"},
		{ID: "kimi-k3", Name: "Kimi K3"},
		{ID: "Doubao-Seed-2.1-Pro", Name: "Doubao Seed 2.1 Pro"},
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
