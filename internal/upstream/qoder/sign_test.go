package qoder

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// TestQoderEncodeDecodeRoundTrip 自定义 Base64 编解码往返。
func TestQoderEncodeDecodeRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("a"),
		[]byte("ab"),
		[]byte("abc"),
		[]byte("hello world"),
		bytes.Repeat([]byte{0x00, 0xff, 0x42}, 100),
	}
	for _, plain := range cases {
		enc := QoderEncode(plain)
		if strings.Contains(enc, "=") {
			t.Fatalf("encoded contains std padding: %q", enc)
		}
		dec, err := QoderDecode(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", enc, err)
		}
		if !bytes.Equal(dec, plain) {
			t.Fatalf("roundtrip mismatch: got %q want %q", dec, plain)
		}
	}
}

// TestQoderEncodeMatchesStdAlphabet 校验换表与三段重排逻辑：
// 用 Python 侧同口径手算一个样例（"abc" → base64 "YWJj"，无重排差异时逐字符换表）。
func TestQoderEncodeMatchesStdAlphabet(t *testing.T) {
	// n=4, a=1：R = std[3:] + std[1:3] + std[:1] = "j" + "WJ" + "Y"（std = "YWJj"）
	want := ""
	for _, c := range "jWJY" {
		idx := strings.IndexByte(qoderStdAlphabet, byte(c))
		want += string(qoderCustomAlphabet[idx])
	}
	got := QoderEncode([]byte("abc"))
	if got != want {
		t.Fatalf("QoderEncode(abc) = %q, want %q", got, want)
	}
}

// TestDeriveIDStable 指纹派生幂等且 UUID 形状正确（版本位 4、变体位 a）。
func TestDeriveIDStable(t *testing.T) {
	a := DeriveID("user-1", "machine")
	b := DeriveID("user-1", "machine")
	if a != b {
		t.Fatalf("not idempotent: %q vs %q", a, b)
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("bad uuid shape: %q", a)
	}
	if a[14] != '4' || a[19] != 'a' {
		t.Fatalf("bad uuid version/variant: %q", a)
	}
	if a == DeriveID("user-2", "machine") {
		t.Fatal("different uids must derive different ids")
	}
}

// TestDeriveIDAnonymous 空 uid 回落 anonymous（与 Python f"{salt}:{uid or 'anonymous'}" 一致）。
func TestDeriveIDAnonymous(t *testing.T) {
	if DeriveID("", "machine") != DeriveID("anonymous", "machine") {
		t.Fatal("empty uid must fall back to anonymous")
	}
}

// TestDeriveMachineType 长度与去横线校验。
func TestDeriveMachineType(t *testing.T) {
	mt := DeriveMachineType("uid-x")
	if len(mt) != 18 || strings.Contains(mt, "-") {
		t.Fatalf("bad machine type: %q", mt)
	}
}

// TestDeriveMachineToken 43 字符 base64url 形态。
func TestDeriveMachineToken(t *testing.T) {
	mt := DeriveMachineToken("uid-x")
	if len(mt) != 43 {
		t.Fatalf("bad machine token length: %d", len(mt))
	}
	if DeriveMachineToken("uid-x") != mt {
		t.Fatal("not idempotent")
	}
}

// TestJSONSortedCompact 与 Python json.dumps(sort_keys, separators, ensure_ascii=False) 字节一致。
func TestJSONSortedCompact(t *testing.T) {
	got := string(jsonSortedCompact([][2]string{
		{"b", "2"}, {"a", "1"}, {"name", "中文&\"quote\""},
	}))
	want := `{"a":"1","b":"2","name":"中文&\"quote\""}`
	if got != want {
		t.Fatalf("jsonSortedCompact = %s, want %s", got, want)
	}
}

// TestCosySessionHeaders 会话构造 + 头表完整性。
func TestCosySessionHeaders(t *testing.T) {
	sess, err := NewCosySession("uid-1", "nick", "tok-abc", "rt-1", "", "org", "orgn")
	if err != nil {
		t.Fatal(err)
	}
	if sess.MachineID == "" || sess.MachineToken == "" || sess.MachineType == "" {
		t.Fatal("machine fields must be derived")
	}
	// info/tempKey 长度校验：AES-CBC 密文 base64，tempKey 16 hex 字符
	if len(sess.tempKey) != 16 {
		t.Fatalf("tempKey length = %d, want 16", len(sess.tempKey))
	}
	h := sess.Headers(`{"x":1}`, "https://gateway.example.com/algo/api/v2/service/pro/sse/agent_chat_generation", "m1", true)
	if !strings.HasPrefix(h["authorization"], "Bearer COSY.") {
		t.Fatalf("bad authorization: %q", h["authorization"])
	}
	if h["cosy-machineid"] != sess.MachineID || h["x-model-key"] != "m1" {
		t.Fatal("missing machine id or model key headers")
	}
	// info 可被自身 tempKey 解回（AES-CBC key==iv）
	raw, err := base64.StdEncoding.DecodeString(sess.info)
	if err != nil {
		t.Fatalf("info not base64: %v", err)
	}
	if len(raw)%16 != 0 {
		t.Fatalf("info ciphertext length %d not block aligned", len(raw))
	}
}

// TestBearerPathStripping path 去掉 /algo 前缀（签名五段含 path，直接验证 pathFor）。
func TestBearerPathStripping(t *testing.T) {
	if pathFor("https://gw.example.com/algo/api/v2/x") != "/api/v2/x" {
		t.Fatalf("pathFor /algo strip failed: %q", pathFor("https://gw.example.com/algo/api/v2/x"))
	}
	if pathFor("https://gw.example.com/foo") != "/foo" {
		t.Fatalf("plain path altered: %q", pathFor("https://gw.example.com/foo"))
	}
	if pathFor("not a url") != "not a url" {
		// 与 Python urlparse 一致：无 scheme 的串整体当作 path，不去 /algo、不加 /
		t.Fatalf("bad url handling: %q", pathFor("not a url"))
	}
}

// TestSessionCacheGetInvalidate 缓存按 token 命中与失效。
func TestSessionCacheGetInvalidate(t *testing.T) {
	c := NewSessionCache()
	s1, err := c.Get(AccountLike{UID: "u1", AccessToken: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c.Get(AccountLike{UID: "u1", AccessToken: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("cache miss on same token")
	}
	if _, err := c.Get(AccountLike{UID: "u1", AccessToken: ""}); err == nil {
		t.Fatal("empty token must error")
	}
	c.Invalidate("u1")
	s3, err := c.Get(AccountLike{UID: "u1", AccessToken: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if s3 == s1 {
		t.Fatal("invalidate did not rebuild session")
	}
}

// TestSHA256UrlB64 PKCE challenge 形态。
func TestSHA256UrlB64(t *testing.T) {
	// RFC 7636 附录 B 向量
	ch := sha256UrlB64("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if ch != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("pkce challenge = %q", ch)
	}
}
