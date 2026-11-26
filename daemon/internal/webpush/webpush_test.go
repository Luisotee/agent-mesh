package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291, Appendix A: the same keys, salt and plaintext give the RFC's message byte for byte.
func TestRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPub := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	auth := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	got, err := encrypt([]byte("When I grow up, I want to be a watermelon"), uaPub, auth, salt, as)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := base64.RawURLEncoding.EncodeToString(got); g != want {
		t.Fatalf("got  %s\nwant %s", g, want)
	}
	ua, err := ecdh.P256().NewPrivateKey(b64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(got, ua, auth)
	if err != nil || string(pt) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("decrypt: %q %v", pt, err)
	}
}

func device(t *testing.T) (*ecdh.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return k, auth
}

func TestRoundTripAndLimits(t *testing.T) {
	k, auth := device(t)
	body, err := Encrypt([]byte(`{"v":1}`), k.PublicKey().Bytes(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Decrypt(body, k, auth); err != nil || string(pt) != `{"v":1}` {
		t.Fatalf("%q %v", pt, err)
	}
	other := make([]byte, 16)
	if _, err := Decrypt(body, k, other); err == nil {
		t.Fatal("decrypted with the wrong auth secret")
	}
	if _, err := Encrypt(make([]byte, MaxPlaintext+1), k.PublicKey().Bytes(), auth); err == nil {
		t.Fatal("took an oversized payload")
	}
}

func TestParseKeysAndEndpoint(t *testing.T) {
	k, auth := device(t)
	pub := base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	if _, _, err := ParseKeys(pub, base64.RawURLEncoding.EncodeToString(auth)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseKeys(pub+"==", base64.URLEncoding.EncodeToString(auth)); err != nil {
		t.Fatalf("padded: %v", err)
	}
	if _, _, err := ParseKeys(pub, "AAAA"); err == nil {
		t.Fatal("short auth accepted")
	}
	if _, _, err := ParseKeys("BAAA", base64.RawURLEncoding.EncodeToString(auth)); err == nil {
		t.Fatal("bad key accepted")
	}
	for raw, ok := range map[string]bool{
		"https://ntfy.sh/upAbc?up=1": true,
		"http://127.0.0.1:8080/up":   true,
		"http://[::1]:9/up":          true,
		"http://localhost/up":        true,
		"http://ntfy.example.com/up": false,
		"http://10.0.0.5/up":         false,
		"ftp://ntfy.sh/up":           false,
		"https://user:pw@ntfy.sh/up": false,
		"/relative":                  false,
		"https:///nohost":            false,
		"https://:443/up":            false,
	} {
		if _, err := ValidEndpoint(raw); (err == nil) != ok {
			t.Errorf("ValidEndpoint(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

func TestPostOutcomes(t *testing.T) {
	var status int
	var hdr http.Header
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		got, _ = io.ReadAll(r.Body)
		switch status {
		case 429:
			w.Header().Set("Retry-After", "7")
		case 302:
			w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	m := Message{Body: []byte("x"), TTL: time.Hour, Urgency: "high", Topic: "abc"}
	for code, want := range map[int]Outcome{201: Delivered, 404: Gone, 410: Gone, 400: Rejected, 413: Rejected, 429: Retry, 503: Retry, 302: Rejected} {
		status = code
		r := Post(context.Background(), NewClient(), srv.URL, m)
		if r.Outcome != want {
			t.Errorf("%d: got %v (%v), want %v", code, r.Outcome, r.Err, want)
		}
		if code == 429 && r.RetryAfter != 7*time.Second {
			t.Errorf("Retry-After: %v", r.RetryAfter)
		}
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("TTL") != "3600" || hdr.Get("Urgency") != "high" || hdr.Get("Topic") != "abc" || string(got) != "x" {
		t.Fatalf("headers %v body %q", hdr, got)
	}
	srv.Close()
	if r := Post(context.Background(), NewClient(), srv.URL, m); r.Outcome != Retry || r.Err == nil {
		t.Fatalf("closed server: %+v", r)
	}
}
