package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
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
// own: a node that can't come up leaves meshd serving everything else, and TailnetStatus says why. The goroutine
// is in bgWG, so close() never closes the node while Listen is still starting it.
func (d *Daemon) serveTailnet(ctx context.Context, tlsSrv *http.Server) {
	tn := d.cfg.Tailnet
	failed := func(what string, err error) {
		d.log.Error(what, "err", err)
		msg := what + ": " + err.Error()
		d.tailnetErr.Store(&msg)
	}
	d.bgWG.Add(1)
	go func() {
		defer d.bgWG.Done()
		ln, err := tn.Listen("tcp", fmt.Sprintf(":%d", tn.Port()))
		if ctx.Err() != nil {
			// Shutting down: a listener that came too late, or a Listen cut short, is no failure.
			if err == nil {
				_ = ln.Close()
			}
			return
		}
		if err != nil {
			failed("listening on the tailnet", err)
			return
		}
		d.log.Info("serving on the tailnet", "port", tn.Port())
		if err := tlsSrv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			failed("serving on the tailnet", err)
		}
	}()
}

// tailnetDirectURL is the address a pairing link carries for the tailnet node: its IPv4 address (IPv6 when it
// has none), which works whether or not the device resolves MagicDNS names. Empty until the node runs, and when meshd can't serve there.
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

// tailnetURL is the URL of the node's first IPv4 address, else its first IPv6 one; empty with neither.
func tailnetURL(addresses []string, port uint16) string {
	var ip4, ip6 netip.Addr
	for _, a := range addresses {
		ip, err := netip.ParseAddr(a)
		switch {
		case err != nil:
		case ip.Is4() && !ip4.IsValid():
			ip4 = ip
		case ip.Is6() && !ip6.IsValid():
			ip6 = ip
		}
	}
	ip := ip4
	if !ip.IsValid() {
		ip = ip6
	}
	if !ip.IsValid() {
		return ""
	}
	if port == 443 {
		if ip.Is6() {
			return "https://[" + ip.String() + "]"
		}
		return "https://" + ip.String()
	}
	return "https://" + net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

// TailnetStatus reports the host's tailnet node. Only the host's own device, or the root of the setup run, may call it: while the node needs
// login, the answer holds the URL that attaches it to whichever tailnet opens it.
func (d *Daemon) TailnetStatus(ctx context.Context, _ *connect.Request[v1.TailnetStatusRequest]) (*connect.Response[v1.TailnetStatusResponse], error) {
	if err := d.requireHostOrSetup(ctx); err != nil {
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

// TailnetMarker is the file in a state dir whose presence makes meshd join the tailnet at its next start, as
// `--tailscale` does (`SetTailnet` writes and removes it; the flag, when given, wins and needs no file).
func TailnetMarker(stateDir string) string { return filepath.Join(stateDir, "tailnet-on") }

// restartDelay is how long meshd waits after answering SetTailnet before it restarts, so the answer reaches the app.
const restartDelay = 700 * time.Millisecond

// SetTailnet turns the host's Tailscale node on or off. The node starts when meshd does, so the change takes effect at its
// next start: when meshd runs under a service manager (cfg.Restart) it restarts itself, its agents' terminals keep running (the
// terminal host is a process of its own), and otherwise the answer says a restart is needed. Only the host's own device may ask:
// a paired device turning the host's network exposure on or off is the host's owner's call.
func (d *Daemon) SetTailnet(ctx context.Context, req *connect.Request[v1.SetTailnetRequest]) (*connect.Response[v1.SetTailnetResponse], error) {
	if err := requireHostDevice(ctx); err != nil {
		return nil, err
	}
	want := req.Msg.Enabled
	marker := TailnetMarker(d.paths.Dir)
	_, statErr := os.Stat(marker)
	marked := statErr == nil
	running := d.cfg.Tailnet != nil
	out := &v1.SetTailnetResponse{Enabled: running}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSetTailnetProcedure, out, func(*store.Tx) error {
		switch {
		case want && running && d.cfg.TailnetFromFlag:
			out.Enabled, out.Note = true, "already on: meshd was started with --tailscale"
		case want && running:
			out.Enabled, out.Note = true, "already on"
		case want && marked:
			out.Enabled, out.RestartNeeded = true, true // marked by an earlier call that no restart has followed
		case want:
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				return failedPrecondition("couldn't write %s: %v", marker, err)
			}
			out.Enabled, out.RestartNeeded = true, true
		case !want && running && d.cfg.TailnetFromFlag:
			out.Enabled, out.Note = true, "meshd was started with --tailscale (in its service unit or environment): remove it there to turn the node off"
		case !want && !running && !marked:
			out.Note = "already off"
		default:
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				return failedPrecondition("couldn't remove %s: %v", marker, err)
			}
			out.RestartNeeded = running
		}
		if out.RestartNeeded && d.cfg.Restart != nil {
			out.RestartNeeded, out.Restarting = false, true
			time.AfterFunc(restartDelay, d.cfg.Restart)
		}
		return nil
	})
	return connect.NewResponse(out), err
}
