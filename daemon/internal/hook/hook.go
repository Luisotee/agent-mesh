// Package hook is what a CLI's hook runs (`meshctl hook <kind> <event>`): it reports the hook to the daemon,
// finding the daemon's address at call time, and spools the report while the daemon is down. It never fails
// the CLI: whatever happens, the hook exits 0 and prints nothing.
package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// MaxPayload bounds what is read from the hook's stdin.
const MaxPayload = 4 << 20

// Options of one hook call.
type Options struct {
	StateDir string
	AgentID  string
	Kind     string
	Event    string
	Stdin    io.Reader
	Now      func() time.Time
	// DialTimeout and CallTimeout bound how long a down daemon holds the CLI up.
	DialTimeout time.Duration
	CallTimeout time.Duration
}

// Run reports one hook. Its error is for the log only.
func Run(o Options) error {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 500 * time.Millisecond
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = 3 * time.Second
	}
	p := statedir.Paths{Dir: o.StateDir}
	payload, err := io.ReadAll(io.LimitReader(o.Stdin, MaxPayload))
	if err != nil {
		return err
	}
	req := &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: "hook-" + uuid.NewString()}, Kind: o.Kind,
		EventName: o.Event, Payload: payload, FiredAt: timestamppb.New(o.Now())}
	tok, err := fsutil.ReadSecret(p.AgentToken(o.AgentID))
	if err != nil {
		return fmt.Errorf("no token for agent %s: %w", o.AgentID, err)
	}
	unlock, err := lockSpool(p, o.AgentID)
	if err != nil {
		return err
	}
	defer unlock()
	conn, err := client.Dial(o.StateDir, tok, client.Options{DialTimeout: o.DialTimeout})
	if err != nil {
		return spool(p, o.AgentID, req, o.Now())
	}
	send := func(r *v1.ReportHookRequest) error {
		ctx, cancel := context.WithTimeout(context.Background(), o.CallTimeout)
		defer cancel()
		_, err := conn.Mesh.ReportHook(ctx, connect.NewRequest(r))
		return err
	}
	// Older spooled hooks go first, so the daemon sees them in order.
	if err := Drain(p, o.AgentID, send); err != nil {
		return errors.Join(err, spool(p, o.AgentID, req, o.Now()))
	}
	if err := send(req); err != nil {
		if Retryable(err) {
			return errors.Join(err, spool(p, o.AgentID, req, o.Now()))
		}
		return fmt.Errorf("the daemon refused the hook: %w", err)
	}
	return nil
}

// Retryable reports whether a failed report should wait in the spool: the daemon was unreachable or didn't
// answer in time. A refusal (a revoked token, a bad request) is dropped instead, or it would block the spool
// for good.
func Retryable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeCanceled, connect.CodeUnknown:
		return true
	}
	return false
}

func lockSpool(p statedir.Paths, agent string) (func(), error) {
	dir := p.Spool(agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// LockSpool takes an agent's spool lock (the daemon drains under it too).
func LockSpool(p statedir.Paths, agent string) (func(), error) { return lockSpool(p, agent) }

func spool(p statedir.Paths, agent string, req *v1.ReportHookRequest, now time.Time) error {
	b, err := protojson.Marshal(req)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.Spool(agent), 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s.json", now.UnixNano(), req.GetMeta().GetMutationId())
	return fsutil.WriteAtomic(filepath.Join(p.Spool(agent), name), b, 0o600)
}

// MaxAttempts is how many times the daemon may answer a spooled hook with an error it can't classify (not
// unreachable, not a refusal) before the hook is dropped, so one that fails every time can't hold up the
// agent's later hooks for good.
const MaxAttempts = 10

// Drain sends an agent's spooled hooks, oldest first, removing each once sent or refused; it stops at the
// first one that should wait longer. The caller holds the spool lock.
func Drain(p statedir.Paths, agent string, send func(*v1.ReportHookRequest) error) error {
	return DrainReporting(p, agent, send, nil)
}

// DrainReporting is Drain that also tells dropped about each hook it gave up on after MaxAttempts failures
// (the file is kept as <name>.bad).
func DrainReporting(p statedir.Paths, agent string, send func(*v1.ReportHookRequest) error,
	dropped func(*v1.ReportHookRequest, error)) error {
	entries, err := os.ReadDir(p.Spool(agent))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(p.Spool(agent), name)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		req := &v1.ReportHookRequest{}
		if err := protojson.Unmarshal(b, req); err != nil {
			os.Rename(path, path+".bad")
			continue
		}
		if err := send(req); err != nil && Retryable(err) {
			// Only an answer the daemon couldn't classify counts: while it is down, a hook waits as long as it must.
			if connect.CodeOf(err) != connect.CodeUnknown {
				return err
			}
			tries := attempts(path) + 1
			if tries < MaxAttempts {
				fsutil.WriteAtomic(path+".tries", []byte(strconv.Itoa(tries)), 0o600)
				return err
			}
			os.Rename(path, path+".bad")
			os.Remove(path + ".tries")
			if dropped != nil {
				dropped(req, err)
			}
			continue
		}
		os.Remove(path)
		os.Remove(path + ".tries")
	}
	return nil
}

func attempts(path string) int {
	b, err := os.ReadFile(path + ".tries")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Agents lists the agents that have a spool dir.
func Agents(p statedir.Paths) ([]string, error) {
	entries, err := os.ReadDir(p.SpoolRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, err
}

// Log appends a line to the state dir's hook log; hooks print nothing, so this is where their failures go.
func Log(stateDir string, format string, args ...any) {
	f, err := os.OpenFile(statedir.Paths{Dir: stateDir}.HookLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
