// Package tailnet is meshd's built-in Tailscale node (`meshd --tailscale`): the daemon joins a tailnet itself,
// through tsnet, with nothing installed on the host. It implements daemon.Tailnet. Only this package and
// cmd/meshd import tsnet, so the daemon's own tests never compile it, and none joins a tailnet.
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"tailscale.com/feature/buildfeatures"
	"tailscale.com/tsnet"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// BuildTags are the tags meshd is built with (install.sh) for --tailscale: no log upload to Tailscale, and no
// peerapi server of Tailscale's own on the node.
const BuildTags = "ts_omit_logtail,ts_omit_peerapiserver"

// CheckBuild refuses a binary built without BuildTags: tsnet would otherwise upload its logs to
// log.tailscale.com and serve Tailscale's peerapi on the node.
func CheckBuild() error {
	var missing []string
	if buildfeatures.HasLogTail {
		missing = append(missing, "ts_omit_logtail (tsnet would upload its logs to log.tailscale.com)")
	}
	if buildfeatures.HasPeerAPIServer {
		missing = append(missing, "ts_omit_peerapiserver (Tailscale's peerapi would listen on the node)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("--tailscale needs meshd built with -tags %s (install.sh does); missing: %s",
			BuildTags, strings.Join(missing, "; "))
	}
	return nil
}

// Config is how the node joins.
type Config struct {
	// Dir holds the node's key and state (<state>/tailscale); keep it, or the node joins again under a new
	// address and every pairing link to the old one stops working.
	Dir string
	// Hostname is the node's name on the tailnet.
	Hostname string
	// Port is the tailnet port meshd serves on (443 when 0).
	Port uint16
	// ControlURL is the control server: a Headscale URL, or empty for Tailscale's own.
	ControlURL string
	// AuthKey logs the node in without a browser; used only while the node needs login.
	AuthKey string
	Logger  *slog.Logger
}

// Node is a tsnet node.
type Node struct {
	srv        *tsnet.Server
	port       uint16
	hostname   string
	controlURL string
}

// New makes the node; it joins on the first Listen. Under test it refuses Tailscale's own control server, so no
// test can reach a real tailnet.
func New(cfg Config) (*Node, error) {
	if cfg.Dir == "" {
		return nil, errors.New("tailnet: no state dir")
	}
	if testing.Testing() && cfg.ControlURL == "" {
		return nil, errors.New("tailnet: a test must give its own control URL (testcontrol), never Tailscale's")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Port == 0 {
		cfg.Port = 443
	}
	return &Node{
		port:       cfg.Port,
		hostname:   cfg.Hostname,
		controlURL: cfg.ControlURL,
		srv: &tsnet.Server{
			Dir:        cfg.Dir,
			Hostname:   cfg.Hostname,
			ControlURL: cfg.ControlURL,
			AuthKey:    cfg.AuthKey,
			Ephemeral:  false,
			Logf: func(format string, args ...any) {
				log.Debug("tailscale: " + fmt.Sprintf(format, args...))
			},
			UserLogf: dedupe(func(line string) { log.Info("tailscale: " + line) }),
		},
	}, nil
}

// dedupe drops a user log line equal to the one before it: tsnet repeats the login URL every 5 s while the node
// waits for it.
func dedupe(emit func(string)) func(string, ...any) {
	var mu sync.Mutex
	last := ""
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mu.Lock()
		same := line == last
		last = line
		mu.Unlock()
		if !same {
			emit(line)
		}
	}
}

// Listen listens on the node's tailnet addresses, starting the node first. It doesn't wait for login.
func (n *Node) Listen(network, addr string) (net.Listener, error) { return n.srv.Listen(network, addr) }

// Port is the tailnet port meshd serves on.
func (n *Node) Port() uint16 { return n.port }

// Status is the node's state, from tsnet's local API.
func (n *Node) Status(ctx context.Context) (*v1.TailnetStatusResponse, error) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := &v1.TailnetStatusResponse{
		State:      st.BackendState,
		AuthUrl:    st.AuthURL,
		Hostname:   n.hostname,
		ControlUrl: n.controlURL,
		Problems:   st.Health,
	}
	for _, ip := range st.TailscaleIPs {
		out.Addresses = append(out.Addresses, ip.String())
	}
	if st.Self != nil {
		out.DnsName = st.Self.DNSName
	}
	return out, nil
}

// Close leaves the tailnet; the node stays registered in Dir.
func (n *Node) Close() error { return n.srv.Close() }
