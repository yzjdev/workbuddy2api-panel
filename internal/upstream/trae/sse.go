// SOLO 自定义 SSE 解析 → OpenAI SSE（流式转换 + 非流式聚合）。
//
// 逐条对齐参考实现 `internal/upstream/solosse.go`。
//
// SOLO 事件序列（实测）：
//
//	id:1
//	event:metadata
//	data:{"model":"","session_id":"...","prompt_completion_id":0,...}
//
//	event:output                      ← ×N，核心内容
//	data:{"response":"<正文增量>","reasoning_content":"<思考增量>","tool_calls":<null/数组>}
//
//	event:token_usage
//	data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
//
//	event:done
//	data:{"finish_reason":"stop"}
package trae

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// SOLOEvent 单条 SOLO SSE 事件（归一化）。
type SOLOEvent struct {
	Event        string          // metadata | timing_cost | output | extra_info | token_usage | done | error
	Response     string          // output: 正文增量
	Reasoning    string          // output: 思考链增量
	ToolCalls    json.RawMessage // output: 工具调用（null / 对象 / 数组）
	Usage        map[string]any  // token_usage
	FinishReason string          // done
	ErrorCode    int64           // error
	ErrorMessage string          // error
}

// StreamError 上游 SSE 流内的业务错误（event:error）。
type StreamError struct {
	Code int64
	Msg  string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

// Kind 把流内错误归类（1005 → 权益不足，其余归客户端错误）。
func (e *StreamError) Kind() Kind {
	if e.Code == 1005 {
		return KindHardPlan
	}
	return KindClient
}

// Rotatable 该流内错误是否属于「可换号」语义（1005 / 4008 / 4011）。
func (e *StreamError) Rotatable() bool {
	switch e.Code {
	case 1005, 4008, 4011:
		return true
	}
	return false
}

// ParseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// sseState 跨行累积一帧 SSE 的 event/data。
type sseState struct {
	event string
	data  strings.Builder
}

func (s *sseState) reset() {
	s.event = ""
	s.data.Reset()
}

// scanLine 处理一行；事件边界（空行）时解析并返回该事件。
func scanLine(st *sseState, line string) *SOLOEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := ParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// Aggregate 读完整 SOLO SSE，聚合正文/思考/tool_calls/usage，产出单个
// OpenAI chat.completion（非流式）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		upstreamErr  error
	)
	st := &sseState{}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				mergeToolCallJSON(toolCalls, &toolOrder, ev.ToolCalls)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &StreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallJSON 把 SOLO output.tool_calls（可能 null/对象/数组）按 index 合并。
func mergeToolCallJSON(toolCalls map[int]map[string]any, toolOrder *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeToolCallDelta 合并流式 tool_call 片段：id/type/function.name 直覆盖，
// function.arguments 拼接。上游用 `function_call` 字段，OpenAI 用 `function`，两者兼容。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		df, _ = delta["function_call"].(map[string]any) // SOLO 专属字段名
	}
	if df == nil {
		return
	}
	// 清理 SOLO 专属字段，只保留标准 OpenAI function 结构（name/arguments）
	delete(df, "namespace")
	delete(df, "partial_arguments")
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免为三行引入 sort 包）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// Stream 把 SOLO SSE 翻成 OpenAI SSE chunk 写入 w，保证至少一个 [DONE]。
//
// onErr 非 nil 时在遇到上游 event:error 时回调（供上层冷却账号/记录日志），
// 错误信息同时注入事件流。
func Stream(w io.Writer, r io.Reader, onErr func(*StreamError)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var pendingUsage map[string]any
	sawDone := false
	st := &sseState{}

	writeChunk := func(delta map[string]any, finish string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "",
			"choices": []any{choice},
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		return flushIfPossible(w)
	}
	writeDONE := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		return flushIfPossible(w)
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]any
					if err := json.Unmarshal(ev.ToolCalls, &tc); err == nil {
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]any); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							if fn, ok := call["function"].(map[string]any); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
						}
						delta["tool_calls"] = tc
					}
				}
				if len(delta) > 0 {
					if err := writeChunk(delta, ""); err != nil {
						return err
					}
				}
			case "token_usage":
				pendingUsage = ev.Usage
			case "done":
				if err := writeChunk(map[string]any{}, ev.FinishReason); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			case "error":
				se := &StreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
				if onErr != nil {
					onErr(se)
				}
				msg, _ := json.Marshal(se.Error())
				if _, err := io.WriteString(w, "event: error\ndata: "+string(msg)+"\n\n"); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			}
		}
		if err == io.EOF {
			break
		}
	}
	if !sawDone {
		// 幂等兜底：上游中断（无 done）仍写 [DONE]。
		return writeDONE()
	}
	return nil
}

// flusher 由 http.ResponseWriter 实现；w 不是 flusher 时无操作。
type flusher interface{ Flush() }

func flushIfPossible(w io.Writer) error {
	if f, ok := w.(flusher); ok {
		f.Flush()
	}
	return nil
}
