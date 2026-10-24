// relay.go 面板「中转账号」接口：OpenAI 兼容网关账号（realm=relay）的增删查。
// 凭证落盘 auths/relay-<uid>.json（嵌套形 + base_url），热加载进池，与既有账号管理复用。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func (p *Panel) registerRelayRoutes() {
	p.mux.HandleFunc("GET /panel/api/relay", p.withAuth(p.relayList))
	p.mux.HandleFunc("POST /panel/api/relay/add", p.withAuth(p.relayAdd))
	p.mux.HandleFunc("POST /panel/api/relay/delete", p.withAuth(p.relayDelete))
	p.mux.HandleFunc("POST /panel/api/relay/models", p.withAuth(p.relayModels))
	p.mux.HandleFunc("POST /panel/api/relay/models/sync", p.withAuth(p.relayModelsSync))
	p.mux.HandleFunc("POST /panel/api/relay/probe", p.withAuth(p.relayProbe))
}

// relayList 列出池内 realm=relay 账号（key 打码）。
func (p *Panel) relayList(w http.ResponseWriter, r *http.Request) {
	type item struct {
		UID      string   `json:"uid"`
		Nickname string   `json:"nickname"`
		BaseURL  string   `json:"base_url"`
		KeyMask  string   `json:"key_mask"`
		Disabled bool     `json:"disabled"`
		Models   []string `json:"models"`
	}
	out := []item{}
	for _, s := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(s.UID)
		if a == nil || a.Realm() != "relay" {
			continue
		}
		k := a.AccessTokenValue()
		mask := "—"
		if len(k) > 8 {
			mask = k[:4] + "****" + k[len(k)-4:]
		}
		out = append(out, item{UID: s.UID, Nickname: s.Nickname, BaseURL: a.BaseURL, KeyMask: mask, Disabled: s.Disabled, Models: a.Models})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relays": out})
}

// relayAdd 添加中转账号：{name, base_url, api_key} → 落盘 + pool.Add 热加载。
func (p *Panel) relayAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Base string `json:"base_url"`
		Key  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	base := strings.TrimRight(strings.TrimSpace(req.Base), "/")
	key := strings.TrimSpace(req.Key)
	if name == "" || base == "" || key == "" {
		writeErr(w, http.StatusBadRequest, "name / base_url / api_key 均必填")
		return
	}
	uid := "relay-" + fmt.Sprintf("%x", time.Now().UnixNano())
	raw, err := json.MarshalIndent(map[string]any{
		"auth": map[string]any{
			"accessToken": key,
			"expiresAt":   time.Now().AddDate(10, 0, 0).Unix(),
			"realm":       "relay",
			"base_url":    base,
		},
		"account": map[string]any{"uid": uid, "nickname": name},
	}, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(p.cfg.AuthDir, "relay-"+strings.TrimPrefix(uid, "relay-")+".json")
	a, err := auth.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.FilePath = path
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// relayModels 实时拉取指定中转账号的模型名单：GET {base}/models（OpenAI 兼容）。
func (p *Panel) relayModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	a := p.cfg.Pool.AuthByUID(strings.TrimSpace(req.UID))
	if a == nil || a.Realm() != "relay" {
		writeErr(w, http.StatusNotFound, "中转账号不存在")
		return
	}
	ids, err := p.cfg.Upstream.RelayModels(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids})
}

// relayModelsSync 拉取中转站模型名单并把勾选的模型保存进账号（auth 文件 + 池内对象）。
// body: {uid, models:[...]}；models 省略/为 null = 全量同步上游返回名单。
func (p *Panel) relayModelsSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID    string   `json:"uid"`
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	a := p.cfg.Pool.AuthByUID(strings.TrimSpace(req.UID))
	if a == nil || a.Realm() != "relay" {
		writeErr(w, http.StatusNotFound, "中转账号不存在")
		return
	}
	ids := req.Models
	if ids == nil {
		var err error
		if ids, err = p.cfg.Upstream.RelayModels(a); err != nil {
			writeErr(w, http.StatusBadGateway, "拉取失败: "+err.Error())
			return
		}
	}
	a.Models = ids
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids})
}

// relayProbe 用未保存的 base_url+key 探测模型名单（添加表单「拉取模型」预览用；
// 面板 CSP connect-src 'self' 禁止前端直连外部域名，探测必须由后端代发）。
func (p *Panel) relayProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Base string `json:"base_url"`
		Key  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	probe, err := auth.Parse([]byte(`{"auth":{"accessToken":` + strconv.Quote(strings.TrimSpace(req.Key)) + `,"realm":"relay","base_url":` + strconv.Quote(strings.TrimRight(strings.TrimSpace(req.Base), "/")) + `}}`))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if probe.BaseURL == "" || probe.AccessToken == "" {
		writeErr(w, http.StatusBadRequest, "base_url / api_key 均必填")
		return
	}
	ids, err := p.cfg.Upstream.RelayModels(probe)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids})
}

// relayDelete 删除中转账号：移出池并删除凭证文件。
func (p *Panel) relayDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	a := p.cfg.Pool.AuthByUID(strings.TrimSpace(req.UID))
	if a == nil || a.Realm() != "relay" {
		writeErr(w, http.StatusNotFound, "中转账号不存在")
		return
	}
	p.cfg.Pool.Remove(a.UID)
	if a.FilePath != "" {
		_ = os.Remove(a.FilePath)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
