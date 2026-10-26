// Qoder 积分查询与每日签到（sash/openapi 纯 Bearer 鉴权，无 COSY 签名）。
// 协议口径：quota/usage 汇总 remain/used/size；campaign 平台为签到主路径
// （桌面端头），旧 daily-check-in 为回退；replayed/ALREADY_CLAIMED 幂等归 already。
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// QuotaUsage /api/v2/quota/usage 汇总结果。
type QuotaUsage struct {
	Remain        int64   // userQuota.remaining + addOnQuota.remaining
	Used          int64
	Size          int64 // total 之和
	SharedRemain  int64 // 团队共享池可用（cap=-1 哨兵不计入）
	PlanTier      string
	IsExceeded    bool
	ExpiresAt     int64 // 顶层 expiresAt（epoch 秒；253402214400 = 永不过期占位）
}

// quotaObj quota 对象通用字段（共享池用 cap/available）。
type quotaObj struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Cap       float64 `json:"cap"`
	Available float64 `json:"available"`
}

// planTier /api/v2/user/plan 套餐名。
func planTier(ctx context.Context, client *http.Client, r Realm, token string) string {
	var out struct {
		PlanTierName string `json:"plan_tier_name"`
		UserType     string `json:"user_type"`
	}
	if err := getJSON(ctx, client, r, "/api/v2/user/plan", token, &out); err != nil {
		return ""
	}
	if out.PlanTierName != "" {
		return out.PlanTierName
	}
	return out.UserType
}

// FetchQuota 查询账号积分/额度（含共享池与套餐名）。
func FetchQuota(ctx context.Context, client *http.Client, r Realm, accessToken string) (*QuotaUsage, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("qoder: empty access token")
	}
	var raw struct {
		UserQuota    *quotaObj `json:"userQuota"`
		AddOnQuota   *quotaObj `json:"addOnQuota"`
		SharedQuota  *quotaObj `json:"sharedQuota"`
		OrgPackage   *quotaObj `json:"orgResourcePackage"`
		IsExceeded   bool      `json:"isQuotaExceeded"`
		ExpiresAt    int64     `json:"expiresAt"`
	}
	if err := getJSON(ctx, client, r, "/api/v2/quota/usage", accessToken, &raw); err != nil {
		return nil, err
	}
	sum := func(o *quotaObj, f func(*quotaObj) float64) float64 {
		if o == nil {
			return 0
		}
		return f(o)
	}
	remain := sum(raw.UserQuota, func(o *quotaObj) float64 { return o.Remaining }) +
		sum(raw.AddOnQuota, func(o *quotaObj) float64 { return o.Remaining })
	used := sum(raw.UserQuota, func(o *quotaObj) float64 { return o.Used }) +
		sum(raw.AddOnQuota, func(o *quotaObj) float64 { return o.Used })
	size := sum(raw.UserQuota, func(o *quotaObj) float64 { return o.Total }) +
		sum(raw.AddOnQuota, func(o *quotaObj) float64 { return o.Total })
	shared := raw.SharedQuota
	if shared == nil {
		shared = raw.OrgPackage
	}
	var sharedRemain int64
	if shared != nil && shared.Cap != -1 { // cap=-1 = 不设上限哨兵，不计入
		sharedRemain = int64(shared.Remaining)
	}
	// 退化体守卫：两个 quota 键都缺 → 上游形态变化，不产出空快照
	if raw.UserQuota == nil && raw.AddOnQuota == nil {
		return nil, fmt.Errorf("quota/usage: no quota objects")
	}
	q := &QuotaUsage{
		Remain: int64(remain), Used: int64(used), Size: int64(size),
		SharedRemain: sharedRemain, IsExceeded: raw.IsExceeded, ExpiresAt: raw.ExpiresAt,
	}
	q.PlanTier = planTier(ctx, client, r, accessToken)
	return q, nil
}

// getJSON Bearer GET 公共封装（普通头）。
func getJSON(ctx context.Context, client *http.Client, r Realm, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.OpenAPI+path, nil)
	if err != nil {
		return err
	}
	for k, v := range plainHeaders(r, token) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &AuthError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s http %d: %.200s", path, resp.StatusCode, string(body))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

// plainHeaders 普通鉴权头（quota/plan/checkin/claim 用）。
func plainHeaders(r Realm, token string) map[string]string {
	return map[string]string{
		"Content-Type":   "application/json",
		"Accept":         "application/json, text/plain, */*",
		"User-Agent":     r.UserAgent,
		"Authorization":  "Bearer " + token,
		"X-Machine-ID":   "", // 留空：网关场景不伪造桌面机器身份（derive 需要 uid，调用侧按需补）
		"Origin":         r.Website,
		"Referer":        r.Website + "/",
	}
}

// ---------------------------------------------------------------------------
// 签到（旧 sash 接口；campaign 活动平台需桌面机器身份头，第一版不做）
// ---------------------------------------------------------------------------

// CheckinStatus 签到状态。
type CheckinStatus struct {
	Active         bool   // status ∈ (CLAIMABLE, CLAIMED)
	TodayCheckedIn bool
	StreakDays     int64  `json:"currentStreakDays"`
	TotalDays      int64  `json:"totalClaimDays"`
	RewardCredits  int64  `json:"rewardCredits"`
	LastClaimedAt  int64  `json:"lastClaimedAt"` // epoch 秒
	NextClaimAt    int64  `json:"nextClaimAt"`
	Available      bool   // 接口存在（404 = 该区域无此接口）
	Raw            string `json:"-"` // 原始 status（CLAIMABLE/CLAIMED/DISABLED…）
}

// FetchCheckinStatus 查询签到状态。
// GET /sash/api/v1/me/daily-check-in/status
func FetchCheckinStatus(ctx context.Context, client *http.Client, r Realm, accessToken string) (*CheckinStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.OpenAPI+"/sash/api/v1/me/daily-check-in/status", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range plainHeaders(r, accessToken) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 410 {
		// 该区域无此接口（intl 实测 404）→ 可用=false，非错误
		return &CheckinStatus{Available: false}, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("checkin status http %d: %.200s", resp.StatusCode, string(body))
	}
	var raw struct {
		Status          string `json:"status"`
		LastClaimedAt   int64  `json:"lastClaimedAt"`
		CurrentStreak   int64  `json:"currentStreakDays"`
		TotalClaimDays  int64  `json:"totalClaimDays"`
		RewardCredits   int64  `json:"rewardCredits"`
		NextClaimAt     int64  `json:"nextClaimAt"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	st := &CheckinStatus{
		Active: raw.Status == "CLAIMABLE" || raw.Status == "CLAIMED",
		StreakDays: raw.CurrentStreak, TotalDays: raw.TotalClaimDays,
		RewardCredits: raw.RewardCredits, LastClaimedAt: raw.LastClaimedAt,
		NextClaimAt: raw.NextClaimAt, Available: true, Raw: raw.Status,
	}
	if raw.Status == "CLAIMED" && raw.LastClaimedAt > 0 {
		st.TodayCheckedIn = sameLocalDay(raw.LastClaimedAt, time.Now())
	}
	return st, nil
}

// sameLocalDay 判断 epoch 秒与本地时间是否同一天。
func sameLocalDay(epochSec int64, now time.Time) bool {
	t := time.Unix(epochSec, 0)
	y1, m1, d1 := t.Local().Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// CheckinResult 签到执行结果。
type CheckinResult struct {
	OK          bool
	Already     bool // 今日已签（幂等归一）
	Reward      int64
	Message     string
}

// DailyCheckin 执行每日签到。
// POST /sash/api/v1/me/daily-check-in/claim，body {}。
// 幂等：409+ALREADY_CLAIMED / result==ALREADY_CLAIMED / success=false 且 error 含 ALREADY。
func DailyCheckin(ctx context.Context, client *http.Client, r Realm, accessToken string) (*CheckinResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.OpenAPI+"/sash/api/v1/me/daily-check-in/claim", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	for k, v := range plainHeaders(r, accessToken) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	bodyStr := string(body)
	// 幂等归一：409 或响应含 ALREADY 标记 → 已签
	if resp.StatusCode == 409 || strings.Contains(bodyStr, "ALREADY_CLAIMED") {
		return &CheckinResult{OK: true, Already: true, Message: "今日已签到"}, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("checkin claim http %d: %.200s", resp.StatusCode, bodyStr)
	}
	var out struct {
		Success      bool   `json:"success"`
		RewardCredits int64 `json:"rewardCredits"`
		Result       string `json:"result"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Result == "ALREADY_CLAIMED" || (out.Success == false && strings.Contains(strings.ToUpper(out.Error), "ALREADY")) {
		return &CheckinResult{OK: true, Already: true, Message: "今日已签到"}, nil
	}
	if out.Success == false {
		// 复查确认：status 显示今日已签 → 归已签（上游状态不一致的兜底）
		if st, err := FetchCheckinStatus(ctx, client, r, accessToken); err == nil && st.TodayCheckedIn {
			return &CheckinResult{OK: true, Already: true, Message: "今日已签到（状态复查确认）"}, nil
		}
		msg := out.Error
		if msg == "" {
			msg = "签到失败（success=false）"
		}
		return &CheckinResult{OK: false, Message: msg}, nil
	}
	return &CheckinResult{OK: true, Reward: out.RewardCredits, Message: fmt.Sprintf("签到成功，获得 %d 积分", out.RewardCredits)}, nil
}
