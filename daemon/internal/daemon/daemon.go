// Package daemon is meshd: the host's daemon. It owns the run state (SQLite), serves the action registry over
// ConnectRPC on a local socket, and streams the event log.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/events"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

// Version is the daemon's version.
const Version = "0.1.0-dev"

// ProtocolVersion is the API's major version; a client speaking another one is told which side to upgrade.
const ProtocolVersion = 1

// ShutdownWait bounds how long a stopping daemon waits for calls in flight.
const ShutdownWait = 5 * time.Second

// RelayConfig configures meshd's outbound connection to a self-hosted relay.
type RelayConfig struct {
	// URL is the relay's HTTP/WebSocket base URL (for example https://relay.example.com).
	URL string
	// TCP is the relay's raw TCP client address (host:port) advertised in pairing payloads.
	TCP string
	// Token is the relay shared secret.
	Token string
	// HostName is the readable host name advertised to the relay and paired devices (defaults to hostname).
	HostName string
}

// Config is how meshd is started.
type Config struct {
	// StateDir holds the database, the socket, the lock and the secrets. Required: there is no default, so a
	// dev or test run can't land in a real one by accident.
	StateDir string
	// TCP, when set, also listens (plain HTTP) on this loopback address ("127.0.0.1:0" picks a port).
	TCP string
	// Listen, when set, also listens with the daemon's TLS certificate on this address (e.g. ":9443") for
	// direct LAN / Tailscale clients. Off by default.
	Listen string
	Relay  RelayConfig
	Now    func() time.Time
	Logger *slog.Logger

	// Settings is the table of settings the host knows; nil is settings.Default().
	Settings *settings.Schema

	// Agents (agents.go). With no TermLaunch the daemon runs without terminals and can't spawn agents.
	Agents AgentConfig
}

// Paths of the files in a state dir.
type Paths = statedir.Paths

// Info is meshd.json.
type Info = statedir.Info

// ReadInfo reads a state dir's meshd.json.
func ReadInfo(stateDir string) (Info, error) { return statedir.ReadInfo(stateDir) }

// Daemon is a running meshd.
type Daemon struct {
	agentmeshv1connect.UnimplementedMeshServiceHandler
	agentmeshv1connect.UnimplementedPairingServiceHandler

	cfg         Config
	paths       Paths
	log         *slog.Logger
	now         func() time.Time
	lock        *fsutil.Lock
	store       *store.Store
	bus         *events.Bus
	reg         *registry.Registry
	schema      *settings.Schema
	daemonID    string
	hostName    string
	instanceID  string
	startedAt   time.Time
	tlsCfg      *tls.Config
	certFP      string
	relay       *relay.Connector
	mux         *http.ServeMux
	stopping    chan struct{}
	stopOnce    sync.Once
	handlerOnce sync.Once
	info        Info
	// capabilities the host advertises in Hello.
	capabilities []string
	// notify, when set, is told that an agent has a message to deliver.
	notify func(agentID string)
	rt     *runtime

	streamsMu  sync.Mutex
	streams    map[string]map[uint64]context.CancelFunc
	nextStream uint64
	bgWG       sync.WaitGroup
}

// New opens a state dir: takes its lock, opens (or migrates) the database, and makes the daemon's id and the
// host's device token on first start.
func New(cfg Config) (*Daemon, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("no state dir given")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	d := &Daemon{cfg: cfg, paths: Paths{Dir: dir}, log: cfg.Logger, now: cfg.Now, stopping: make(chan struct{}),
		instanceID: ids.New("i", 10), mux: http.NewServeMux(),
		capabilities: []string{"events.resume", "mutations", "pairing"},
		streams:      map[string]map[uint64]context.CancelFunc{}}
	if d.lock, err = fsutil.TryLock(d.paths.Lock()); err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			return nil, fmt.Errorf("another meshd is running on %s", dir)
		}
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			d.close()
		}
	}()
	if d.store, err = store.OpenAt(d.paths.DB(), cfg.Now); err != nil {
		return nil, err
	}
	if d.reg, err = registry.Mesh(); err != nil {
		return nil, err
	}
	if d.schema = cfg.Settings; d.schema == nil {
		d.schema = settings.Default()
	}
	d.bus = events.New(d.store)
	d.store.OnCommit = d.bus.Publish
	if d.rt, err = newRuntime(cfg.Agents); err != nil {
		return nil, err
	}
	d.notify = d.deliverSoon
	d.bus.AgentView = d.view
	d.mux.HandleFunc("/v1/term/{agent}", d.serveTerminal)
	if d.daemonID, err = d.loadDaemonID(); err != nil {
		return nil, err
	}
	d.hostName = strings.TrimSpace(cfg.Relay.HostName)
	if d.hostName == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			d.hostName = hn
		} else {
			d.hostName = d.daemonID
		}
	}
	if d.tlsCfg, d.certFP, err = tlsutil.LoadOrCreate(d.paths.TLSCert(), d.paths.TLSKey(), d.daemonID); err != nil {
		return nil, err
	}
	if cfg.Listen != "" {
		d.capabilities = append(d.capabilities, "tls")
	}
	if cfg.Relay.URL != "" {
		d.relay, err = relay.NewConnector(relay.ConnectorConfig{
			RelayURL:      cfg.Relay.URL,
			Secret:        cfg.Relay.Token,
			HostID:        d.daemonID,
			HostName:      d.hostName,
			DaemonVersion: Version,
			Logger:        d.log,
		})
		if err != nil {
			return nil, fmt.Errorf("relay: %w", err)
		}
		d.capabilities = append(d.capabilities, "relay")
	}
	if err := d.ensureDeviceToken(context.Background()); err != nil {
		return nil, err
	}
	ok = true
	return d, nil
}

func (d *Daemon) loadDaemonID() (string, error) {
	b, err := os.ReadFile(d.paths.DaemonID())
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	if err == nil {
		err = os.ErrNotExist // empty: a crash while it was first written
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id := ids.New("h", 12)
	return id, fsutil.WriteAtomic(d.paths.DaemonID(), []byte(id), 0o600)
}

// maxSocketPath is what sockaddr_un.sun_path holds on Linux (108 with its NUL); a longer one fails with
// the same bare "invalid argument".
const maxSocketPath = 107

// The host's own device: the CLI the user runs on this machine.
const localDeviceID, localDeviceName = "device-local", "local"

// ensureDeviceToken makes the local device's token on first start, and re-registers the existing one when the
// database is new (its hash is all the database keeps).
func (d *Daemon) ensureDeviceToken(ctx context.Context) error {
	tok, err := fsutil.ReadSecret(d.paths.DeviceToken())
	if errors.Is(err, os.ErrNotExist) {
		tok = auth.NewToken()
		if err := fsutil.WriteAtomic(d.paths.DeviceToken(), []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := d.store.Read(ctx).Token(auth.Hash(tok)); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// A token that is stored but revoked stays revoked: the device gets a new one, not its old hash back.
	if known, err := d.store.Read(ctx).TokenKnown(auth.Hash(tok)); err != nil {
		return err
	} else if known {
		tok = auth.NewToken()
		if err := fsutil.WriteAtomic(d.paths.DeviceToken(), []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
	}
	return d.store.Write(ctx, nil, "", func(t *store.Tx) error {
		return t.PutToken(auth.Hash(tok), store.TokenRecord{Class: v1.ActorClass_ACTOR_CLASS_DEVICE,
			SubjectID: localDeviceID, Name: localDeviceName})
	})
}

// ResolveToken implements auth.Resolver.
func (d *Daemon) ResolveToken(ctx context.Context, hash string) (*v1.Actor, error) {
	rec, err := d.store.Read(ctx).Token(hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.ErrUnknownToken
	}
	if err != nil {
		return nil, err
	}
	return &v1.Actor{Class: rec.Class, Id: rec.SubjectID, Name: rec.Name, RunId: rec.RunID}, nil
}

// Store is the daemon's database.
func (d *Daemon) Store() *store.Store { return d.store }

// Registry is the daemon's action registry.
func (d *Daemon) Registry() *registry.Registry { return d.reg }

// Paths are the daemon's state files.
func (d *Daemon) Paths() Paths { return d.paths }

// Handle adds an HTTP handler beside the API (the terminal proxy).
func (d *Daemon) Handle(pattern string, h http.Handler) { d.mux.Handle(pattern, h) }

// SetNotify sets what is told that an agent has a message to deliver.
func (d *Daemon) SetNotify(fn func(agentID string)) { d.notify = fn }

// AddCapabilities adds to what Hello advertises.
func (d *Daemon) AddCapabilities(c ...string) { d.capabilities = append(d.capabilities, c...) }

// SetAgentView sets how an agent is shown (derived state) in lists and snapshots.
func (d *Daemon) SetAgentView(fn func(*v1.Agent) *v1.Agent) { d.bus.AgentView = fn }

func (d *Daemon) agentView(a *v1.Agent) *v1.Agent {
	if d.bus.AgentView != nil {
		return d.bus.AgentView(a)
	}
	return a
}

// Stopping is closed when the daemon starts shutting down.
func (d *Daemon) Stopping() <-chan struct{} { return d.stopping }

// CertFingerprint returns the daemon's TLS certificate SHA-256 fingerprint.
func (d *Daemon) CertFingerprint() string { return d.certFP }

// RelayOnline reports whether the daemon's relay control connection is registered and healthy.
func (d *Daemon) RelayOnline() bool { return d.relay != nil && d.relay.Online() }

// Handler is the daemon's HTTP handler: the API, browsers refused.
func (d *Daemon) Handler() http.Handler {
	d.handlerOnce.Do(func() {
		path, h := agentmeshv1connect.NewMeshServiceHandler(d,
			connect.WithInterceptors(auth.NewInterceptor(d.reg, d), d.hintInterceptor()))
		d.mux.Handle(path, h)
		ppath, ph := agentmeshv1connect.NewPairingServiceHandler(d)
		d.mux.Handle(ppath, ph)
	})
	return auth.RefuseBrowsers(d.mux)
}

// Serve listens on the state dir's socket (and TCP/TLS/relay when configured), writes meshd.json, and serves
// until ctx ends; then it ends every event stream with an End message and stops within ShutdownWait.
func (d *Daemon) Serve(ctx context.Context) error {
	defer d.close()
	d.startedAt = d.now()
	// Terminals first: adopt what survived the last daemon before anything can act on its agents.
	if err := d.startTerminals(ctx); err != nil {
		return err
	}
	sock := d.paths.Socket()
	if len(sock) > maxSocketPath {
		return fmt.Errorf("the socket path %s is %d bytes, over the %d a unix socket takes: use a shorter --state directory",
			sock, len(sock), maxSocketPath)
	}
	// We hold the lock, so a socket file left here belongs to a dead daemon.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var plainListeners []net.Listener
	var tlsListeners []net.Listener
	closeAll := func() {
		for _, l := range plainListeners {
			_ = l.Close()
		}
		for _, l := range tlsListeners {
			_ = l.Close()
		}
	}
	ul, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ul.Close()
		return err
	}
	plainListeners = append(plainListeners, ul)
	d.info = Info{Socket: sock, PID: os.Getpid(), DaemonID: d.daemonID, InstanceID: d.instanceID,
		CertFingerprint: d.certFP, StartedAt: d.startedAt, Version: Version}
	if d.cfg.TCP != "" {
		tl, err := listenLoopback(d.cfg.TCP)
		if err != nil {
			closeAll()
			return err
		}
		plainListeners = append(plainListeners, tl)
		d.info.TCP = tl.Addr().String()
	}
	if d.cfg.Listen != "" {
		ll, err := net.Listen("tcp", d.cfg.Listen)
		if err != nil {
			closeAll()
			return err
		}
		ll = relay.NoDelayListener(ll)
		tlsListeners = append(tlsListeners, ll)
		d.info.Listen = ll.Addr().String()
	}
	info, _ := json.MarshalIndent(d.info, "", "  ")
	if err := fsutil.WriteAtomic(d.paths.Info(), info, 0o600); err != nil {
		closeAll()
		os.Remove(sock)
		return err
	}
	plainProtocols := new(http.Protocols)
	plainProtocols.SetHTTP1(true)
	plainProtocols.SetUnencryptedHTTP2(true)
	plainSrv := &http.Server{Handler: d.Handler(), Protocols: plainProtocols, ReadHeaderTimeout: 10 * time.Second}

	tlsProtocols := new(http.Protocols)
	tlsProtocols.SetHTTP1(true)
	tlsProtocols.SetHTTP2(true)
	tlsSrv := &http.Server{Handler: d.Handler(), TLSConfig: d.tlsCfg, Protocols: tlsProtocols, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, len(plainListeners)+len(tlsListeners)+1)
	for _, l := range plainListeners {
		go func(l net.Listener) { errc <- plainSrv.Serve(l) }(l)
	}
	for _, l := range tlsListeners {
		go func(l net.Listener) { errc <- tlsSrv.ServeTLS(l, "", "") }(l)
	}
	d.log.Info("meshd serving", "socket", sock, "tcp", d.info.TCP, "listen", d.info.Listen,
		"relay", d.cfg.Relay.URL, "daemon", d.daemonID, "instance", d.instanceID)
	bg, stopBG := context.WithCancel(ctx)
	defer stopBG()
	d.runBackground(bg)
	if d.relay != nil {
		go func() { _ = d.relay.Run(bg) }()
		go func() {
			if err := tlsSrv.ServeTLS(d.relay.Listener(), "", ""); err != nil &&
				!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errc <- err
			}
		}()
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	d.stopOnce.Do(func() { close(d.stopping) })
	stopBG()
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownWait)
	defer cancel()
	if err := plainSrv.Shutdown(sctx); err != nil {
		plainSrv.Close()
	}
	if err := tlsSrv.Shutdown(sctx); err != nil {
		tlsSrv.Close()
	}
	os.Remove(d.paths.Info())
	os.Remove(sock)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return serveErr
}

func listenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("%s is not a loopback address: remote access goes through the relay", addr)
	}
	return net.Listen("tcp", addr)
}

func (d *Daemon) close() {
	d.stopOnce.Do(func() { close(d.stopping) })
	d.bgWG.Wait()
	if d.store != nil {
		d.store.Close()
	}
	d.lock.Release()
}

// tsOf is a protobuf timestamp, nil for the zero time.
func tsOf(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// RunFolders are the folders every run's dir holds.
var RunFolders = statedir.RunFolders
