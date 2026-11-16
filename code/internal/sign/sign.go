// Package sign implements Qoder's request encoding and COSY signing.
//
// It reproduces, byte for byte, what the official Qoder CLI sends:
//
//  1. A custom Base64 variant (three-segment rotation + custom alphabet,
//     '=' replaced by '$') used to encode the request body.
//  2. COSY session state: a random 16-byte session key is wrapped with the
//     server's RSA public key; the identity JSON is AES-128-CBC encrypted with
//     that same key acting as both key and IV.
//  3. The per-request bearer:
//     md5(payload_b64 \n cosyKey \n date \n body \n path)
package sign

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/fingerprint"
)

// ---------------------------------------------------------------------------
// Custom Base64 variant
// ---------------------------------------------------------------------------

// CustomAlphabet is the Qoder-specific Base64 alphabet.
const CustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// StdAlphabet is the standard Base64 alphabet, for the reverse mapping.
const StdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// PadChar replaces '=' in the encoded output.
const PadChar = '$'

var (
	stdToCustom [256]byte
	customToStd [256]byte
)

func init() {
	if len(CustomAlphabet) != 64 {
		panic("sign: custom alphabet must be 64 chars")
	}
	seen := make(map[byte]bool, 64)
	for i := 0; i < 64; i++ {
		c := CustomAlphabet[i]
		if seen[c] {
			panic("sign: duplicate byte in custom alphabet")
		}
		seen[c] = true
		stdToCustom[StdAlphabet[i]] = c
		customToStd[c] = StdAlphabet[i]
	}
	stdToCustom['='] = PadChar
	customToStd[PadChar] = '='
}

// QoderEncode encodes plain into Qoder's custom Base64 variant.
//
//	std  = standard_base64(plain)
//	a    = len(std) / 3
//	out  = map(std[n-a:] + std[a:n-a] + std[:a])
func QoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	var b strings.Builder
	b.Grow(n)
	b.WriteString(std[n-a:])
	b.WriteString(std[a : n-a])
	b.WriteString(std[:a])
	rearranged := b.String()

	out := make([]byte, len(rearranged))
	for i := 0; i < len(rearranged); i++ {
		out[i] = stdToCustom[rearranged[i]]
	}
	return string(out)
}

// QoderDecode reverses QoderEncode. Used by tests and diagnostics.
func QoderDecode(encoded string) ([]byte, error) {
	std := make([]byte, len(encoded))
	for i := 0; i < len(encoded); i++ {
		std[i] = customToStd[encoded[i]]
	}
	n := len(std)
	a := n / 3
	if n < 3*a {
		return nil, errors.New("sign: encoded length not divisible in three parts")
	}
	// encoded = R1 + R2 + R3 where R1 = std[:a], R2 = std[a:n-a], R3 = std[n-a:]
	r1, r2, r3 := std[:a], std[a:n-a], std[n-a:]
	original := make([]byte, 0, n)
	original = append(original, r3...)
	original = append(original, r2...)
	original = append(original, r1...)
	return base64.StdEncoding.DecodeString(string(original))
}

// ---------------------------------------------------------------------------
// AES helpers
// ---------------------------------------------------------------------------

// AESCBCEncrypt performs AES-128-CBC with PKCS#7 padding. Qoder's `info`
// field uses key == iv.
func AESCBCEncrypt(plain, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, fmt.Errorf("sign: aes key/iv must be 16 bytes, got %d/%d", len(key), len(iv))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := 16 - len(plain)%16
	data := make([]byte, len(plain)+pad)
	copy(data, plain)
	for i := len(plain); i < len(data); i++ {
		data[i] = byte(pad)
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return out, nil
}

// AESCBCDecrypt performs AES-128-CBC decryption with strict PKCS#7 checking.
func AESCBCDecrypt(data, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, errors.New("sign: aes key/iv must be 16 bytes")
	}
	if len(data) == 0 || len(data)%16 != 0 {
		return nil, fmt.Errorf("sign: ciphertext length %d not block aligned", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || pad > len(out) {
		return nil, errors.New("sign: bad PKCS7 padding")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, errors.New("sign: bad PKCS7 padding")
		}
	}
	return out[:len(out)-pad], nil
}

// AESGCMDecrypt decrypts an AES-GCM payload with a strict tag check.
// keyLen must be 16 or 32 bytes.
func AESGCMDecrypt(key, nonce, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("sign: gcm nonce must be %d bytes", gcm.NonceSize())
	}
	return gcm.Open(nil, nonce, sealed, nil)
}

// ChromiumDecryptV10 decrypts an Electron/Chromium os_crypt "v10" blob:
// "v10" + 12-byte nonce + AES-256-GCM ciphertext||tag.
func ChromiumDecryptV10(blob, key []byte) ([]byte, error) {
	if len(blob) < 3+12+16 || string(blob[:3]) != "v10" {
		return nil, errors.New("sign: not a v10 payload")
	}
	return AESGCMDecrypt(key, blob[3:15], blob[15:])
}

// ---------------------------------------------------------------------------
// RSA PKCS#1 v1.5
// ---------------------------------------------------------------------------

// ServerPubPEM is the RSA public key hardcoded in the official Qoder client.
// The private half lives on the server, so this key cannot be changed.
const ServerPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var (
	rsaOnce sync.Once
	rsaN    *big.Int
	rsaE    int
	rsaErr  error
)

func loadRSAPub() (*big.Int, int, error) {
	rsaOnce.Do(func() {
		block, _ := pem.Decode([]byte(ServerPubPEM))
		if block == nil {
			rsaErr = errors.New("sign: cannot decode server public key PEM")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			rsaErr = fmt.Errorf("sign: parse server public key: %w", err)
			return
		}
		rp, ok := pub.(*rsa.PublicKey)
		if !ok || rp.N == nil {
			rsaErr = errors.New("sign: server public key is not RSA")
			return
		}
		rsaN, rsaE = rp.N, rp.E
	})
	return rsaN, rsaE, rsaErr
}

// RSAPKCS1v15Encrypt encrypts plain with RSA PKCS#1 v1.5 (type 2).
//
// The exponentiation is done with math/big on purpose: Go's crypto/rsa refuses
// keys below 2048 bits in recent releases, while the Qoder server key is 1024
// bits and cannot be swapped out.
func RSAPKCS1v15Encrypt(plain []byte) ([]byte, error) {
	n, e, err := loadRSAPub()
	if err != nil {
		return nil, err
	}
	k := (n.BitLen() + 7) / 8
	if len(plain) > k-11 {
		return nil, errors.New("sign: message too long for RSA block")
	}
	psLen := k - 3 - len(plain)
	ps := make([]byte, 0, psLen)
	buf := make([]byte, psLen*2+16)
	for len(ps) < psLen {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		for _, b := range buf {
			if b == 0 || len(ps) == psLen {
				continue
			}
			ps = append(ps, b)
		}
	}
	em := make([]byte, 0, k)
	em = append(em, 0x00, 0x02)
	em = append(em, ps...)
	em = append(em, 0x00)
	em = append(em, plain...)

	m := new(big.Int).SetBytes(em)
	c := new(big.Int).Exp(m, big.NewInt(int64(e)), n)
	out := make([]byte, k)
	cb := c.Bytes()
	copy(out[k-len(cb):], cb)
	return out, nil
}

// ---------------------------------------------------------------------------
// Sorted compact JSON
// ---------------------------------------------------------------------------

// JSONSortedCompact renders mapping as compact JSON with keys sorted and no
// whitespace. The server binds its signature bytes to this exact form.
func JSONSortedCompact(mapping map[string]any) []byte {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		v := mapping[k]
		if v == nil {
			v = ""
		}
		vb, err := marshalNoEscape(v)
		if err != nil {
			vb = []byte(`""`)
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

func marshalNoEscape(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}

// ---------------------------------------------------------------------------
// COSY session
// ---------------------------------------------------------------------------

// DefaultUserType is used when the account carries no user type.
const DefaultUserType = "personal_professional_trial"

// Identity keys carried inside the encrypted `info` blob. Adding or removing a
// key changes what the server decrypts, so this list is protocol, not config.
var identityKeys = []string{
	"name", "aid", "uid", "yx_uid", "organization_id", "organization_name",
	"user_type", "security_oauth_token", "refresh_token",
}

// Session is one account's COSY signing state.
type Session struct {
	UID             string
	MachineID       string
	MachineToken    string
	MachineType     string
	MachineHostname string
	AccessToken     string
	TempKey         string
	CosyKey         string
	Info            string
}

// NewSession builds a COSY session for one account.
func NewSession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName string) (*Session, error) {
	if userType == "" {
		userType = DefaultUserType
	}
	tmp := randomHex(16) // 16 ASCII chars act as the AES-128 key/iv
	if len(tmp) < 16 {
		return nil, errors.New("sign: cannot generate session key")
	}
	tmp = tmp[:16]

	wrapped, err := RSAPKCS1v15Encrypt([]byte(tmp))
	if err != nil {
		return nil, err
	}
	identity := map[string]any{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      orgID,
		"organization_name":    orgName,
		"user_type":            userType,
		"security_oauth_token": accessToken,
		"refresh_token":        refreshToken,
	}
	plain := JSONSortedCompact(identity)
	enc, err := AESCBCEncrypt(plain, []byte(tmp), []byte(tmp))
	if err != nil {
		return nil, err
	}
	return &Session{
		UID:             uid,
		MachineID:       fingerprint.DeriveMachineID(uid),
		MachineToken:    fingerprint.DeriveMachineToken(uid),
		MachineType:     fingerprint.DeriveMachineType(uid),
		MachineHostname: fingerprint.DeriveHostname(uid),
		AccessToken:     accessToken,
		TempKey:         tmp,
		CosyKey:         base64.StdEncoding.EncodeToString(wrapped),
		Info:            base64.StdEncoding.EncodeToString(enc),
	}, nil
}

// Bearer returns (payloadB64, date, bearerToken).
// The signature covers the encoded body and the request path with the leading
// "/algo" stripped, so switching gateway hosts does not invalidate it.
func (s *Session) Bearer(body, rawURL string) (string, string, string) {
	path := "/"
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	path = strings.TrimPrefix(path, "/algo")

	payload := map[string]any{
		"cosyVersion": CosyVersion(),
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   randomUUID(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(JSONSortedCompact(payload))
	date := fmt.Sprintf("%d", time.Now().Unix())
	raw := payloadB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + path
	sum := md5.Sum([]byte(raw))
	sig := hex.EncodeToString(sum[:])
	return payloadB64, date, "Bearer COSY." + payloadB64 + "." + sig
}

// Headers builds the full COSY header set for one upstream call.
func (s *Session) Headers(body, rawURL, modelKey string, sse bool, accept string) map[string]string {
	_, date, bearer := s.Bearer(body, rawURL)
	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-clienttype":       CosyClientType(),
		"cosy-date":             date,
		"cosy-user":             s.UID,
		"cosy-key":              s.CosyKey,
		"accept":                accept,
		"authorization":         bearer,
		"accept-encoding":       "identity",
		"cosy-version":          CosyVersion(),
		"cosy-machineid":        s.MachineID,
		"cosy-machinetoken":     s.MachineToken,
		"cosy-machinetype":      s.MachineType,
		"cosy-clientip":         "169.254.198.161",
		"login-version":         "v2",
		"user-agent":            CosyUserAgent(),
	}
	if CosyOSHeadersEnabled() {
		h["cosy-machineos"] = CosyMachineOS()
		if sse {
			h["cosy-machinehostname"] = s.MachineHostname
		}
	}
	if sse {
		h["cache-control"] = "no-cache"
	}
	if modelKey != "" {
		h["x-model-key"] = modelKey
		h["x-model-source"] = "system"
	}
	return h
}

// SessionCache caches one Session per account, invalidated when the access
// token rotates.
type SessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	token   string
	session *Session
}

// NewSessionCache returns a ready-to-use cache.
func NewSessionCache() *SessionCache {
	return &SessionCache{entries: make(map[string]sessionEntry)}
}

// Get returns the cached session for an account, building one when needed.
func (c *SessionCache) Get(a AccountLike) (*Session, error) {
	token := a.AccessTokenValue()
	if token == "" {
		return nil, errors.New("cosy: empty access token")
	}
	key := a.UIDValue()
	if key == "" {
		key = token[:min(16, len(token))]
	}
	c.mu.Lock()
	if hit, ok := c.entries[key]; ok && hit.token == token {
		c.mu.Unlock()
		return hit.session, nil
	}
	c.mu.Unlock()

	sess, err := NewSession(a.UIDValue(), a.NicknameValue(), token,
		a.RefreshTokenValue(), a.UserTypeValue(), a.OrgIDValue(), a.OrgNameValue())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = sessionEntry{token: token, session: sess}
	c.mu.Unlock()
	return sess, nil
}

// Invalidate drops the cached session for one UID.
func (c *SessionCache) Invalidate(uid string) {
	c.mu.Lock()
	delete(c.entries, uid)
	c.mu.Unlock()
}

// AccountLike is the minimal account surface a COSY session needs.
type AccountLike interface {
	UIDValue() string
	NicknameValue() string
	AccessTokenValue() string
	RefreshTokenValue() string
	UserTypeValue() string
	OrgIDValue() string
	OrgNameValue() string
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// time-derived value so the caller still gets a usable key.
		return hex.EncodeToString([]byte(fmt.Sprintf("%016x", time.Now().UnixNano())))
	}
	s := hex.EncodeToString(buf)
	return s[:n]
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SHA256HexShort returns the first n hex characters of sha256(text).
func SHA256HexShort(text string, n int) string {
	sum := sha256.Sum256([]byte(text))
	s := hex.EncodeToString(sum[:])
	if n > len(s) {
		n = len(s)
	}
	return s[:n]
}

func sortStrings(s []string) {
	// Insertion sort keeps this dependency-free and the slices are tiny.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MarshalNoEscape is exported for callers that need Python-compatible JSON.
func MarshalNoEscape(v any) ([]byte, error) { return marshalNoEscape(v) }
