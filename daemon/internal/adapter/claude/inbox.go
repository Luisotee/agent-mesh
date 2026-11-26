package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

// Claude Code's cross-session inbox (design §11 of docs/design.md, read from the 2.1.310 bundle): every session
// listens on a unix socket, and one JSON line written to it, `{"type":"user","message":{"role":"user","content":…}}`,
// is a prompt: an idle session starts a turn, a busy one takes it between two tool calls. Every message goes
// through a receive-side gate, `crossSessionInbound` (`accept`, `hold` or `refuse`), so the daemon writes only to a
// session it launched with `accept` in its `--settings` and whose settings still say so.

// CapInboxSocket is the capability of an adapter that can hand a doorbell to its CLI without typing it.
const CapInboxSocket = "inbox-socket"

// InboundAccept is the `crossSessionInbound` every launch carries: it beats the user's own value.
const InboundAccept = "accept"

// InboxTimeout is how long a write into the inbox socket may take.
const InboxTimeout = 2 * time.Second

// ManagedDir is where Claude Code reads managed policy on Linux: `managed-settings.json`, then
// `managed-settings.d/*.json` in name order.
const ManagedDir = "/etc/claude-code"

var inboundRank = map[string]int{"accept": 0, "hold": 1, "refuse": 2}

// DeliverNative writes the doorbell into the agent's inbox socket when it can be sure the socket is the agent's
// and takes it. It never retries a write: Claude Code delivers a repeated frame again. A reason other than a
// written frame is not an error: the daemon types the doorbell instead.
func (a *Adapter) DeliverNative(_ context.Context, req *connect.Request[adapterv1.DeliverNativeRequest]) (*connect.Response[adapterv1.DeliverNativeResponse], error) {
	m := req.Msg
	no := func(format string, args ...any) (*connect.Response[adapterv1.DeliverNativeResponse], error) {
		return connect.NewResponse(&adapterv1.DeliverNativeResponse{Reason: fmt.Sprintf(format, args...)}), nil
	}
	managed := a.ManagedDir
	if managed == "" {
		managed = ManagedDir
	}
	home := configHome(m.Env)
	if home == "" {
		return no("no Claude config folder")
	}
	rec, ok := processRecord(home, int(m.Pid), m.PidStartTicks)
	if !ok {
		return no("no record of process %d (not Claude Code, or a later process at that pid)", m.Pid)
	}
	if rec.SocketPath == "" {
		return no("the session has no inbox socket")
	}
	procDir := a.ProcDir
	if procDir == "" {
		procDir = "/proc"
	}
	// The flag is the one this process was launched with, not the one a launch now would carry: an agent started
	// before the flag was added (a daemon upgraded under a running agent, an adopted one) may not accept.
	if value, source := inboundSetting(m.Cwd, managed, home, launchInbound(procDir, int(m.Pid))); value != InboundAccept {
		return no("crossSessionInbound is %s (%s settings)", value, source)
	}
	if err := writeInbox(rec.SocketPath, m.Text); err != nil {
		return no("writing the inbox socket: %v", err)
	}
	return connect.NewResponse(&adapterv1.DeliverNativeResponse{Delivered: true}), nil
}

// launchInbound is the `crossSessionInbound` of the `--settings` a process was launched with, from its command line
// (inline JSON, or the path of a settings file); "" when it has none, or none that sets it.
func launchInbound(procDir string, pid int) string {
	raw, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	value := ""
	for i, arg := range args {
		var v string
		switch {
		case arg == "--settings" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(arg, "--settings="):
			v = strings.TrimPrefix(arg, "--settings=")
		default:
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(v), "{") {
			var doc struct {
				Inbound string `json:"crossSessionInbound"`
			}
			if json.Unmarshal([]byte(v), &doc) == nil && doc.Inbound != "" {
				value = doc.Inbound
			}
		} else if got := settingsInbound(v); got != "" && got != "invalid" {
			value = got
		}
	}
	return value
}

// record is the file a Claude Code process keeps about itself (`sessions/<pid>.json` under its config folder).
type record struct {
	SocketPath string
}

// processRecord reads it. Not ok: no such file, or one an earlier process with the same pid left (its `procStart`
// is that process's start, in clock ticks since boot, as the terminal host records it).
func processRecord(home string, pid int, startTicks uint64) (record, bool) {
	b, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return record{}, false
	}
	var raw struct {
		Pid       int    `json:"pid"`
		ProcStart any    `json:"procStart"`
		Socket    string `json:"messagingSocketPath"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Pid != pid || raw.ProcStart == nil || startTicks == 0 ||
		fmt.Sprint(raw.ProcStart) != strconv.FormatUint(startTicks, 10) {
		return record{}, false
	}
	return record{SocketPath: raw.Socket}, true
}

// writeInbox writes one user frame into a Claude session's inbox socket. A path that isn't a socket owned by
// this user (a symlink, another user's) isn't written to. The socket never answers, so a write is all there is
// to go by. No auth line: Claude Code requires one only on Windows. The check and the dial are two steps: a path
// swapped between them (by the same user, who could write the record and the socket anyway) isn't caught.
func writeInbox(path, text string) error {
	path = strings.TrimPrefix(path, "uds:")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); info.Mode()&os.ModeSocket == 0 || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("not a socket of this user")
	}
	frame, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", path, InboxTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(InboxTimeout))
	_, err = conn.Write(append(frame, '\n'))
	return err
}

// settingsInbound is a settings file's `crossSessionInbound`: "" when unset (or no such file), "invalid" for a
// value Claude Code doesn't know (a list, say).
func settingsInbound(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	raw, ok := doc["crossSessionInbound"]
	if !ok || string(raw) == "null" {
		return ""
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "invalid"
	}
	if _, known := inboundRank[v]; !known {
		return "invalid"
	}
	return v
}

// inboundSetting is the `crossSessionInbound` a Claude session started in cwd with `--settings` `flag` runs with,
// and where it comes from, as Claude Code 2.1.310 decides it: managed policy, then the flag, then the user's
// settings, the first that sets it; the repo's `.claude/settings*.json` may only tighten it. A value it doesn't
// know, in any of them, holds whatever else is set (its settings warning). Managed drop-ins are merged over the
// base file in name order, so the last that sets it wins. ("", ""): unset, so the session's permission mode
// decides. Server-managed policy isn't read.
func inboundSetting(cwd, managedDir, home, flag string) (string, string) {
	drop, _ := filepath.Glob(filepath.Join(managedDir, "managed-settings.d", "*.json"))
	sort.Strings(drop)
	var managed []string
	for _, p := range append([]string{filepath.Join(managedDir, "managed-settings.json")}, drop...) {
		managed = append(managed, settingsInbound(p))
	}
	user := settingsInbound(filepath.Join(home, "settings.json"))
	var repo []string
	if cwd != "" {
		for _, name := range []string{"settings.local.json", "settings.json"} {
			repo = append(repo, settingsInbound(filepath.Join(cwd, ".claude", name)))
		}
	}
	invalid := user == "invalid"
	for _, v := range append(append([]string{}, managed...), repo...) {
		invalid = invalid || v == "invalid"
	}
	known := func(vs []string) []string {
		var out []string
		for _, v := range vs {
			if v != "invalid" {
				out = append(out, v)
			}
		}
		return out
	}
	if user == "invalid" {
		user = ""
	}
	value, source := "", ""
	for _, v := range known(managed) {
		if v != "" {
			value, source = v, "managed"
		}
	}
	if value == "" && flag != "" {
		value, source = flag, "flag"
	}
	if value == "" && user != "" {
		value, source = user, "user"
	}
	rank := func(v string) int {
		if v == "" {
			return 0
		}
		return inboundRank[v]
	}
	for _, v := range known(repo) {
		if v != "" && inboundRank[v] > rank(value) {
			value, source = v, "repo"
		}
	}
	if invalid && rank(value) < inboundRank["hold"] { // the settings warning holds
		return "hold", "invalid"
	}
	return value, source
}

// Session is what a running Claude Code process records about itself (`sessions/<pid>.json`): the conversation it
// holds, where it runs, its inbox socket, and what it is doing (`idle`, `busy`, `waiting`, `shell`).
type Session struct {
	ID, Cwd, SocketPath, Status string
}

// ReadSession reads the record of the process pid that started at startTicks, under the Claude config folder env
// names. Not ok: no such file, or one an earlier process with that pid left.
func ReadSession(env map[string]string, pid int, startTicks uint64) (Session, bool) {
	home := configHome(env)
	if home == "" || startTicks == 0 {
		return Session{}, false
	}
	b, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return Session{}, false
	}
	var raw struct {
		Pid       int    `json:"pid"`
		ProcStart any    `json:"procStart"`
		Socket    string `json:"messagingSocketPath"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Status    string `json:"status"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Pid != pid || raw.ProcStart == nil ||
		fmt.Sprint(raw.ProcStart) != strconv.FormatUint(startTicks, 10) {
		return Session{}, false
	}
	return Session{ID: raw.SessionID, Cwd: raw.Cwd, SocketPath: raw.Socket, Status: raw.Status}, true
}

// InboundPolicy is the `crossSessionInbound` a running process takes frames under, and whose setting says so:
// "accept" is the one a doorbell can be written under (DeliverNative checks the same).
func InboundPolicy(env map[string]string, cwd string, pid int) (value, source string) {
	return inboundSetting(cwd, ManagedDir, configHome(env), launchInbound("/proc", pid))
}
