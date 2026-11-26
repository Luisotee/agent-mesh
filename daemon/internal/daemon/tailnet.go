package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Tailnet is the host's own Tailscale node (`meshd --tailscale`; the real one is internal/tailnet, over tsnet).
// meshd serves its TLS API on Listen exactly as on `--listen`: the same handler, certificate and device tokens.
type Tailnet interface {
	// Listen listens on the node's tailnet addresses (network "tcp", addr ":<port>").
	Listen(network, addr string) (net.Listener, error)
	// Port is the tailnet port meshd serves on.
	Port() uint16
	// Status is the node's state: every field but enabled and direct_url.
	Status(ctx context.Context) (*v1.TailnetStatusResponse, error)
	// Close leaves the tailnet (the node stays registered: its state dir keeps it).
	Close() error
}

// tailnetStateRunning is Tailscale's backend state once the node is up.
const tailnetStateRunning = "Running"

// serveTailnet serves the TLS API on the tailnet. Listening may wait on the control server, so it runs on its
// own: a node that can't come up leaves meshd serving everything else, and TailnetStatus says why.
func (d *Daemon) serveTailnet(ctx context.Context, tlsSrv *http.Server) {
	tn := d.cfg.Tailnet
	failed := func(what string, err error) {
		d.log.Error(what, "err", err)
		msg := what + ": " + err.Error()
		d.tailnetErr.Store(&msg)
	}
	go func() {
		ln, err := tn.Listen("tcp", fmt.Sprintf(":%d", tn.Port()))
		if err != nil {
			failed("listening on the tailnet", err)
			return
		}
		if ctx.Err() != nil {
			_ = ln.Close()
			return
		}
		d.log.Info("serving on the tailnet", "port", tn.Port())
		if err := tlsSrv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			failed("serving on the tailnet", err)
		}
	}()
}

// tailnetDirectURL is the address a pairing link carries for the tailnet node: its IPv4 address, which works
// whether or not the device resolves MagicDNS names. Empty until the node runs, and when meshd can't serve there.
func (d *Daemon) tailnetDirectURL(ctx context.Context) string {
	if d.cfg.Tailnet == nil || d.tailnetErr.Load() != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := d.cfg.Tailnet.Status(ctx)
	if err != nil || st.GetState() != tailnetStateRunning {
		return ""
	}
	return tailnetURL(st.GetAddresses(), d.cfg.Tailnet.Port())
}

func tailnetURL(addresses []string, port uint16) string {
	for _, a := range addresses {
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.Is4() {
			continue
		}
		if port == 443 {
			return "https://" + ip.String()
		}
		return "https://" + net.JoinHostPort(ip.String(), fmt.Sprint(port))
	}
	return ""
}

// TailnetStatus reports the host's tailnet node. Only the host's own device may call it: while the node needs
// login, the answer holds the URL that attaches it to whichever tailnet opens it.
func (d *Daemon) TailnetStatus(ctx context.Context, _ *connect.Request[v1.TailnetStatusRequest]) (*connect.Response[v1.TailnetStatusResponse], error) {
	if err := requireHostDevice(ctx); err != nil {
		return nil, err
	}
	if d.cfg.Tailnet == nil {
		return connect.NewResponse(&v1.TailnetStatusResponse{}), nil
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := d.cfg.Tailnet.Status(sctx)
	if err != nil {
		st = &v1.TailnetStatusResponse{Problems: []string{err.Error()}}
	}
	st.Enabled = true
	if e := d.tailnetErr.Load(); e != nil {
		st.Problems = append(st.Problems, *e)
	} else if st.GetState() == tailnetStateRunning {
		st.DirectUrl = tailnetURL(st.GetAddresses(), d.cfg.Tailnet.Port())
	}
	return connect.NewResponse(st), nil
}
