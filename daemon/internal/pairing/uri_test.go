package pairing

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestURIRoundTrip(t *testing.T) {
	sum := sha256.Sum256([]byte("cert"))
	in := Payload{
		HostID:           "h1234567890ab",
		HostName:         "workstation",
		CertFingerprint:  "SHA256:" + hex.EncodeToString(sum[:]),
		Token:            "amp_secret",
		RelayURL:         "https://relay.example.com",
		RelayTCP:         "relay.example.com:8443",
		RelayClientToken: "amr_client",
		DirectURL:        "https://100.64.0.1:9443",
		DeviceName:       "Pixel 9",
	}
	uri, err := EncodeURI(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	wantFP := "sha256:" + hex.EncodeToString(sum[:])
	if got.HostID != in.HostID || got.HostName != in.HostName || got.CertFingerprint != wantFP ||
		got.Token != in.Token || got.RelayURL != in.RelayURL || got.RelayTCP != in.RelayTCP ||
		got.RelayClientToken != in.RelayClientToken || got.DirectURL != in.DirectURL || got.DeviceName != in.DeviceName {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, in)
	}
}

func TestParseURIValidation(t *testing.T) {
	bad := []string{
		"https://example.com/pair",
		"agentmesh://other?v=1&h=a&t=b&fp=00",
		"agentmesh://pair?v=2&h=a&t=b&fp=" + string(make([]byte, 64)),
		"agentmesh://pair?v=1&h=a&fp=bad",
	}
	for _, raw := range bad {
		if _, err := ParseURI(raw); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}
