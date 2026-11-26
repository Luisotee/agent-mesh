// Command meshd is agent-mesh's host daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/tailnet"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	self, _ := os.Executable()
	here := filepath.Dir(self)
	state := flag.String("state", os.Getenv(client.StateEnv), "state dir (or $"+client.StateEnv+"); required")
	tcp := flag.String("tcp", "", "also listen (plain HTTP) on this loopback address, e.g. 127.0.0.1:0")
	listen := flag.String("listen", os.Getenv("AGENT_MESH_LISTEN"), "opt-in TLS listen address for direct remote clients, e.g. :9443 (off by default)")
	relayURL := flag.String("relay-url", os.Getenv("AGENT_MESH_RELAY_URL"), "self-hosted relay URL (for example https://relay.example.com)")
	relayTCP := flag.String("relay-tcp", os.Getenv("AGENT_MESH_RELAY_TCP"), "relay raw TCP client address (for example relay.example.com:8443) for pairing payloads")
	relayTokenFile := flag.String("relay-token-file", os.Getenv("AGENT_MESH_RELAY_TOKEN_FILE"), "file holding the relay shared secret (mode 0600, or $AGENT_MESH_RELAY_TOKEN)")
	tsOn := flag.Bool("tailscale", envBool("AGENT_MESH_TAILSCALE"), "join a tailnet (Tailscale, or Headscale with --tailscale-control-url) and serve the TLS API there (or $AGENT_MESH_TAILSCALE=1)")
	tsHost := flag.String("tailscale-hostname", os.Getenv("AGENT_MESH_TAILSCALE_HOSTNAME"), "the node's name on the tailnet (default mesh-<host name>)")
	tsPort := flag.Uint("tailscale-port", 443, "the tailnet port the TLS API is served on")
	tsControl := flag.String("tailscale-control-url", os.Getenv("AGENT_MESH_TAILSCALE_CONTROL_URL"), "the control server, such as a Headscale URL (default Tailscale's)")
	tsKeyFile := flag.String("tailscale-auth-key-file", os.Getenv("AGENT_MESH_TAILSCALE_AUTH_KEY_FILE"), "file holding a Tailscale auth key (mode 0600, or $AGENT_MESH_TAILSCALE_AUTH_KEY): logs the node in without a browser")
	hostName := flag.String("host-name", os.Getenv("AGENT_MESH_HOST_NAME"), "readable host name advertised over relay and pairing (default hostname)")
	maxLife := flag.Duration("max-lifetime", 0, "exit after this long, and start mesh-term with the same "+
		"(tests: no orphan survives a killed test)")
	termLaunch := flag.String("term-launch", string(termclient.LaunchDetached),
		"how to start the terminal host when none answers: detached, systemd, external, or none (no agents)")
	termBinary := flag.String("term-binary", filepath.Join(here, "mesh-term"), "mesh-term, for --term-launch detached")
	termUnit := flag.String("term-unit", "agent-mesh-term.service", "the user unit, for --term-launch systemd")
	mesh := flag.String("mesh", filepath.Join(here, "mesh"), "the mesh CLI: the agents' hooks run it, and its folder goes first on their PATH")
	share := flag.String("share", filepath.Join(here, "..", "share"), "the daemon's protocol and skills (protocol/, skills/): agents get it as $"+client.ShareEnv+", their first doorbell names its protocol, and Claude agents load its skills; ignored when it isn't there")
	worktrees := flag.String("worktrees", "", "where agents' worktrees go (default <state>/worktrees)")
	policyFile := flag.String("policy-file", "", "bin/mesh's policy file (~/.config/agent-mesh.json): imported into the settings the first time meshd starts with this path (never read again; `mesh import-settings` re-applies it)")
	skip := flag.Bool("skip-permissions", false, "start agents with their CLI's permission prompts off")
	noLoginPath := flag.Bool("no-login-path", false, "do not resolve PATH from a login shell at startup")
	var bins, envs multi
	flag.Var(&bins, "cli", "kind=path of an agent CLI, e.g. claude=/usr/local/bin/claude (repeatable)")
	flag.Var(&envs, "agent-env", "KEY=VALUE added to every agent's environment (repeatable)")
	flag.Parse()
	// Agents run in worktrees: a relative path to the CLI would not lead anywhere there, nor name its folder.
	if abs, err := filepath.Abs(*mesh); err == nil {
		*mesh = abs
	}
	shareGiven := false
	flag.Visit(func(f *flag.Flag) { shareGiven = shareGiven || f.Name == "share" })
	if abs, err := filepath.Abs(*share); err == nil {
		if st, err := os.Stat(filepath.Join(abs, "protocol")); err == nil && st.IsDir() {
			*share = abs
		} else {
			*share = ""
		}
	} else {
		*share = ""
	}
	if shareGiven && *share == "" {
		fmt.Fprintln(os.Stderr, "meshd: --share has no protocol/ folder: agents get no protocol pointer and no skills")
	}
	if *state == "" {
		fmt.Fprintln(os.Stderr, "meshd: give --state or $"+client.StateEnv)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	agents := daemon.AgentConfig{TermBinary: *termBinary, TermUnit: *termUnit, Mesh: *mesh, Share: *share,
		Worktrees: *worktrees, SkipPermissions: *skip, Binaries: map[string]string{}, Env: map[string]string{}}
	if *termLaunch != "none" {
		agents.TermLaunch = termclient.Launch(*termLaunch)
	}
	if *maxLife > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *maxLife)
		defer cancel()
		agents.TermArgs = []string{"--max-lifetime", maxLife.String()}
	}
	for _, b := range bins {
		k, v, ok := strings.Cut(b, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "meshd: --cli %q: want kind=path\n", b)
			os.Exit(2)
		}
		agents.Binaries[k] = v
	}
	for _, e := range envs {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "meshd: --agent-env %q: want KEY=VALUE\n", e)
			os.Exit(2)
		}
		agents.Env[k] = v
	}
	if !*noLoginPath && agents.Env["PATH"] == "" {
		if resolved, err := daemon.ResolveLoginPath(ctx, os.Getenv("SHELL"), os.Getenv("HOME"), os.Getenv("PATH")); err != nil {
			log.Warn("login shell PATH resolution failed; using fallback PATH", "err", err)
			_ = os.Setenv("PATH", resolved)
		} else if resolved != "" {
			_ = os.Setenv("PATH", resolved)
		}
	}
	var relaySecret string
	if *relayURL != "" {
		if *relayTokenFile != "" {
			var err error
			relaySecret, err = fsutil.ReadSecret(*relayTokenFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "meshd: --relay-token-file:", err)
				os.Exit(2)
			}
		} else {
			relaySecret = strings.TrimSpace(os.Getenv("AGENT_MESH_RELAY_TOKEN"))
		}
		if relaySecret == "" {
			fmt.Fprintln(os.Stderr, "meshd: --relay-url needs --relay-token-file, $AGENT_MESH_RELAY_TOKEN_FILE, or $AGENT_MESH_RELAY_TOKEN")
			os.Exit(2)
		}
	}
	var tn daemon.Tailnet
	if *tsOn {
		node, err := startTailnet(*state, *hostName, *tsHost, *tsPort, *tsControl, *tsKeyFile, log)
		if err != nil {
			fmt.Fprintln(os.Stderr, "meshd:", err)
			os.Exit(2)
		}
		tn = node
	}
	d, err := daemon.New(daemon.Config{
		StateDir:   *state,
		PolicyFile: *policyFile,
		TCP:        *tcp,
		Listen:     *listen,
		Relay: daemon.RelayConfig{
			URL:      *relayURL,
			TCP:      *relayTCP,
			Token:    relaySecret,
			HostName: *hostName,
		},
		Tailnet: tn,
		Logger:  log,
		Now:     time.Now,
		Agents:  agents,
	})
	if err != nil {
		if tn != nil {
			_ = tn.Close()
		}
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
	if err := d.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// startTailnet makes the host's Tailscale node in <state>/tailscale. The auth key is read once and dropped from
// the environment, so nothing meshd starts inherits it.
func startTailnet(state, hostName, tsHost string, port uint, controlURL, keyFile string, log *slog.Logger) (*tailnet.Node, error) {
	if err := tailnet.CheckBuild(); err != nil {
		return nil, err
	}
	if port == 0 || port > 65535 {
		return nil, fmt.Errorf("--tailscale-port %d: want 1-65535", port)
	}
	var authKey string
	if keyFile != "" {
		var err error
		if authKey, err = fsutil.ReadSecret(keyFile); err != nil {
			return nil, fmt.Errorf("--tailscale-auth-key-file: %w", err)
		}
	} else {
		authKey = strings.TrimSpace(os.Getenv("AGENT_MESH_TAILSCALE_AUTH_KEY"))
	}
	_ = os.Unsetenv("AGENT_MESH_TAILSCALE_AUTH_KEY")
	if tsHost == "" {
		if hostName == "" {
			hostName, _ = os.Hostname()
		}
		tsHost = "mesh-" + hostName
	}
	return tailnet.New(tailnet.Config{
		Dir:        filepath.Join(state, "tailscale"),
		Hostname:   tsHost,
		Port:       uint16(port),
		ControlURL: strings.TrimSpace(controlURL),
		AuthKey:    authKey,
		Logger:     log,
	})
}
