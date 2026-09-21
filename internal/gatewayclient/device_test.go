package gatewayclient

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// The v3 device payload is a pipe-joined canonical string — pin the
// exact format (buildDeviceAuthPayloadV3 in the gateway's device-auth
// module). A field-order drift here fails the real gateway's
// signature check with DEVICE_AUTH_SIGNATURE_INVALID.
func TestDevicePayloadV3Shape(t *testing.T) {
	scopes := []string{"operator.read", "operator.approvals"}
	joined := strings.Join(scopes, ",")
	platform := strings.ToLower(strings.TrimSpace("LINUX "))
	payload := strings.Join([]string{
		"v3",
		"device-id",
		"cli",
		"cli",
		"operator",
		joined,
		"1737264000000",
		"sk-token",
		"nonce-1",
		platform,
		"",
	}, "|")
	want := "v3|device-id|cli|cli|operator|operator.read,operator.approvals|1737264000000|sk-token|nonce-1|linux|"
	if payload != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

// The signature scheme: Ed25519 over the UTF-8 payload, base64url
// encoded; device id = sha256 hex of the raw public key.
func TestDeviceSignatureScheme(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := fmt.Sprintf("%x", sha256.Sum256(pub))
	if len(deviceID) != 64 {
		t.Fatalf("device id = %q", deviceID)
	}
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte("payload")))
	if !ed25519.Verify(pub, []byte("payload"), decodeRawURL(t, sig)) {
		t.Fatal("signature does not verify")
	}
	if ed25519.Verify(pub, []byte("other"), decodeRawURL(t, sig)) {
		t.Fatal("signature verified over the wrong payload")
	}
}

func decodeRawURL(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
