// Qoder 账号录入：OAuth 设备授权（本地 device flow）+ PAT 导入。
// 与 workbuddy 的 login/start→login/poll 不同：Qoder 的授权 URL 与轮询都由网关
// 本地构造/发起（panel 只做会话登记与转交），state 直接使用 qoder.LoginSession.State。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream/qoder"
)

// qoderLoginSession 进行中的 Qoder 授权会话（本地 state → LoginSession）。
type qoderLoginSession struct {
	sess      *qoder.LoginSession
	created   time.Time
	machineID string
}

// qoderLoginStart POST /panel/api/qoder/login/start  body {realm: "cn"|"global"}
func (p *Panel) qoderLoginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Realm == "global" {
				realm = "global"
			}
		}
	}
	qr := qoder.RealmFor(realm)
	machineID := p.qoderMachineID(realm)
	sess, err := qoder.StartLogin(qr, machineID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "start login: "+err.Error())
		return
	}
	p.qoderLoginMu.Lock()
	// 回收过期会话
	for s, e := range p.qoderLogins {
		if time.Since(e.created) > loginTTL {
			delete(p.qoderLogins, s)
		}
	}
	p.qoderLogins[sess.State] = qoderLoginSession{sess: sess, created: time.Now(), machineID: machineID}
	p.qoderLoginMu.Unlock()
	log.Printf("panel: 发起 Qoder OAuth 添加账号 realm=%s（state=%s...）", realm, sess.State[:min(8, len(sess.State))])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": sess.AuthURL, "state": sess.State, "realm": realm,
	})
}

// qoderLoginPoll GET /panel/api/qoder/login/poll?state=...
func (p *Panel) qoderLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.qoderLoginMu.Lock()
	entry, known := p.qoderLogins[state]
	p.qoderLoginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	qr := qoder.RealmFor(entry.sess.Realm)
	res := qoder.PollToken(r.Context(), http.DefaultClient, qr, entry.sess)
	switch res.Status {
	case "pending", "expired":
		if res.Status == "expired" {
			p.qoderLoginMu.Lock()
			delete(p.qoderLogins, state)
			p.qoderLoginMu.Unlock()
		}
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": res.Message, "status": res.Status})
		return
	case "error":
		writeErr(w, http.StatusBadGateway, res.Message)
		return
	}

	// ok：补身份（失败不阻塞，仅缺展示名）
	var ui *qoder.UserInfo
	if uiRaw, err := qoder.Userinfo(r.Context(), http.DefaultClient, qr, res.Token); err == nil {
		ui = uiRaw
	}
	uid := res.UserID
	nickname := ""
	userType := qoder.DefaultUserType
	orgID, orgName := "", ""
	if ui != nil {
		if ui.ID != "" {
			uid = ui.ID
		}
		nickname = ui.Name
		if ui.UserType != "" {
			userType = ui.UserType
		}
		orgID, orgName = ui.OrganizationID, ui.OrganizationName
	}
	if uid == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）")
		return
	}
	if !validUID(uid) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	// expires：expires_in 毫秒；未知时 device token 缺省 30 天
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	if res.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Millisecond)
	}
	if err := p.saveQoderAccount(entry, uid, nickname, res.Token, res.RefreshToken, expiresAt.Unix(),
		userType, orgID, orgName); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.qoderLoginMu.Lock()
	delete(p.qoderLogins, state)
	p.qoderLoginMu.Unlock()
	log.Printf("panel: Qoder 新账号已热加载 uid=%s nickname=%q realm=%s", uid, nickname, entry.sess.Realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "uid": uid, "nickname": nickname, "realm": entry.sess.Realm,
	})
}

// qoderPATAdd POST /panel/api/qoder/pat  body {realm, pat}
func (p *Panel) qoderPATAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
		PAT   string `json:"pat"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	realm := "cn"
	if req.Realm == "global" {
		realm = "global"
	}
	pat := strings.TrimSpace(req.PAT)
	if pat == "" {
		writeErr(w, http.StatusBadRequest, "pat 必填")
		return
	}
	qr := qoder.RealmFor(realm)
	tp, err := qoder.ImportPAT(r.Context(), http.DefaultClient, qr, pat)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "PAT 交换失败: "+err.Error())
		return
	}
	uid, nickname := "", ""
	userType, orgID, orgName := qoder.DefaultUserType, "", ""
	if ui, err := qoder.Userinfo(r.Context(), http.DefaultClient, qr, tp.Token); err == nil && ui.ID != "" {
		uid, nickname = ui.ID, ui.Name
		if ui.UserType != "" {
			userType = ui.UserType
		}
		orgID, orgName = ui.OrganizationID, ui.OrganizationName
	}
	if uid == "" {
		uid = "p-" + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	if !validUID(uid) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}
	expiresAt := time.Now().Add(time.Duration(tp.ExpiresInMs) * time.Millisecond)
	entry := qoderLoginSession{machineID: p.qoderMachineID(realm)}
	if err := p.saveQoderAccountPAT(entry, uid, nickname, tp, pat, expiresAt.Unix(),
		userType, orgID, orgName, realm); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	log.Printf("panel: Qoder PAT 账号已热加载 uid=%s nickname=%q realm=%s", uid, nickname, realm)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nickname, "realm": realm})
}

// saveQoderAccount 构造 qoder realm 的 Auth 落盘并热加载进池。
// 凭证嵌套形扩展键（qoder 专有）：user_type / organization_id / organization_name /
// personal_token / machine_id（多账号隔离的设备指纹种子）。
func (p *Panel) saveQoderAccount(entry qoderLoginSession, uid, nickname, token, refreshToken string,
	expiresAt int64, userType, orgID, orgName string) error {
	return p.saveQoderAccountPAT(entry, uid, nickname,
		&qoder.TokenPair{Token: token, RefreshToken: refreshToken},
		"", expiresAt, userType, orgID, orgName, entry.sess.Realm)
}

// saveQoderAccountPAT 同上，PAT 导入路径（保留 pt- 原值与 job kind）。
func (p *Panel) saveQoderAccountPAT(entry qoderLoginSession, uid, nickname string, tp *qoder.TokenPair,
	pat string, expiresAt int64, userType, orgID, orgName, realm string) error {
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return fmt.Errorf("mkdir auth dir: %w", err)
	}
	authMap := map[string]any{
		"accessToken":       tp.Token,
		"refreshToken":      tp.RefreshToken,
		"expiresAt":         expiresAt,
		"realm":             qoderRealmOf(realm), // "cn"→"qoder"、"global"→"qoder_global"（路由域，不能直接落 cn/global——那会被当 workbuddy 账号）
		"user_type":         userType,
		"organization_id":   orgID,
		"organization_name": orgName,
		"machine_id":        entry.machineID,
	}
	if pat != "" {
		authMap["personal_token"] = pat
	}
	raw, err := json.MarshalIndent(map[string]any{
		"auth":    authMap,
		"account": map[string]any{"uid": uid, "nickname": nickname},
	}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(p.cfg.AuthDir, fmt.Sprintf("qoder-%s.json", uid))
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

// qoderRealmOf 区域名（"cn"/"global"）→ auth realm（"qoder"/"qoder_global"）。
func qoderRealmOf(realm string) string {
	if realm == "global" {
		return "qoder_global"
	}
	return "qoder"
}

// qoderMachineID 按 realm 生成（内存态；与 auth 文件 machine_id 键同源）。
// 每账号一个稳定 ID 由落盘键保证；会话内复用进程级生成即可（官方口径：缺失才生成）。
func (p *Panel) qoderMachineID(realm string) string {
	return qoder.NewMachineID()
}
