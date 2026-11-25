// Command mesh-relay is the self-hosted relay for agent-mesh. Daemons dial out to --listen (HTTP/WS,
// intended to sit behind nginx-proxy-manager TLS on 443); paired apps connect to --client-listen (raw TCP,
// carrying end-to-end pinned TLS after the MESH1 preamble). The relay never inspects or terminates inner TLS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
)

const (
	tokenEnv     = "AGENT_MESH_RELAY_TOKEN"
	tokenFileEnv = "AGENT_MESH_RELAY_TOKEN_FILE"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP/WebSocket listen address for daemon control, dial-back streams, and /healthz")
	clientListen := flag.String("client-listen", ":8443", "raw TCP listen address for native clients (MESH1 preamble + raw splice); empty disables")
	tokenFile := flag.String("token-file", os.Getenv(tokenFileEnv), "file holding the relay shared secret (mode 0600, or $"+tokenEnv+")")
	genToken := flag.Bool("gen-token", false, "print a fresh random relay secret and exit")
	tlsCert := flag.String("tls-cert", "", "optional TLS certificate for --listen (omit when behind nginx-proxy-manager)")
	tlsKey := flag.String("tls-key", "", "optional TLS private key for --listen")
	maxLife := flag.Duration("max-lifetime", 0, "exit after this long (tests)")
	flag.Parse()

	if *genToken {
		fmt.Println(relay.NewSecret())
		return
	}
	secret, err := loadSecret(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh-relay:", err)
		os.Exit(2)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "mesh-relay: pass both --tls-cert and --tls-key, or neither")
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv, err := relay.NewServer(relay.ServerConfig{Secret: secret, Logger: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh-relay:", err)
		os.Exit(1)
	}

	httpLn, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh-relay:", err)
		os.Exit(1)
	}
	var tcpLn net.Listener
	if *clientListen != "" {
		tcpLn, err = net.Listen("tcp", *clientListen)
		if err != nil {
			httpLn.Close()
			fmt.Fprintln(os.Stderr, "mesh-relay:", err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *maxLife > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *maxLife)
		defer cancel()
	}

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() {
		if *tlsCert != "" {
			errc <- httpSrv.ServeTLS(relay.NoDelayListener(httpLn), *tlsCert, *tlsKey)
			return
		}
		errc <- httpSrv.Serve(relay.NoDelayListener(httpLn))
	}()
	if tcpLn != nil {
		go func() { errc <- srv.ServeClientTCP(ctx, tcpLn) }()
	}
	tcpAddr := ""
	if tcpLn != nil {
		tcpAddr = tcpLn.Addr().String()
	}
	log.Info("mesh-relay serving", "listen", httpLn.Addr().String(), "client_listen", tcpAddr)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}

	srv.Close()
	if tcpLn != nil {
		_ = tcpLn.Close()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		_ = httpSrv.Close()
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if serveErr != nil {
		fmt.Fprintln(os.Stderr, "mesh-relay:", serveErr)
		os.Exit(1)
	}
}

func loadSecret(path string) (string, error) {
	if path != "" {
		return fsutil.ReadSecret(path)
	}
	if v := strings.TrimSpace(os.Getenv(tokenEnv)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("give --token-file, $%s, or $%s", tokenFileEnv, tokenEnv)
}
