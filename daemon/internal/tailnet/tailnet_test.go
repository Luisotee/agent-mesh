package tailnet

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"

	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

// startControl runs Tailscale's test control server and a DERP/STUN server on 127.0.0.1, as tsnet's own tests
// do: the nodes here never reach Tailscale or the network.
func startControl(t *testing.T, requireAuth bool) (string, *testcontrol.Server) {
	t.Helper()
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{
		DERPMap:        integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1"),
		DNSConfig:      &tailcfg.DNSConfig{Proxied: true},
		MagicDNSDomain: "tail-scale.ts.net",
		RequireAuth:    requireAuth,
		Logf:           logger.Discard,
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control.HTTPTestServer.URL, control
}

func waitState(t *testing.T, n *Node, want string) *statusOf {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := n.Status(ctx)
		cancel()
		if err == nil && st.State == want {
			return &statusOf{st.State, st.AuthUrl, st.Addresses, st.Hostname}
		}
		if time.Now().After(deadline) {
			t.Fatalf("node never reached %s: %v %v", want, st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type statusOf struct {
	state, authURL string
	addresses      []string
	hostname       string
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "tn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestNewRefusesTailscaleControlUnderTest(t *testing.T) {
	if _, err := New(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("New with Tailscale's own control server succeeded under test")
	}
}

func TestCheckBuildNamesTheTags(t *testing.T) {
	// Tests build without BuildTags, so the check refuses and says which tags to add.
	err := CheckBuild()
	if err == nil || !strings.Contains(err.Error(), BuildTags) {
		t.Fatalf("CheckBuild = %v", err)
	}
}

func TestDedupeDropsRepeats(t *testing.T) {
	var got []string
	f := dedupe(func(s string) { got = append(got, s) })
	for _, l := range []string{"go to: %s", "go to: %s", "running"} {
		f(l, "https://x")
	}
	if len(got) != 2 {
		t.Fatalf("got %q", got)
	}
}

// The node joins a local tailnet, serves pinned TLS there, and a peer reaches it by its tailnet address.
func TestNodeServesPinnedTLSOnTheTailnet(t *testing.T) {
	controlURL, _ := startControl(t, false)
	dir := shortDir(t)
	n, err := New(Config{Dir: filepath.Join(dir, "tailscale"), Hostname: "mesh-test", Port: 8443, ControlURL: controlURL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	if fi, err := os.Stat(filepath.Join(dir, "tailscale")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", fi, err)
	}
	ln, err := n.Listen("tcp", ":8443")
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, fp, err := tlsutil.LoadOrCreate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "h")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{TLSConfig: tlsCfg, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("meshd"))
	})}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { srv.Close() })

	st := waitState(t, n, "Running")
	if len(st.addresses) == 0 || st.hostname != "mesh-test" {
		t.Fatalf("status: %+v", st)
	}
	var ip4 netip.Addr
	for _, a := range st.addresses {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			ip4 = ip
		}
	}
	if !ip4.IsValid() {
		t.Fatalf("no IPv4 address in %v", st.addresses)
	}

	peer := &tsnet.Server{Dir: filepath.Join(dir, "peer"), Hostname: "phone", ControlURL: controlURL,
		Store: new(mem.Store), Ephemeral: true}
	t.Cleanup(func() { peer.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := peer.Up(ctx); err != nil {
		t.Fatal(err)
	}
	pinned, err := tlsutil.PinnedConfig(fp, "http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return peer.Dial(ctx, network, addr)
		},
		TLSClientConfig: pinned,
	}}
	url := "https://" + net.JoinHostPort(ip4.String(), "8443") + "/"
	var lastErr error
	for ctx.Err() == nil {
		res, err := hc.Get(url)
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(b) != "meshd" {
				t.Fatalf("got %q", b)
			}
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the peer never reached the node over the tailnet: %v", lastErr)
}

// Without an auth key the node waits for login, and its status holds the URL to open.
func TestNodeNeedsLoginShowsTheURL(t *testing.T) {
	controlURL, control := startControl(t, true)
	n, err := New(Config{Dir: filepath.Join(shortDir(t), "tailscale"), Hostname: "mesh-login", ControlURL: controlURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	ln, err := n.Listen("tcp", ":443")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	deadline := time.Now().Add(30 * time.Second)
	var authURL string
	for authURL == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := n.Status(ctx)
		cancel()
		if err == nil && st.State == "NeedsLogin" {
			authURL = st.AuthUrl
		}
		if time.Now().After(deadline) {
			t.Fatalf("no login URL: %v %v", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !control.CompleteAuth(authURL) {
		t.Fatalf("control didn't know the login URL %q", authURL)
	}
	waitState(t, n, "Running")
}
