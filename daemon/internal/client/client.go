// Package client dials a host's daemon the way every local caller does (the CLI, hooks, tests): it reads the
// daemon's address from its state dir at call time and sends a bearer token.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// BaseURL is the URL clients use over the unix socket; the host part is ignored.
const BaseURL = "http://meshd"

// TokenEnv is the environment variable that carries an agent's token.
const TokenEnv = "AGENT_MESH_TOKEN"

// StateEnv names the daemon's state dir.
const StateEnv = "AGENT_MESH_STATE"

// UnixHTTP returns an HTTP client whose every connection goes to a unix socket.
func UnixHTTP(socket string, dialTimeout time.Duration) *http.Client {
	d := net.Dialer{Timeout: dialTimeout}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 4,
	}}
}

// Options tune Dial.
type Options struct {
	// DialTimeout bounds connecting to the socket (default 2 s).
	DialTimeout time.Duration
}

// Token picks the caller's token: AGENT_MESH_TOKEN when set (an agent), else the host's device token.
func Token(stateDir string) (string, error) {
	if tok := os.Getenv(TokenEnv); tok != "" {
		return tok, nil
	}
	tok, err := fsutil.ReadSecret(daemon.Paths{Dir: stateDir}.DeviceToken())
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no %s and no device token in %s: has meshd run there?", TokenEnv, stateDir)
	}
	return tok, err
}

// Conn is a dialled daemon.
type Conn struct {
	HTTP  *http.Client
	Token string
	Mesh  agentmeshv1connect.MeshServiceClient
}

// Dial makes a client for the daemon of a state dir. It reads the socket's path now, not once per process,
// so it follows a restarted daemon.
func Dial(stateDir, token string, opts Options) (*Conn, error) {
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 2 * time.Second
	}
	socket := daemon.Paths{Dir: stateDir}.Socket()
	if info, err := daemon.ReadInfo(stateDir); err == nil && info.Socket != "" {
		socket = info.Socket
	}
	hc := UnixHTTP(socket, opts.DialTimeout)
	return &Conn{HTTP: hc, Token: token, Mesh: agentmeshv1connect.NewMeshServiceClient(hc, BaseURL,
		connect.WithInterceptors(auth.ClientToken(token)))}, nil
}
