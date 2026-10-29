package trae

import (
	"encoding/json"
	"testing"
	"time"
)

// contactConfig 构造 display_contact_config 这个「JSON 字符串」字段。
func contactConfig(t *testing.T, v map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal contact config: %v", err)
	}
	return string(raw)
}

// TestParseConfigEntryTiers 校验上下文档位 / 思考档位 / 倍率的完整解析。
func TestParseConfigEntryTiers(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).Unix()
	entry := map[string]any{
		"config_name": "glm-5.2",
		"usage":       "chat_completion",
		"display_config": map[string]any{
			"display_name": "GLM-5.2",
			"multimodal":   true,
			"max_mode":     true,
		},
		// ⚠️ 档位是对象：dev = 常规档、max = Max 模式档
		"context_window_tokens": map[string]any{
			"dev": float64(200000),
			"max": float64(1000000),
		},
		// 明细按 model_name 后缀区分常规/ Max 模式
		"model_detail_list": []any{
			map[string]any{"model_name": "glm-5.2__max", "max_tokens": float64(64000)},
			map[string]any{"model_name": "glm-5.2__dev", "max_tokens": float64(32000)},
		},
		"reasoning_effort_config": map[string]any{
			"support_thinking": true,
			"default_level":    "high",
			"options":          []any{"light", "high", "extra_high"},
		},
		"display_contact_config": contactConfig(t, map[string]any{
			"consumption_rate": map[string]any{
				"enable": true,
				"data":   map[string]any{"rate": 0.08},
			},
			"activity_discount": map[string]any{
				"enable": true,
				"data": map[string]any{
					"current": map[string]any{
						"discount_type":            "limited",
						"before_consumption_rate":  0.2,
						"consumption_rate":         0.08,
						"end_at":                   float64(future),
					},
				},
			},
		}),
	}

	m, ok := parseConfigEntry(entry, time.Now().Unix())
	if !ok {
		t.Fatal("应解析成功")
	}
	if m.ID != "glm-5.2" || m.Name != "GLM-5.2" {
		t.Errorf("id/name = %q / %q", m.ID, m.Name)
	}
	if !m.Multimodal || !m.MaxMode {
		t.Errorf("能力标记不对：multimodal=%v maxMode=%v", m.Multimodal, m.MaxMode)
	}
	if m.ContextWindow != 200000 || m.MaxContextWindow != 1000000 {
		t.Errorf("上下文档位 = %d / %d，期望 200000 / 1000000", m.ContextWindow, m.MaxContextWindow)
	}
	// 输出上限按后缀取：__dev 常规、__max 为 Max 模式
	if m.MaxTokens != 32000 || m.MaxModeOutputTokens != 64000 {
		t.Errorf("输出上限 = %d / %d，期望 32000 / 64000", m.MaxTokens, m.MaxModeOutputTokens)
	}
	if len(m.ReasoningEfforts) != 3 || m.DefaultEffort != "high" {
		t.Errorf("思考档位 = %v / 默认 %q", m.ReasoningEfforts, m.DefaultEffort)
	}
	if !m.HasCredits || m.CreditsRate != 0.08 {
		t.Errorf("倍率 = %v（has=%v），期望 0.08", m.CreditsRate, m.HasCredits)
	}
	if m.OriginalCreditsRate != 0.2 || m.PromoEndsAt != future {
		t.Errorf("折扣 = %v / 截止 %d，期望 0.2 / %d", m.OriginalCreditsRate, m.PromoEndsAt, future)
	}
}

// TestParseConfigEntryFreeModel 校验 rate=0 被当作**合法免费**，而不是「没取到」。
func TestParseConfigEntryFreeModel(t *testing.T) {
	entry := map[string]any{
		"config_name": "free-model",
		"display_contact_config": contactConfig(t, map[string]any{
			"consumption_rate": map[string]any{
				"enable": true,
				"data":   map[string]any{"rate": 0},
			},
		}),
	}
	m, ok := parseConfigEntry(entry, time.Now().Unix())
	if !ok {
		t.Fatal("应解析成功")
	}
	if !m.HasCredits {
		t.Errorf("rate=0 应标记为已取到倍率（免费）")
	}
	if m.CreditsRate != 0 {
		t.Errorf("倍率 = %v，期望 0", m.CreditsRate)
	}
}

// TestParseConfigEntryThinkingDisabled 校验 support_thinking=false 时不声明档位。
func TestParseConfigEntryThinkingDisabled(t *testing.T) {
	entry := map[string]any{
		"config_name": "no-think",
		"reasoning_effort_config": map[string]any{
			"support_thinking": false,
			"options":          []any{"high"},
		},
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if len(m.ReasoningEfforts) != 0 {
		t.Errorf("support_thinking=false 时不应声明档位，得到 %v", m.ReasoningEfforts)
	}
}

// TestParseConfigEntryDiscountNone 校验 discount_type=none（错峰型）不被当成活动折扣。
func TestParseConfigEntryDiscountNone(t *testing.T) {
	entry := map[string]any{
		"config_name": "off-peak",
		"display_contact_config": contactConfig(t, map[string]any{
			"activity_discount": map[string]any{
				"enable": true,
				"data": map[string]any{
					"current": map[string]any{
						"discount_type":           "none",
						"before_consumption_rate": 0.13,
						"consumption_rate":        0.13,
					},
				},
			},
		}),
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.OriginalCreditsRate != 0 {
		t.Errorf("discount_type=none 不应展示折扣，得到 %v", m.OriginalCreditsRate)
	}
}

// TestParseConfigEntryDiscountExpired 校验过期活动不再展示。
func TestParseConfigEntryDiscountExpired(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Unix()
	entry := map[string]any{
		"config_name": "expired-promo",
		"display_contact_config": contactConfig(t, map[string]any{
			"activity_discount": map[string]any{
				"enable": true,
				"data": map[string]any{
					"current": map[string]any{
						"discount_type":           "limited",
						"before_consumption_rate": 0.2,
						"consumption_rate":        0.1,
						"end_at":                  float64(past),
					},
				},
			},
		}),
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.OriginalCreditsRate != 0 || m.PromoEndsAt != 0 {
		t.Errorf("过期活动不应展示，得到 %v / %d", m.OriginalCreditsRate, m.PromoEndsAt)
	}
}

// TestParseConfigEntryNoContactConfig 校验缺字段时不编造倍率。
func TestParseConfigEntryNoContactConfig(t *testing.T) {
	m, ok := parseConfigEntry(map[string]any{"config_name": "plain"}, time.Now().Unix())
	if !ok {
		t.Fatal("应解析成功")
	}
	if m.HasCredits || m.CreditsRate != 0 {
		t.Errorf("无 contact_config 时不应声明倍率：%v / %v", m.CreditsRate, m.HasCredits)
	}
	if m.ContextWindow != 0 || m.MaxContextWindow != 0 {
		t.Errorf("无 context_window_tokens 时窗口应为 0：%d / %d", m.ContextWindow, m.MaxContextWindow)
	}
}

// TestContextWindowNumericForm 校验 context_window_tokens 是裸数字时也能取到窗口
// （早期只认对象形态，数字形态会被丢成 0 → 面板显示「—」）。
func TestContextWindowNumericForm(t *testing.T) {
	entry := map[string]any{
		"config_name":           "numeric-ctx",
		"context_window_tokens": float64(200000),
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d，期望 200000", m.ContextWindow)
	}
	if m.MaxContextWindow != 0 {
		t.Errorf("未声明 max_mode 时不应有 Max 档，得到 %d", m.MaxContextWindow)
	}
}

// TestContextWindowFromDetails 校验条目级缺失时回退到 model_detail_list 的窗口。
func TestContextWindowFromDetails(t *testing.T) {
	entry := map[string]any{
		"config_name": "detail-ctx",
		"model_detail_list": []any{
			map[string]any{"model_name": "detail-ctx__dev", "context_window_tokens": map[string]any{"dev": float64(128000)}},
			map[string]any{"model_name": "detail-ctx__max", "context_window_tokens": map[string]any{"max": float64(1000000)}},
		},
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d，期望 128000（取明细级 dev）", m.ContextWindow)
	}
}

// TestMaxTierRequiresMaxMode 校验未声明 max_mode 时不透出 Max 档（否则虚报 1M）。
func TestMaxTierRequiresMaxMode(t *testing.T) {
	entry := map[string]any{
		"config_name": "no-max-mode",
		"context_window_tokens": map[string]any{
			"dev": float64(200000),
			"max": float64(1000000),
		},
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d，期望 200000", m.ContextWindow)
	}
	if m.MaxContextWindow != 0 {
		t.Errorf("未声明 max_mode 时 MaxContextWindow 应为 0，得到 %d", m.MaxContextWindow)
	}
}

// TestDevMissingFallsBackToMaxWithMaxMode 校验 dev 缺失但支持 Max 模式时用 max 兜底。
func TestDevMissingFallsBackToMaxWithMaxMode(t *testing.T) {
	entry := map[string]any{
		"config_name":           "only-max",
		"display_config":        map[string]any{"max_mode": true},
		"context_window_tokens": map[string]any{"max": float64(1000000)},
	}
	m, _ := parseConfigEntry(entry, time.Now().Unix())
	if m.ContextWindow != 1000000 || m.MaxContextWindow != 1000000 {
		t.Errorf("兜底不对：ContextWindow=%d MaxContextWindow=%d，期望均为 1000000",
			m.ContextWindow, m.MaxContextWindow)
	}
}
