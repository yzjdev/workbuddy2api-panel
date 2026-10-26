// Qoder 出站 chat 分支：realm=qoder / qoder_global 的模型名进入。
// 与 relayChat 同构的轮换骨架，出站走 COSY 签名 + SSE 信封解包。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	qoderpkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/qoder"
)

// qoderChat Qoder 直通 chat：qoder:/qoder_global: 前缀点名进入。
// 返回最终 HTTP 状态码（调用方记入 st.status）。
func (h *Handler) qoderChat(w http.ResponseWriter, r *http.Request, body []byte, tried map[string]bool, st *chatStat, bareModel, realm string) int {
	if h.cfg.Qoder == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "qoder_disabled",
			"Qoder 上游未启用（无 qoder realm 账号或客户端未装配）")
		return http.StatusServiceUnavailable
	}
	var req qoderpkg.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 chat.completions JSON")
		return http.StatusBadRequest
	}
	// 入站模型名可能是「前缀:模型」以外的形态（如站内别名）——出站用剥好前缀的裸名。
	req.Model = bareModel
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "missing model")
		return http.StatusBadRequest
	}
	// 对外 id 是展示名（Qwen3.8-Flash 等），路由层反解回上游 key（qfmodel）；
	// 未命中目录时把裸名当 key 直传（官方 key 形态如 qfmodel 也允许直接点名）。
	// 解析在轮换循环内按账号进行（不同区域目录可能不同）。

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickQoder(tried, realm)
		if acct == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "没有可用 Qoder 账号")
			return http.StatusServiceUnavailable
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue // 并发抢名额失败 → 换号
		}
		st.uid, st.nick = acct.UID, acct.Nickname

		// 每次换号都按该账号的目录反解模型 key（不同区域目录可能不同）。
		out, err := h.cfg.Qoder.Chat(r.Context(), qoderAccountOf(acct), withQoderModelKey(&req, h.resolveQoderModelKey(acct, bareModel)))
		if err != nil {
			h.noteQoderError(acct, err)
			h.cfg.Pool.Release(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		st.status = out.StatusCode
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(out.StatusCode)
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 16*1024)
		for {
			n, rerr := out.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		_ = out.Body.Close()
		h.cfg.Pool.Release(acct.UID)
		if out.StatusCode >= 400 {
			h.cfg.Pool.NoteError(acct.UID)
			st.outcome = reqlog.OutcomeHTTPError
			return out.StatusCode
		}
		st.outcome = reqlog.OutcomeSuccess
		return out.StatusCode
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "Qoder 账号均不可用")
	return http.StatusServiceUnavailable
}

// qoderModelExternalID Qoder 模型对外 id：优先官方展示名（Qwen3.8-Flash，
// 客户端可直接分辨），空则回落上游 key。
func qoderModelExternalID(m qoderpkg.ModelCatalogEntry) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Key
}

// qoderModelEntry /v1/models 的 Qoder 条目：官方目录字段 → OpenAI models
// 富字段口径（与 cn/global 条目同键名，客户端零适配）。
// 倍率/促销/思考档位/上下文映射规则与 panel.models 的 Qoder 段一致（两处口径
// 如需调整必须同步改）。
func qoderModelEntry(realm, realmName string, m qoderpkg.ModelCatalogEntry) map[string]any {
	entry := map[string]any{
		"id":                 realm + ":" + qoderModelExternalID(m),
		"object":             "model",
		"created":            1753600000,
		"owned_by":           "qoder/" + realmName,
		"name":               m.DisplayName,
		"is_default":         m.IsDefault,
		"supports_images":    m.IsVL,
		"supports_reasoning": m.IsReasoning,
	}
	// 积分倍率（workbuddy 条目的 credits 是 "x0.08" 文案口径）。
	credits := formatQoderFactor(m.PriceFactor)
	entry["credits"] = credits
	// 促销：活动折扣优先；否则现价≠原价时按官方基准限时价展示（0x = 限时免费，
	// 牌价回落 original_price_factor）。
	if m.Promotion != nil && m.Promotion.DiscountFactor > 0 {
		entry["promo_factor"] = m.Promotion.DiscountFactor
		entry["promo_credits"] = formatQoderFactor(m.Promotion.DiscountFactor)
		entry["promo_label"] = qoderPromoBadge(m.Promotion)
		if d := qoderPromoDesc(m.Promotion); d != "" {
			entry["description"] = d
		}
	} else if m.OriginalPriceFactor > 0 && m.OriginalPriceFactor != m.PriceFactor {
		entry["promo_factor"] = m.PriceFactor
		entry["promo_credits"] = credits
		if m.PriceFactor == 0 {
			entry["promo_label"] = "限时免费"
			entry["credits"] = formatQoderFactor(m.OriginalPriceFactor)
		}
	}
	// 思考档位：disabled 非空 = 可关；enabled.efforts 档位表（含默认档）。
	if m.Thinking != nil {
		if m.Thinking.Disabled != nil {
			entry["can_disable_thinking"] = true
		}
		if m.Thinking.Enabled != nil {
			effs := make([]string, 0, len(m.Thinking.Enabled.Efforts))
			def := ""
			for name, e := range m.Thinking.Enabled.Efforts {
				effs = append(effs, name)
				if e.IsDefault {
					def = name
				}
			}
			sort.Strings(effs)
			entry["reasoning_supported_efforts"] = effs
			entry["supported_efforts"] = effs
			if def != "" {
				entry["reasoning_default_effort"] = def
				entry["default_effort"] = def
			}
		}
	}
	// 上下文窗口：context_config 默认档优先，无默认取最大档。
	ctxLen := 0
	ctxDefault := 0
	for _, c := range m.Contexts {
		if c.IsDefault && c.TokenCount > ctxDefault {
			ctxDefault = c.TokenCount
		}
		if c.TokenCount > ctxLen {
			ctxLen = c.TokenCount
		}
	}
	if ctxDefault > 0 {
		entry["context_length"] = ctxDefault
		entry["max_allowed_size"] = ctxDefault
	} else if ctxLen > 0 {
		entry["context_length"] = ctxLen
		entry["max_allowed_size"] = ctxLen
	}
	return entry
}

// formatQoderFactor 倍率系数 → "x0.5" 文案（0 → "x0"，与 workbuddy 的 x 前缀口径一致）。
func formatQoderFactor(f float64) string {
	return "x" + strconv.FormatFloat(f, 'f', -1, 64)
}

// qoderPromoBadge 促销标签（badge 中文优先）。
func qoderPromoBadge(p *qoderpkg.Promotion) string {
	if p.Badge != nil {
		if p.Badge.Zh != "" {
			return p.Badge.Zh
		}
		return p.Badge.En
	}
	return "限时优惠"
}

// qoderPromoDesc 促销说明（中文优先，附时段；空返回空串不写 description）。
func qoderPromoDesc(p *qoderpkg.Promotion) string {
	if p.Description == nil {
		return ""
	}
	if p.Description.Zh != "" {
		return p.Description.Zh
	}
	return p.Description.En
}

// resolveQoderModelKey 对外模型名 → 上游 key：在账号目录里按
// displayName（大小写不敏感）或 key 匹配；未命中把原名当 key 直传
// （官方 key 形态如 qfmodel 也允许直接点名）。
func (h *Handler) resolveQoderModelKey(a *auth.Auth, external string) string {
	for _, m := range h.fetchQoderModels(a) {
		if strings.EqualFold(m.DisplayName, external) || m.Key == external {
			return m.Key
		}
	}
	return external
}

// withQoderModelKey 返回把 Model 字段改为上游 key 的请求副本（不污染调用方的 req）。
func withQoderModelKey(req *qoderpkg.ChatRequest, modelKey string) *qoderpkg.ChatRequest {
	cp := *req
	cp.Model = modelKey
	return &cp
}

// pickQoder Qoder 账号轮换选号：realm 谓词过滤（qoder / qoder_global）。
func (h *Handler) pickQoder(tried map[string]bool, realm string) *auth.Auth {
	for _, s := range h.cfg.Pool.List() {
		if s.Disabled || tried[s.UID] {
			continue
		}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			continue
		}
		ar := a.Realm()
		if ar != realm && !(realm == "qoder" && ar == "qoder") {
			continue
		}
		if !a.IsQoder() {
			continue
		}
		return a
	}
	// 兜底：List 顺序未命中时按池的 realm 选号器再试一次（model 传空跳过模型级冷却豁免）。
	return h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
}

// noteQoderError 按 Qoder 错误语义罚号：401/403 短冷却；信封非 200/429 模型级语义
// 第一版统一走账号级 NoteError（模型级冷却语义待真账号联调后细化）。
func (h *Handler) noteQoderError(acct *auth.Auth, err error) {
	if he, ok := err.(*qoderpkg.HTTPError); ok {
		switch qoderpkg.ClassifyHTTPStatus(he.StatusCode) {
		case qoderpkg.ClassAuth:
			if qoderpkg.IsSessionDeadBody(he.Body) {
				h.cfg.Pool.Disable(acct.UID, "qoder session dead")
				return
			}
			h.cfg.Pool.NoteError(acct.UID)
		default:
			h.cfg.Pool.NoteError(acct.UID)
		}
		log.Printf("WARN: [server] qoder upstream acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), err)
		return
	}
	log.Printf("WARN: [server] qoder transport acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), err)
	h.cfg.Pool.NoteFailures(acct.UID)
}

// qoderAccountOf auth.Auth → qoder 出站账号视图（只读快照，无锁竞争：
// AccessTokenValue/RefreshTokenValue 内部加锁）。
func qoderAccountOf(a *auth.Auth) qoderpkg.Account {
	return qoderpkg.Account{
		UID:              a.UID,
		Nickname:         a.Nickname,
		AccessToken:      a.AccessTokenValue(),
		RefreshToken:     a.RefreshTokenValue(),
		UserType:         a.QoderUserType(),
		OrganizationID:   a.OrganizationID,
		OrganizationName: a.OrganizationName,
		Global:           a.Realm() == "qoder_global",
	}
}
