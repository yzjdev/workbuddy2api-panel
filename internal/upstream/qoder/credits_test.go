package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchQuota 汇总口径：userQuota + addOnQuota；共享池 cap=-1 不计入。
func TestFetchQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"userQuota":   map[string]any{"total": 1000, "used": 300, "remaining": 700},
				"addOnQuota":  map[string]any{"total": 500, "used": 100, "remaining": 400},
				"sharedQuota": map[string]any{"cap": -1, "remaining": 9999},
				"isQuotaExceeded": false,
				"expiresAt": 253402214400,
			})
		case "/api/v2/user/plan":
			_ = json.NewEncoder(w).Encode(map[string]any{"plan_tier_name": "Pro Trial"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	q, err := FetchQuota(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if q.Remain != 1100 || q.Used != 400 || q.Size != 1500 {
		t.Fatalf("bad sum: %+v", q)
	}
	if q.SharedRemain != 0 {
		t.Fatalf("cap=-1 sentinel must not count: %d", q.SharedRemain)
	}
	if q.PlanTier != "Pro Trial" || q.ExpiresAt != 253402214400 {
		t.Fatalf("plan/expiry: %+v", q)
	}
}

// TestFetchCheckinStatus 状态解析与 404=不可用语义。
func TestFetchCheckinStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sash/api/v1/me/daily-check-in/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "CLAIMED", "lastClaimedAt": timeNowUnix(),
				"currentStreakDays": 5, "totalClaimDays": 30, "rewardCredits": 100,
			})
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	st, err := FetchCheckinStatus(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || !st.Active || !st.TodayCheckedIn || st.StreakDays != 5 {
		t.Fatalf("bad status: %+v", st)
	}
}

// TestFetchCheckinStatus404 intl 无此接口 → Available=false 非错误。
func TestFetchCheckinStatus404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	st, err := FetchCheckinStatus(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if st.Available {
		t.Fatal("404 must map to Available=false")
	}
}

// TestDailyCheckinAlready 幂等：409 + ALREADY_CLAIMED → already。
func TestDailyCheckinAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"ALREADY_CLAIMED"}`))
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	res, err := DailyCheckin(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Already {
		t.Fatalf("bad result: %+v", res)
	}
}

// TestDailyCheckinOk 正常签到取 rewardCredits。
func TestDailyCheckinOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "rewardCredits": 120})
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	res, err := DailyCheckin(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Already || res.Reward != 120 {
		t.Fatalf("bad result: %+v", res)
	}
}

// timeNowUnix 返回今天本地零点附近的 epoch 秒（保证 sameLocalDay 命中今天）。
func timeNowUnix() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).Unix()
}
