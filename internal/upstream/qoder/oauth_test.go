package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMakePKCE verifier 长度/字符集与 challenge 形态。
func TestMakePKCE(t *testing.T) {
	ver, ch, err := MakePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(ver) != 64 {
		t.Fatalf("verifier length = %d", len(ver))
	}
	for _, c := range ver {
		if !strings.ContainsRune(pkceAlphabet, c) {
			t.Fatalf("verifier char %q outside alphabet", c)
		}
	}
	if ch != sha256UrlB64(ver) {
		t.Fatal("challenge mismatch")
	}
}

// TestStartLoginURL 双区授权 URL 参数差异。
func TestStartLoginURL(t *testing.T) {
	cn, err := StartLogin(RealmCN, "mid-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cn.AuthURL, "https://qoder.com.cn/device/selectAccounts?") {
		t.Fatalf("cn auth url: %s", cn.AuthURL)
	}
	if !strings.Contains(cn.AuthURL, "redirect_uri=") || !strings.Contains(cn.AuthURL, "challenge_method=S256") {
		t.Fatalf("cn auth url missing params: %s", cn.AuthURL)
	}
	if !strings.HasPrefix(cn.State, "qd-") {
		t.Fatalf("state prefix: %s", cn.State)
	}

	intl, err := StartLogin(RealmIntl, "mid-2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(intl.AuthURL, "https://qoder.com/device/selectAccounts?") {
		t.Fatalf("intl auth url: %s", intl.AuthURL)
	}
	if strings.Contains(intl.AuthURL, "redirect_uri=") {
		t.Fatalf("intl must not send redirect_uri: %s", intl.AuthURL)
	}
}

// TestPollTokenPending404 404 → pending。
func TestPollTokenPending404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deviceToken/poll" {
			t.Errorf("bad path %s", r.URL.Path)
		}
		if r.URL.Query().Get("verifier") == "" {
			t.Error("missing verifier")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	sess := &LoginSession{Verifier: "v", Nonce: "n", CreatedAt: time.Now()}
	res := PollToken(context.Background(), srv.Client(), r, sess)
	if res.Status != "pending" {
		t.Fatalf("want pending, got %+v", res)
	}
}

// TestPollTokenOK 200 + token → ok。
func TestPollTokenOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "dt-abc", "refresh_token": "drt-xyz", "user_id": "u1", "expires_in": 1000,
		})
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	sess := &LoginSession{Verifier: "v", Nonce: "n", CreatedAt: time.Now()}
	res := PollToken(context.Background(), srv.Client(), r, sess)
	if res.Status != "ok" || res.Token != "dt-abc" || res.RefreshToken != "drt-xyz" || res.UserID != "u1" {
		t.Fatalf("bad result: %+v", res)
	}
}

// TestPollTokenExpired 600s 过期。
func TestPollTokenExpired(t *testing.T) {
	sess := &LoginSession{Verifier: "v", Nonce: "n", CreatedAt: time.Now().Add(-700 * time.Second)}
	res := PollToken(context.Background(), http.DefaultClient, RealmCN, sess)
	if res.Status != "expired" {
		t.Fatalf("want expired, got %s", res.Status)
	}
}

// TestImportPAT 前缀校验 + 交换端点 + 毫秒口径。
func TestImportPAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobToken/exchange" {
			t.Errorf("path %s", r.URL.Path)
		}
		var body struct {
			PersonalToken string `json:"personal_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.PersonalToken != "pt-k1" {
			t.Errorf("personal_token = %q", body.PersonalToken)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "jt-1", "refresh_token": "jrt-1", "expires_in": 3600000})
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	if _, err := ImportPAT(context.Background(), srv.Client(), r, "bad"); err == nil {
		t.Fatal("non pt- token must be rejected")
	}
	tp, err := ImportPAT(context.Background(), srv.Client(), r, "pt-k1")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Token != "jt-1" || tp.Kind != "job" || tp.ExpiresInMs != 3600000 {
		t.Fatalf("bad pair: %+v", tp)
	}
}

// TestRefreshDevice drt- 前缀走 device 分支。
func TestRefreshDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deviceToken/refresh" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "dt-new", "refresh_token": "drt-new", "expires_in": 1000})
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	tp, err := Refresh(context.Background(), srv.Client(), r, "drt-old", "")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Kind != "device" || tp.Token != "dt-new" || tp.RefreshToken != "drt-new" {
		t.Fatalf("bad pair: %+v", tp)
	}
}

// TestRefreshJobFallbackPAT jrt- 刷新失败回落 PAT。
func TestRefreshJobFallbackPAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/refresh":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"TOKEN_EXPIRE"}`))
		case "/api/v1/jobToken/exchange":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "jt-2", "refresh_token": "jrt-2", "expires_in": 1000})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	r := RealmCN
	r.OpenAPI = srv.URL
	tp, err := Refresh(context.Background(), srv.Client(), r, "jrt-old", "pt-k")
	if err != nil {
		t.Fatal(err)
	}
	if tp.Token != "jt-2" || tp.Kind != "job" {
		t.Fatalf("fallback failed: %+v", tp)
	}
}

// TestUserinfoAliases 驼峰别名兜底。
func TestUserinfoAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tk" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"id":"u9","name":"nick","user_type":"personal_professional_trial","organizationId":"o1","organizationName":"on"}`))
	}))
	defer srv.Close()
	r := RealmIntl
	r.OpenAPI = srv.URL
	ui, err := Userinfo(context.Background(), srv.Client(), r, "tk")
	if err != nil {
		t.Fatal(err)
	}
	if ui.ID != "u9" || ui.OrganizationID != "o1" || ui.OrganizationName != "on" {
		t.Fatalf("bad userinfo: %+v", ui)
	}
}
