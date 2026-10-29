// 面板侧 TRAE 接入：授权链接登录（本地回调 / 粘贴回调链接兜底）、账号落盘、
// 模型名口径、出站账号视图。
//
// 登录闭环对齐参考实现 `trae2api-web`：
//
//	1. start  → 生成 machineID/deviceID（hex32）+ pending，回调地址固定
//	           `http://127.0.0.1:18080/authorize`（与官方客户端/login.sh 一致）；
//	2. 浏览器 → 用户登录后 TRAE 重定向回该地址：
//	           - 本地 18080 在听 → 自动收下并落盘（面板轮询即刻成功）；
//	           - 18080 被占用/不可达 → 用户复制地址栏链接，粘贴到面板（兜底路径）；
//	3. 处理   → ExchangeToken（refreshToken 轮换）+ GetUserInfo → 落盘 → 标记 pending；
//	4. poll   → 前端轮询取结果（回调与前端是两个请求，必须经会话传递）。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/trae"
)

// traeLoginSession 一次进行中的 TRAE 授权登录。
type traeLoginSession struct {
	machineID string
	deviceID  string
	traceID   string
	created   time.Time

	mu       sync.Mutex
	done     bool
	uid      string
	nickname string
	err      string
}

// traeLoginStart POST /panel/api/trae/login/start
//
// 返回 {ok, url, state, callback_url, callback_ready}：
// 前端展示 url，若 callback_ready=false 则提示用户粘贴回调链接。
func (p *Panel) traeLoginStart(w http.ResponseWriter, r *http.Request) {
	machineID, err := randomHexN(16) // hex32
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed: "+err.Error())
		return
	}
	deviceID, err := randomHexN(16) // hex32
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed: "+err.Error())
		return
	}
	pendingID, err := randomHexN(8) // hex16
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed: "+err.Error())
		return
	}

	// 回调地址固定 127.0.0.1:18080/authorize（参考实现口径）。
	callbackURL := traepkg.CallbackURL(traepkg.CallbackPort)
	callbackReady := p.ensureTraeCallback() == nil
	loginURL := traepkg.BuildLoginURL(machineID, deviceID, callbackURL)

	p.traeLoginMu.Lock()
	if p.traeLogins == nil {
		p.traeLogins = map[string]*traeLoginSession{}
	}
	for s, sess := range p.traeLogins {
		if time.Since(sess.created) > loginTTL {
			delete(p.traeLogins, s)
		}
	}
	p.traeLogins[pendingID] = &traeLoginSession{
		machineID: machineID,
		deviceID:  deviceID,
		traceID:   traepkg.MachineTraceID(machineID, deviceID),
		created:   time.Now(),
	}
	p.traeLoginMu.Unlock()

	log.Printf("panel: 发起 TRAE 授权登录（pending=%s... callback=%s ready=%v）",
		pendingID[:min(8, len(pendingID))], callbackURL, callbackReady)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": loginURL, "state": pendingID,
		"callback_url": callbackURL, "callback_ready": callbackReady,
	})
}

// traeLoginPaste POST /panel/api/trae/login/paste  body {state, callback_url}
//
// 兜底路径：本地 18080 不可用（或被占用）时，用户把浏览器地址栏里的
// 完整回调链接粘贴回来，由服务端完成换 token 与落盘。
func (p *Panel) traeLoginPaste(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	sess := p.traeSession(req.State)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	info, err := traepkg.ParseCallback(req.CallbackURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "回调链接解析失败: "+err.Error())
		return
	}
	uid, nickname, err := p.completeTraeLogin(r.Context(), sess, info)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "uid": uid, "nickname": nickname})
}

// traeLoginPoll GET /panel/api/trae/login/poll?state=...
func (p *Panel) traeLoginPoll(w http.ResponseWriter, r *http.Request) {
	sess := p.traeSession(r.URL.Query().Get("state"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	sess.mu.Lock()
	done, uid, nickname, errMsg := sess.done, sess.uid, sess.nickname, sess.err
	sess.mu.Unlock()
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	if errMsg != "" {
		writeErr(w, http.StatusBadGateway, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "uid": uid, "nickname": nickname, "realm": "trae",
	})
}

// traeSession 按 pendingID 取会话（过期即清）。
func (p *Panel) traeSession(pendingID string) *traeLoginSession {
	if pendingID == "" {
		return nil
	}
	p.traeLoginMu.Lock()
	defer p.traeLoginMu.Unlock()
	sess, ok := p.traeLogins[pendingID]
	if !ok {
		return nil
	}
	if time.Since(sess.created) > loginTTL {
		delete(p.traeLogins, pendingID)
		return nil
	}
	return sess
}

// traeCallbackHandler 本地回调服务的处理函数（路径 /authorize）。
//
// ⚠️ 免鉴权：浏览器重定向不带 Bearer。安全性由「一次性 pending + trace 关联 +
// loginTTL 过期 + 只绑 127.0.0.1」保证。
//
// 关联方式对齐参考实现：TRAE 回调不回传 machine_id/device_id，但回传
// **loginTraceID**（= MachineTraceID(machine,device)）；命中即取回登录时生成的那一对 id。
func (p *Panel) traeCallbackHandler(w http.ResponseWriter, r *http.Request) {
	info, err := traepkg.ParseCallback("http://127.0.0.1" + r.URL.String())
	if err != nil {
		traeCallbackPage(w, http.StatusBadRequest, "登录回调解析失败", err.Error())
		return
	}
	sess := p.findTraeSessionByTrace(info)
	if sess == nil {
		traeCallbackPage(w, http.StatusBadRequest, "未找到进行中的登录会话",
			"请回到面板重新点击「获取授权链接」（会话已过期或已被使用）。")
		return
	}
	uid, nickname, err := p.completeTraeLogin(r.Context(), sess, info)
	if err != nil {
		traeCallbackPage(w, http.StatusBadGateway, "登录失败", err.Error())
		return
	}
	traeCallbackPage(w, http.StatusOK, "登录成功",
		fmt.Sprintf("账号 %s 已添加到网关，可以关闭此页面回到面板。", html.EscapeString(nickname)))
	log.Printf("panel: TRAE 回调登录成功 uid=%s nickname=%q", uid, nickname)
}

// findTraeSessionByTrace 用回调里的 trace / machine_id 反查 pending。
//
// 优先精确匹配（trace 或 machine_id）；都没有时，若只有**一个**待处理会话则用它
// —— 回调偶尔不回传任何关联字段，此时单会话语义与参考实现一致（每次登录一个落点）。
func (p *Panel) findTraeSessionByTrace(info *traepkg.CallbackInfo) *traeLoginSession {
	p.traeLoginMu.Lock()
	defer p.traeLoginMu.Unlock()
	var single *traeLoginSession
	pendings := 0
	for _, sess := range p.traeLogins {
		if time.Since(sess.created) > loginTTL {
			continue
		}
		pendings++
		single = sess
		if info.LoginTraceID != "" && sess.traceID == info.LoginTraceID {
			return sess
		}
		if info.MachineID != "" && sess.machineID == info.MachineID {
			return sess
		}
	}
	if pendings == 1 {
		return single
	}
	return nil
}

// completeTraeLogin 完成一次登录：ExchangeToken → GetUserInfo → 落盘 → 标记会话。
func (p *Panel) completeTraeLogin(ctx context.Context, sess *traeLoginSession, info *traepkg.CallbackInfo) (string, string, error) {
	client := traepkg.NewClient(nil)
	acct := traepkg.Account{
		UID:          info.UID,
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		MachineID:    sess.machineID,
		DeviceID:     sess.deviceID,
	}
	// 有 refreshToken → ExchangeToken 换新 access（并轮换 refreshToken）
	if info.RefreshToken != "" {
		ex, err := client.ExchangeToken(acct)
		if err != nil {
			sess.fail(err)
			return "", "", fmt.Errorf("ExchangeToken 失败: %w", err)
		}
		acct.AccessToken = ex.AccessToken
		acct.RefreshToken = ex.RefreshToken
		acct.ExpiresAt = ex.ExpiresAt
	} else {
		acct.ExpiresAt = info.ExpiresAt
		if acct.ExpiresAt == 0 {
			acct.ExpiresAt = traepkg.JWTExpiry(acct.AccessToken)
		}
	}
	if acct.AccessToken == "" {
		err := fmt.Errorf("换 token 后没有 access_token")
		sess.fail(err)
		return "", "", err
	}

	// GetUserInfo 补全 uid/昵称（失败不阻塞，沿用回调值）
	if ui, err := client.GetUserInfo(acct); err == nil && ui.UID != "" {
		acct.UID = ui.UID
		if ui.Nickname != "" {
			acct.Nickname = ui.Nickname
		}
	}
	if acct.UID == "" {
		err := fmt.Errorf("无法确定 uid（回调与 GetUserInfo 均为空）")
		sess.fail(err)
		return "", "", err
	}
	if !validUID(acct.UID) {
		err := fmt.Errorf("上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		sess.fail(err)
		return "", "", err
	}
	nickname := acct.Nickname
	if nickname == "" {
		nickname = "TRAE " + acct.UID
	}
	if err := p.saveTraeAccount(acct.UID, nickname, acct.AccessToken, acct.RefreshToken,
		sess.machineID, sess.deviceID, acct.ExpiresAt); err != nil {
		sess.fail(fmt.Errorf("save auth: %w", err))
		return "", "", err
	}
	sess.mu.Lock()
	sess.done, sess.uid, sess.nickname, sess.err = true, acct.UID, nickname, ""
	sess.mu.Unlock()
	return acct.UID, nickname, nil
}

// fail 标记会话失败。
func (s *traeLoginSession) fail(err error) {
	s.mu.Lock()
	s.done, s.err = true, err.Error()
	s.mu.Unlock()
}

// ensureTraeCallback 懒启动 TRAE 本地回调服务（只绑 127.0.0.1:18080）。
//
// 端口被占用时不致命：返回错误，前端据此提示「粘贴回调链接」兜底。
func (p *Panel) ensureTraeCallback() error {
	p.traeCallbackOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:"+traepkg.CallbackPort)
		if err != nil {
			p.traeCallbackErr = fmt.Errorf("listen 127.0.0.1:%s: %w", traepkg.CallbackPort, err)
			log.Printf("panel: TRAE 回调端口不可用（%v）——登录降级为粘贴回调链接", p.traeCallbackErr)
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc(traepkg.CallbackPath, p.traeCallbackHandler)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			// 运行期错误吞掉：由 loginTTL 超时兜底，不能让回调服务异常带崩面板。
			_ = srv.Serve(ln)
		}()
		log.Printf("panel: TRAE 本地回调服务已监听 %s", traepkg.CallbackURL(traepkg.CallbackPort))
	})
	return p.traeCallbackErr
}

// traeCallbackPage 回调结果的浏览器可见页面。
func traeCallbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>TRAE 登录 · `+html.EscapeString(title)+`</title>
<style>body{font-family:-apple-system,"Segoe UI",Roboto,"Helvetica Neue",sans-serif;
background:#0f1115;color:#e8eaed;display:flex;align-items:center;justify-content:center;
height:100vh;margin:0}div{max-width:520px;padding:32px;text-align:center}
h1{font-size:20px;margin:0 0 12px}p{color:#9aa0a6;line-height:1.7;margin:0}</style></head>
<body><div><h1>`+html.EscapeString(title)+`</h1><p>`+html.EscapeString(detail)+`</p></div></body></html>`)
}

// saveTraeAccount 构造 realm=trae 的 Auth 落盘并热加载进池。
//
// 键名与参考实现一致（camelCase：machineId / deviceId / apiHost），便于两边凭据互通。
func (p *Panel) saveTraeAccount(uid, nickname, token, refreshToken, machineID, deviceID string, expiresAt int64) error {
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return fmt.Errorf("mkdir auth dir: %w", err)
	}
	raw, err := json.MarshalIndent(map[string]any{
		"account": map[string]any{"uid": uid, "enterpriseId": "", "nickname": nickname},
		"auth": map[string]any{
			"accessToken":  token,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt,
			"realm":        "trae",
			"domain":       "trae.cn",
			"apiHost":      traepkg.OAuthHost,
			"machineId":    machineID,
			"deviceId":     deviceID,
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(p.cfg.AuthDir, fmt.Sprintf("trae-%s.json", uid))
	a, err := auth.Parse(raw)
	if err != nil {
		return err
	}
	a.FilePath = path
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(uid)
	return nil
}

// traeAccountView auth.Auth → trae 出站账号视图（与 server.traeAccountOf 同口径）。
func traeAccountView(a *auth.Auth) traepkg.Account {
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

// traeModelName 对外展示名（空则回落上游 config_name）。
func traeModelName(m traepkg.ModelInfo) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// randomHexN 生成 n 字节随机 hex（2n 字符）。
func randomHexN(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// formatFactorText 倍率 → 最短数字文案。
func formatFactorText(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", f), "0"), ".")
}

var _ = sync.Mutex{}
