// relay.go 中转站（OpenAI 兼容网关）直通出站：realm="relay" 的账号走 base_url + key，
// 请求/响应原样透传，不做 workbuddy 协议翻译、不刷新 token、不签到/余额。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// relayBase 归一化中转站 base（去尾斜杠）；未配置报错。
// 约定：base_url 需带版本前缀（如 https://api.xx.com/v1），出站拼 {base}/chat/completions。
func relayBase(a *auth.Auth) (string, error) {
	b := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if b == "" {
		return "", fmt.Errorf("relay 账号 %s 未配置 base_url", a.UID)
	}
	return b, nil
}

// ChatRelay 中转站直通：POST {base}/chat/completions（OpenAI 兼容，Bearer key）。
// 响应原样返回（SSE/JSON 均不翻译），调用方负责关闭 resp.Body 并透传状态码。
func (c *Client) ChatRelay(ctx context.Context, a *auth.Auth, body []byte) (*http.Response, error) {
	base, err := relayBase(a)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	return c.ChatHTTP.Do(req)
}

// RelayModels 拉取中转站模型名单：GET {base}/models（OpenAI 兼容 data[].id）。
func (c *Client) RelayModels(a *auth.Auth) ([]string, error) {
	base, err := relayBase(a)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay models: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
