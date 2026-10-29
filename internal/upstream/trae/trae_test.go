package trae

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestBuildLoginURL 校验授权链接的关键参数（复刻 login.sh 的参数集）。
func TestBuildLoginURL(t *testing.T) {
	machineID := strings.Repeat("a", 32)
	deviceID := strings.Repeat("b", 32)
	got := BuildLoginURL(machineID, deviceID, "http://127.0.0.1:18080/authorize")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"login_version":     "1",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"plugin_version":    PluginVersion,
		"auth_type":         "local",
		"client_id":         ClientID,
		"redirect":          "0",
		"machine_id":        machineID,
		"device_id":         deviceID,
		"x_device_id":       deviceID,
		"x_machine_id":      machineID,
		"x_device_brand":    "PC",
		"x_device_type":     "PC",
		"x_os_version":      "1.0",
		"x_app_version":     IDEVersion,
		"x_app_type":        "stable",
		"auth_callback_url": "http://127.0.0.1:18080/authorize",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("参数 %s = %q，期望 %q", k, got, v)
		}
	}
	if q.Get("login_trace_id") != MachineTraceID(machineID, deviceID) {
		t.Errorf("login_trace_id = %q", q.Get("login_trace_id"))
	}
	if !strings.HasPrefix(got, ConsoleHost+"/authorization?") {
		t.Errorf("链接前缀不对：%s", got)
	}
}

// TestParseCallbackTokenFlow 校验回调解析：refreshToken 优先，userInfo 字段口径。
func TestParseCallbackTokenFlow(t *testing.T) {
	raw := "http://127.0.0.1:18080/authorize?refreshToken=rt-1" +
		"&userInfo=" + url.QueryEscape(`{"UserID":"u-1","ScreenName":"测试用户","TenantID":"t-9"}`) +
		"&loginTraceID=abc123"
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if info.RefreshToken != "rt-1" {
		t.Errorf("refreshToken = %q", info.RefreshToken)
	}
	if info.UID != "u-1" || info.EnterpriseID != "t-9" {
		t.Errorf("userInfo 解析不对：%+v", info)
	}
	if info.Nickname != "测试用户" {
		t.Errorf("昵称 = %q", info.Nickname)
	}
	// ⚠️ 回调参数名是驼峰 loginTraceID
	if info.LoginTraceID != "abc123" {
		t.Errorf("loginTraceID = %q", info.LoginTraceID)
	}
}

// TestParseCallbackUserJwtFallback 校验缺 refreshToken 时回退 userJwt。
func TestParseCallbackUserJwtFallback(t *testing.T) {
	raw := "http://127.0.0.1:18080/authorize?userInfo=" + url.QueryEscape(`{"UserID":"u-2"}`) +
		"&userJwt=" + url.QueryEscape(`{"Token":"jwt-token","RefreshToken":"jwt-rt"}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if info.RefreshToken != "jwt-rt" {
		t.Errorf("应回退 userJwt.RefreshToken，得到 %q", info.RefreshToken)
	}

	// 连 refreshToken 都没有 → 用 userJwt.Token 兜底
	raw2 := "http://127.0.0.1:18080/authorize?userJwt=" + url.QueryEscape(`{"Token":"only-token"}`)
	info2, err := ParseCallback(raw2)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if info2.AccessToken != "only-token" {
		t.Errorf("应回退 userJwt.Token，得到 %q", info2.AccessToken)
	}
}

// TestParseCallbackMissingAll 空回调给出可读错误。
func TestParseCallbackMissingAll(t *testing.T) {
	if _, err := ParseCallback(""); err == nil {
		t.Fatal("空回调应报错")
	}
	if _, err := ParseCallback("http://127.0.0.1:18080/authorize"); err == nil {
		t.Fatal("无 token 的回调应报错")
	}
}

// TestPrepareBody 校验 OpenAI → SOLO 请求体改写。
func TestPrepareBody(t *testing.T) {
	src := []byte(`{
		"model": "glm-5.2",
		"stream": false,
		"messages": [
			{"role":"system","content":"sys"},
			{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}},{"id":"c2","function":{}}]},
			{"role":"user","content":"hi"}
		],
		"tool_choice": "none",
		"tools": [{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]
	}`)
	out := PrepareBody(src)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["stream"] != true {
		t.Errorf("stream 应强制 true")
	}
	if body["function"] != Function {
		t.Errorf("function 应为 %s，得到 %v", Function, body["function"])
	}
	if body["config_name"] != "glm-5.2" || body["model"] != "glm-5.2" {
		t.Errorf("config_name/model 不对：%v / %v", body["config_name"], body["model"])
	}
	// tool_choice=none → tools 一并删除
	if _, exists := body["tools"]; exists {
		t.Errorf("tool_choice=none 应删掉 tools")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 条数 %d", len(msgs))
	}
	// 字符串 content → parts 数组
	first, _ := msgs[0].(map[string]any)
	if _, ok := first["content"].([]any); !ok {
		t.Errorf("字符串 content 应转成 parts 数组，得到 %#v", first["content"])
	}
	// assistant 的 tool_calls: function → function_call，无 name 的剔除
	second, _ := msgs[1].(map[string]any)
	tcs, _ := second["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("应只剩 1 条有效 tool_call，得到 %d", len(tcs))
	}
	tc0, _ := tcs[0].(map[string]any)
	if _, ok := tc0["function_call"]; !ok {
		t.Errorf("应改写成 function_call：%#v", tc0)
	}
	if _, ok := tc0["function"]; ok {
		t.Errorf("不应保留 function 字段")
	}
}

// TestPrepareBodyToolsSerialized 校验 tools.parameters 被序列化成字符串。
func TestPrepareBodyToolsSerialized(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`)
	var body map[string]any
	if err := json.Unmarshal(PrepareBody(src), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 条数 %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if _, isStr := fn["parameters"].(string); !isStr {
		t.Errorf("parameters 应为 JSON 字符串，得到 %#v", fn["parameters"])
	}
}

// TestStreamTranslation 校验 SOLO 事件 → OpenAI SSE：正文/思考/usage/done/[DONE]。
func TestStreamTranslation(t *testing.T) {
	upstream := strings.Join([]string{
		"event: metadata",
		`data: {"session_id":"s1"}`,
		"",
		"event: output",
		`data: {"response":"你好","reasoning_content":"想一想"}`,
		"",
		"event: token_usage",
		`data: {"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if err := Stream(&out, strings.NewReader(upstream), nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`"content":"你好"`,
		`"reasoning_content":"想一想"`,
		`"total_tokens":15`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %s，实际：%s", want, got)
		}
	}
}

// TestStreamErrorEvent 校验流内 error 事件既回调 onErr 也写进事件流。
func TestStreamErrorEvent(t *testing.T) {
	upstream := "event: error\ndata: {\"code\":1005,\"message\":\"plan limit\"}\n\n"
	var out bytes.Buffer
	var captured *StreamError
	if err := Stream(&out, strings.NewReader(upstream), func(se *StreamError) { captured = se }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if captured == nil || captured.Code != 1005 {
		t.Fatalf("onErr 未收到流内错误：%+v", captured)
	}
	if captured.Kind() != KindHardPlan {
		t.Errorf("1005 应归类为 plan_limit，得到 %v", captured.Kind())
	}
	if !strings.Contains(out.String(), "event: error") {
		t.Errorf("应把错误写进事件流：%s", out.String())
	}
}

// TestStreamNoDone 校验上游中断（无 done）仍补 [DONE]。
func TestStreamNoDone(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"x\"}\n\n"
	var out bytes.Buffer
	if err := Stream(&out, strings.NewReader(upstream), nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Errorf("应补 [DONE]：%s", out.String())
	}
}

// TestAggregate 校验非流式聚合。
func TestAggregate(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"a\"}\n\n" +
		"event: output\ndata: {\"response\":\"b\"}\n\n" +
		"event: token_usage\ndata: {\"total_tokens\":3}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	resp, err := Aggregate(strings.NewReader(upstream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数 %d", len(choices))
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if msg["content"] != "ab" {
		t.Errorf("聚合正文 = %q，期望 ab", msg["content"])
	}
}

// TestClassify 校验错误分类顺序（1005 优先、401 会话失效）。
func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Kind
	}{
		{200, "", KindNone},
		{400, `{"code":1005,"msg":"plan limit"}`, KindHardPlan},
		{400, `{"code":4008,"msg":"exceeded the quota"}`, KindQuotaExceeded},
		{401, `{"msg":"token invalid"}`, KindSessionDead},
		{429, "", KindSoftRate},
		{404, "", KindNotFound},
		{502, "", KindServer},
		{400, `{"msg":"param is invalid"}`, KindClient},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，期望 %v", c.status, c.body, got, c.want)
		}
	}
}

// TestMachineTraceID 校验 trace id 取尾部 16 字符、不足左补 0。
func TestMachineTraceID(t *testing.T) {
	if got := MachineTraceID("0123456789abcdef0123456789abcdef", "1234567890123456"); got != "1234567890123456" {
		t.Errorf("尾部 16 字符 = %q", got)
	}
	got := MachineTraceID("abc", "def")
	if len(got) != 16 || !strings.HasSuffix(got, "abcdef") {
		t.Errorf("短串应左补 0 到 16 位，得到 %q", got)
	}
}

// TestJWTExpiry 校验 JWT exp 解析。
func TestJWTExpiry(t *testing.T) {
	// {"exp":1767225600} 的 base64url
	token := "x.eyJleHAiOjE3NjcyMjU2MDB9.y"
	if got := JWTExpiry(token); got != 1767225600 {
		t.Errorf("JWTExpiry = %d", got)
	}
	if got := JWTExpiry("bad"); got != 0 {
		t.Errorf("非法 token 应返回 0，得到 %d", got)
	}
}

// TestEntUsage 校验积分聚合口径：remain = Σ(limit - used)，total = Σ limit，
// 且 limit<=0 的权益包被跳过（不计入包数）。
func TestEntUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpEntUsage {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		// UG 头的关键约束：Authorization 是 Cloud-IDE-JWT，且必须带 X-User-Region
		if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-User-Region"); got != "CN" {
			t.Errorf("X-User-Region = %q", got)
		}
		if got := r.Header.Get("X-Device-Id"); got != "dev" {
			t.Errorf("X-Device-Id = %q", got)
		}
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":250}},
			{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}},
			{"entitlement_base_info":{"quota":{"credits_limit":0}},"usage":{"credits_amount":0}}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(nil)
	c.UgHost = srv.URL
	remain, limit, used, packs, err := c.EntUsage(Account{AccessToken: "tok", DeviceID: "dev"})
	if err != nil {
		t.Fatalf("EntUsage: %v", err)
	}
	if limit != 1500 || used != 350 || remain != 1150 {
		t.Errorf("聚合不对：remain=%d limit=%d used=%d", remain, limit, used)
	}
	if packs != 2 {
		t.Errorf("权益包数 = %d，期望 2（limit=0 的应跳过）", packs)
	}
}

// TestCheckinStatusAndClaim 校验签到状态（顶层字段）与签到调用。
func TestCheckinStatusAndClaim(t *testing.T) {
	var claimed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCheckinStatus:
			_, _ = w.Write([]byte(`{"checked_in":false,"credits":300,"enable":true}`))
		case EpCheckinClaim:
			claimed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(nil)
	c.UgHost = srv.URL
	checkedIn, credits, enable, err := c.CheckinStatus(Account{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if checkedIn || !enable || credits != 300 {
		t.Errorf("状态解析不对：checkedIn=%v credits=%d enable=%v", checkedIn, credits, enable)
	}
	if err := c.CheckinClaim(Account{AccessToken: "tok"}); err != nil {
		t.Fatalf("CheckinClaim: %v", err)
	}
	if !claimed {
		t.Errorf("未真正调用签到端点")
	}
}

// TestExchangeTokenResultEnvelope 校验 ExchangeToken 响应包在 Result 里的解析。
//
// 回归锁：早期实现在 data/顶层找 token，导致换 token 恒失败（登录「一直不行」）。
func TestExchangeTokenResultEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpExchange {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		// TokenExpireAt 是毫秒，必须归一化为秒（取一个未来时间戳，避免落入
		// 「已过期 → 用 TokenExpireDuration」的兜底分支）
		_, _ = w.Write([]byte(`{"Result":{"Token":"new-access","RefreshToken":"new-refresh","TokenExpireAt":4000000000123}}`))
	}))
	defer srv.Close()

	c := NewClient(nil)
	c.OAuthHost = srv.URL
	res, err := c.ExchangeToken(Account{RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}
	if res.AccessToken != "new-access" || res.RefreshToken != "new-refresh" {
		t.Errorf("token 解析不对：%+v", res)
	}
	// TokenExpireAt 是毫秒，必须归一化为秒
	if res.ExpiresAt != 4000000000 {
		t.Errorf("ExpiresAt = %d，期望 4000000000（毫秒→秒）", res.ExpiresAt)
	}
}

// TestGetUserInfoHeaderAndEnvelope 校验 GetUserInfo 用 X-Cloudide-Token 且响应在 Result 里。
func TestGetUserInfoHeaderAndEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Cloudide-Token"); got != "tok" {
			t.Errorf("X-Cloudide-Token = %q（不是 Authorization）", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "ReqSource") {
			t.Errorf("请求体应含 ReqSource：%s", string(body))
		}
		_, _ = w.Write([]byte(`{"Result":{"UserID":"u-9","ScreenName":"昵称","EnterpriseID":"e-1"}}`))
	}))
	defer srv.Close()

	c := NewClient(nil)
	c.OAuthHost = srv.URL
	ui, err := c.GetUserInfo(Account{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	if ui.UID != "u-9" || ui.Nickname != "昵称" || ui.EnterpriseID != "e-1" {
		t.Errorf("解析不对：%+v", ui)
	}
}
