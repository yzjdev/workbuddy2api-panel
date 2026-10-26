// Qoder 数据面：上游请求体构造、SSE 信封解包、错误分类、模型目录。
//
// 上游唯一推理端点：
//
//	POST {gateway}/algo/api/v2/service/pro/sse/agent_chat_generation
//	     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
//
// 请求体 = baseprompt 骨架（官方模板）+ 会话覆写，整体 QoderEncode 后出站；
// 响应为嵌套信封 SSE：data:{"headers":...,"body":"<内层 OpenAI chunk JSON 串>","statusCodeValue":200}。
package qoder

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

//go:embed baseprompt.json
var basepromptRaw []byte

// ChatPath 推理端点（含 query）。
const ChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// ModelsPath 官方模型清单端点。
const ModelsPath = "/algo/api/v2/model/list?Encode=1"

// ThinkingConfig 模型思考档位（thinking_config）。
type ThinkingConfig struct {
	// Disabled 非空 = 支持关闭思考。
	Disabled map[string]any `json:"disabled,omitempty"`
	// Enabled 非空 = 支持开启思考；efforts 为档位表（low/medium/high/xhigh…）。
	Enabled *struct {
		IsDefault bool `json:"is_default"`
		Efforts   map[string]struct {
			IsDefault bool `json:"is_default,omitempty"`
		} `json:"efforts"`
	} `json:"enabled,omitempty"`
}

// Promotion 错峰/限时促销信息。
type Promotion struct {
	Active bool `json:"active"`
	Badge  *struct {
		Zh string `json:"zh"`
		En string `json:"en"`
	} `json:"badge,omitempty"`
	Description *struct {
		Zh string `json:"zh"`
		En string `json:"en"`
	} `json:"description,omitempty"`
	DiscountFactor float64 `json:"discount_factor"` // 促销倍率（如 0.4 = 4 折）
	WindowStart    string  `json:"window_start"`    // "22:00"
	WindowEnd      string  `json:"window_end"`      // "08:00"
}

// ModelCatalogEntry 模型元数据（/algo/api/v2/model/list 的 chat[] 条目）。
type ModelCatalogEntry struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	IsVL           bool   `json:"is_vl"`
	IsReasoning    bool   `json:"is_reasoning"`
	MaxInputTokens int    `json:"max_input_tokens"`
	// 积分倍率：上游单价系数（0 = 免费，如 Qwen3.8-Flash）。
	PriceFactor         float64         `json:"price_factor"`
	OriginalPriceFactor float64         `json:"original_price_factor"` // 促销前原倍率
	IsDefault           bool            `json:"is_default"`
	Enable              *bool           `json:"enable"`
	Thinking            *ThinkingConfig `json:"thinking_config"`
	Promotion           *Promotion      `json:"promotion"`
	// Contexts 官方 context_config 键集合（"200K"/"400K"/"1M"…，含 is_default 标记）。
	Contexts map[string]struct {
		TokenCount int  `json:"token_count"`
		IsDefault  bool `json:"is_default"`
	} `json:"context_config"`
}

// 静态模型目录（官方清单快照；动态 /model/list 失败时兜底）。
var staticModels = map[string][]ModelCatalogEntry{
	"cn": {
		{Key: "q37fmodel", DisplayName: "Qwen3.8-Flash"},
		{Key: "gm51model", DisplayName: "GLM-5.1"},
		{Key: "lite", DisplayName: "Lite"},
	},
	"global": {
		{Key: "lite", DisplayName: "Lite"},
		{Key: "efficient", DisplayName: "Efficient"},
		{Key: "performance", DisplayName: "Performance"},
		{Key: "ultimate", DisplayName: "Ultimate"},
	},
}

// ModelsForRealm 返回某区域的模型目录（静态兜底版）。
func ModelsForRealm(realm string) []ModelCatalogEntry {
	if m, ok := staticModels[realm]; ok {
		return m
	}
	return staticModels["cn"]
}

// CatalogLookup 查模型元数据；未命中返回零值（默认 max_input_tokens=180000）。
func CatalogLookup(realm, modelKey string) ModelCatalogEntry {
	for _, m := range ModelsForRealm(realm) {
		if m.Key == modelKey {
			return m
		}
	}
	return ModelCatalogEntry{MaxInputTokens: 180000}
}

// ChatRequest OpenAI chat.completions 请求中本实现消费的字段。
type ChatRequest struct {
	Model               string        `json:"model"`
	Messages            []ChatMessage `json:"messages"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	Stream              bool          `json:"stream"`
}

// ChatMessage OpenAI 消息（content 支持 string 与 parts 数组两种形态）。
type ChatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	Reasoning string          `json:"reasoning_content,omitempty"`
}

// ContentText 提取消息文本（string 原样；parts 数组拼接 text 部分）。
func (m ChatMessage) ContentText() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if t, ok := p["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// buildBody 构造 agent_chat_generation 上游请求体（返回原始 JSON 字节，
// 由调用方 QoderEncode 后出站）。
//
// baseprompt 骨架覆写规则（与逆向结论一致）：request/session/business 换新、
// messages = [system] + 扁平会话、model_config 按目录填、chat_context.text
// 高亮最新 user 提示词、parameters 透传 max_tokens/reasoning_effort。
func buildBody(req *ChatRequest, modelKey, realm, userType string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(basepromptRaw, &body); err != nil {
		return nil, fmt.Errorf("baseprompt: %w", err)
	}
	// 模板只保留 system 基座（自带示例 user 轮次丢弃），追加真实会话。
	tmplMsgs, _ := body["messages"].([]any)
	var sysMsgs []any
	for _, m := range tmplMsgs {
		mm, _ := m.(map[string]any)
		if mm != nil && mm["role"] == "system" {
			sysMsgs = append(sysMsgs, mm)
		}
	}
	// 扁平化：system 合并成一条；assistant/user 取文本。
	var systemText string
	var flat []map[string]any
	prompt := ""
	for _, m := range req.Messages {
		text := m.ContentText()
		switch m.Role {
		case "system":
			if systemText == "" {
				systemText = text
			} else if text != "" {
				systemText += "\n\n" + text
			}
		case "assistant", "user":
			if text == "" {
				continue
			}
			flat = append(flat, map[string]any{"role": m.Role, "content": text})
			if m.Role == "user" {
				prompt = text // 最新 user 提示词（倒序最后一个 = 顺序最后一个非空）
			}
		}
	}
	msgs := sysMsgs
	if systemText != "" {
		msgs = []any{map[string]any{"role": "system", "content": systemText}}
	}
	for _, m := range flat {
		msgs = append(msgs, m)
	}
	body["messages"] = msgs

	nid := newUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = newUUID()
	body["session_id"] = newUUID()
	body["stream"] = true
	body["agent_id"] = "agent_common"
	if userType == "" {
		userType = DefaultUserType
	}
	body["aliyun_user_type"] = userType

	// model_config
	meta := CatalogLookup(realm, modelKey)
	mc, _ := body["model_config"].(map[string]any)
	if mc == nil {
		mc = map[string]any{}
	}
	mc["key"] = modelKey
	if meta.DisplayName != "" {
		mc["display_name"] = meta.DisplayName
	} else {
		mc["display_name"] = modelKey
	}
	mc["model"] = ""
	mc["format"] = "openai"
	mc["is_vl"] = meta.IsVL
	mc["is_reasoning"] = meta.IsReasoning
	mc["api_key"] = ""
	mc["url"] = ""
	mc["source"] = "system"
	if meta.MaxInputTokens > 0 {
		mc["max_input_tokens"] = meta.MaxInputTokens
	} else {
		mc["max_input_tokens"] = 180000
	}
	body["model_config"] = mc

	// chat_context 高亮
	cc, _ := body["chat_context"].(map[string]any)
	if cc == nil {
		cc = map[string]any{}
	}
	txt, _ := cc["text"].(map[string]any)
	if txt == nil {
		txt = map[string]any{}
	}
	txt["text"] = prompt
	if t, ok := txt["type"].(string); !ok || t == "" {
		txt["type"] = "text"
	}
	cc["text"] = txt
	extra, _ := cc["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	oc, _ := extra["originalContent"].(map[string]any)
	if oc == nil {
		oc = map[string]any{}
	}
	oc["text"] = prompt
	if t, ok := oc["type"].(string); !ok || t == "" {
		oc["type"] = "text"
	}
	extra["originalContent"] = oc
	mcCopy := map[string]any{}
	for k, v := range mc {
		mcCopy[k] = v
	}
	extra["modelConfig"] = mcCopy
	cc["extra"] = extra
	body["chat_context"] = cc

	// parameters
	params, _ := body["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens > 0 {
		params["max_tokens"] = maxTokens
	}
	if req.ReasoningEffort != "" {
		params["reasoning_effort"] = req.ReasoningEffort
	}
	body["parameters"] = params

	// tools：第一版不做工具调用，置空（避免泄漏桌面端自带 agent 工具）。
	body["tools"] = []any{}

	// business 会话卡片
	bizName := prompt
	if bizName == "" {
		bizName = "chat"
	}
	if len([]rune(bizName)) > 30 {
		bizName = string([]rune(bizName)[:30])
	}
	biz, _ := body["business"].(map[string]any)
	if biz == nil {
		biz = map[string]any{}
	}
	biz["id"] = newUUID()
	biz["begin_at"] = time.Now().UnixMilli()
	biz["name"] = bizName
	if _, ok := biz["product"]; !ok {
		biz["product"] = "cli"
	}
	if _, ok := biz["type"]; !ok {
		biz["type"] = "agent"
	}
	if _, ok := biz["stage"]; !ok {
		biz["stage"] = "start"
	}
	body["business"] = biz

	return json.Marshal(body)
}

// ---------------------------------------------------------------------------
// SSE 信封解包
// ---------------------------------------------------------------------------

// EnvelopeFrame 上游信封帧。
type EnvelopeFrame struct {
	Headers         map[string]any `json:"headers"`
	Body            string         `json:"body"`
	StatusCodeValue int            `json:"statusCodeValue"`
}

// UnwrapFrame 解一帧信封：status 非 200 报错；[DONE] 返回结束；
// 正常返回内层 OpenAI chunk 原始 JSON 串。
// 返回 (chunkJSON, done, err)。
func UnwrapFrame(data []byte) (string, bool, error) {
	var outer EnvelopeFrame
	if err := json.Unmarshal(data, &outer); err != nil {
		return "", false, nil // 非 JSON 帧（event:finish 等）忽略
	}
	if outer.StatusCodeValue != 0 && outer.StatusCodeValue != 200 {
		return "", false, &UpstreamStatus{
			StatusCode: outer.StatusCodeValue,
			Body:       outer.Body,
		}
	}
	if outer.Body == "" {
		return "", false, nil
	}
	if outer.Body == "[DONE]" {
		return "", true, nil
	}
	return outer.Body, false, nil
}

// UpstreamStatus 信封帧携带的非 200 状态。
type UpstreamStatus struct {
	StatusCode int
	Body       string
}

func (e *UpstreamStatus) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.StatusCode, e.Body)
}

// ---------------------------------------------------------------------------
// 错误分类（pool 冷却口径）
// ---------------------------------------------------------------------------

// Classify 上游错误分类结果。
type Classify int

const (
	ClassTransient Classify = iota // 传输/5xx/418：短冷却换号重试
	ClassAuth                      // 401/403 凭证拒：长冷却 + 可能 session dead
	ClassRateLimit                 // 429：模型级冷却
	ClassFatal                     // 413/参数/审核等确定性 4xx：不轮换直接报
)

// ClassifyHTTPStatus HTTP 状态码分类（pool.NoteError 语义映射用）。
func ClassifyHTTPStatus(code int) Classify {
	switch {
	case code == 401 || code == 403:
		return ClassAuth
	case code == 429:
		return ClassRateLimit
	case code == 418 || code >= 500:
		return ClassTransient
	case code >= 400:
		return ClassFatal
	default:
		return ClassTransient
	}
}

// IsSessionDeadBody 报告响应体是否为死会话标记（TOKEN_EXPIRE/12153/offline）。
func IsSessionDeadBody(body string) bool {
	return strings.Contains(body, "TOKEN_EXPIRE") ||
		strings.Contains(body, "12153") ||
		strings.Contains(body, "Offline user session not found")
}
