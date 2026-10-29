// get_detail_param 条目解析：上下文档位、思考档位、计费倍率、能力标记。
//
// 字段口径来自参考实现（dsh-our-free-model 的 `trae.ts` parseTraeConfigEntry 一族，
// 与 `trae2api-web` 的 get_detail_param 同一响应形状）：
//
//	entry.config_name                     模型 id（= 出站 config_name）
//	entry.display_config.display_name     展示名
//	entry.display_config.multimodal       是否吃图
//	entry.display_config.max_mode         是否支持 Max 模式（1M）
//	entry.context_window_tokens.{dev,max} 上下文窗口：dev=常规档、max=Max 模式档
//	entry.model_detail_list[].max_tokens  输出上限（按 model_name 后缀 __dev/__max 区分）
//	entry.reasoning_effort_config         思考档位 {options, default_level, support_thinking}
//	entry.display_contact_config          **JSON 字符串**，内含 consumption_rate / activity_discount
package trae

import (
	"encoding/json"
	"math"
	"strings"
)

// ModelInfo 动态模型信息（含档位与倍率）。
type ModelInfo struct {
	ID   string
	Name string

	// ContextWindow 常规上下文档（context_window_tokens.dev）。
	ContextWindow int64
	// MaxContextWindow Max 模式档（context_window_tokens.max，通常 1M）。
	MaxContextWindow int64
	// MaxMode 上游是否声明可走 Max 模式。
	MaxMode bool

	// MaxTokens 常规模式输出上限（model_detail_list 里 __dev 那条）。
	MaxTokens int64
	// MaxModeOutputTokens Max 模式输出上限（__max 那条）。
	MaxModeOutputTokens int64

	// Multimodal 是否接受图片输入（display_config.multimodal）。
	Multimodal bool

	// ReasoningEfforts 思考档位（空 = 该模型不提供推理等级）。
	ReasoningEfforts []string
	// DefaultEffort 默认思考档位。
	DefaultEffort string

	// CreditsRate 消耗倍率（0 是**合法值** = 免费）。
	CreditsRate float64
	// HasCredits 是否真的取到倍率 —— 用于区分「免费」与「没取到」。
	HasCredits bool
	// OriginalCreditsRate 活动折扣前倍率（仅当折扣**当前确实生效**时非零）。
	OriginalCreditsRate float64
	// PromoEndsAt 折扣截止时刻（Unix 秒，0 = 未知/不限时）。
	PromoEndsAt int64
}

// parseConfigEntry 解析单条 config_info_list 条目。
func parseConfigEntry(entry map[string]any, nowSec int64) (ModelInfo, bool) {
	id := readString(entry, "config_name", "ConfigName")
	if id == "" {
		return ModelInfo{}, false
	}
	display := readObject(entry, "display_config", "DisplayConfig")

	name := id
	if display != nil {
		if v := readString(display, "display_name", "DisplayName"); v != "" {
			name = v
		}
	}

	m := ModelInfo{ID: id, Name: name}
	m.ContextWindow, m.MaxContextWindow = readContextWindowTiers(entry)
	m.MaxTokens = readDetailMaxTokens(entry, "__dev")
	m.MaxModeOutputTokens = readDetailMaxTokens(entry, "__max")

	if display != nil {
		m.MaxMode = readBool(display, "max_mode", "MaxMode")
		m.Multimodal = readBool(display, "multimodal", "Multimodal")
	}
	// Max 档只在远端声明 max_mode 时才算数：没声明却把 1M 透出，会让客户端
	// 按 1M 塞上下文、实际请求按常规档被拒。
	if m.MaxMode {
		if m.MaxContextWindow == 0 {
			m.MaxContextWindow = m.ContextWindow
		}
	} else {
		m.MaxContextWindow = 0
	}
	// dev 档缺失、但模型确实支持 Max 模式时，才用 max 档兜底显示常规窗口
	// （反向则不行：不能拿未声明的 max 虚报 1M）。
	if m.ContextWindow == 0 && m.MaxMode && m.MaxContextWindow > 0 {
		m.ContextWindow = m.MaxContextWindow
	}
	// 没标 __dev 明细时退回第一条明细的输出上限。
	if m.MaxTokens == 0 {
		m.MaxTokens = readDetailMaxTokens(entry, "")
	}

	m.ReasoningEfforts, m.DefaultEffort = readReasoningConfig(entry)
	m.CreditsRate, m.HasCredits = readConsumptionRate(entry)
	m.OriginalCreditsRate, m.PromoEndsAt = readActivityDiscount(entry, nowSec)
	return m, true
}

// readContextWindowTiers 读上下文的两个档：dev（常规）与 max（Max 模式）。
//
// ⚠️ 两者**绝不能混用**：把 max 当常规窗口声明，客户端会以为有 1M 上下文，
// 而实际请求未开 Max 模式时上游按常规档校验。故这里只做**取值**，是否回退由
// parseConfigEntry 按 max_mode 决定。
//
// 三种形态都认（实测主形态是第一种，后两种是防御）：
//  1. 对象 `{dev, max}`；
//  2. 裸数字/字符串 —— 直接就是常规窗口；
//  3. 条目级缺失时，回退到 `model_detail_list[].context_window_tokens`。
func readContextWindowTiers(entry map[string]any) (dev, max int64) {
	if tiers := readObject(entry, "context_window_tokens", "ContextWindowTokens"); tiers != nil {
		dev = int64(readNumber(tiers, "dev", "Dev"))
		max = int64(readNumber(tiers, "max", "Max"))
		return dev, max
	}
	// 裸数字形态：字段本身就是常规窗口
	if v, ok := lookupNumber(entry, "context_window_tokens", "ContextWindowTokens",
		"max_input_tokens", "context_length"); ok && v > 0 {
		return int64(v), 0
	}
	// 明细级兜底：部分条目把窗口挂在 model_detail_list 上
	for _, d := range readArray(entry, "model_detail_list", "ModelDetailList") {
		detail, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if tiers := readObject(detail, "context_window_tokens", "ContextWindowTokens"); tiers != nil {
			if v := int64(readNumber(tiers, "dev", "Dev")); v > dev {
				dev = v
			}
			if v := int64(readNumber(tiers, "max", "Max")); v > max {
				max = v
			}
			continue
		}
		if v, ok := lookupNumber(detail, "context_window_tokens", "ContextWindowTokens"); ok && int64(v) > dev {
			dev = int64(v)
		}
	}
	return dev, max
}

// readDetailMaxTokens 读 model_detail_list[].max_tokens。
//
// suffix 非空时优先取 model_name 以该后缀结尾的那条（上游约定 `__dev` / `__max`），
// 找不到再退回第一条明细。
func readDetailMaxTokens(entry map[string]any, suffix string) int64 {
	details := readArray(entry, "model_detail_list", "ModelDetailList")
	if len(details) == 0 {
		return 0
	}
	var chosen map[string]any
	if suffix != "" {
		for _, d := range details {
			item, ok := d.(map[string]any)
			if !ok {
				continue
			}
			if strings.HasSuffix(readString(item, "model_name", "ModelName", "config_name"), suffix) {
				chosen = item
				break
			}
		}
	}
	if chosen == nil {
		chosen, _ = details[0].(map[string]any)
	}
	if chosen == nil {
		return 0
	}
	return int64(readNumber(chosen, "max_tokens", "MaxTokens"))
}

// readReasoningConfig 读思考档位。
//
// 真实形状：`{ default_level, options: ["light","high","extra_high"], support_thinking }`。
// options 兼容字符串数组与对象数组（`{openclawLevel|level}`）。
// support_thinking === false 时**不声明档位**（给了选择器也发不出去）。
func readReasoningConfig(entry map[string]any) ([]string, string) {
	cfg := readObject(entry, "reasoning_effort_config", "ReasoningEffortConfig")
	if cfg == nil {
		return nil, ""
	}
	if support, ok := lookupBool(cfg, "support_thinking", "SupportThinking"); ok && !support {
		return nil, ""
	}
	options := make([]string, 0, 4)
	for _, item := range readArray(cfg, "options", "Options") {
		switch v := item.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				options = append(options, s)
			}
		case map[string]any:
			if s := readString(v, "openclawLevel", "level", "Level"); s != "" {
				options = append(options, s)
			}
		}
	}
	def := readString(cfg, "default_level", "DefaultLevel")
	if len(options) == 0 {
		return nil, ""
	}
	return options, def
}

// readConsumptionRate 读消耗倍率（display_contact_config 是 **JSON 字符串**）。
//
// 三条判据：enable !== false；data.rate 是有限非负数；**0 是合法值（免费）**。
func readConsumptionRate(entry map[string]any) (float64, bool) {
	cfg := readObjectString(entry, "display_contact_config", "DisplayContactConfig")
	if cfg == nil {
		return 0, false
	}
	rate := readObject(cfg, "consumption_rate", "ConsumptionRate")
	if rate == nil {
		return 0, false
	}
	if enabled, ok := lookupBool(rate, "enable", "Enable"); ok && !enabled {
		return 0, false
	}
	data := readObject(rate, "data", "Data")
	if data == nil {
		return 0, false
	}
	v, ok := lookupNumber(data, "rate", "Rate")
	if !ok || math.IsInf(v, 0) || math.IsNaN(v) || v < 0 {
		return 0, false
	}
	return v, true
}

// readActivityDiscount 读活动折扣，**只在当前确实生效时**返回原价。
//
// 关键陷阱：`enable: true` 不等于「正在打折」——`discount_type: "none"`（错峰型）
// 是 `before == after`，照显会变成 `x0.13→x0.13`，让用户以为有活动。
// 故要求：type 非 none；before 为正；before 严格大于 after；且未过期。
func readActivityDiscount(entry map[string]any, nowSec int64) (original float64, endsAt int64) {
	cfg := readObjectString(entry, "display_contact_config", "DisplayContactConfig")
	if cfg == nil {
		return 0, 0
	}
	discount := readObject(cfg, "activity_discount", "ActivityDiscount")
	if discount == nil {
		return 0, 0
	}
	if enabled, ok := lookupBool(discount, "enable", "Enable"); ok && !enabled {
		return 0, 0
	}
	data := readObject(discount, "data", "Data")
	if data == nil {
		return 0, 0
	}
	current := readObject(data, "current", "Current")
	if current == nil {
		return 0, 0
	}
	typ := strings.ToLower(strings.TrimSpace(readString(current, "discount_type", "discountType")))
	if typ == "" || typ == "none" {
		return 0, 0
	}
	before, okBefore := lookupNumber(current, "before_consumption_rate", "beforeConsumptionRate")
	if !okBefore || before <= 0 {
		return 0, 0
	}
	if after, ok := lookupNumber(current, "consumption_rate", "consumptionRate"); ok && before <= after {
		return 0, 0 // 没真的降价
	}
	// 截止时间：data 下任意子对象的 end_at
	for _, v := range data {
		child, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if end := int64(readNumber(child, "end_at", "endAt")); end > 0 {
			endsAt = end
			break
		}
	}
	if endsAt > 0 && endsAt <= nowSec {
		return 0, 0 // 已过期：不再按折扣价预期
	}
	return before, endsAt
}

// ---- 宽松读取辅助（兼容 snake_case / PascalCase 两种键名）----

// readString 依次尝试多个键，返回第一个非空字符串。
func readString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// readBool 读布尔值（缺省 false）。
func readBool(m map[string]any, keys ...string) bool {
	v, _ := lookupBool(m, keys...)
	return v
}

// lookupBool 读布尔值并报告是否存在。
func lookupBool(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

// readNumber 读数字（缺省 0）。
func readNumber(m map[string]any, keys ...string) float64 {
	v, _ := lookupNumber(m, keys...)
	return v
}

// lookupNumber 读数字并报告是否存在（兼容字符串形态）。
func lookupNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case string:
			var f float64
			if err := json.Unmarshal([]byte(v), &f); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// readObject 读子对象（缺省 nil）。
func readObject(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
	}
	return nil
}

// readObjectString 读「JSON 字符串形态」的子对象（display_contact_config 即为此形态）。
func readObjectString(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		s, ok := m[k].(string)
		if !ok || s == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err == nil && obj != nil {
			return obj
		}
	}
	return nil
}

// readArray 读数组（缺省 nil）。
func readArray(m map[string]any, keys ...string) []any {
	for _, k := range keys {
		if v, ok := m[k].([]any); ok {
			return v
		}
	}
	return nil
}
