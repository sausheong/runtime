package rheader

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	Timestamp = "X-Runtime-Timestamp"
	Nonce     = "X-Runtime-Nonce"
	Signature = "X-Runtime-Signature"
)

var (
	ErrSignatureMissing = errors.New("runtime identity signature missing")
	ErrSignatureExpired = errors.New("runtime identity signature expired")
	ErrSignatureInvalid = errors.New("runtime identity signature invalid")
)

// EncodePrivateKey and EncodePublicKey use raw URL-safe base64 so keys can be
// supplied consistently through environment variables and Kubernetes Secrets.
func EncodePrivateKey(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func GenerateKeyPair() (privateEncoded, publicEncoded string, err error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return EncodePrivateKey(privateKey), EncodePublicKey(publicKey), nil
}

func decodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: private key", ErrSignatureInvalid)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("%w: private key", ErrSignatureInvalid)
	}
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key", ErrSignatureInvalid)
	}
	return ed25519.PublicKey(raw), nil
}

// PublicKeyFor derives the verification key an agent needs from the control
// plane's private key. Ed25519 public keys are a function of the private key,
// so requiring an operator to supply both is busywork whose only possible
// outcome is a mismatch. Configure the public key explicitly only to pin it.
func PublicKeyFor(privateEncoded string) (string, error) {
	privateKey, err := decodePrivateKey(privateEncoded)
	if err != nil {
		return "", err
	}
	pub := privateKey.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(pub), nil
}

// ValidateKeyPair proves that the configured public key corresponds to the
// control plane's private key without exposing either value.
func ValidateKeyPair(privateEncoded, publicEncoded string) error {
	privateKey, err := decodePrivateKey(privateEncoded)
	if err != nil {
		return err
	}
	publicKey, err := decodePublicKey(publicEncoded)
	if err != nil {
		return err
	}
	if !privateKey.Public().(ed25519.PublicKey).Equal(publicKey) {
		return errors.New("runtime identity signing keys do not form a pair")
	}
	return nil
}

// Sign authenticates the internal identity projection with a control-plane-only
// Ed25519 private key. Method, request target, timestamp, and random nonce bind
// the projection to exactly one request.
func Sign(r *http.Request, privateEncoded string, now time.Time) error {
	privateKey, err := decodePrivateKey(privateEncoded)
	if err != nil {
		return err
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("runtime identity nonce: %w", err)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	r.Header.Set(Timestamp, ts)
	r.Header.Set(Nonce, nonce)
	r.Header.Set(Signature, base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(privateKey, signedMessage(r, ts, nonce)),
	))
	return nil
}

// Verify accepts a signed projection only within maxSkew of now. Replay
// prevention is lifecycle-owned by the receiving HTTP server and uses the
// returned nonce.
func Verify(r *http.Request, publicEncoded string, now time.Time, maxSkew time.Duration) (string, error) {
	ts := r.Header.Get(Timestamp)
	nonce := r.Header.Get(Nonce)
	got := r.Header.Get(Signature)
	if ts == "" || nonce == "" || got == "" {
		return "", ErrSignatureMissing
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrSignatureInvalid
	}
	delta := now.Sub(time.Unix(seconds, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > maxSkew {
		return "", ErrSignatureExpired
	}
	publicKey, err := decodePublicKey(publicEncoded)
	if err != nil {
		return "", err
	}
	signature, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil || !ed25519.Verify(publicKey, signedMessage(r, ts, nonce), signature) {
		return "", ErrSignatureInvalid
	}
	return nonce, nil
}

func signedMessage(r *http.Request, ts, nonce string) []byte {
	target := r.URL.EscapedPath()
	if target == "" {
		target = "/"
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return []byte(strings.Join([]string{
		ts,
		nonce,
		r.Method,
		target,
		r.Header.Get(User),
		r.Header.Get(Tenant),
		r.Header.Get(Role),
		r.Header.Get(Assertion),
	}, "\n"))
}
