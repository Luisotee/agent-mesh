package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

// BaseURL is the HTTPS placeholder URL clients pass to ConnectRPC / WebSocket constructors when routing over
// DialRelayTCP or DialRelayWS so http.Transport invokes DialTLSContext.
const BaseURL = "https://meshd"

var (
	// ErrAuth means the relay rejected the client relay token.
	ErrAuth = errors.New("relay rejected client token")
	// ErrOffline means the target daemon is not connected to the relay.
	ErrOffline = errors.New("daemon offline on relay")
	// ErrTimeout means the daemon did not dial back in time.
	ErrTimeout = errors.New("relay dial-back timed out")
)

// DialRelayTCP opens a raw TCP connection to relayTCPAddr, completes the MESH1 preamble, and upgrades the
// socket to end-to-end TLS pinned to certFingerprint.
func DialRelayTCP(ctx context.Context, relayTCPAddr, hostID, clientToken, certFingerprint string, nextProtos ...string) (net.Conn, error) {
	tlsCfg, err := tlsutil.PinnedConfig(certFingerprint, nextProtos...)
	if err != nil {
		return nil, err
	}
	raw, err := DialTCPNoDelay(ctx, "tcp", relayTCPAddr)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()

	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprintf(raw, "MESH1 %s %s\n", hostID, clientToken); err != nil {
		return nil, err
	}
	line, extra, err := readPreambleLine(raw, 512)
	if err != nil {
		return nil, fmt.Errorf("relay handshake: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	if line != "OK" {
		return nil, parseRelayError(line)
	}
	inner := raw
	if len(extra) > 0 {
		inner = &prefixConn{Conn: raw, extra: extra}
	}
	tc := tls.Client(inner, tlsCfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	ok = true
	return tc, nil
}

func parseRelayError(line string) error {
	rest, ok := strings.CutPrefix(line, "ERR ")
	if !ok {
		return fmt.Errorf("unexpected relay response %q", line)
	}
	code, msg, _ := strings.Cut(rest, " ")
	switch code {
	case "auth":
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case "offline":
		return fmt.Errorf("%w: %s", ErrOffline, msg)
	case "timeout":
		return fmt.Errorf("%w: %s", ErrTimeout, msg)
	default:
		return fmt.Errorf("relay %s: %s", code, msg)
	}
}

// DialRelayWS opens a WebSocket stream through /v1/client/connect and upgrades it to pinned end-to-end TLS.
func DialRelayWS(ctx context.Context, relayHTTP *http.Client, relayURL, hostID, clientToken, certFingerprint string, nextProtos ...string) (net.Conn, error) {
	tlsCfg, err := tlsutil.PinnedConfig(certFingerprint, nextProtos...)
	if err != nil {
		return nil, err
	}
	wsBase, err := toWSBase(relayURL)
	if err != nil {
		return nil, err
	}
	u := wsBase + "/v1/client/connect?host_id=" + url.QueryEscape(hostID)
	hdr := http.Header{"Authorization": {"Bearer " + clientToken}}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: relayHTTP,
		HTTPHeader: hdr,
	})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, ErrAuth
			case http.StatusServiceUnavailable:
				return nil, ErrOffline
			case http.StatusGatewayTimeout:
				return nil, ErrTimeout
			}
		}
		return nil, err
	}
	sc := NewWSConn(ws, nil, nil)
	tc := tls.Client(sc, tlsCfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = sc.Close()
		return nil, err
	}
	return tc, nil
}

// NewTCPHTTPClient returns an *http.Client that routes all requests through the relay's raw TCP endpoint
// with pinned end-to-end TLS. Set http2=false for WebSocket upgrades (/v1/term/{agent}).
func NewTCPHTTPClient(relayTCPAddr, hostID, clientToken, certFingerprint string, http2 bool) (*http.Client, error) {
	if _, err := tlsutil.ParseFingerprint(certFingerprint); err != nil {
		return nil, err
	}
	protos := []string{"http/1.1"}
	if http2 {
		protos = []string{"h2", "http/1.1"}
	}
	tr := &http.Transport{
		ForceAttemptHTTP2: http2,
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return DialRelayTCP(ctx, relayTCPAddr, hostID, clientToken, certFingerprint, protos...)
		},
		MaxIdleConnsPerHost: 4,
	}
	return &http.Client{Transport: tr}, nil
}

// NewDirectHTTPClient returns an *http.Client for direct HTTPS connections to `meshd --listen`, pinned to
// certFingerprint. Set http2=false for WebSocket upgrades (/v1/term/{agent}).
func NewDirectHTTPClient(certFingerprint string, http2 bool) (*http.Client, error) {
	protos := []string{"http/1.1"}
	if http2 {
		protos = []string{"h2", "http/1.1"}
	}
	tlsCfg, err := tlsutil.PinnedConfig(certFingerprint, protos...)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   http2,
		TLSClientConfig:     tlsCfg,
		DialContext:         DialTCPNoDelay,
		MaxIdleConnsPerHost: 4,
	}
	return &http.Client{Transport: tr}, nil
}
