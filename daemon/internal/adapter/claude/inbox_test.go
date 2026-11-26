package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

// inbox is a fake Claude session: a socket under a short path and the record Claude keeps of itself.
type inbox struct {
	home, sock string
	frames     chan string
}

func startInbox(t *testing.T, pid int, ticks uint64) *inbox {
	t.Helper()
	dir, err := os.MkdirTemp("", "ib")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	in := &inbox{home: filepath.Join(dir, "claude"), sock: filepath.Join(dir, "s.sock"), frames: make(chan string, 4)}
	l, err := net.Listen("unix", in.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			in.frames <- line
		}
	}()
	in.record(t, pid, strconv.FormatUint(ticks, 10), in.sock)
	return in
}

func (in *inbox) record(t *testing.T, pid int, procStart, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(in.home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"pid": pid, "procStart": procStart, "messagingSocketPath": sock})
	if err := os.WriteFile(filepath.Join(in.home, "sessions", strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deliver(t *testing.T, a *Adapter, in *inbox, pid int, ticks uint64, cwd string) *adapterv1.DeliverNativeResponse {
	t.Helper()
	res, err := a.DeliverNative(context.Background(), connect.NewRequest(&adapterv1.DeliverNativeRequest{Pid: int64(pid),
		PidStartTicks: ticks, Cwd: cwd, Env: map[string]string{"CLAUDE_CONFIG_DIR": in.home}, Text: "📬 [mesh] hi"}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverNativeWritesOneUserFrame(t *testing.T) {
	a := &Adapter{ManagedDir: t.TempDir()}
	in := startInbox(t, 4242, 777)
	res := deliver(t, a, in, 4242, 777, t.TempDir())
	if !res.Delivered {
		t.Fatalf("not delivered: %s", res.Reason)
	}
	var frame struct {
		Type    string                         `json:"type"`
		Message struct{ Role, Content string } `json:"message"`
	}
	select {
	case line := <-in.frames:
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
	}
	if frame.Type != "user" || frame.Message.Role != "user" || frame.Message.Content != "📬 [mesh] hi" {
		t.Errorf("frame %+v", frame)
	}
}

func TestDeliverNativeRefusesWhatIsNotTheAgentsOwnSocket(t *testing.T) {
	a := &Adapter{ManagedDir: t.TempDir()}
	cwd := t.TempDir()
	in := startInbox(t, 4242, 777)
	for name, c := range map[string]struct {
		pid   int
		ticks uint64
	}{
		"another process start at the same pid": {4242, 778},
		"no record for the pid":                 {4243, 777},
		"unknown start ticks":                   {4242, 0},
	} {
		if res := deliver(t, a, in, c.pid, c.ticks, cwd); res.Delivered {
			t.Errorf("%s: delivered", name)
		}
	}
	// A record with no socket, one whose path is gone, one that is a plain file and one that is a symlink to the real socket.
	in.record(t, 4242, "777", "")
	if res := deliver(t, a, in, 4242, 777, cwd); res.Delivered || res.Reason == "" {
		t.Errorf("no socket: %+v", res)
	}
	in.record(t, 4242, "777", filepath.Join(filepath.Dir(in.sock), "gone.sock"))
	if res := deliver(t, a, in, 4242, 777, cwd); res.Delivered {
		t.Errorf("gone socket: %+v", res)
	}
	plain := filepath.Join(filepath.Dir(in.sock), "plain")
	write(t, plain, "x")
	in.record(t, 4242, "777", plain)
	if res := deliver(t, a, in, 4242, 777, cwd); res.Delivered {
		t.Errorf("plain file: %+v", res)
	}
	link := filepath.Join(filepath.Dir(in.sock), "link.sock")
	if err := os.Symlink(in.sock, link); err != nil {
		t.Fatal(err)
	}
	in.record(t, 4242, "777", link)
	if res := deliver(t, a, in, 4242, 777, cwd); res.Delivered {
		t.Errorf("symlink: %+v", res)
	}
	select {
	case line := <-in.frames:
		t.Errorf("a frame was written: %q", line)
	default:
	}
}

func TestInboundSettingFollowsClaudeCodesPrecedence(t *testing.T) {
	val := func(v string) string { return `{"crossSessionInbound": ` + v + `}` }
	cases := []struct {
		name                         string
		managed, dropIns, user, repo []string // file contents; "" is no file
		flag                         string
		value, source                string
	}{
		{name: "nothing set", value: "", source: ""},
		{name: "the flag", flag: "accept", value: "accept", source: "flag"},
		{name: "managed beats the flag", managed: []string{val(`"hold"`)}, flag: "accept", value: "hold", source: "managed"},
		{name: "the last drop-in wins", managed: []string{val(`"hold"`)}, dropIns: []string{val(`"refuse"`), val(`"accept"`)},
			flag: "accept", value: "accept", source: "managed"},
		{name: "the flag beats the user", user: []string{val(`"hold"`)}, flag: "accept", value: "accept", source: "flag"},
		{name: "the user's when there is no flag", user: []string{val(`"refuse"`)}, value: "refuse", source: "user"},
		{name: "a repo may tighten", repo: []string{val(`"hold"`)}, flag: "accept", value: "hold", source: "repo"},
		{name: "a repo may not loosen", repo: []string{val(`"accept"`)}, user: []string{val(`"hold"`)}, value: "hold", source: "user"},
		{name: "an unknown value holds", user: []string{val(`["accept"]`)}, flag: "accept", value: "hold", source: "invalid"},
		{name: "an unknown value in a repo holds too", repo: []string{val(`"maybe"`)}, flag: "accept", value: "hold", source: "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			managed, home, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			for i, s := range c.managed {
				write(t, filepath.Join(managed, "managed-settings.json"), s)
				_ = i
			}
			for i, s := range c.dropIns {
				write(t, filepath.Join(managed, "managed-settings.d", strconv.Itoa(10+i)+"-x.json"), s)
			}
			for _, s := range c.user {
				write(t, filepath.Join(home, "settings.json"), s)
			}
			for _, s := range c.repo {
				write(t, filepath.Join(cwd, ".claude", "settings.json"), s)
			}
			value, source := inboundSetting(cwd, managed, home, c.flag)
			if value != c.value || source != c.source {
				t.Errorf("got (%q, %q), want (%q, %q)", value, source, c.value, c.source)
			}
		})
	}
}

func TestDeliverNativeStopsAtASettingThatHolds(t *testing.T) {
	managed := t.TempDir()
	write(t, filepath.Join(managed, "managed-settings.json"), `{"crossSessionInbound":"hold"}`)
	a := &Adapter{ManagedDir: managed}
	in := startInbox(t, 4242, 777)
	if res := deliver(t, a, in, 4242, 777, t.TempDir()); res.Delivered || res.Reason == "" {
		t.Errorf("%+v", res)
	}
	select {
	case line := <-in.frames:
		t.Errorf("a frame was written: %q", line)
	default:
	}
}

func TestEveryLaunchAcceptsCrossSessionMessages(t *testing.T) {
	s, err := Settings([]string{"mesh", "hook", "claude"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["crossSessionInbound"] != "accept" {
		t.Errorf("settings %s", s)
	}
}
