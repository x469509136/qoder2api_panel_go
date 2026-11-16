package sign

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/xzs/bridge_qoder/internal/fingerprint"
)

func TestQoderEncodeRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("{}"),
		[]byte(`{"a":1}`),
		[]byte(`{"messages":[{"role":"user","content":"你好，世界"}]}`),
		bytes.Repeat([]byte("x"), 1000),
	}
	for _, in := range cases {
		enc := QoderEncode(in)
		out, err := QoderDecode(enc)
		if err != nil {
			t.Fatalf("decode(%q): %v", in, err)
		}
		if !bytes.Equal(in, out) {
			t.Fatalf("round trip mismatch for %q: got %q", in, out)
		}
		// Padding must use '$', never '='.
		if bytes.ContainsRune([]byte(enc), '=') {
			t.Fatalf("encoded form still contains '=': %q", enc)
		}
		// Length matches padded Base64 (the property open_upstream relies on).
		want := 4 * ((len(in) + 2) / 3)
		if len(enc) != want {
			t.Fatalf("length %d, want %d", len(enc), want)
		}
	}
}

func TestQoderEncodeSegments(t *testing.T) {
	// "abc" -> standard Base64 "YWJj" (4 chars). a = 4/3 = 1.
	// rearranged = std[3:] + std[1:3] + std[:1] = "j" + "WJ" + "Y" = "jWJY"
	got := QoderEncode([]byte("abc"))
	// Map each char through the custom alphabet.
	want := string([]byte{
		stdToCustom['j'], stdToCustom['W'], stdToCustom['J'], stdToCustom['Y'],
	})
	if got != want {
		t.Fatalf("qoder encode: got %q want %q", got, want)
	}
}

func TestAESCBCCipher(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("hello qoder identity payload")
	enc, err := AESCBCEncrypt(plain, key, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc)%16 != 0 {
		t.Fatalf("ciphertext not block aligned: %d", len(enc))
	}
	dec, err := AESCBCDecrypt(enc, key, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("decrypt mismatch: %q", dec)
	}
	if _, err := AESCBCDecrypt(enc, []byte("fedcba9876543210"), key); err == nil {
		t.Fatal("expected padding error with the wrong key")
	}
}

func TestRSAPKCS1v15(t *testing.T) {
	out, err := RSAPKCS1v15Encrypt([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 128 {
		t.Fatalf("ciphertext length %d, want 128 (1024-bit key)", len(out))
	}
	// The ciphertext is pseudorandom; what must hold is that a message longer
	// than k-11 is rejected and that the result is < n.
	n, _, _ := loadRSAPub()
	if c := new(big.Int).SetBytes(out); c.Cmp(n) >= 0 {
		t.Fatal("ciphertext is not reduced mod n")
	}
	if _, err := RSAPKCS1v15Encrypt(make([]byte, 120)); err == nil {
		t.Fatal("expected 'message too long' for a 120-byte payload")
	}
}

func TestJSONSortedCompact(t *testing.T) {
	got := string(JSONSortedCompact(map[string]any{
		"b": 2, "a": "x", "c": nil,
	}))
	want := `{"a":"x","b":2,"c":""}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// Non-ASCII must not be escaped.
	got = string(JSONSortedCompact(map[string]any{"k": "中文"}))
	if got != `{"k":"中文"}` {
		t.Fatalf("non-ASCII escaped: %s", got)
	}
}

func TestSessionBearerShape(t *testing.T) {
	s, err := NewSession("uid-1", "nick", "dt-token", "drt-token", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.TempKey) != 16 {
		t.Fatalf("temp key length %d, want 16", len(s.TempKey))
	}
	payload, date, bearer := s.Bearer("encodedbody", "https://gateway.qoder.com.cn/algo/api/v2/x?Encode=1")
	if date == "" {
		t.Fatal("empty date")
	}
	if len(payload) == 0 {
		t.Fatal("empty payload")
	}
	if len(bearer) <= len("Bearer COSY.") || bearer[:12] != "Bearer COSY." {
		t.Fatalf("bad bearer: %q", bearer)
	}
	// The payload decodes to a sorted-compact JSON object with the five keys.
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, k := range []string{"cosyVersion", "ideVersion", "info", "requestId", "version"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("payload missing key %q: %s", k, raw)
		}
	}
	// Path in the signature has "/algo" stripped; signing must be stable for
	// two different hosts with the same path.
	_, _, bearer2 := s.Bearer("encodedbody", "https://api3.qoder.sh/algo/api/v2/x?Encode=1")
	// Only the payload's random requestId differs, so compare signature length.
	if len(bearer) != len(bearer2) {
		t.Fatalf("bearer length differs across hosts: %d vs %d", len(bearer), len(bearer2))
	}
}

func TestSessionHeaders(t *testing.T) {
	s, err := NewSession("uid-2", "", "dt-x", "", "personal_professional_trial", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Headers("body", "https://gateway.qoder.com.cn/algo/x", "qmodel_38max", true, "text/event-stream")
	must := []string{
		"cosy-data-policy", "content-type", "cosy-business-product",
		"cosy-business-type", "cosy-scene", "cosy-clienttype", "cosy-date",
		"cosy-user", "cosy-key", "accept", "authorization", "accept-encoding",
		"cosy-version", "cosy-machineid", "cosy-machinetoken", "cosy-machinetype",
		"cosy-clientip", "login-version", "user-agent", "cache-control",
		"x-model-key", "x-model-source",
	}
	for _, k := range must {
		if _, ok := h[k]; !ok {
			t.Fatalf("missing header %q", k)
		}
	}
	if h["x-model-key"] != "qmodel_38max" {
		t.Fatalf("x-model-key = %q", h["x-model-key"])
	}
	// Non-SSE requests must not carry cache-control or hostname.
	h2 := s.Headers("body", "https://gateway.qoder.com.cn/algo/x", "", false, "application/json")
	if _, ok := h2["cache-control"]; ok {
		t.Fatal("non-SSE request carried cache-control")
	}
	if _, ok := h2["cosy-machinehostname"]; ok {
		t.Fatal("non-SSE request carried cosy-machinehostname")
	}
}

func TestDeriveIDShapes(t *testing.T) {
	uid := "01a1df2a-48fe-7d45-b1d1-79323ae6bb09"
	fingerprint.SetLegacy(false)
	defer fingerprint.SetLegacy(false)

	id := fingerprint.DeriveMachineID(uid)
	if len(id) != 36 {
		t.Fatalf("UUID shape length %d, want 36: %q", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("not UUID-shaped: %q", id)
	}
	if id[14] != '4' {
		t.Fatalf("version nibble = %c, want 4: %q", id[14], id)
	}
	if id[19] != 'a' {
		t.Fatalf("variant nibble = %c, want a: %q", id[19], id)
	}
	// Deterministic.
	if fingerprint.DeriveMachineID(uid) != id {
		t.Fatal("derive is not deterministic")
	}
	// Different per account.
	if fingerprint.DeriveMachineID("other") == id {
		t.Fatal("two accounts share a machine id")
	}
	// Different per salt.
	if fingerprint.DeriveSessionID(uid) == id {
		t.Fatal("machine and session ids collide")
	}
	if got := fingerprint.DeriveMachineType(uid); len(got) != 18 {
		t.Fatalf("machine type length %d, want 18: %q", len(got), got)
	}
	if got := fingerprint.DeriveMachineToken(uid); len(got) != 43 {
		t.Fatalf("machine token length %d, want 43: %q", len(got), got)
	}
	if got := fingerprint.DeriveHostname(uid); len(got) != 15 || got[:8] != "DESKTOP-" {
		t.Fatalf("hostname shape: %q", got)
	}
	if got := fingerprint.GenerateRequestID(uid); len(got) != 32 {
		t.Fatalf("request id length %d, want 32", len(got))
	}
}

func TestAESGCMRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	nonce := bytes.Repeat([]byte{1}, 12)
	plain := []byte("secret desktop credential")
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	sealed := gcm.Seal(nil, nonce, plain, nil)
	out, err := AESGCMDecrypt(key, nonce, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("gcm mismatch: %q", out)
	}
	// Corrupt tag must fail.
	sealed[len(sealed)-1] ^= 0xff
	if _, err := AESGCMDecrypt(key, nonce, sealed); err == nil {
		t.Fatal("expected GCM authentication failure")
	}
}

// TestQoderEncodeGolden pins the encoder to outputs produced by the reference
// Python implementation (qoder_sign.qoder_encode), so a refactor cannot drift.
func TestQoderEncodeGolden(t *testing.T) {
	cases := []struct{ in, want string }{
		{"{}", "$kwm"},
		{`{"a":1}`, "ep$$BMn%mYKi"},
		{`{"messages":[{"role":"user","content":"hello"}]}`,
			"wDSQwBMn*PHFruHq*eF.WOWrD,ByBEF^DxB*GoKMu(QmYKtDxj^#SJLNYBySkr*N"},
		{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			"ImZiImZiImZiIm_$$ZiImZiImZiImZiImZimZiImZiImZiImZiIm"},
	}
	for _, c := range cases {
		if got := QoderEncode([]byte(c.in)); got != c.want {
			t.Fatalf("QoderEncode(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
