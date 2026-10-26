// Qoder 出站 HTTP：COSY 签名请求 + SSE 信封流 → 标准 OpenAI SSE 转发。
//
// 出站链路（Chat Completions）：
//
//	buildBody → QoderEncode（自定义 Base64）→ COSY 签名头
//	POST {gateway}{ChatPath} → 逐帧解信封 → "data: {内层 chunk}\n\n" 透传
package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client Qoder 上游 HTTP 客户端（单账号一次调用的出站会话）。
type Client struct {
	HTTP  *http.Client
	Cache *SessionCache
	Realm Realm
}

// NewClient 构造（http 传 nil 用默认带超时的客户端）。
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0} // SSE 长连接，不设总超时；靠 ctx 控制
	}
	return &Client{HTTP: httpClient, Cache: NewSessionCache()}
}

// Account 出站所需账号视图（来自 auth.Auth 的锁定快照）。
type Account struct {
	UID              string
	Nickname         string
	AccessToken      string
	RefreshToken     string
	UserType         string
	OrganizationID   string
	OrganizationName string
	Global           bool // true = intl 网关
}

// StreamResult 一次流式调用的结果。
type StreamResult struct {
	StatusCode int
	Body       io.ReadCloser // 解包后的标准 OpenAI SSE 流（调用方读完关闭）
}

// Chat 发起推理请求，返回解包后的 SSE 流（StatusCode / Body）。
// 出站 body 整体 QoderEncode；头部带完整 COSY 签名。
func (c *Client) Chat(ctx context.Context, acct Account, req *ChatRequest) (*StreamResult, error) {
	realm := "cn"
	gateway := RealmCN.Gateway
	if acct.Global {
		realm = "global"
		gateway = RealmIntl.Gateway
	}
	modelKey := req.Model
	raw, err := buildBody(req, modelKey, realm, acct.UserType)
	if err != nil {
		return nil, err
	}
	encoded := QoderEncode(raw)
	sess, err := c.Cache.Get(AccountLike{
		UID: acct.UID, Nickname: acct.Nickname, AccessToken: acct.AccessToken,
		RefreshToken: acct.RefreshToken, UserType: acct.UserType,
		OrganizationID: acct.OrganizationID, OrganizationName: acct.OrganizationName,
	})
	if err != nil {
		return nil, err
	}
	url := gateway + ChatPath
	headers := sess.Headers(encoded, url, modelKey, true)
	payload := strings.NewReader(encoded)
	reqHTTP, err := http.NewRequestWithContext(ctx, http.MethodPost, url, payload)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		reqHTTP.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(reqHTTP)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	// SSE 逐帧读 + 解信封 → 标准 OpenAI SSE 管道
	pr, pw := io.Pipe()
	go func() {
		err := unwrapSSEStream(resp.Body, pw)
		resp.Body.Close()
		pw.CloseWithError(err)
	}()
	return &StreamResult{StatusCode: resp.StatusCode, Body: pr}, nil
}

// unwrapSSEStream 读上游原始 SSE，解信封，把内层 chunk 以标准 "data: ...\n\n"
// 写入 out。读到 [DONE] 透传 "[DONE]" 结束。
func unwrapSSEStream(r io.Reader, out *io.PipeWriter) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// 上游帧形态 "data:{...}"（无空格）；也可能有空格变体
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var data []byte
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data = bytes.TrimSpace(trimmed[len("data:"):])
		} else {
			continue // event:finish 等非 data 行忽略
		}
		chunk, done, err := UnwrapFrame(data)
		if err != nil {
			return err
		}
		if done {
			_, werr := out.Write([]byte("data: [DONE]\n\n"))
			return werr
		}
		if chunk == "" {
			continue
		}
		if _, werr := out.Write([]byte("data: " + chunk + "\n\n")); werr != nil {
			return werr
		}
	}
	return scanner.Err()
}

// HTTPError 上游非 200 响应。
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("qoder upstream http %d: %s", e.StatusCode, e.Body)
}

// FetchModels 动态拉官方模型清单（失败返回静态目录）。
// 响应被信封包裹的可能性存在，先按裸 JSON 解；结构未知时优雅降级。
func (c *Client) FetchModels(ctx context.Context, acct Account) ([]ModelCatalogEntry, error) {
	realm := RealmCN
	if acct.Global {
		realm = RealmIntl
	}
	sess, err := c.Cache.Get(AccountLike{
		UID: acct.UID, AccessToken: acct.AccessToken, RefreshToken: acct.RefreshToken,
		UserType: acct.UserType, OrganizationID: acct.OrganizationID, OrganizationName: acct.OrganizationName,
	})
	if err != nil {
		return nil, err
	}
	url := realm.Gateway + ModelsPath
	reqHTTP, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range sess.Headers("", url, "", false) {
		reqHTTP.Header.Set(k, v)
	}
	client := c.HTTP
	if client.Timeout == 0 {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(reqHTTP)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var parsed struct {
		// 官方响应形态：{"chat":[...]}（实测 2026-10），models 数组在 chat 键下
		Chat []ModelCatalogEntry `json:"chat"`
		Data []ModelCatalogEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ModelsForRealm(ifGlobal(acct.Global)), nil // 优雅降级
	}
	out := make([]ModelCatalogEntry, 0, len(parsed.Chat)+len(parsed.Data))
	for _, m := range parsed.Chat {
		if m.Enable != nil && !*m.Enable {
			continue
		}
		out = append(out, m)
	}
	out = append(out, parsed.Data...)
	if len(out) == 0 {
		return ModelsForRealm(ifGlobal(acct.Global)), nil
	}
	return out, nil
}

func ifGlobal(b bool) string {
	if b {
		return "global"
	}
	return "cn"
}
