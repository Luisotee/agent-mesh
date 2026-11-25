// Package daemon is meshd: the host's daemon. It owns the run state (SQLite), serves the action registry over
// ConnectRPC on a local socket, and streams the event log.
package daemon

import (
	"context"
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
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Version is the daemon's version.
const Version = "0.1.0-dev"

// ProtocolVersion is the API's major version; a client speaking another one is told which side to upgrade.
const ProtocolVersion = 1

// ShutdownWait bounds how long a stopping daemon waits for calls in flight.
const ShutdownWait = 5 * time.Second

// Config is how meshd is started.
type Config struct {
	// StateDir holds the database, the socket, the lock and the secrets. Required: there is no default, so a
	// dev or test run can't land in a real one by accident.
	StateDir string
	// TCP, when set, also listens on this loopback address ("127.0.0.1:0" picks a port).
	TCP    string
	Now    func() time.Time
	Logger *slog.Logger
}

// Paths of the files in a state dir.
type Paths struct{ Dir string }

func (p Paths) Lock() string        { return filepath.Join(p.Dir, "meshd.lock") }
func (p Paths) Socket() string      { return filepath.Join(p.Dir, "meshd.sock") }
func (p Paths) Info() string        { return filepath.Join(p.Dir, "meshd.json") }
func (p Paths) DB() string          { return filepath.Join(p.Dir, "meshd.db") }
func (p Paths) DeviceToken() string { return filepath.Join(p.Dir, "device.token") }
func (p Paths) DaemonID() string    { return filepath.Join(p.Dir, "daemon.id") }

// Info is meshd.json: how clients and hooks find the daemon, read at call time.
type Info struct {
	Socket     string    `json:"socket"`
	TCP        string    `json:"tcp,omitempty"`
	PID        int       `json:"pid"`
	DaemonID   string    `json:"daemon_id"`
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
	Version    string    `json:"version"`
}

// ReadInfo reads a state dir's meshd.json.
func ReadInfo(stateDir string) (Info, error) {
	var info Info
	b, err := os.ReadFile(Paths{stateDir}.Info())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// Daemon is a running meshd.
type Daemon struct {
	agentmeshv1connect.UnimplementedMeshServiceHandler

	cfg         Config
	paths       Paths
	log         *slog.Logger
	now         func() time.Time
	lock        *fsutil.Lock
	store       *store.Store
	bus         *events.Bus
	reg         *registry.Registry
	daemonID    string
	instanceID  string
	startedAt   time.Time
	mux         *http.ServeMux
	stopping    chan struct{}
	stopOnce    sync.Once
	handlerOnce sync.Once
	info        Info
	// capabilities the host advertises in Hello.
	capabilities []string
	// notify, when set, is told that an agent has a message to deliver.
	notify func(agentID string)
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
	d := &Daemon{cfg: cfg, paths: Paths{dir}, log: cfg.Logger, now: cfg.Now, stopping: make(chan struct{}),
		instanceID: ids.New("i", 10), mux: http.NewServeMux(),
		capabilities: []string{"events.resume", "mutations"}}
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
	d.bus = events.New(d.store)
	d.store.OnCommit = d.bus.Publish
	if d.daemonID, err = d.loadDaemonID(); err != nil {
		return nil, err
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

// Handler is the daemon's HTTP handler: the API, browsers refused.
func (d *Daemon) Handler() http.Handler {
	d.handlerOnce.Do(func() {
		path, h := agentmeshv1connect.NewMeshServiceHandler(d,
			connect.WithInterceptors(auth.NewInterceptor(d.reg, d)))
		d.mux.Handle(path, h)
	})
	return auth.RefuseBrowsers(d.mux)
}

// Serve listens on the state dir's socket (and TCP when configured), writes meshd.json, and serves until ctx
// ends; then it ends every event stream with an End message and stops within ShutdownWait.
func (d *Daemon) Serve(ctx context.Context) error {
	defer d.close()
	d.startedAt = d.now()
	sock := d.paths.Socket()
	// We hold the lock, so a socket file left here belongs to a dead daemon.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var listeners []net.Listener
	ul, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ul.Close()
		return err
	}
	listeners = append(listeners, ul)
	d.info = Info{Socket: sock, PID: os.Getpid(), DaemonID: d.daemonID, InstanceID: d.instanceID,
		StartedAt: d.startedAt, Version: Version}
	if d.cfg.TCP != "" {
		tl, err := listenLoopback(d.cfg.TCP)
		if err != nil {
			ul.Close()
			return err
		}
		listeners = append(listeners, tl)
		d.info.TCP = tl.Addr().String()
	}
	info, _ := json.MarshalIndent(d.info, "", "  ")
	if err := fsutil.WriteAtomic(d.paths.Info(), info, 0o600); err != nil {
		for _, l := range listeners {
			l.Close()
		}
		os.Remove(sock)
		return err
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: d.Handler(), Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) { errc <- srv.Serve(l) }(l)
	}
	d.log.Info("meshd serving", "socket", sock, "tcp", d.info.TCP, "daemon", d.daemonID, "instance", d.instanceID)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	d.stopOnce.Do(func() { close(d.stopping) })
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
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
