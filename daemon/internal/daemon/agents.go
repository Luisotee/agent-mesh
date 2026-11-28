package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/copilot"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/reducer"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

// AgentConfig is how the daemon runs agents.
type AgentConfig struct {
	// TermLaunch starts the terminal host when none answers; empty runs without terminals (no agents).
	TermLaunch termclient.Launch
	// TermBinary is mesh-term, for the detached launch; TermArgs are passed on to it.
	TermBinary string
	TermArgs   []string
	// TermUnit is the systemd user unit, for the systemd launch.
	TermUnit string
	// Mesh is the absolute path of the CLI whose `hook` command the agents' hooks run.
	Mesh string
	// Share is the absolute path of the daemon's share folder (protocol/ and skills/, installed by
	// `install.sh`): agents get it as $AGENT_MESH_SHARE, their first doorbell names its protocol, and a
	// Claude agent loads its skills. Empty: none of that.
	Share string
	// Binaries maps an adapter kind to its CLI's path ("claude" → /usr/bin/claude); tests point it at fakes.
	Binaries map[string]string
	// LookPath resolves a CLI binary name when Binaries has no entry for its kind; nil defaults to searching
	// the agent's PATH then exec.LookPath. Tests set it so absent Binaries entries never pick up host CLIs.
	LookPath func(name string) (string, bool)
	// Adapters are the CLI adapters; nil means the built-ins.
	Adapters []adapter.Adapter
	// Worktrees is where agents' worktrees go; empty means <state>/worktrees.
	Worktrees string
	// SkipPermissions starts agents with their CLI's permission prompts off.
	SkipPermissions bool
	// UnknownAfter shows a working agent nothing was heard from for this long as unknown (default 10 min).
	UnknownAfter time.Duration
	// Env is added to (and overrides) the environment every agent gets.
	Env map[string]string
	// DeliveryDelay is the pause between a doorbell's text and its Enter (default 150 ms): a TUI reads input
	// that arrives in one burst as a paste.
	DeliveryDelay time.Duration
	// TypingQuiet is how long after a client with input rights last typed a doorbell still waits (default 15 s).
	// A client that is attached but quiet doesn't hold messages back.
	TypingQuiet time.Duration
	// SpoolEvery is how often the hook spool is drained (default 2 s).
	SpoolEvery time.Duration
	// ReadySettle is how long the terminal of an adapter without a startup hook must be quiet, once it has drawn,
	// before the agent is marked idle (default 1 s); one still drawing after settleCap of them is marked anyway.
	ReadySettle time.Duration
	// MeminfoPath and PressurePath are the memory guard's sources (defaults /proc/meminfo and
	// /proc/pressure/memory); tests point them at files.
	MeminfoPath, PressurePath string
	// Cores overrides the machine's core count in the auto sizing of `caps.working` (tests); 0 reads it.
	Cores int
	// CapacityEvery is how often the agents' memory and CPU are measured (default 30 s).
	CapacityEvery time.Duration
	// RetireEvery is how often agents idle past `idle_retire` after their report are looked for (default 10 s).
	RetireEvery time.Duration
	// AccountEvery is how often the accountability pass runs (default 5 s).
	AccountEvery time.Duration
	// SweepAgentPoll and SweepKillPoll override the process-sweep poll intervals (defaults 500 ms and 300 ms).
	SweepAgentPoll, SweepKillPoll time.Duration
	// Sys overrides OS process, affinity, signal, and systemd scope calls (for tests).
	Sys *SystemOps
	// NoTerminalWatch leaves out the watch on the terminal host's exits, so only adopt and the calls the daemon
	// makes reconcile terminals. For tests of those paths: the host's watch replays exited terminals, which
	// would pass them either way.
	NoTerminalWatch bool
	// Usage overrides external quota/GitHub calls (for tests).
	Usage *UsageOps
	// ReserveEvery is how often reserve alerts check open runs (default 15m).
	ReserveEvery time.Duration
	// CompactQuietWait, CompactStartWait and CompactTimeout override how long a live compaction waits for a user to
	// stop typing, for the CLI to begin compacting after `/compact` was typed, and for it to finish (defaults 60 s,
	// 60 s and 15 min).
	CompactQuietWait, CompactStartWait, CompactTimeout time.Duration
	// CompactSettle is how long a live compaction the CLI reported done waits for its transcript to show it before
	// the agent goes on (default 20 s).
	CompactSettle time.Duration
	// Reset overrides the root reset's timings (reset.go); zero values are the defaults.
	Reset ResetTimings
	// ContextEvery is how often agents with a compaction or a root reset still to carry out are looked at (default
	// 5 s).
	ContextEvery time.Duration
	// RunCLI runs a CLI command to its end with exactly the environment given (a headless compaction); nil runs it
	// for real. Tests set it so no real CLI runs.
	RunCLI func(ctx context.Context, argv []string, env map[string]string, cwd string) (string, error)
	// WrapTermAPI wraps the terminal host client before adopt and watchTerminals start (for tests).
	WrapTermAPI func(termv1connect.TermHostServiceClient) termv1connect.TermHostServiceClient
}

// The environment an agent inherits from the daemon; anything else (other secrets, the caller's own session) is
// left out, and `--agent-env KEY=VALUE` adds to it. MESH_* and Claude's session variables are never on this list.
// Besides the shell basics it carries what a CLI needs to reach its model: Claude's login and config
// (CLAUDE_CONFIG_DIR, CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_*), the proxy variables, and the Bedrock and Vertex
// settings and credentials. A meshd started by a systemd user unit has none of these unless its unit sets them.
var inheritedEnv = []string{"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"SSH_AUTH_SOCK",
	"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION",
	"GOOGLE_APPLICATION_CREDENTIALS", "GEMINI_API_KEY", "GOOGLE_API_KEY", "AWS_PROFILE", "AWS_REGION",
	"AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

// Provider API keys passed only to OpenCode agents whose model isn't an `opencode/*` free/built-in model,
// so read-only free reviewers of an untrusted PR never receive third-party provider secrets.
var opencodeInheritedEnv = []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "OPENCODE_API_KEY"}

type ticket struct {
	agentID    string
	shellID    string // a project shell's terminal (agentID is empty)
	terminalID string
	actorID    string
	input      bool
	expires    time.Time
}

type runtime struct {
	cfg      AgentConfig
	adapters *adapter.Registry
	term     *termclient.Client
	hello    *termv1.HelloResponse
	sys      *SystemOps

	mu            sync.Mutex
	lastOutput    map[string]time.Time // terminal id → newest output mark
	firstOutput   map[string]time.Time // terminal id → first output time (for adapters without startup-hook)
	tickets       map[string]ticket
	delivering    map[string]*sync.Mutex // agent id → its lock; one mutex per agent for the daemon's lifetime (never deleted: a holder may be waited on)
	worktrees     map[string]*sync.Mutex // worktree path → its git lock (independent of agentLock so ResumeAgent never deadlocks with reopen)
	launching     map[string]bool        // reopen id → its new process died while a newer reopen replaced it
	introduced    map[string]bool        // terminal id → its first doorbell has named the protocol
	rings         map[string]*ring       // agent id → its doorbell in flight (delivery.go)
	held          map[string]*held       // agent id → its doorbell kept back by a human's draft
	nativeOff     map[string]time.Time   // terminal id → a frame written to its inbox socket wasn't taken: type until then
	deliverQueued map[string]bool        // agent id → a delivery is queued for it (deliverSoon)
	promptHeld    map[string]string      // agent id → why its next queued prompt wasn't typed at the last try (prompts.go)
	promptTyped   map[string]time.Time   // agent id → when a queued prompt was typed and no activity has followed yet (prompts.go)
	lastEsc       map[string]time.Time   // agent id → when InterruptAgent last sent it Esc (prompts.go)
	guard         guard
	sampler       capSampler // what the agents measured (capacity.go)
	cpus          cpuTracker
	usage         usageState      // usage cache / rate_limits (usage.go)
	openCode      openCodeRuntime // OpenCode polling and model cache (opencode.go)
	copilot       copilotRuntime  // Copilot polling (copilot.go)
	ctxwork       contextRuntime  // headless compactions in flight (compact.go)
	keeper        keeperState     // when the root keeper last looked (reset.go)
}

func newRuntime(cfg AgentConfig) (*runtime, error) {
	if cfg.UnknownAfter == 0 {
		cfg.UnknownAfter = 10 * time.Minute
	}
	if cfg.TypingQuiet == 0 {
		cfg.TypingQuiet = 15 * time.Second
	}
	if cfg.DeliveryDelay == 0 {
		cfg.DeliveryDelay = 150 * time.Millisecond
	}
	if cfg.SpoolEvery == 0 {
		cfg.SpoolEvery = 2 * time.Second
	}
	if cfg.ReadySettle == 0 {
		cfg.ReadySettle = 1 * time.Second
	}
	if cfg.Adapters == nil {
		cfg.Adapters = []adapter.Adapter{claude.New(), agy.New(), opencode.New(), copilot.New()}
	}
	reg, err := adapter.NewRegistry(context.Background(), cfg.Adapters...)
	if err != nil {
		return nil, err
	}
	return &runtime{cfg: cfg, adapters: reg, sys: resolveSystemOps(cfg.Sys), lastOutput: map[string]time.Time{}, firstOutput: map[string]time.Time{},
		tickets: map[string]ticket{}, delivering: map[string]*sync.Mutex{}, launching: map[string]bool{}, introduced: map[string]bool{},
		rings: map[string]*ring{}, held: map[string]*held{}, nativeOff: map[string]time.Time{}, deliverQueued: map[string]bool{}, promptHeld: map[string]string{}, promptTyped: map[string]time.Time{}, lastEsc: map[string]time.Time{}}, nil
}

// view is an agent as shown: a working agent long silent reads as unknown.
func (d *Daemon) view(a *v1.Agent) *v1.Agent {
	d.rt.mu.Lock()
	out := d.rt.lastOutput[a.TerminalId]
	d.rt.mu.Unlock()
	shown := reducer.Display(reducer.Of(a), d.now(), out, d.rt.cfg.UnknownAfter)
	moved := movedBranch(a)
	if shown == a.State && moved == "" {
		return a
	}
	a = proto.Clone(a).(*v1.Agent)
	a.State = shown
	if moved != "" {
		a.Branch = moved // the branch it is on now, not the one it was spawned on (branch.go)
	}
	return a
}

// ── terminal host ──

func (d *Daemon) startTerminals(ctx context.Context) error {
	if d.rt.cfg.TermLaunch == "" {
		return nil
	}
	c, hello, err := termclient.Ensure(ctx, d.paths.Term(), termclient.Options{Launch: d.rt.cfg.TermLaunch,
		Binary: d.rt.cfg.TermBinary, Args: d.rt.cfg.TermArgs, Unit: d.rt.cfg.TermUnit})
	if err != nil {
		return fmt.Errorf("terminal host: %w", err)
	}
	if d.rt.cfg.WrapTermAPI != nil {
		c.API = d.rt.cfg.WrapTermAPI(c.API)
	}
	d.rt.term, d.rt.hello = c, hello
	d.capabilities = append(d.capabilities, "terminals")
	for _, k := range d.rt.adapters.Kinds() {
		d.capabilities = append(d.capabilities, "adapter:"+k)
	}
	d.log.Info("terminal host", "instance", hello.InstanceId, "pid", hello.Pid)
	return d.adopt(ctx)
}

// adopt reconciles the agents the database says are running with the terminals the host has: a running one
// is taken back, never killed; an exited one gets its exit recorded; a missing one is marked lost. Terminals
// no agent claims are left running and logged.
func (d *Daemon) adopt(ctx context.Context) error {
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return err
	}
	byID := map[string]*termv1.Terminal{}
	for _, t := range res.Msg.Terminals {
		byID[t.Id] = t
	}
	claimed := map[string]bool{}
	if err := d.adoptReopens(ctx, res.Msg.Terminals, claimed); err != nil {
		return err
	}
	agents, err := d.store.Read(ctx).AgentsWithTerminals()
	if err != nil {
		return err
	}
	var adoptSweeps []*v1.Agent
	for _, a := range agents {
		if claimed[a.TerminalId] {
			continue
		}
		claimed[a.TerminalId] = true
		t, ok := byID[a.TerminalId]
		if a.State == v1.AgentState_AGENT_STATE_RETIRED && ok && !t.Exited {
			// A crash between marking it retired and stopping its process: finish the stop, and sweep leftovers
			// off the startup path so a slow process exit doesn't hold up daemon boot.
			if err := d.stopRetiredProcess(ctx, a.Id); err != nil {
				return err
			}
			adoptSweeps = append(adoptSweeps, a)
			continue
		}
		if a.State == v1.AgentState_AGENT_STATE_EXITED || a.State == v1.AgentState_AGENT_STATE_RETIRED {
			// A crash between recording the exit and acknowledging it leaves the terminal (its ring and
			// emulator) in the host: acknowledge it now. The exit is already recorded, so this only acks.
			if ok && t.Exited {
				if err := d.recordExit(ctx, t.Id, a.Id, t.Labels["reopen"], t.ExitCode, t.Signal, ""); err != nil {
					return err
				}
			}
			continue
		}
		switch {
		case !ok:
			if err := d.recordExit(ctx, a.TerminalId, "", "", -1, 0, "lost"); err != nil {
				return err
			}
		case t.Exited:
			if err := d.recordExit(ctx, t.Id, t.Labels["agent"], t.Labels["reopen"], t.ExitCode, t.Signal, ""); err != nil {
				return err
			}
		default:
			err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
				_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id,
					Payload: &v1.Event_AgentAdopted{AgentAdopted: &v1.AgentAdopted{TerminalId: t.Id}}})
				return err
			})
			if err != nil {
				return err
			}
			if t.StartedAt != nil {
				d.rt.guard.seed(a.Id, t.StartedAt.AsTime(), d.now())
			}
			if t.Pid > 0 {
				d.adoptCPUHolder(ctx, a, int(t.Pid), t.PidStartTicks)
			}
			d.log.Info("adopted terminal", "agent", a.Name, "terminal", t.Id, "pid", t.Pid)
		}
	}
	for _, t := range res.Msg.Terminals {
		if !claimed[t.Id] && t.Labels["kind"] == shellKind {
			// A project shell: the host holds it, nothing is stored. A live one is simply still there; a dead one is forgotten.
			claimed[t.Id] = true
			if t.Exited {
				if err := d.shellExited(ctx, t.Id); err != nil {
					return err
				}
			} else {
				d.log.Info("adopted shell", "terminal", t.Id, "pid", t.Pid, "repo", t.Labels["repo"])
			}
		}
		if !claimed[t.Id] {
			d.log.Warn("terminal no agent claims; left running", "terminal", t.Id, "pid", t.Pid, "labels", t.Labels)
		}
	}
	d.sweepAsync(adoptSweeps)
	return nil
}

// agentOfExit is the agent a terminal's exit is about, as recordExit finds it (nil when none): read before the write, for
// what exitWords needs.
func (d *Daemon) agentOfExit(ctx context.Context, terminalID, agentID, reopenID string) *v1.Agent {
	r := d.store.Read(ctx)
	a, err := r.AgentByTerminal(terminalID)
	if errors.Is(err, store.ErrNotFound) && agentID != "" {
		a, err = r.GetAgent(agentID)
		if err == nil && a.TerminalId != "" && a.TerminalId != terminalID {
			return nil // another process's exit, as recordExit finds
		}
	}
	if err != nil {
		return nil
	}
	if p := a.PendingReopen; p != nil && p.Id != reopenID {
		return nil // a reopen's own stop of the old process: recordExit records nothing and tells no one
	}
	return a
}

// recordExit marks a terminal's agent exited (unless it already is), then lets the host forget the terminal.
// The agent is found by the terminal, or by the agent label the terminal was created with: a CLI that exits
// at once can do so before its spawn recorded the terminal. A terminal no agent claims is not acknowledged.
//
// While a reopen is pending, the exit of the agent's terminal is the reopen stopping it on purpose: not
// recorded, nor its token revoked; the reopen relaunches the conversation (reopenID, the terminal's reopen
// label, tells a reopen's own new terminal, whose exit is a real one).
func (d *Daemon) recordExit(ctx context.Context, terminalID, agentID, reopenID string, code, signal int32, reason string) error {
	if reason == "" {
		reason = "exited"
		if signal != 0 {
			reason = "killed"
		}
	}
	claimed, replaced, realExit, owner := false, false, false, ""
	var alert *v1.Message
	words := d.exitWords(ctx, d.agentOfExit(ctx, terminalID, agentID, reopenID)) // read before the write that records the exit
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.AgentByTerminal(terminalID)
		if errors.Is(err, store.ErrNotFound) && agentID != "" {
			a, err = tx.GetAgent(agentID)
			if err == nil && a.TerminalId != "" && a.TerminalId != terminalID {
				err = store.ErrNotFound
			}
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		claimed, owner = true, a.Id
		if isAdopted(a) {
			// The terminal of the root an adopted session took the row over from: its process was stopped on
			// purpose, and the row is a session the daemon never ran. Only the terminal is forgotten.
			return nil
		}
		if a.State == v1.AgentState_AGENT_STATE_RETIRED {
			// Stopped on purpose: nothing to record, only the terminal to forget.
			if a.TerminalId == terminalID {
				a.TerminalId = ""
				return tx.PutAgent(a)
			}
			return nil
		}
		if p := a.PendingReopen; p != nil && p.Id != reopenID {
			replaced = true
			if a.TerminalId != terminalID {
				// A reopen's new process that died before its terminal was recorded, its reopen replaced:
				// finishReopen must not record it.
				d.rt.mu.Lock()
				if _, ok := d.rt.launching[reopenID]; ok && a.TerminalId == "" {
					d.rt.launching[reopenID] = true
				}
				d.rt.mu.Unlock()
				return nil
			}
			a.TerminalId = "" // its process is gone: the reopen needn't wait for it to be idle
			return tx.PutAgent(a)
		}
		if a.State == v1.AgentState_AGENT_STATE_EXITED {
			return nil
		}
		// A reopen's own terminal that died: the reopen happened, and its agent exited.
		realExit = true
		lastSaid := a.StateDetail // the exit event below overwrites it
		a.PendingReopen = nil
		a.TerminalId = terminalID
		tx.Actor = &v1.Actor{Class: ClassOf(a.Role), Id: a.Id, Name: a.Name, RunId: a.RunId}
		ev := &adapterv1.AgentEvent{Time: timestamppb.New(tx.Now()),
			Kind: &adapterv1.AgentEvent_ProcessExited{ProcessExited: &adapterv1.ProcessExited{ExitCode: code, Signal: signal}}}
		if err := d.applyEvents(tx, a, []*adapterv1.AgentEvent{ev}); err != nil {
			return err
		}
		if reason == "lost" {
			// A lost terminal is exit code -1 like a stop whose process was gone: the stored detail tells them apart.
			if err := setExitDetail(tx, a, "lost"); err != nil {
				return err
			}
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentExited{
			AgentExited: &v1.AgentExited{ExitCode: code, Signal: signal, Reason: reason}}}); err != nil {
			return err
		}
		// Its parent hears of it when it still owed a report (a stop someone asked for is told too: it is the
		// parent's agent that ended).
		if alert, err = d.alertExitTx(tx, a, lastSaid, words, code, signal, reason); err != nil {
			return err
		}
		return tx.RevokeTokens(a.Id)
	})
	if err != nil || !claimed {
		return err
	}
	if alert != nil && d.notify != nil {
		d.notify(alert.ToId)
	}
	if realExit {
		d.rt.guard.release(owner) // it launched, or failed to, and holds no more of the host's memory
	}
	if replaced {
		// Its reopen goes on (a process that died mid-turn with one pending needn't wait for an IDLE).
		d.deliverSoon(owner)
	}
	if reason == "lost" {
		return nil
	}
	d.rt.mu.Lock()
	delete(d.rt.lastOutput, terminalID)
	delete(d.rt.introduced, terminalID)
	delete(d.rt.nativeOff, terminalID)
	delete(d.rt.firstOutput, terminalID)
	delete(d.rt.held, owner) // a draft held for the old process says nothing of the next one's
	d.rt.mu.Unlock()
	d.clearOpenCodeTerminal(terminalID)
	d.clearCopilotTerminal(terminalID)
	_, err = d.rt.term.API.AckExit(ctx, connect.NewRequest(&termv1.AckExitRequest{Id: terminalID}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		err = nil // the reopen acknowledged it first
	}
	return err
}

func (d *Daemon) runBackground(ctx context.Context) {
	d.goBG(func() { d.sweepHeld(ctx) }) // tasks held for a done that came while the daemon was down, and the ones a busy host left
	if d.rt.term != nil && !d.rt.cfg.NoTerminalWatch {
		d.goBG(func() { d.watchTerminals(ctx) })
	}
	d.goBG(func() {
		t := time.NewTicker(d.rt.cfg.SpoolEvery)
		defer t.Stop()
		var pruned, retired, limited, accounted, reserved, contextTick, sampled, prChecked, prWatched time.Time
		accountEvery := d.rt.cfg.AccountEvery
		if accountEvery == 0 {
			accountEvery = accountEveryDefault
		}
		reserveEvery := d.rt.cfg.ReserveEvery
		if reserveEvery == 0 {
			reserveEvery = reserveAlertEvery
		}
		var retiring, limiting, alertRunning, pollingOpenCode, pollingCopilot, sampling, watchingPRs atomic.Bool
		sampled = d.now() // the first capacity pass is one interval after start, not at once
		prWatched = sampled
		retireEvery := d.rt.cfg.RetireEvery
		if retireEvery == 0 {
			retireEvery = retireQuiet
		}
		for {
			d.drainSpool(ctx)
			d.settleStartingAgents(ctx)
			if pollingOpenCode.CompareAndSwap(false, true) {
				// Off this loop: readScreen waits up to 3 s per agent, and the spool and the doorbells must not.
				d.goBG(func() {
					defer pollingOpenCode.Store(false)
					d.pollOpenCode(ctx)
				})
			}
			if pollingCopilot.CompareAndSwap(false, true) {
				d.goBG(func() {
					defer pollingCopilot.Store(false)
					d.pollCopilot(ctx)
				})
			}
			d.retryDoorbells(ctx)
			if now := d.now(); now.Sub(contextTick) >= orDuration(d.rt.cfg.ContextEvery, contextEvery) {
				contextTick = now
				d.contextPass(ctx)
			}
			if now := d.now(); now.Sub(retired) >= retireEvery && retiring.CompareAndSwap(false, true) {
				retired = now
				// Off this loop: stopping a process waits for it, and the spool and the doorbells must not.
				d.goBG(func() {
					defer retiring.Store(false)
					d.retireIdle(ctx)
				})
			}
			if now := d.now(); now.Sub(accounted) >= accountEvery {
				accounted = now
				d.accountability(ctx)
				d.adoptedPass(ctx)
			}
			if now := d.now(); now.Sub(limited) >= retireEvery && limiting.CompareAndSwap(false, true) {
				limited = now
				// Off this loop, as the retire pass is: a reopen takes the agent's lock and the store's writer.
				d.goBG(func() {
					defer limiting.Store(false)
					d.limitPass(ctx)
				})
			}
			if now := d.now(); now.Sub(reserved) >= reserveEvery && alertRunning.CompareAndSwap(false, true) {
				reserved = now
				d.goBG(func() {
					defer alertRunning.Store(false)
					d.reserveAlertPass(ctx)
					d.agyLoginAlertPass(ctx)
				})
			}
			if now := d.now(); now.Sub(sampled) >= orDuration(d.rt.cfg.CapacityEvery, capacityEvery) && sampling.CompareAndSwap(false, true) {
				sampled = now
				// Off this loop: it reads /proc for every agent's process tree.
				d.goBG(func() {
					defer sampling.Store(false)
					d.sampleCapacity(ctx)
				})
			}
			if now := d.now(); now.Sub(prChecked) >= prWatchCheckEvery {
				prChecked = now
				if every := d.prWatchEvery(ctx); every > 0 && now.Sub(prWatched) >= every && watchingPRs.CompareAndSwap(false, true) {
					prWatched = now
					// Off this loop: it runs `gh`, which waits for the network.
					d.goBG(func() {
						defer watchingPRs.Store(false)
						d.prFeedbackPass(ctx)
					})
				}
			}
			if now := d.now(); now.Sub(pruned) > time.Hour {
				pruned = now
				d.pruneMutations(ctx)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// Retention of the idempotency records: a hook's is short (it is retried within minutes, and there is one per
// hook), any other write's is long enough for a client to retry across a restart.
const (
	hookMutationRetention = 24 * time.Hour
	mutationRetention     = 14 * 24 * time.Hour
)

func (d *Daemon) pruneMutations(ctx context.Context) {
	now := d.now()
	var pruned int64
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		if _, err := tx.PruneMutations(now.Add(-hookMutationRetention), now.Add(-mutationRetention)); err != nil {
			return err
		}
		if _, err := tx.PrunePrompts(now.Add(-mutationRetention)); err != nil {
			return err
		}
		n, err := tx.PrunePairings(now)
		pruned = n
		return err
	})
	if err != nil && ctx.Err() == nil {
		d.log.Warn("pruning the mutation records", "err", err)
	}
	if err == nil && pruned > 0 {
		d.relayTokensChanged()
	}
}

// watchTerminals follows exits and output marks, reconnecting until the daemon stops.
func (d *Daemon) watchTerminals(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		stream, err := d.rt.term.API.WatchTerminals(ctx, connect.NewRequest(&termv1.WatchTerminalsRequest{}))
		if err == nil {
			for stream.Receive() {
				backoff = 100 * time.Millisecond
				switch f := stream.Msg().Frame.(type) {
				case *termv1.WatchTerminalsResponse_Exited:
					if f.Exited.Labels["kind"] == shellKind {
						if err := d.shellExited(ctx, f.Exited.Id); err != nil && ctx.Err() == nil {
							d.log.Error("forgetting a shell", "terminal", f.Exited.Id, "err", err)
						}
						continue
					}
					if err := d.recordExit(ctx, f.Exited.Id, f.Exited.Labels["agent"], f.Exited.Labels["reopen"], f.Exited.ExitCode, f.Exited.Signal, ""); err != nil && ctx.Err() == nil {
						d.log.Error("recording an exit", "terminal", f.Exited.Id, "err", err)
					}
				case *termv1.WatchTerminalsResponse_Output:
					t := f.Output.Time.AsTime()
					d.rt.mu.Lock()
					d.rt.lastOutput[f.Output.Id] = t
					if _, seen := d.rt.firstOutput[f.Output.Id]; !seen {
						d.rt.firstOutput[f.Output.Id] = t
					}
					d.rt.mu.Unlock()
				}
			}
			stream.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

// drainSpool applies the hooks that fired while the daemon was down, oldest first per agent.
func (d *Daemon) drainSpool(ctx context.Context) {
	agents, err := hook.Agents(d.paths)
	if err != nil {
		d.log.Warn("reading the hook spool", "err", err)
		return
	}
	for _, id := range agents {
		a, err := d.store.Read(ctx).GetAgent(id)
		if errors.Is(err, store.ErrNotFound) {
			os.RemoveAll(d.paths.Spool(id)) // an agent that never ran (its launch failed)
			continue
		}
		if err != nil {
			return
		}
		unlock, err := hook.LockSpool(d.paths, id)
		if err != nil {
			continue
		}
		actx := auth.WithActor(ctx, &v1.Actor{Class: ClassOf(a.Role), Id: a.Id, Name: a.Name, RunId: a.RunId})
		err = hook.DrainReporting(d.paths, id, func(req *v1.ReportHookRequest) error {
			_, err := d.reportHook(actx, req)
			return err
		}, func(req *v1.ReportHookRequest, cause error) {
			d.log.Warn("dropped a hook that failed every time", "agent", a.Name, "event", req.EventName, "err", cause)
			d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
				_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_HookDecodeFailed{
					HookDecodeFailed: &v1.HookDecodeFailed{Error: fmt.Sprintf("dropped a %s hook after %d failed attempts over %s: %v",
						req.EventName, hook.MaxAttempts, hook.MaxAge, cause)}}})
				return err
			})
		})
		if err == nil && a.State == v1.AgentState_AGENT_STATE_EXITED {
			// Nothing more comes from an exited agent: its spool dir goes once it is empty.
			if left, _ := filepath.Glob(filepath.Join(d.paths.Spool(id), "*.json")); len(left) == 0 {
				os.RemoveAll(d.paths.Spool(id))
			}
		}
		unlock()
		if err != nil && ctx.Err() == nil {
			d.log.Warn("draining the hook spool", "agent", a.Name, "err", err)
		}
	}
}

// ── hooks and the reducer ──

// applyEvents runs an agent's events through the reducer and stores the result, with an event when the
// state or the needs-input count changed, and one per payload that didn't decode.
func (d *Daemon) applyEvents(tx *store.Tx, a *v1.Agent, evs []*adapterv1.AgentEvent) error {
	before := reducer.Of(a)
	s := before
	for _, ev := range evs {
		if bad := ev.GetDecodeError(); bad != nil {
			if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_HookDecodeFailed{
				HookDecodeFailed: &v1.HookDecodeFailed{Error: bad.Error}}}); err != nil {
				return err
			}
		}
		if rl := ev.GetRateLimited(); rl != nil {
			if err := d.noteLimit(tx, a, rl, ev.Time.AsTime()); err != nil {
				return err
			}
		}
		s = reducer.Reduce(s, ev)
	}
	a.State, a.NeedsInput, a.StateDetail, a.SessionId, a.ExitCode = s.State, s.NeedsInput, s.Detail, s.SessionID, s.ExitCode
	a.StateBeforeCompact = s.BeforeCompact
	noteHookEvents(a, evs, tx.Now())
	if !s.LastActivity.IsZero() {
		a.LastActivity = timestamppb.New(s.LastActivity)
	}
	if err := tx.PutAgent(a); err != nil {
		return err
	}
	if before.State == v1.AgentState_AGENT_STATE_LIMITED && s.State != v1.AgentState_AGENT_STATE_LIMITED {
		d.clearOpenCodeEvidence(a.Id)
	}
	// The detail alone (the tool now running) changes on every tool call: kept on the agent, not logged, but told to
	// the live subscribers as an ephemeral frame (what the apps' dock shows).
	if s.State == before.State && s.NeedsInput == before.NeedsInput {
		for _, ev := range evs {
			if ev.GetActivity() != nil {
				// Told once the write has committed: a rolled-back one changed nothing.
				frame := &v1.AgentActivity{RunId: a.RunId, AgentId: a.Id, Detail: s.Detail, Time: timestamppb.New(s.LastActivity)}
				tx.AfterCommit(func() { d.bus.PublishActivity(frame) })
				break
			}
		}
		return nil
	}
	_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentStateChanged{
		AgentStateChanged: &v1.AgentStateChanged{From: before.State, To: s.State, Detail: s.Detail, NeedsInput: s.NeedsInput}}})
	return err
}

// ReportHook takes a hook of the calling agent's CLI.
func (d *Daemon) ReportHook(ctx context.Context, req *connect.Request[v1.ReportHookRequest]) (*connect.Response[v1.ReportHookResponse], error) {
	out, err := d.reportHook(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (d *Daemon) reportHook(ctx context.Context, req *v1.ReportHookRequest) (*v1.ReportHookResponse, error) {
	actor := auth.ActorFrom(ctx)
	a, err := d.store.Read(ctx).GetAgent(actor.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no agent %s", actor.Id)
	}
	if err != nil {
		return nil, err
	}
	if req.Kind != a.Kind {
		return nil, invalid("a %s hook for a %s agent", req.Kind, a.Kind)
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return nil, err
	}
	fired := req.FiredAt
	if fired == nil {
		fired = timestamppb.New(d.now())
	}
	dec, err := ad.DecodeHook(ctx, connect.NewRequest(&adapterv1.DecodeHookRequest{AgentId: a.Id,
		SessionId: a.SessionId, EventName: req.EventName, Payload: req.Payload, FiredAt: fired}))
	if err != nil {
		return nil, err
	}
	out := &v1.ReportHookResponse{}
	_, err = d.write(ctx, req.Meta, agentmeshv1connect.MeshServiceReportHookProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(actor.Id)
		if err != nil {
			return err
		}
		if a.State == v1.AgentState_AGENT_STATE_RETIRED {
			out.State = a.State // a hook of the process it stopped, spooled before: nothing to apply
			return nil
		}
		if err := d.applyEvents(tx, a, dec.Msg.Events); err != nil {
			return err
		}
		out.State = a.State
		return nil
	})
	if err != nil {
		return nil, err
	}
	if src := reminderSource(a, dec.Msg.Events); src != "" {
		// What the session that started again needs to know (compact, clear, resume): the hook prints it.
		if cur, err := d.store.Read(ctx).GetAgent(a.Id); err == nil {
			out.Context = hookText(d.reminderText(ctx, cur, src))
		}
	}
	d.permissionEnded(a.Id, req.EventName, req.Payload) // an agent that went on has no prompt waiting
	// A doorbell in flight is checked against this very activity, not at the next tick.
	if takesDoorbell(out.State) || d.hasRing(actor.Id) {
		d.deliverSoon(actor.Id)
	}
	return out, nil
}

// ── spawning ──

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// mayStart is who may start which role: a device any, the root workers and read-only agents, a worker only
// read-only agents.
func mayStart(c v1.ActorClass, role v1.AgentRole) bool {
	switch c {
	case v1.ActorClass_ACTOR_CLASS_DEVICE:
		return true
	case v1.ActorClass_ACTOR_CLASS_ROOT:
		return role != v1.AgentRole_AGENT_ROLE_ROOT
	case v1.ActorClass_ACTOR_CLASS_WORKER:
		return role == v1.AgentRole_AGENT_ROLE_ASKER || role == v1.AgentRole_AGENT_ROLE_REVIEWER
	}
	return false
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultBase is where a worktree starts when the spawn names no base: the remote's default branch (`origin/main`,
// as the repo's own origin/HEAD says), not whatever branch the user's checkout happens to be on, so a worker
// never branches off a feature branch by accident. A repo with no origin/HEAD starts from its HEAD. It is the
// local copy of the remote branch, as `bin/mesh` takes it: fetch first when it should be newer.
func defaultBase(ctx context.Context, top string) string {
	if ref, err := git(ctx, top, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil && ref != "" && ref != "origin/HEAD" {
		if _, err := git(ctx, top, "rev-parse", "--verify", "-q", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return "HEAD"
}

func (d *Daemon) worktrees() string {
	if d.rt.cfg.Worktrees != "" {
		return d.rt.cfg.Worktrees
	}
	return d.paths.Worktrees()
}

// agentEnv is the environment an agent starts with: an allowlist of the daemon's, a terminal, the agent's
// identity, and the configured extras.
func (d *Daemon) agentEnv(a *v1.Agent, token string) map[string]string {
	env := map[string]string{}
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if a.Kind == opencode.Kind && !strings.HasPrefix(a.Model, "opencode/") {
		for _, k := range opencodeInheritedEnv {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
	}
	if a.Kind == copilot.Kind {
		for _, k := range copilotInheritedEnv {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
	}
	env["TERM"], env["COLORTERM"] = "xterm-256color", "truecolor"
	env[client.TokenEnv], env[client.StateEnv] = token, d.paths.Dir
	env["AGENT_MESH_AGENT"], env["AGENT_MESH_RUN"] = a.Id, a.RunId
	env[client.RunDirEnv] = d.paths.RunDir(a.RunId)
	if s := d.rt.cfg.Share; filepath.IsAbs(s) {
		env[client.ShareEnv] = s
	}
	for k, v := range d.rt.cfg.Env {
		env[k] = v
	}
	// The folder of the host's own `mesh` goes first on the agent's PATH, so a bare `mesh` is this daemon's CLI
	// whatever else is installed.
	if m := d.rt.cfg.Mesh; filepath.IsAbs(m) {
		rest := env["PATH"]
		if rest == "" {
			rest = "/usr/local/bin:/usr/bin:/bin" // a daemon with no PATH: never leave the agent with only our folder
		}
		env["PATH"] = filepath.Dir(m) + string(os.PathListSeparator) + rest
	}
	return env
}

// ensureRunDir makes a run's folders; it is safe to call again.
func (d *Daemon) ensureRunDir(run string) error {
	for _, f := range RunFolders {
		if err := os.MkdirAll(filepath.Join(d.paths.RunDir(run), f), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// plainSlug is the slug a plain agent is made with: the one asked for, else it with `-2`, `-3` … the first that no other
// plain agent of the repo that is still open (not retired) has in its name. Its name is what its tab shows.
func plainSlug(r store.Reader, repo, slug string) (string, error) {
	all, err := r.AllAgents()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, a := range all {
		if a.Plain && a.Repo == repo && a.State != v1.AgentState_AGENT_STATE_RETIRED {
			taken[a.Name] = true
		}
	}
	for n := 1; ; n++ {
		cand := slug
		if n > 1 {
			cand = fmt.Sprintf("%s-%d", slug, n)
		}
		if !taken[filepath.Base(repo)+"-"+cand] {
			return cand, nil
		}
	}
}

// SpawnAgent starts an agent in a new worktree of a repo, in a terminal of the host.
func (d *Daemon) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	role := m.Role
	if role == v1.AgentRole_AGENT_ROLE_UNSPECIFIED {
		role = v1.AgentRole_AGENT_ROLE_WORKER
	}
	if !mayStart(actor.Class, role) {
		return nil, denied("%s may not start a %s", actor.Class, role)
	}
	if err := checkReviewBy(m.Reviews); err != nil {
		return nil, err
	}
	// A root that starts a change worker says what it is for: the one line the user reads beside its name.
	if actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && role == v1.AgentRole_AGENT_ROLE_WORKER && strings.TrimSpace(m.About) == "" {
		return nil, invalid("say what the worker is for: --about \"<one line, up to %d characters>\"", maxAbout)
	}
	// A plain agent is the user's own conversation: a root that gets no task, so the mesh's procedure never reaches it.
	if m.Plain && (role != v1.AgentRole_AGENT_ROLE_ROOT || strings.TrimSpace(m.Task) != "" || m.Plan) {
		return nil, invalid("a plain agent is a root with no task and no plan: type to it in its chat instead")
	}
	if d.rt.term == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this daemon runs without a terminal host"))
	}
	kind := m.Kind
	if kind == "" {
		kind = claude.Kind
	}
	ad, err := d.rt.adapters.Get(kind)
	if err != nil {
		return nil, err
	}
	if !slugRE.MatchString(m.Slug) {
		return nil, invalid("slug %q: lowercase letters, digits and dashes", m.Slug)
	}
	runID := m.RunId
	if IsAgent(actor.Class) {
		if runID == "" {
			runID = actor.RunId
		} else if runID != actor.RunId {
			return nil, denied("an agent starts agents only in its own run")
		}
	}
	r := d.store.Read(ctx)
	run, err := r.GetRun(runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no run %q", runID)
	}
	if err != nil {
		return nil, err
	}
	if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %s is closed", runID))
	}
	// A retried spawn returns the first one's agent without creating anything.
	if mid := m.Meta.GetMutationId(); mid != "" {
		if _, resp, err := r.Mutation(actor.Id, mid); err == nil {
			out := &v1.SpawnAgentResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	if role != v1.AgentRole_AGENT_ROLE_ROOT {
		d.warmUsage(ctx, r, kind, agyResolveKey{model: m.Model, effort: m.Effort})
	}
	d.warmOpenCodeModels(ctx, kind)
	// A repo set against free models (freemodels.go) gets no free OpenCode model; an unresolvable repo is the
	// error repoTop gives below.
	var freeOff, freeRepo string
	if kind == opencode.Kind && strings.TrimSpace(m.Repo) != "" {
		if t, err := d.repoTop(ctx, m.Repo); err == nil {
			freeRepo, freeOff = t, d.freeModelsOff(r, t)
		}
	}
	// What it runs on: a tier, or a model the user named; never a default (bin/mesh's need_tier). A root's model
	// is the user's own (design §9): it launches on the CLI's default unless a tier or model is named.
	var model, effort string
	if freeOff != "" && role == v1.AgentRole_AGENT_ROLE_ROOT && m.Tier == "" && m.Model == "" && m.Effort == "" {
		// Nothing named, so the CLI's default model, which may be a free one: a root here names a paid model.
		return nil, freeRootRefusal(freeRepo, freeOff)
	}
	if role != v1.AgentRole_AGENT_ROLE_ROOT || m.Tier != "" || m.Model != "" || m.Effort != "" {
		if role == v1.AgentRole_AGENT_ROLE_ROOT && m.Tier == "" && m.Model == "" {
			if freeOff != "" {
				return nil, freeRootRefusal(freeRepo, freeOff) // the CLI's default model may be a free one
			}
			// An effort alone: the CLI's default model at that effort.
			if !validEffort(m.Effort) {
				return nil, invalid("effort %q: one of %s", m.Effort, strings.Join(settings.Efforts, ", "))
			}
			effort = m.Effort
		} else if model, effort, err = d.pickIn(r, freeRepo, freeOff, kind, m.Tier, m.Model, m.Effort, "the agent"); err != nil {
			return nil, err
		}
	}
	if m.ExecTier != "" {
		if !m.Plan {
			return nil, invalid("exec_tier is for a task with a plan phase (plan)")
		}
		if _, _, err := d.tierChain(r, kind, m.ExecTier); err != nil {
			return nil, err
		}
	}
	// The reserve, cap and memory floor, early, before a worktree is made for nothing; the launch checks cap and
	// memory again where the agent is stored (reserving its memory then).
	if role != v1.AgentRole_AGENT_ROLE_ROOT {
		if err := d.reserveCheck(r, runID, kind, model, effort, "an agent in "+m.Repo); err != nil {
			return nil, err
		}
	}
	if err := d.capGuard(r, "an agent in "+m.Repo, "", capShared(ctx)...); err != nil {
		return nil, err
	}
	if err := d.checkMemoryGuard(r, "an agent in "+m.Repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Repo) == "" {
		return nil, invalid("no repo given")
	}
	top, err := d.repoTop(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	mcpNames, _, err := parseMCPArg(m.Mcp)
	if err != nil {
		return nil, err
	}
	if err := d.checkMCP(ctx, kind, top, mcpNames); err != nil {
		return nil, err
	}
	if err := d.checkMCPLease(r, mcpNames, "", auth.ActorFrom(ctx)); err != nil {
		return nil, err
	}
	slug := m.Slug
	if m.Plain {
		// Each plain agent is a run of its own, so the run's check for a name taken would let two of a CLI in one repo
		// share a name, and a tab: the second takes `-2`, the third `-3`. Plain spawns go one at a time from here to the
		// store write, so two at once do not both take the same slug (`New agent` clicked twice).
		d.plainSpawn.Lock()
		defer d.plainSpawn.Unlock()
		if slug, err = plainSlug(d.store.Read(ctx), top, m.Slug); err != nil {
			return nil, err
		}
	}
	name := filepath.Base(top) + "-" + slug
	if same, err := r.AgentsNamed(runID, name); err != nil {
		return nil, err
	} else if len(same) > 0 {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("run %s already has %s", runID, name))
	}
	base := m.Base
	if base == "" {
		base = defaultBase(ctx, top)
	}
	if strings.HasPrefix(base, "-") {
		return nil, invalid("base %q looks like an option", base)
	}
	if err := d.ensureRunDir(runID); err != nil {
		return nil, err
	}
	branch := "mesh/" + runID + "/" + slug
	worktree := filepath.Join(d.worktrees(), runID, name)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return nil, err
	}
	if _, err := git(ctx, top, "worktree", "add", "-b", branch, worktree, base); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	d.includeInto(ctx, top, worktree)
	// Where its branch started, so a branch that never moved can be told from one with work (StopRun).
	baseCommit, _ := git(ctx, worktree, "rev-parse", "HEAD")

	token := auth.NewToken()
	parent := ""
	if IsAgent(actor.Class) {
		parent = actor.Id
	}
	now := d.now()
	agent := &v1.Agent{Id: ids.New("a", 10), RunId: runID, Name: name, Role: role, ParentId: parent, Kind: kind,
		Repo: top, Worktree: worktree, Branch: branch, BaseCommit: baseCommit, Model: model, Effort: effort, Tier: spawnTier(m),
		State: v1.AgentState_AGENT_STATE_STARTING, LastActivity: timestamppb.New(now), CreatedAt: timestamppb.New(now),
		// A planner plans under the deny list until approved; a read-only role keeps it.
		DenyEdits: m.Plan || adapter.ReadOnly(role), Mcp: mcpNames, ReviewBy: m.Reviews,
		// A named model or effort covers the whole task: no plan phase, now or later.
		Pinned: role != v1.AgentRole_AGENT_ROLE_ROOT && !m.Plan && (m.Model != "" || m.Effort != ""), Plain: m.Plain}
	if m.Plan {
		agent.Phase, agent.ExecTier = v1.AgentPhase_AGENT_PHASE_PLANNING, m.ExecTier
	}
	agent.About, agent.AboutSource = spawnAbout(m, role, run)
	// Held until its terminal is recorded: a reopen asked for meanwhile would see no terminal, take the agent
	// for stopped, and start a second process on its conversation.
	mu := d.agentLock(agent.Id)
	mu.Lock()
	defer mu.Unlock()
	cleanup := func() {
		git(context.Background(), top, "worktree", "remove", "--force", worktree)
		git(context.Background(), top, "branch", "-D", branch)
		// The lean launch's MCP file (planLaunch writes it) may hold servers' secrets.
		os.RemoveAll(d.paths.Agent(agent.Id))
	}
	// The launch is planned before the agent is stored, so its conversation id is known before its first hook.
	plan, err := d.planLaunch(ctx, ad, agent, token, "")
	if err != nil {
		cleanup()
		return nil, err
	}
	agent.SessionId = plan.SessionId
	if spawnPlannedHook != nil {
		spawnPlannedHook()
	}
	out := &v1.SpawnAgentResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceSpawnAgentProcedure, out, func(tx *store.Tx) error {
		// The run was open when this began: StopRun may have closed it since (the writer serializes the two).
		if run, err := tx.GetRun(runID); err != nil {
			return err
		} else if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			return failedPrecondition("run %s is closed", runID)
		}
		if err := d.mayLaunch(tx.Reader, "agent "+name, agent.Id, capShared(ctx)...); err != nil {
			return err
		}
		if err := d.checkMCPLease(tx.Reader, agent.Mcp, agent.Id, tx.Actor); err != nil {
			return err
		}
		if err := tx.PutAgent(agent); err != nil {
			return err
		}
		if err := tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(role), SubjectID: agent.Id,
			Name: name, RunID: runID}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: runID, AgentId: agent.Id,
			Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: agent}}}); err != nil {
			return err
		}
		if strings.TrimSpace(m.Task) != "" {
			task := withOpenCodeBodyNote(kind, m.Task)
			msg := &v1.Message{Id: ids.New("m", 10), RunId: runID, From: tx.Actor, ToId: agent.Id, ToName: name,
				Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: task, CreatedAt: timestamppb.New(tx.Now())}
			if err := tx.PutMessage(msg); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{RunId: runID, AgentId: agent.Id,
				Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: msg}}}); err != nil {
				return err
			}
		}
		// A server its repo loads that another agent holds stayed out of its MCP file (leanLaunch): tell it.
		if _, err := d.noteHeldUnnamed(tx, agent); err != nil {
			return err
		}
		out.Agent = agent
		return nil
	})
	if err != nil {
		d.rt.guard.release(agent.Id)
		cleanup()
		return nil, err
	}
	if spawnCommittedHook != nil {
		spawnCommittedHook(agent.Id)
	}
	launched, err := d.launch(ctx, agent, token, plan)
	if err != nil {
		d.log.Error("launch failed", "agent", name, "err", err)
		d.failLaunch(ctx, actor, m.Meta.GetMutationId(), agent, err)
		d.rt.guard.release(agent.Id)
		cleanup()
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("launching %s: %w", name, err))
	}
	// Its first hooks may have come before its terminal was recorded: ring for its task now.
	d.deliverSoon(agent.Id)
	return connect.NewResponse(&v1.SpawnAgentResponse{Agent: d.view(launched), Capacity: d.capacityLine(ctx)}), nil
}

// planLaunch asks the adapter for an agent's command line: a fresh conversation, or (resume set) a reopen of
// that one. The model, effort and deny list are the agent's.
func (d *Daemon) planLaunch(ctx context.Context, ad adapter.Adapter, a *v1.Agent, token, resume string) (*adapterv1.PlanLaunchResponse, error) {
	mesh := d.rt.cfg.Mesh
	if mesh == "" {
		return nil, errors.New("no mesh configured for the hooks")
	}
	env := d.agentEnv(a, token)
	lean, err := d.leanLaunch(ctx, a, env)
	if err != nil {
		return nil, fmt.Errorf("planning %s's lean session: %w", a.Name, err)
	}
	plan, err := ad.PlanLaunch(ctx, connect.NewRequest(&adapterv1.PlanLaunchRequest{AgentId: a.Id, AgentName: a.Name,
		RunId: a.RunId, Role: a.Role, Cwd: a.Worktree, Repo: a.Repo, Model: a.Model, Effort: a.Effort, Binary: d.rt.cfg.Binaries[a.Kind],
		HookArgv: []string{mesh, "hook", a.Kind, "--state", d.paths.Dir, "--agent", a.Id},
		Env:      env, SkipPermissions: d.rt.cfg.SkipPermissions,
		ResumeSessionId: resume, DenyEdits: a.DenyEdits, Lean: lean}))
	if err != nil {
		return nil, err
	}
	// The host runs argv[0] on its own PATH (a systemd unit's is minimal): a bare name is resolved here, on the
	// agent's.
	if len(plan.Msg.Argv) > 0 && !strings.Contains(plan.Msg.Argv[0], "/") {
		if path, ok := lookPath(plan.Msg.Argv[0], plan.Msg.Env["PATH"]); ok {
			plan.Msg.Argv[0] = path
		}
	}
	return plan.Msg, nil
}

// startTerminal writes the token the hooks read and starts the planned command in a terminal labelled with the
// agent (and with the reopen it carries out, if any).
func (d *Daemon) startTerminal(ctx context.Context, a *v1.Agent, token string, plan *adapterv1.PlanLaunchResponse,
	reopenID string) (string, error) {
	if err := os.MkdirAll(d.paths.Agent(a.Id), 0o700); err != nil {
		return "", err
	}
	if err := fsutil.WriteAtomic(d.paths.AgentToken(a.Id), []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := d.writeWorktreeFiles(ctx, a.Worktree, plan.WorktreeFiles); err != nil {
		return "", err
	}
	labels := map[string]string{"agent": a.Id, "run": a.RunId, "name": a.Name}
	if reopenID != "" {
		labels["reopen"] = reopenID
	}
	term, err := d.rt.term.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: plan.Argv, Env: plan.Env, Cwd: a.Worktree, Cols: 120, Rows: 40, Labels: labels}))
	if err != nil {
		return "", err
	}
	if a.Role != v1.AgentRole_AGENT_ROLE_ROOT && term.Msg.Terminal.Pid > 0 {
		d.confineAgent(ctx, int(term.Msg.Terminal.Pid), term.Msg.Terminal.PidStartTicks, a.Name)
	}
	return term.Msg.Terminal.Id, nil
}

// launch starts an agent's first terminal and records it.
func (d *Daemon) launch(ctx context.Context, a *v1.Agent, token string, plan *adapterv1.PlanLaunchResponse) (*v1.Agent, error) {
	tid, err := d.startTerminal(ctx, a, token, plan, "")
	if err != nil {
		return nil, err
	}
	var out *v1.Agent
	err = d.store.Write(ctx, auth.ActorFrom(ctx), "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		cur.TerminalId = tid
		out = cur
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		// Tell the apps which terminal it is: AgentSpawned was written before it existed, and without this they learn
		// it only when they list the agents again (their Terminal tab would be missing for the first half minute).
		_, err = tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id,
			Payload: &v1.Event_AgentAdopted{AgentAdopted: &v1.AgentAdopted{TerminalId: tid, Launched: true}}})
		return err
	})
	if err != nil {
		// A terminal no agent records would run on unseen: stop it.
		d.rt.term.API.KillTerminal(context.Background(), connect.NewRequest(&termv1.KillTerminalRequest{Id: tid}))
	}
	return out, err
}

// failLaunch undoes a launch that never started: the agent's record, token, pending messages and the
// spawn's mutation go, so the same slug (or a retry of the same write) can try again; the events stay, ending
// with the agent's exit as "launch failed".
func (d *Daemon) failLaunch(ctx context.Context, actor *v1.Actor, mutationID string, a *v1.Agent, cause error) {
	err := d.store.Write(context.Background(), actor, "", func(tx *store.Tx) error {
		if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentExited{
			AgentExited: &v1.AgentExited{ExitCode: -1, Reason: "launch failed: " + cause.Error()}}}); err != nil {
			return err
		}
		if err := tx.RevokeTokens(a.Id); err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM messages WHERE to_id = ?`, a.Id); err != nil {
			return err
		}
		if mutationID != "" {
			if err := tx.Exec(`DELETE FROM mutations WHERE actor_id = ? AND id = ?`, actor.Id, mutationID); err != nil {
				return err
			}
		}
		return tx.DeleteAgent(a.Id)
	})
	os.RemoveAll(d.paths.Agent(a.Id))
	if err != nil {
		d.log.Error("recording a failed launch", "agent", a.Name, "err", err)
	}
}

// StopAgent stops an agent's process group; its exit is recorded when the host reports it.
func (d *Daemon) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if IsAgent(actor.Class) && a.ParentId != actor.Id {
		return nil, denied("an agent stops only its own children")
	}
	if isAdopted(a) {
		return nil, failedPrecondition("%s is your own Claude Code session (process %d): mesh never stops it; `mesh retire` takes it out of the run", a.Name, a.AdoptedPid)
	}
	// Held against a reopen in progress: a stop must not be undone by the relaunch.
	mu := d.agentLock(a.Id)
	mu.Lock()
	defer mu.Unlock()
	if stopAgentLockedHook != nil {
		stopAgentLockedHook()
	}
	out := &v1.StopAgentResponse{}
	dropped := false
	var alert *v1.Message
	words := ""
	if a.PendingReopen != nil && a.TerminalId == "" { // the case that records the exit below
		words = d.exitWords(ctx, a)
	}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceStopAgentProcedure, out, func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		if cur.PendingReopen != nil {
			if err := cancelReopen(tx, cur, "stopped"); err != nil {
				return err
			}
			dropped = true
			if cur.TerminalId == "" && !stopped(cur) {
				// Its process already died, the exit left to the reopen this drops: record it now, or nothing would.
				if alert, err = d.recordStopped(tx, cur, words); err != nil {
					return err
				}
			}
			if err := tx.PutAgent(cur); err != nil {
				return err
			}
		}
		a, out.Agent = cur, d.view(cur)
		return nil
	}); err != nil {
		return nil, err
	}
	if alert != nil && d.notify != nil {
		d.notify(alert.ToId)
	}
	if dropped {
		d.rt.guard.release(a.Id) // the resume it ends would have launched, and no longer will
	}
	if !stopped(a) && a.TerminalId != "" && d.rt.term != nil {
		if _, err := d.rt.term.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: a.TerminalId})); err != nil &&
			connect.CodeOf(err) != connect.CodeNotFound {
			return nil, err
		}
	}
	return connect.NewResponse(out), nil
}

// setExitDetail says why an agent exited where the reducer, which sees only an exit code and a signal, left no word.
func setExitDetail(tx *store.Tx, a *v1.Agent, detail string) error {
	a.StateDetail = detail
	return tx.PutAgent(a)
}

// recordStopped records the exit of an agent whose process is already gone (applied to cur; the caller stores it).
func (d *Daemon) recordStopped(tx *store.Tx, a *v1.Agent, words string) (*v1.Message, error) {
	lastSaid := a.StateDetail // the exit event below overwrites it
	ev := &adapterv1.AgentEvent{Time: timestamppb.New(tx.Now()),
		Kind: &adapterv1.AgentEvent_ProcessExited{ProcessExited: &adapterv1.ProcessExited{ExitCode: -1}}}
	if err := d.applyEvents(tx, a, []*adapterv1.AgentEvent{ev}); err != nil {
		return nil, err
	}
	if err := setExitDetail(tx, a, "stopped"); err != nil { // not a lost terminal, which is -1 with the detail "lost"
		return nil, err
	}
	if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentExited{
		AgentExited: &v1.AgentExited{ExitCode: -1, Reason: "stopped"}}}); err != nil {
		return nil, err
	}
	alert, err := d.alertExitTx(tx, a, lastSaid, words, -1, 0, "stopped")
	if err != nil {
		return nil, err
	}
	return alert, tx.RevokeTokens(a.Id)
}

// ── terminal streams through the daemon ──

// TicketTTL is how long an OpenTerminal ticket stays usable.
const TicketTTL = 30 * time.Second

// OpenTerminal hands out a one-time ticket for an agent's terminal stream.
func (d *Daemon) OpenTerminal(ctx context.Context, req *connect.Request[v1.OpenTerminalRequest]) (*connect.Response[v1.OpenTerminalResponse], error) {
	actor := auth.ActorFrom(ctx)
	if req.Msg.Shell != "" {
		return d.openShellTerminal(ctx, req.Msg)
	}
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if IsAgent(actor.Class) && a.RunId != actor.RunId {
		return nil, denied("an agent opens terminals only in its own run")
	}
	if a.TerminalId == "" || d.rt.term == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no terminal", a.Name))
	}
	path, tk, input, exp := d.issueTerminalTicket(actor, a, req.Msg.Input)
	return connect.NewResponse(&v1.OpenTerminalResponse{Path: path, Ticket: tk, Input: input, ExpiresAt: timestamppb.New(exp)}), nil
}

func (d *Daemon) issueTerminalTicket(actor *v1.Actor, a *v1.Agent, askInput bool) (path, tk string, input bool, exp time.Time) {
	return d.issueTicket(actor, ticket{agentID: a.Id, terminalID: a.TerminalId}, askInput, "/v1/term/"+a.Id)
}

// issueTicket hands out a one-time ticket for a terminal's stream at path; input only to an actor with the right.
func (d *Daemon) issueTicket(actor *v1.Actor, t ticket, askInput bool, path string) (string, string, bool, time.Time) {
	input := askInput && d.reg.Has("terminal.input", actor.Class)
	tk := ids.Secret("tkt_")
	exp := d.now().Add(TicketTTL)
	t.actorID, t.input, t.expires = actor.Id, input, exp
	d.rt.mu.Lock()
	for k, old := range d.rt.tickets {
		if d.now().After(old.expires) {
			delete(d.rt.tickets, k)
		}
	}
	d.rt.tickets[tk] = t
	d.rt.mu.Unlock()
	return path + "?ticket=" + url.QueryEscape(tk), tk, input, exp
}

// serveTerminal checks a ticket and splices the client's WebSocket to the host's stream for that agent's terminal.
func (d *Daemon) serveTerminal(w http.ResponseWriter, r *http.Request) {
	d.spliceTerminal(w, r, func(t ticket) bool { return t.shellID == "" && t.agentID == r.PathValue("agent") })
}

// serveShell is the same for a project shell, at /v1/shell/{id}: a ticket for an agent's terminal does not open a shell, nor
// the other way round.
func (d *Daemon) serveShell(w http.ResponseWriter, r *http.Request) {
	d.spliceTerminal(w, r, func(t ticket) bool { return t.shellID != "" && t.shellID == r.PathValue("id") })
}

func (d *Daemon) spliceTerminal(w http.ResponseWriter, r *http.Request, mine func(ticket) bool) {
	tk := r.URL.Query().Get("ticket")
	d.rt.mu.Lock()
	t, ok := d.rt.tickets[tk]
	delete(d.rt.tickets, tk)
	d.rt.mu.Unlock()
	if !ok || d.now().After(t.expires) || !mine(t) || d.rt.term == nil {
		http.Error(w, "bad or expired ticket", http.StatusUnauthorized)
		return
	}
	sctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if t.actorID != "" {
		defer d.trackStream(t.actorID, cancel)()
		// Re-check after trackStream: if RevokeDevice ran between OpenTerminal/auth and trackStream,
		// disconnectActor already completed and this catches the revocation cleanly.
		active, err := d.store.Read(r.Context()).HasActiveToken(t.actorID)
		if err != nil {
			http.Error(w, "cannot verify actor", http.StatusServiceUnavailable)
			return
		}
		if !active {
			http.Error(w, "actor revoked", http.StatusUnauthorized)
			return
		}
	}
	in := "0"
	if t.input {
		in = "1"
	}
	host := d.rt.term
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "mesh-term", Path: "/term/" + t.terminalID, RawQuery: "input=" + in}
			pr.Out.Host = "mesh-term"
			pr.Out.Header.Set("Authorization", "Bearer "+host.Token)
		},
		Transport: host.HTTP.Transport,
	}
	proxy.ServeHTTP(w, r.WithContext(sctx))
}

// lookPath finds an executable by name in a PATH list.
func lookPath(name, pathList string) (string, bool) {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// spawnTier is the tier an agent is recorded on: none when a model is named, which has no chain to walk.
func spawnTier(m *v1.SpawnAgentRequest) string {
	if m.Model != "" {
		return ""
	}
	return m.Tier
}
