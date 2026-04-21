package nova

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SignJWT mints an ES256 JWT that Nova's auth-callout will accept as the
// MQTT password for this gateway. The exact wire format matches the
// scheme used in srcful-novacore/services/auth-callout/main.go:
//
//	header  = {"alg":"ES256", "typ":"JWT", "device": gatewaySerial}
//	payload = {"iat":..., "exp":..., "jti": random-hex}
//
// The signature is raw R||S (not DER), 64 bytes, base64url-no-pad.
// ttl controls the exp claim; a few minutes is plenty — reconnect mints
// a fresh one.
func (id *Identity) SignJWT(gatewaySerial string, ttl time.Duration) (string, error) {
	if gatewaySerial == "" {
		return "", fmt.Errorf("nova: gateway serial is empty")
	}
	header := map[string]string{"alg": "ES256", "typ": "JWT", "device": gatewaySerial}
	now := time.Now()
	payload := map[string]any{
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": randomHex(16),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) +
		"." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, id.priv, hash[:])
	if err != nil {
		return "", fmt.Errorf("nova: jwt sign: %w", err)
	}
	sig := make([]byte, 64)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
