// OpenAI → SOLO llm_utils_chat 请求体改写。
//
// 逐条对齐参考实现 `internal/upstream/payload.go` 的 PrepareBody：
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function:"solo_work_lite", stream:true,
//	         config_name:<model>, model:<model>}
package trae

import (
	"encoding/json"
	"strings"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
//
// 改写规则：
//  1. messages: content 字符串 → [{"type":"text","text":...}]；已是数组 → 透传
//  2. stream: 强制 true（非流式由服务端聚合）
//  3. model → config_name + model（**不剥后缀**，参考实现直接透传）
//  4. function: 固定 solo_work_lite
//  5. tools/tool_choice: 归一化（"none" 删 tools；auto/required 保留；function 提取 name）
//  6. assistant 的 tool_calls: function → function_call，无 name 的整条剔除
//  7. tools[].function.parameters 对象 → JSON 字符串（上游 struct 是 string）
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = Function

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)

			// assistant 消息回传 tool_calls: OpenAI function → 上游 function_call
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						// 上游要求 FunctionCall.Name 必填：无 name 的 tool_call 剔除
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}

			if !present || content == nil {
				continue // 无 content 的消息（如纯 tool_calls assistant）跳过 content 改写
			}
			if s, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
			// 已是数组 → 透传（兼容多模态）
		}
	}

	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultConfigName
	}
	obj["config_name"] = model
	obj["model"] = model

	normalizeToolChoice(obj)
	normalizeTools(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//
//   - "none" / {"type":"none"} → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 把 OpenAI tools 转为 SOLO 上游格式。
//
// 上游 FunctionDefinition.tools[].function.parameters 是 **string** 类型
// （OpenAI 标准是 object）→ 需把 parameters 序列化为 JSON 字符串；
// 条目不是 map 或缺 function 时整体剔除（避免上游反序列化失败）。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
