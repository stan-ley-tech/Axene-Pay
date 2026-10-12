// Package webhooks delivers signed payment event notifications to merchant
// endpoints (ADR-0010).
package webhooks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sealer encrypts endpoint signing secrets at rest with AES-256-GCM. The
// endpoint's public ID is authenticated data, so a ciphertext copied to
// another endpoint fails to open.
type Sealer struct {
	aead    cipher.AEAD
	version int32
}

// NewSealer returns a Sealer for a 32-byte key.
func NewSealer(key []byte, version int32) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("webhooks: secrets key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, version: version}, nil
}

// Seal returns nonce || ciphertext.
func (s *Sealer) Seal(plaintext, aad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plaintext, aad)
}

// Open reverses Seal.
func (s *Sealer) Open(blob, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(blob) < n {
		return nil, errors.New("webhooks: sealed secret too short")
	}
	pt, err := s.aead.Open(nil, blob[:n], blob[n:], aad)
	if err != nil {
		return nil, errors.New("webhooks: cannot decrypt signing secret")
	}
	return pt, nil
}

// newSecret returns a signing secret: whsec_ + 32 random bytes, base64url.
func newSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// SignatureHeader is the header carrying the delivery signature.
const SignatureHeader = "Axene-Signature"

// Sign returns the Axene-Signature header value for body sent at t:
// "t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>.<body>")>".
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

// Verify checks a signature header the way merchants should: the HMAC must
// match and the timestamp must be within tolerance of now (replay
// protection). It is exported so it serves as the reference implementation.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("webhooks: malformed signature header")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("webhooks: signature timestamp outside tolerance (%s)", d.Round(time.Second))
	}
	want := mac(secret, ts, body)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("webhooks: signature mismatch")
}

func mac(secret, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}
