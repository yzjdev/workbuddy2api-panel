// Package qoder 实现 Qoder（qoder.com.cn / qoder.com）上游的 COSY 签名、
// 设备指纹派生、自定义 Base64 与 OAuth 设备授权。
//
// 协议事实来源：官方 qodercli 1.1.75 的逆向结论（与 qoder2api-hub 同源口径）。
// 签名五段式：
//
//	tempKey(16 ASCII hex) 随机会话密钥
//	cosyKey  = base64(RSA_PKCS1v15(tempKey))
//	info     = base64(AES-128-CBC(sorted-compact identity_json, key=iv=tempKey))
//	payload  = base64(sorted-compact {cosyVersion, ideVersion, info, requestId, version})
//	date     = unix 秒
//	sig      = md5(payload + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + path)
//	Bearer   = "COSY." + payload + "." + sig
//	path     = url.path 去掉 "/algo" 前缀
package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CosyVersion 对齐官方 qodercli 1.1.75（cosy-version 头与 payload 共用）。
const CosyVersion = "1.1.75"

// userAgent 官方 CLI 的推理/模型请求不设自定义 UA：请求经打包的 undici fetch
// 发出，由 undici 补默认值 "undici"。
const userAgent = "undici"

// serverPubPEM Qoder 服务端公钥（官方客户端硬编码，1024 位，e=65537）。
const serverPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var serverPubKey = mustParsePKIXPubKey(serverPubPEM)

func mustParsePKIXPubKey(pemStr string) *rsa.PublicKey {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		panic("qoder: bad server public key pem")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("qoder: parse server public key: " + err.Error())
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		panic("qoder: server public key is not RSA")
	}
	return rsaPub
}

// ---------------------------------------------------------------------------
// Qoder 自定义 Base64 变体：标准 base64 字符表换表 + 三段重排 + "$" 填充
// ---------------------------------------------------------------------------

const (
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderPad            = "$"
)

var customEncoding = mustNewCustomEncoding()

func mustNewCustomEncoding() *base64.Encoding {
	// base64.NewEncoding 要求填充符为 '='，故这里用标准填充做换表，
	// 逐字符映射到自定义字母表，填充符单独替换。
	enc := base64.StdEncoding
	_ = enc
	return base64.NewEncoding(qoderCustomAlphabet).WithPadding(base64.NoPadding)
}

// QoderEncode 把明文编码成 Qoder 自定义 Base64 变体（出站请求体格式）。
// 正向：std = b64(plain)；R = std[n-a:] + std[a:n-a] + std[:a]（尾/中/首三段），
// 再逐字符换表、'=' 替换为 '$'。
func QoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var b strings.Builder
	b.Grow(len(rearranged))
	for _, c := range []byte(rearranged) {
		if c == '=' {
			b.WriteByte(qoderPad[0])
			continue
		}
		idx := strings.IndexByte(qoderStdAlphabet, c)
		if idx < 0 {
			// 理论不可达：std 只含标准字母表与 '='
			b.WriteByte(c)
			continue
		}
		b.WriteByte(qoderCustomAlphabet[idx])
	}
	return b.String()
}

// QoderDecode 是 QoderEncode 的逆运算（测试与调试用）。
func QoderDecode(encoded string) ([]byte, error) {
	var b strings.Builder
	b.Grow(len(encoded))
	for _, c := range []byte(encoded) {
		if c == qoderPad[0] {
			b.WriteByte('=')
			continue
		}
		idx := strings.IndexByte(qoderCustomAlphabet, c)
		if idx < 0 {
			return nil, fmt.Errorf("qoder: invalid custom base64 char %q", c)
		}
		b.WriteByte(qoderStdAlphabet[idx])
	}
	std := b.String()
	n := len(std)
	a := n / 3
	r1, r2, r3 := std[:a], std[a:n-a], std[n-a:]
	original := r3 + r2 + r1
	return base64.StdEncoding.DecodeString(original)
}

// ---------------------------------------------------------------------------
// 设备指纹派生（与官方客户端形态对齐：UUIDv4 形状，版本位 4 / 变体位 a）
// ---------------------------------------------------------------------------

// DeriveID 由 uid + salt 稳定派生带连字符的 UUID 形状设备/会话标识。
// 幂等：同一账号每次调用产生相同值，避免随机机器码触发上游风控。
func DeriveID(uid, salt string) string {
	seed := salt + ":" + uid
	if uid == "" {
		seed = salt + ":anonymous"
	}
	sum := md5.Sum([]byte(seed))
	h := hex.EncodeToString(sum[:])
	// 伪造 UUIDv4 的版本位（第 13 位）与变体位（第 17 位），与官方同形。
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// DeriveMachineType 稳定派生 18 位去横线的 machine_type。
func DeriveMachineType(uid string) string {
	return strings.ReplaceAll(DeriveID(uid, "machinetype"), "-", "")[:18]
}

// DeriveMachineToken 稳定派生 machine_token（base64url 风格的随机串外观）。
func DeriveMachineToken(uid string) string {
	raw := sha512.Sum512([]byte("machinetoken:" + uid))
	return base64.RawURLEncoding.EncodeToString(raw[:])[:43]
}

// deriveHostname 按账号稳定派生主机名（形如 DESKTOP-1A2B3C4）。
func deriveHostname(uid string) string {
	sum := sha512.Sum512([]byte("hostname:" + uid))
	h := hex.EncodeToString(sum[:])
	return "DESKTOP-" + strings.ToUpper(h[:7])
}

// ---------------------------------------------------------------------------
// sorted-compact JSON：键排序 + 无空格（服务端签名字节与此强绑定）
// ---------------------------------------------------------------------------

// jsonSortedCompact 生成与 Python json.dumps(..., ensure_ascii=False,
// separators=(',',':'), sort_keys=True) 字节一致的紧凑 JSON。
// None → ""（空串）。值只支持 string（COSY 身份体与 payload 全部为字符串键值）。
func jsonSortedCompact(kv [][2]string) []byte {
	sort.Slice(kv, func(i, j int) bool { return kv[i][0] < kv[j][0] })
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range kv {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := marshalNoEscape(e[0])
		v, _ := marshalNoEscape(e[1])
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// marshalNoEscape json.Marshal 但关闭 HTML 转义（& < > 保持原样，
// 对齐 Python json.dumps 默认行为——服务端签名字节强绑定）。
func marshalNoEscape(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ---------------------------------------------------------------------------
// COSY 会话
// ---------------------------------------------------------------------------

// DefaultUserType 官方缺省 user_type。
const DefaultUserType = "personal_professional_trial"

// CosySession 单账号的 COSY 签名会话：machine 系列字段按账号 UID 稳定派生；
// tempKey/cosyKey/info 在会话创建时生成，access token 轮换后整体重建。
type CosySession struct {
	UID             string
	MachineID       string
	MachineToken    string
	MachineType     string
	MachineHostname string
	AccessToken     string

	tempKey string // 16 ASCII hex，AES-128 key == iv
	cosyKey string
	info    string
}

// NewCosySession 构造签名会话。
func NewCosySession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName string) (*CosySession, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("cosy: empty access token")
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	tempKey := hex.EncodeToString(keyBytes)[:16]
	ciphertext, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, []byte(tempKey))
	if err != nil {
		return nil, fmt.Errorf("cosy: rsa wrap: %w", err)
	}
	cosyKey := base64.StdEncoding.EncodeToString(ciphertext)

	identity := [][2]string{
		{"name", nickname},
		{"aid", uid},
		{"uid", uid},
		{"yx_uid", ""},
		{"organization_id", orgID},
		{"organization_name", orgName},
		{"user_type", orDefault(userType, DefaultUserType)},
		{"security_oauth_token", accessToken},
		{"refresh_token", refreshToken},
	}
	plain := jsonSortedCompact(identity)
	info, err := aesCBCEncrypt(plain, []byte(tempKey))
	if err != nil {
		return nil, err
	}
	return &CosySession{
		UID:             uid,
		MachineID:       DeriveID(uid, "machine"),
		MachineToken:    DeriveMachineToken(uid),
		MachineType:     DeriveMachineType(uid),
		MachineHostname: deriveHostname(uid),
		AccessToken:     accessToken,
		tempKey:         tempKey,
		cosyKey:         cosyKey,
		info:            base64.StdEncoding.EncodeToString(info),
	}, nil
}

func orDefault(v, dflt string) string {
	if v == "" {
		return dflt
	}
	return v
}

// pathFor 推理签名覆盖的 path：url.path 去掉 "/algo" 前缀。
func pathFor(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return "/"
	}
	return strings.TrimPrefix(u.Path, "/algo")
}

// bearer 返回 (payload_b64, date, Authorization 值)。
func (s *CosySession) bearer(body, rawURL string) (string, string, string) {
	path := pathFor(rawURL)
	requestID := newUUID()
	payload := jsonSortedCompact([][2]string{
		{"cosyVersion", CosyVersion},
		{"ideVersion", ""},
		{"info", s.info},
		{"requestId", requestID},
		{"version", "v1"},
	})
	payloadB64 := base64.StdEncoding.EncodeToString(payload)
	date := fmt.Sprintf("%d", time.Now().Unix())
	raw := strings.Join([]string{payloadB64, s.cosyKey, date, body, path}, "\n")
	sig := fmt.Sprintf("%x", md5.Sum([]byte(raw)))
	return payloadB64, date, "Bearer COSY." + payloadB64 + "." + sig
}

// Headers 构造一次推理/模型接口的完整 COSY 签名头（对齐 qodercli 1.1.75）。
func (s *CosySession) Headers(body, rawURL, modelKey string, sse bool) map[string]string {
	_, date, authorization := s.bearer(body, rawURL)
	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             s.UID,
		"cosy-key":              s.cosyKey,
		"accept":                "text/event-stream",
		"authorization":         authorization,
		"accept-encoding":       "identity",
		"cosy-version":          CosyVersion,
		"cosy-machineid":        s.MachineID,
		"cosy-machinetoken":     s.MachineToken,
		"cosy-machinetype":      s.MachineType,
		"cosy-clientip":         "169.254.198.161",
		"login-version":         "v2",
		"user-agent":            userAgent,
		"cosy-machineos":        "x86_64_win32",
		"cosy-machinehostname":  s.MachineHostname,
		"cache-control":         "no-cache",
	}
	if !sse {
		delete(h, "cache-control")
	}
	if modelKey != "" {
		h["x-model-key"] = modelKey
		h["x-model-source"] = "system"
	}
	return h
}

// SessionCache 账号 UID → CosySession 的进程级缓存；access token 变化即失效。
type SessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	token string
	sess  *CosySession
}

// NewSessionCache 构造空缓存。
func NewSessionCache() *SessionCache {
	return &SessionCache{entries: make(map[string]sessionEntry)}
}

// AccountLike 缓存所需的账号视图。
type AccountLike struct {
	UID              string
	Nickname         string
	AccessToken      string
	RefreshToken     string
	UserType         string
	OrganizationID   string
	OrganizationName string
}

// Get 取（或重建）该账号的签名会话。
func (c *SessionCache) Get(acct AccountLike) (*CosySession, error) {
	if acct.AccessToken == "" {
		return nil, fmt.Errorf("cosy: empty access token")
	}
	key := acct.UID
	if key == "" {
		key = acct.AccessToken[:16]
	}
	c.mu.Lock()
	hit, ok := c.entries[key]
	if ok && hit.token == acct.AccessToken {
		c.mu.Unlock()
		return hit.sess, nil
	}
	c.mu.Unlock()
	sess, err := NewCosySession(acct.UID, acct.Nickname, acct.AccessToken,
		acct.RefreshToken, acct.UserType, acct.OrganizationID, acct.OrganizationName)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = sessionEntry{token: acct.AccessToken, sess: sess}
	c.mu.Unlock()
	return sess, nil
}

// Invalidate 使某账号的缓存会话失效（token 刷新后调用）。
func (c *SessionCache) Invalidate(uid string) {
	c.mu.Lock()
	delete(c.entries, uid)
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// AES-128-CBC（key == iv，PKCS7 填充）
// ---------------------------------------------------------------------------

func aesCBCEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	return out, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(append([]byte{}, data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// newUUID 生成带横线的随机 UUIDv4 字符串。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端退化：时间戳兜底（与官方形态无关紧要，仅请求去重用）
		return fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffff, time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sha256UrlB64 PKCE challenge：base64url(sha256(verifier)) 去 padding。
func sha256UrlB64(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
