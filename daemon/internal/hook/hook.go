// Package hook is what a CLI's hook runs (`meshctl hook <kind> <event>`): it reports the hook to the daemon,
// finding the daemon's address at call time, and spools the report while the daemon is down. It never fails
// the CLI: whatever happens, the hook exits 0 and prints nothing (but an agy PreToolUse denial).
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// MaxPayload bounds what is read from the hook's stdin.
const MaxPayload = 4 << 20

// Options of one hook call.
type Options struct {
	StateDir    string
	AgentID     string
	Kind        string
	Event       string
	Stdin       io.Reader
	Stdout      io.Writer
	Cwd         string
	DenyPaths   []string
	DenyPublish bool
	// CheckOnly checks the deny rules and reports nothing: the hook of a session that isn't the agent's (a person
	// running agy in the agent's worktree), whose rules still hold there.
	CheckOnly bool
	Now       func() time.Time
	// CaptureDir, when set, also saves each hook's raw payload there, numbered in order, for the fixtures
	// (daemon/scripts/capture-claude-hooks.sh). Never set by the daemon: only a person capturing sets it.
	CaptureDir string
	// DialTimeout and CallTimeout bound how long a down daemon holds the CLI up.
	DialTimeout time.Duration
	CallTimeout time.Duration
}

// QuickTimeout bounds a quick report (an agy PreToolUse): the tool waits on the hook, and agy denies a tool
// whose PreToolUse hook outlives its timeout (HookTimeout), so this one never waits on the spool lock or a
// slow daemon.
const QuickTimeout = time.Second

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
	fired := o.Now() // before stdin is read: the order of near-simultaneous hooks is the order they were called in
	payload, readErr := io.ReadAll(io.LimitReader(o.Stdin, MaxPayload+1))
	oversize := len(payload) > MaxPayload
	if oversize {
		payload = payload[:MaxPayload]
	}
	preTool := o.Kind == agy.Kind && o.Event == "PreToolUse"
	// agy PreToolUse rules are checked here, so they hold while the daemon is down, and fail closed: a payload
	// that can't be read whole is denied.
	if preTool && (len(o.DenyPaths) > 0 || o.DenyPublish) {
		deny, reason := agy.EvaluatePreToolUse(payload, o.Cwd, o.DenyPaths, o.DenyPublish)
		switch {
		case readErr != nil:
			deny, reason = true, "read-only/planning agent: the tool call could not be read ("+readErr.Error()+")"
		case oversize:
			deny, reason = true, fmt.Sprintf("read-only/planning agent: the tool call is over %d bytes, too large to check", MaxPayload)
		}
		if deny && o.Stdout != nil {
			b, _ := json.Marshal(struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}{"deny", reason})
			o.Stdout.Write(append(b, '\n'))
		}
	}
	if readErr != nil {
		return readErr
	}
	if o.CheckOnly {
		return nil
	}
	// Capturing is a side channel: when it fails the hook still reports (its error is returned after).
	var captureErr error
	if o.CaptureDir != "" {
		if captureErr = capture(o.CaptureDir, o.Event, payload, fired); captureErr != nil {
			captureErr = fmt.Errorf("capturing the payload: %w", captureErr)
		}
	}
	if preTool {
		return errors.Join(captureErr, reportQuick(o, p, payload, fired))
	}
	return errors.Join(captureErr, report(o, p, payload, fired))
}

func hookRequest(o Options, payload []byte, fired time.Time) *v1.ReportHookRequest {
	return &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: "hook-" + uuid.NewString()}, Kind: o.Kind,
		EventName: o.Event, Payload: payload, FiredAt: timestamppb.New(fired)}
}

// report sends one hook to the daemon, or spools it.
func report(o Options, p statedir.Paths, payload []byte, fired time.Time) error {
	req := hookRequest(o, payload, fired)
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

// reportQuick is report for a hook the CLI's tool call waits on: it never drains, and spools at once when another
// hook holds the spool lock or older hooks are spooled (the next hook, or the daemon's drain, sends it after
// them, in order). Otherwise it sends within QuickTimeout, spooling on failure. A spool written without the lock
// is safe: its holder either lists it and sends it in order, or leaves it to the next drain, and nothing later
// of this agent fires until this hook exits.
func reportQuick(o Options, p statedir.Paths, payload []byte, fired time.Time) error {
	req := hookRequest(o, payload, fired)
	tok, err := fsutil.ReadSecret(p.AgentToken(o.AgentID))
	if err != nil {
		return fmt.Errorf("no token for agent %s: %w", o.AgentID, err)
	}
	unlock, locked, err := tryLockSpool(p, o.AgentID)
	if err != nil {
		return err
	}
	if !locked {
		return spool(p, o.AgentID, req, o.Now())
	}
	defer unlock()
	if spooled(p, o.AgentID) {
		return spool(p, o.AgentID, req, o.Now())
	}
	timeout, dial := quickTimeouts(o)
	conn, err := client.Dial(o.StateDir, tok, client.Options{DialTimeout: dial})
	if err != nil {
		return spool(p, o.AgentID, req, o.Now())
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := conn.Mesh.ReportHook(ctx, connect.NewRequest(req)); err != nil {
		if Retryable(err) {
			return errors.Join(err, spool(p, o.AgentID, req, o.Now()))
		}
		return fmt.Errorf("the daemon refused the hook: %w", err)
	}
	return nil
}

// quickTimeouts are the call and dial timeouts of a quick report: o's, capped at QuickTimeout, which an unset
// (zero) one gets too, so it never falls back to a longer default.
func quickTimeouts(o Options) (call, dial time.Duration) {
	call = QuickTimeout
	if o.CallTimeout > 0 {
		call = min(o.CallTimeout, QuickTimeout)
	}
	dial = call
	if o.DialTimeout > 0 {
		dial = min(o.DialTimeout, call)
	}
	return call, dial
}

// spooled reports whether an agent has hooks waiting in its spool.
func spooled(p statedir.Paths, agent string) bool {
	entries, _ := os.ReadDir(p.Spool(agent))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}

// capture saves a raw payload as `<unix nanos>-<event>.json`: named by time, so the order of firing is the order
// of the names.
func capture(dir, event string, payload []byte, now time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d-%s.json", now.UnixNano(), event)), payload, 0o600)
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

// tryLockSpool is lockSpool that doesn't wait: locked is false while another holds the lock.
func tryLockSpool(p statedir.Paths, agent string) (unlock func(), locked bool, err error) {
	dir := p.Spool(agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
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

// MaxAge is how long such a hook must also have been failing, counted from its first failed attempt, before it is
// dropped: ten quick tries at the drain's pace are seconds of store trouble, which must not cost a Stop.
const MaxAge = 5 * time.Minute

// now is the clock of the attempt counting, for tests.
var now = time.Now

// Drain sends an agent's spooled hooks, oldest first, removing each once sent or refused; it stops at the
// first one that should wait longer, however it failed, and never drops one: the hook process (which calls it)
// has no way to report a drop. The caller holds the spool lock.
func Drain(p statedir.Paths, agent string, send func(*v1.ReportHookRequest) error) error {
	return drain(p, agent, send, nil, false)
}

// DrainReporting is Drain for the daemon: it counts the failures it can't classify, and gives up on a hook after
// MaxAttempts of them over MaxAge, telling dropped (the file is kept as <name>.bad).
func DrainReporting(p statedir.Paths, agent string, send func(*v1.ReportHookRequest) error,
	dropped func(*v1.ReportHookRequest, error)) error {
	return drain(p, agent, send, dropped, true)
}

func drain(p statedir.Paths, agent string, send func(*v1.ReportHookRequest) error,
	dropped func(*v1.ReportHookRequest, error), count bool) error {
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
			if !count || connect.CodeOf(err) != connect.CodeUnknown {
				return err
			}
			tries, first := attempts(path)
			if first.IsZero() {
				first = now()
			}
			tries++
			if tries < MaxAttempts || now().Sub(first) < MaxAge {
				fsutil.WriteAtomic(path+".tries", []byte(fmt.Sprintf("%d %d", tries, first.Unix())), 0o600)
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

// attempts reads a hook's failure record: how many answers it got that were errors, and when the first came.
func attempts(path string) (int, time.Time) {
	b, err := os.ReadFile(path + ".tries")
	if err != nil {
		return 0, time.Time{}
	}
	var n int
	var unix int64
	if _, err := fmt.Sscanf(string(b), "%d %d", &n, &unix); err != nil {
		return 0, time.Time{}
	}
	return n, time.Unix(unix, 0)
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
