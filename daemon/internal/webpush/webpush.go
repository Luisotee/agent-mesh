// Package webpush sends a WebPush message (RFC 8030) encrypted for one subscription (RFC 8291, `aes128gcm`
// content coding of RFC 8188): what a UnifiedPush endpoint (ntfy, or any distributor) takes. Only the standard
// library's crypto is used; RFC 8291's own test vector pins it. No VAPID (RFC 8292): ntfy doesn't require it, and a
// push not encrypted with the device's keys fails to decrypt on the phone, which drops it.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// recordSize is the one record's size: the whole message is a single record.
	recordSize = 4096
	// MaxPlaintext is the largest payload Encrypt takes: one record (minus the delimiter and the GCM tag), well
	// under ntfy's 4,096-byte message limit with the 86-byte header.
	MaxPlaintext = 3000
	// MaxEndpoint is the longest endpoint URL accepted.
	MaxEndpoint = 2048
)

// DecodeKey decodes base64url (with or without padding), and standard base64 too, which some clients send.
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// ParseKeys checks a subscription's keys: p256dh an uncompressed P-256 point, auth 16 bytes.
func ParseKeys(p256dh, auth string) (pub, secret []byte, err error) {
	pub, err = DecodeKey(p256dh)
	if err != nil {
		return nil, nil, fmt.Errorf("p256dh is not base64url: %w", err)
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil || len(pub) != 65 {
		return nil, nil, errors.New("p256dh is not an uncompressed P-256 public key")
	}
	secret, err = DecodeKey(auth)
	if err != nil {
		return nil, nil, fmt.Errorf("auth is not base64url: %w", err)
	}
	if len(secret) != 16 {
		return nil, nil, fmt.Errorf("auth is %d bytes, not 16", len(secret))
	}
	return pub, secret, nil
}

// ValidEndpoint checks an endpoint URL: `https://`, or `http://` to a loopback host only (tests, a ntfy on the
// same machine), with no user info, at most MaxEndpoint bytes. meshd POSTs ciphertext there and nothing else.
func ValidEndpoint(raw string) (*url.URL, error) {
	if len(raw) > MaxEndpoint {
		return nil, fmt.Errorf("endpoint is longer than %d bytes", MaxEndpoint)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("endpoint is not a URL: %w", err)
	}
	if u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("endpoint must be an absolute http(s) URL with a host and no user info")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, errors.New("endpoint must be https (plain http only to a loopback host)")
		}
	default:
		return nil, errors.New("endpoint must be https")
	}
	return u, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Encrypt encrypts plaintext for a subscription (its p256dh public key and auth secret), as one aes128gcm record.
func Encrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return encrypt(plaintext, uaPublic, authSecret, salt, as)
}

func encrypt(plaintext, uaPublic, authSecret, salt []byte, as *ecdh.PrivateKey) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, fmt.Errorf("payload is %d bytes, more than %d", len(plaintext), MaxPlaintext)
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	asPublic := as.PublicKey().Bytes()
	cek, nonce, err := keys(as, ua, uaPublic, asPublic, authSecret, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	// The last (and only) record ends with the 0x02 delimiter; no padding.
	record := append(append([]byte{}, plaintext...), 2)
	var b bytes.Buffer
	b.Write(salt)
	_ = binary.Write(&b, binary.BigEndian, uint32(recordSize))
	b.WriteByte(byte(len(asPublic)))
	b.Write(asPublic)
	b.Write(gcm.Seal(nil, nonce, record, nil))
	return b.Bytes(), nil
}

// keys derives the content encryption key and nonce (RFC 8291 section 3.4, RFC 8188 section 2.2). priv is the
// local key of the ECDH (the sender's ephemeral key, or the device's own when decrypting), peer the other side.
func keys(priv *ecdh.PrivateKey, peer *ecdh.PublicKey, uaPublic, asPublic, authSecret, salt []byte) (cek, nonce []byte, err error) {
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, nil, err
	}
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Decrypt reverses Encrypt with the device's private key and auth secret: what a phone does. meshd itself never
// decrypts; tests and fake push services do.
func Decrypt(body []byte, ua *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("too short for an aes128gcm header")
	}
	salt, idLen := body[:16], int(body[20])
	if len(body) < 21+idLen {
		return nil, errors.New("too short for its key id")
	}
	asPublic, ct := body[21:21+idLen], body[21+idLen:]
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, fmt.Errorf("key id: %w", err)
	}
	cek, nonce, err := keys(ua, as, ua.PublicKey().Bytes(), asPublic, authSecret, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	// Strip the padding and the delimiter of the last record.
	i := bytes.LastIndexByte(pt, 2)
	if i < 0 || len(bytes.Trim(pt[i+1:], "\x00")) != 0 {
		return nil, errors.New("no last-record delimiter")
	}
	return pt[:i], nil
}

// Outcome is what a push service's answer means for the sender.
type Outcome int

const (
	// Delivered: the push service took the message (2xx).
	Delivered Outcome = iota
	// Gone: the subscription no longer exists (404, 410): forget the endpoint.
	Gone
	// Rejected: this message will never be taken (other 4xx but 429): drop it, keep the endpoint.
	Rejected
	// Retry: try again later (429, 5xx, a network error), after RetryAfter when the service said.
	Retry
)

func (o Outcome) String() string {
	return [...]string{"delivered", "gone", "rejected", "retry"}[o]
}

// Result is one POST's outcome.
type Result struct {
	Outcome    Outcome
	Status     int
	RetryAfter time.Duration
	Err        error
}

// Message is one push: the encrypted body and the RFC 8030 headers.
type Message struct {
	Body []byte
	// TTL is how long the push service keeps it for an offline device.
	TTL time.Duration
	// Urgency is "very-low", "low", "normal" or "high".
	Urgency string
	// Topic, when set, replaces a message with the same topic still waiting at the push service (at most 32
	// characters of the base64url alphabet).
	Topic string
}

// NewClient is the HTTP client for pushes: a 15 s timeout and no redirects, so a push goes only where the
// endpoint says.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Post sends one message to an endpoint.
func Post(ctx context.Context, client *http.Client, endpoint string, m Message) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(m.Body))
	if err != nil {
		return Result{Outcome: Rejected, Err: err}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL/time.Second)))
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Outcome: Retry, Err: err}
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	r := Result{Status: resp.StatusCode}
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300:
		r.Outcome = Delivered
		return r
	case c == http.StatusNotFound || c == http.StatusGone:
		r.Outcome = Gone
	case c == http.StatusTooManyRequests || c >= 500:
		r.Outcome = Retry
		r.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	default:
		r.Outcome = Rejected
	}
	msg := strings.TrimSpace(string(snippet))
	if msg == "" {
		r.Err = fmt.Errorf("push service answered %s", resp.Status)
	} else {
		r.Err = fmt.Errorf("push service answered %s: %s", resp.Status, msg)
	}
	return r
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
