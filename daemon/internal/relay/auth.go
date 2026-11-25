// Package relay implements the zero-trust TCP/WebSocket stream relay: daemons dial out and keep a control
// WebSocket; for each client connection the relay asks the daemon to dial back with a one-time ticket and
// splices the raw byte streams without terminating or inspecting inner TLS.
package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"

	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
)

// SecretPrefix identifies relay shared secrets and derived client relay tokens.
const SecretPrefix = "amr_"

// NewSecret returns a fresh 256-bit relay secret.
func NewSecret() string { return ids.Secret(SecretPrefix) }

// ClientToken derives a per-host client relay token from the daemon/relay shared secret. Paired apps present
// this token to the relay so they can connect only to that host without ever holding the daemon's secret.
func ClientToken(relaySecret, hostID string) string {
	mac := hmac.New(sha256.New, []byte(relaySecret))
	mac.Write([]byte("agentmesh-client-v1:" + hostID))
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ValidSecret reports whether got matches the relay secret in constant time.
func ValidSecret(want, got string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// ValidClientToken reports whether got is a valid client relay token for hostID.
func ValidClientToken(relaySecret, hostID, got string) bool {
	if relaySecret == "" || hostID == "" || got == "" {
		return false
	}
	want := ClientToken(relaySecret, hostID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
