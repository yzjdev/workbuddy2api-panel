package qoder

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUnwrapFrameOk 正常帧解包。
func TestUnwrapFrameOk(t *testing.T) {
	inner := `{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`
	frame, _ := json.Marshal(EnvelopeFrame{Body: inner, StatusCodeValue: 200})
	chunk, done, err := UnwrapFrame(frame)
	if err != nil || done {
		t.Fatalf("err=%v done=%v", err, done)
	}
	if chunk != inner {
		t.Fatalf("chunk = %s", chunk)
	}
}

// TestUnwrapFrameDone [DONE] 结束帧。
func TestUnwrapFrameDone(t *testing.T) {
	frame, _ := json.Marshal(EnvelopeFrame{Body: "[DONE]"})
	_, done, err := UnwrapFrame(frame)
	if err != nil || !done {
		t.Fatalf("err=%v done=%v", err, done)
	}
}

// TestUnwrapFrameStatus 非 200 信封 → UpstreamStatus。
func TestUnwrapFrameStatus(t *testing.T) {
	frame, _ := json.Marshal(EnvelopeFrame{Body: "quota exceeded", StatusCodeValue: 403})
	_, _, err := UnwrapFrame(frame)
	if err == nil {
		t.Fatal("want error")
	}
	us, ok := err.(*UpstreamStatus)
	if !ok || us.StatusCode != 403 {
		t.Fatalf("bad error: %v", err)
	}
	// 分类：403 → Auth
	if ClassifyHTTPStatus(403) != ClassAuth {
		t.Fatal("403 must classify as auth")
	}
}

// TestUnwrapFrameIgnore event:finish 等非 JSON 帧静默忽略。
func TestUnwrapFrameIgnore(t *testing.T) {
	chunk, done, err := UnwrapFrame([]byte("not json"))
	if err != nil || done || chunk != "" {
		t.Fatalf("non-json frame must be ignored: %q %v %v", chunk, done, err)
	}
}

// TestBuildBodyShape 请求体骨架覆写规则。
func TestBuildBodyShape(t *testing.T) {
	req := &ChatRequest{
		Model:    "Qwen3.8-Flash",
		Messages: []ChatMessage{
			{Role: "system", Content: jsonRaw(`"be nice"`)},
			{Role: "user", Content: jsonRaw(`"hello"`)},
			{Role: "assistant", Content: jsonRaw(`"hi there"`)},
			{Role: "user", Content: jsonRaw(`"final question"`)},
		},
		MaxTokens:      4096,
		ReasoningEffort: "high",
	}
	raw, err := buildBody(req, "q37fmodel", "cn", "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	// 模板键保持
	for _, k := range []string{"chat_task", "is_reply", "session_type", "source", "version"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("template key %s lost", k)
		}
	}
	// 覆写键
	if body["stream"] != true || body["agent_id"] != "agent_common" {
		t.Fatal("stream/agent_id not set")
	}
	if body["aliyun_user_type"] != DefaultUserType {
		t.Fatalf("user_type = %v", body["aliyun_user_type"])
	}
	// messages：system + 3 条会话（示例 user 轮次已丢弃）
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be nice" {
		t.Fatalf("system msg = %v", m0)
	}
	// model_config
	mc, _ := body["model_config"].(map[string]any)
	if mc["key"] != "q37fmodel" || mc["source"] != "system" {
		t.Fatalf("model_config = %v", mc)
	}
	// chat_context 高亮 = 最新 user 文本
	cc, _ := body["chat_context"].(map[string]any)
	txt, _ := cc["text"].(map[string]any)
	if txt["text"] != "final question" {
		t.Fatalf("prompt highlight = %v", txt["text"])
	}
	// parameters
	params, _ := body["parameters"].(map[string]any)
	if params["max_tokens"] != float64(4096) || params["reasoning_effort"] != "high" {
		t.Fatalf("parameters = %v", params)
	}
	// tools 置空
	if tools, ok := body["tools"].([]any); !ok || len(tools) != 0 {
		t.Fatalf("tools = %v", body["tools"])
	}
	// business
	biz, _ := body["business"].(map[string]any)
	if biz["name"] != "final question" || biz["product"] != "cli" {
		t.Fatalf("business = %v", biz)
	}
	// id 字段为合法 uuid 形态
	for _, k := range []string{"request_id", "session_id", "request_set_id"} {
		s, _ := body[k].(string)
		if len(s) != 36 || strings.Count(s, "-") != 4 {
			t.Fatalf("%s = %q not uuid", k, s)
		}
	}
}

// TestBuildBodyPartsContent 多模态 parts 数组的文本提取。
func TestBuildBodyPartsContent(t *testing.T) {
	req := &ChatRequest{
		Model: "lite",
		Messages: []ChatMessage{
			{Role: "user", Content: jsonRaw(`[{"type":"text","text":"part1"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}},{"type":"text","text":"part2"}]`)},
		},
	}
	raw, err := buildBody(req, "lite", "cn", "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	cc, _ := body["chat_context"].(map[string]any)
	txt, _ := cc["text"].(map[string]any)
	if txt["text"] != "part1part2" {
		t.Fatalf("parts text = %v", txt["text"])
	}
}

// TestChatRequestDecode 标准 OpenAI 请求可解码。
func TestChatRequestDecode(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"Qwen3.8-Flash",
		"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"text","text":"x"}]}],
		"max_tokens":100,"stream":true
	}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].ContentText() != "hi" || req.Messages[1].ContentText() != "x" {
		t.Fatal("content extraction failed")
	}
}

// TestIsSessionDeadBody 死会话标记。
func TestIsSessionDeadBody(t *testing.T) {
	if !IsSessionDeadBody(`{"code":"TOKEN_EXPIRE"}`) || !IsSessionDeadBody("err 12153") {
		t.Fatal("dead markers not detected")
	}
	if IsSessionDeadBody(`{"code":"OK"}`) {
		t.Fatal("false positive")
	}
}

func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }
