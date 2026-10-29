// TRAE 出站 chat 分支：realm=trae 的模型名进入。
//
// 与 relayChat / qoderChat 同构的轮换骨架，差异在出站协议：
// OpenAI 入站 → SOLO 请求体 → SOLO 事件流 → 标准 OpenAI SSE（转换在 trae 包内完成）。
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/trae"
)

// traeChat TRAE 直通 chat：trae: 前缀点名进入。
// 返回最终 HTTP 状态码（调用方记入 st.status）。
func (h *Handler) traeChat(w http.ResponseWriter, r *http.Request, body []byte, tried map[string]bool, st *chatStat, bareModel string) int {
	if h.cfg.Trae == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "trae_disabled",
			"TRAE 上游未启用（无 trae realm 账号或客户端未装配）")
		return http.StatusServiceUnavailable
	}
	if bareModel == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "missing model")
		return http.StatusBadRequest
	}
	// 入站模型名可能是展示名（GLM-5.2）或 config_name（glm-5.2）；上游只认后者。
	// 反解在轮换循环内按账号进行（目录可能按账号不同），故这里只做一次格式校验。
	if _, ok := replaceJSONModel(body, bareModel); !ok {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 chat.completions JSON")
		return http.StatusBadRequest
	}

	lastKind := traepkg.KindNone
	lastStatus := http.StatusServiceUnavailable
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickUpstream(tried, "trae")
		if acct == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "没有可用 TRAE 账号")
			return http.StatusServiceUnavailable
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue // 并发抢名额失败 → 换号
		}
		st.uid, st.nick = acct.UID, acct.Nickname

		// 模型级冷却的键必须与 /v1/models 的 id 后缀一致（= config_name），
		// 故出站与记账都用反解后的 key，而不是用户原样输入的展示名。
		modelKey := h.resolveTraeModel(acct, bareModel)
		// TRAE 上游不回单次积分，倍率是唯一可展示的成本口径：记进流水行与归档。
		if st.rateLabel == "" {
			st.rateLabel = h.traeModelRateLabel(acct, modelKey)
		}
		outBody, ok := replaceJSONModel(body, modelKey)
		if !ok {
			h.cfg.Pool.Release(acct.UID)
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 chat.completions JSON")
			return http.StatusBadRequest
		}

		sseBody, err := h.cfg.Trae.ChatStream(traeAccountOf(acct), outBody)
		if err != nil {
			h.cfg.Pool.Release(acct.UID)
			lastKind, lastStatus = h.noteTraeError(acct, err, modelKey)
			if !traepkg.ShouldRotate(lastKind) || !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}

		// 上游 200：先写 SSE 头，再边翻译边刷出。
		st.status = http.StatusOK
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		var streamErr *traepkg.StreamError
		streamErrCh := make(chan *traepkg.StreamError, 1)
		go func() { streamErrCh <- nil }() // 占位，保证下面读取不阻塞
		<-streamErrCh

		streamErr = nil
		onErr := func(se *traepkg.StreamError) { streamErr = se }
		streamWriteErr := traepkg.Stream(w, sseBody, onErr)
		_ = sseBody.Close()
		h.cfg.Pool.Release(acct.UID)

		if streamWriteErr != nil {
			// 客户端断开或写失败：不算上游错误，直接结束。
			log.Printf("WARN: [server] trae stream write acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), streamWriteErr)
			st.outcome = reqlog.OutcomeHTTPError
			return st.status
		}
		if streamErr != nil {
			log.Printf("WARN: [server] trae stream acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), streamErr)
			if streamErr.Rotatable() {
				// 流内业务错误（1005/4008/4011）同样是**模型级**语义：
				// 只锁该模型，账号在其它模型上仍可用。
				h.noteTraeModelLimit(acct, modelKey, "trae stream "+streamErr.Error())
				st.outcome = reqlog.OutcomeHTTPError
				return http.StatusTooManyRequests
			}
			h.cfg.Pool.NoteError(acct.UID)
			st.outcome = reqlog.OutcomeHTTPError
			return st.status
		}
		st.outcome = reqlog.OutcomeSuccess
		return st.status
	}
	code := "no_healthy_account"
	if lastKind == traepkg.KindQuotaExceeded || lastKind == traepkg.KindHardPlan {
		code = "quota_exceeded"
	}
	writeOpenAIError(w, lastStatus, code, "TRAE 账号均不可用")
	return lastStatus
}

// traeModelEntry /v1/models 的 TRAE 条目（与 cn/global/qoder 条目同键名口径）。
func traeModelEntry(m traepkg.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":                 "trae:" + m.ID,
		"object":             "model",
		"created":            1753600000,
		"owned_by":           "trae",
		"name":               m.Name,
		"supports_images":    m.Multimodal,
		"supports_reasoning": len(m.ReasoningEfforts) > 0,
	}
	// 上下文档位：常规档作 context_length；Max 模式档单独透出（两者绝不混用）。
	if m.ContextWindow > 0 {
		entry["context_length"] = m.ContextWindow
		entry["max_allowed_size"] = m.ContextWindow
	}
	if m.MaxMode && m.MaxContextWindow > 0 {
		entry["max_mode"] = true
		entry["max_context_length"] = m.MaxContextWindow
	}
	if m.MaxTokens > 0 {
		entry["max_output_tokens"] = m.MaxTokens
	}
	if m.MaxModeOutputTokens > 0 {
		entry["max_mode_output_tokens"] = m.MaxModeOutputTokens
	}
	// 思考档位（与 qoder 条目同键名，客户端零适配）。
	if len(m.ReasoningEfforts) > 0 {
		entry["reasoning_supported_efforts"] = m.ReasoningEfforts
		entry["supported_efforts"] = m.ReasoningEfforts
		if m.DefaultEffort != "" {
			entry["reasoning_default_effort"] = m.DefaultEffort
			entry["default_effort"] = m.DefaultEffort
		}
	}
	// 倍率（qoder 条目是 "x0.08" 前缀口径；0 是合法值 = 免费）。
	if m.HasCredits {
		entry["credits"] = "x" + formatFactor(m.CreditsRate)
		if m.OriginalCreditsRate > 0 && m.OriginalCreditsRate > m.CreditsRate {
			entry["promo_factor"] = m.CreditsRate
			entry["promo_credits"] = "x" + formatFactor(m.CreditsRate)
			entry["promo_label"] = "限时折扣"
		}
	}
	return entry
}

// pickUpstream TRAE 域的选号：按 realm 谓词过滤 + 跳过已试。
func (h *Handler) pickUpstream(tried map[string]bool, realm string) *auth.Auth {
	for _, s := range h.cfg.Pool.List() {
		if s.Disabled || tried[s.UID] {
			continue
		}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil || a.Realm() != realm {
			continue
		}
		return a
	}
	return h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
}

// noteTraeError 按 TRAE 错误语义罚号并返回分类。
func (h *Handler) noteTraeError(acct *auth.Auth, err error, model string) (traepkg.Kind, int) {
	if he, ok := err.(*traepkg.HTTPError); ok {
		kind := he.Kind
		switch {
		case traepkg.IsTerminal(kind) || traepkg.SessionDeadBody(he.Body):
			// 会话失效：只能重新登录。
			h.cfg.Pool.Disable(acct.UID, "trae session dead")
		case kind == traepkg.KindQuotaExceeded:
			// 4008 = 账号级积分耗尽（TRAE 的 ide_credits 是账号维度，不分模型），
			// 故冷却整个账号到次日 04:00，与 workbuddy 的「余额不足」同口径。
			h.cfg.Pool.CooldownUntilTomorrow4AM(acct.UID, "trae 积分耗尽")
		case traepkg.RecordsRateLimit(kind):
			// 429 限流 / 1005 权益不足 / 404 模型不存在 —— 都是**模型级**语义：
			// 只锁该模型，账号在其它模型上仍可用（切模型豁免）。
			h.noteTraeModelLimit(acct, model, "trae "+kind.String())
		default:
			h.cfg.Pool.NoteError(acct.UID)
		}
		log.Printf("WARN: [server] trae upstream acct=%s model=%s: %v",
			logfmt.Label(acct.UID, acct.Nickname), model, err)
		return kind, traeStatusOf(kind)
	}
	log.Printf("WARN: [server] trae transport acct=%s model=%s: %v",
		logfmt.Label(acct.UID, acct.Nickname), model, err)
	h.cfg.Pool.NoteFailures(acct.UID)
	return traepkg.KindServer, http.StatusBadGateway
}

// noteTraeModelLimit 记一次**模型级**冷却：只锁该模型，账号在其它模型上仍可用。
//
// ⚠️ 必须传非零 resetAt：`CooldownSoftForModel` 的语义分两支 —— resetAt 非零时
// 只写 modelCooldowns[model]（正是「切模型豁免」）；resetAt 为零时会退化成
// **账号级** until，把一个模型的失败扩散到整个账号。故这里显式传 now+d。
//
// 该记录同时是面板「模型与档位」页可用性标签的证据来源：没有它，TRAE 模型
// 只会永远显示「未验证」（无锁 + 无近期请求）。
func (h *Handler) noteTraeModelLimit(acct *auth.Auth, model, reason string) {
	if model == "" {
		return
	}
	d := h.softCooldown()
	if d <= 0 {
		d = 60 * time.Second
	}
	h.cfg.Pool.CooldownSoftForModel(acct.UID, d, time.Now().Add(d), model, reason)
}

// traeStatusOf 错误类别 → 回给客户端的 HTTP 状态码。
func traeStatusOf(kind traepkg.Kind) int {
	switch kind {
	case traepkg.KindSessionDead:
		return http.StatusUnauthorized
	case traepkg.KindQuotaExceeded, traepkg.KindSoftRate, traepkg.KindHardPlan:
		return http.StatusTooManyRequests
	case traepkg.KindNotFound:
		return http.StatusNotFound
	case traepkg.KindServer:
		return http.StatusBadGateway
	case traepkg.KindClient:
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

// replaceJSONModel 返回把请求体 model 字段改写为上游裸名的副本。
//
// 入站模型名可能是「trae:GLM-5.2」这种带前缀形态，上游只认 config_name；
// 只改 model 一个字段，其余原样保留（PrepareBody 会在 trae 包里做进一步改写）。
func replaceJSONModel(body []byte, model string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, false
	}
	payload["model"] = model
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}

// formatFactor 倍率 → 最短数字文案（0.5 → "0.5"、1 → "1"、0.08 → "0.08"）。
func formatFactor(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// traeAccountOf auth.Auth → trae 出站账号视图（只读快照，取值方法内部加锁）。
func traeAccountOf(a *auth.Auth) traepkg.Account {
	return traepkg.Account{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		MachineID:    a.MachineID,
		DeviceID:     a.DeviceID,
		ExpiresAt:    a.ExpiresAt,
	}
}

// traeModelsCache /v1/models 的 TRAE 段缓存：10min 正缓存 + 失败冷却。
var traeModelsCache struct {
	sync.RWMutex
	entries map[string]traeModelsEntry
}

type traeModelsEntry struct {
	models   []traepkg.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// fetchTraeModels 拉某 TRAE 账号的模型目录（缓存优先；失败回落兜底目录）。
func (h *Handler) fetchTraeModels(a *auth.Auth) []traepkg.ModelInfo {
	uid := a.UID
	traeModelsCache.Lock()
	if traeModelsCache.entries == nil {
		traeModelsCache.entries = make(map[string]traeModelsEntry)
	}
	e, ok := traeModelsCache.entries[uid]
	traeModelsCache.Unlock()
	if ok && len(e.models) > 0 && time.Since(e.fetched) < qoderModelsTTL {
		return e.models
	}
	if ok && len(e.models) == 0 && time.Since(e.lastFail) < modelsFetchFailCooldown {
		return traepkg.FallbackModels()
	}
	if h.cfg.Trae == nil {
		return traepkg.FallbackModels()
	}
	models, err := h.cfg.Trae.FetchModels(traeAccountOf(a))
	now := time.Now()
	traeModelsCache.Lock()
	if err != nil || len(models) == 0 {
		traeModelsCache.entries[uid] = traeModelsEntry{lastFail: now}
		log.Printf("WARN: [server] /v1/models: TRAE 模型目录拉取失败（%s 内不再重试，回落兜底目录）: %v",
			modelsFetchFailCooldown, err)
	} else {
		traeModelsCache.entries[uid] = traeModelsEntry{models: models, fetched: now}
	}
	traeModelsCache.Unlock()
	if err != nil || len(models) == 0 {
		return traepkg.FallbackModels()
	}
	return models
}

// traeModelRateLabel 取该模型的计费倍率文案（"x0.08"）。
//
// 上游不回单次积分（ide_user_ent_usage 只有余额），所以成本口径只能用倍率标注。
// 未知或该模型没取到倍率时返回空 —— 宁可空着，也不编造一个 0。
func (h *Handler) traeModelRateLabel(a *auth.Auth, modelKey string) string {
	for _, m := range h.fetchTraeModels(a) {
		if m.ID == modelKey && m.HasCredits {
			return "x" + formatFactor(m.CreditsRate)
		}
	}
	return ""
}

// resolveTraeModel 对外模型名 → 上游 config_name（命中目录则用其 ID，否则原样直传）。
func (h *Handler) resolveTraeModel(a *auth.Auth, external string) string {
	for _, m := range h.fetchTraeModels(a) {
		if strings.EqualFold(m.Name, external) || m.ID == external {
			return m.ID
		}
	}
	return external
}

var _ = context.Background
