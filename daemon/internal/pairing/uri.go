// Package pairing encodes and parses the canonical agentmesh://pair QR/link payload shared by the daemon,
// CLI, desktop app, and Android app.
package pairing

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

// Version is the pairing URI schema version.
const Version = "1"

// Payload is the structured data encoded in an agentmesh://pair URI.
type Payload struct {
	HostID           string
	HostName         string
	CertFingerprint  string
	Token            string
	RelayURL         string
	RelayTCP         string
	RelayClientToken string
	DirectURL        string
	DeviceName       string
}

// EncodeURI formats p as an agentmesh://pair?... URI.
func EncodeURI(p Payload) (string, error) {
	if strings.TrimSpace(p.HostID) == "" {
		return "", errors.New("pairing URI requires host_id")
	}
	if strings.TrimSpace(p.Token) == "" {
		return "", errors.New("pairing URI requires token")
	}
	sum, err := tlsutil.ParseFingerprint(p.CertFingerprint)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("v", Version)
	q.Set("h", p.HostID)
	q.Set("fp", tlsutil.FormatFingerprintBytes(sum))
	q.Set("t", p.Token)
	if p.HostName != "" {
		q.Set("hn", p.HostName)
	}
	if p.RelayURL != "" {
		q.Set("r", p.RelayURL)
	}
	if p.RelayTCP != "" {
		q.Set("rtcp", p.RelayTCP)
	}
	if p.RelayClientToken != "" {
		q.Set("rt", p.RelayClientToken)
	}
	if p.DirectURL != "" {
		q.Set("d", p.DirectURL)
	}
	if p.DeviceName != "" {
		q.Set("n", p.DeviceName)
	}
	return (&url.URL{Scheme: "agentmesh", Host: "pair", RawQuery: q.Encode()}).String(), nil
}

// ParseURI parses and validates an agentmesh://pair?... URI.
func ParseURI(raw string) (Payload, error) {
	var p Payload
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return p, fmt.Errorf("invalid pairing URI: %w", err)
	}
	if u.Scheme != "agentmesh" || u.Host != "pair" {
		return p, fmt.Errorf("unexpected pairing URI scheme/host %q", u.Scheme+"://"+u.Host)
	}
	q := u.Query()
	if v := q.Get("v"); v != Version {
		return p, fmt.Errorf("unsupported pairing URI version %q", v)
	}
	p.HostID = q.Get("h")
	p.Token = q.Get("t")
	if p.HostID == "" || p.Token == "" {
		return p, errors.New("pairing URI missing required host_id or token")
	}
	sum, err := tlsutil.ParseFingerprint(q.Get("fp"))
	if err != nil {
		return p, err
	}
	p.CertFingerprint = tlsutil.FormatFingerprintBytes(sum)
	p.HostName = q.Get("hn")
	p.RelayURL = q.Get("r")
	p.RelayTCP = q.Get("rtcp")
	p.RelayClientToken = q.Get("rt")
	p.DirectURL = q.Get("d")
	p.DeviceName = q.Get("n")
	return p, nil
}
