package daemon

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Project shells: a plain login shell in a project's folder, held by the terminal host like an agent's terminal and found by
// its labels (`kind=shell`, `repo`): nothing of it is stored, so it survives an app restart and a daemon restart as the host
// does. It is not an agent: no run, no row, no events, no confinement, and no token in its environment.

const (
	shellKind = "shell"
	// maxShells is how many live shells the host keeps: each is a process and a screen.
	maxShells = 20
	// shellGrace is how long a closed shell has to leave on SIGHUP before it is sent SIGTERM and then SIGKILL.
	shellGrace = 3 * time.Second
	// hostCallTimeout bounds a call to the terminal host made while the store's write lock is held.
	hostCallTimeout = 10 * time.Second
)

// shellEnv is what a shell starts with: the user's basics and session and none of the daemon's secrets (no AGENT_MESH_*, no
// token, no API key). A login shell reads the user's profile for the rest. HOME and PATH may come from the host's configured
// environment (a test's own home).
func (d *Daemon) shellEnv() map[string]string {
	env := map[string]string{}
	// Session variables a login profile doesn't set (the user's bus, ssh agent, display) are the user's own, not the daemon's secrets.
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG", "TZ", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "DISPLAY", "WAYLAND_DISPLAY"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "LC_") {
			env[k] = v
		}
	}
	for _, k := range []string{"HOME", "PATH"} {
		if v, ok := d.rt.cfg.Env[k]; ok {
			env[k] = v
		}
	}
	env["TERM"], env["COLORTERM"] = "xterm-256color", "truecolor"
	return env
}

// shellArgv is the user's login shell: $SHELL -l, else /bin/bash -l.
func shellArgv() []string {
	sh := os.Getenv("SHELL")
	if sh == "" || !strings.HasPrefix(sh, "/") {
		sh = "/bin/bash"
	}
	if _, err := exec.LookPath(sh); err != nil {
		sh = "/bin/bash"
	}
	return []string{sh, "-l"}
}

func shellOf(t *termv1.Terminal) *v1.Shell {
	return &v1.Shell{Id: t.Id, Repo: t.Labels["repo"], StartedAt: t.StartedAt, Exited: t.Exited, ExitCode: t.ExitCode}
}

// shells are the host's terminals labelled as shells, oldest first.
func (d *Daemon) shells(ctx context.Context) ([]*termv1.Terminal, error) {
	if d.rt.term == nil {
		return nil, failedPrecondition("this host runs no terminal host")
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return nil, err
	}
	var out []*termv1.Terminal
	for _, t := range res.Msg.Terminals {
		if t.Labels["kind"] == shellKind {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StartedAt.AsTime().Before(out[j].StartedAt.AsTime())
	})
	return out, nil
}

func (d *Daemon) OpenShell(ctx context.Context, req *connect.Request[v1.OpenShellRequest]) (*connect.Response[v1.OpenShellResponse], error) {
	actor := auth.ActorFrom(ctx)
	if IsAgent(actor.Class) || !d.reg.Has("terminal.input", actor.Class) {
		return nil, denied("only a device with the terminal.input right opens a shell")
	}
	if strings.TrimSpace(req.Msg.Repo) == "" {
		return nil, invalid("name the project (--repo: its path, or a folder name under repos.dir)")
	}
	top, err := d.repoTop(ctx, req.Msg.Repo)
	if err != nil {
		return nil, err
	}
	paths, _, _, err := d.projectPaths(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(paths, func(p string) bool { return sameDir(p, top) })
	if i < 0 {
		return nil, notFound("%s is not a project the host knows (a folder of `repos.dir`, or a repo an agent worked in: `mesh projects`)", req.Msg.Repo)
	}
	top = paths[i]
	out := &v1.OpenShellResponse{}
	var created string
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceOpenShellProcedure, out, func(*store.Tx) error {
		// The host is asked with a deadline of its own: this runs under the store's write lock, and a wedged host must not stall every write.
		ctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
		defer cancel()
		// Counted inside the write, so a replay of an applied request neither is refused nor uses a place.
		live, err := d.shells(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, t := range live {
			if !t.Exited {
				n++
			}
		}
		if n >= maxShells {
			return failedPrecondition("%d shells are running: end one first (`mesh shells`, `mesh close-shell`)", n)
		}
		res, err := d.rt.term.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
			Argv: shellArgv(), Env: d.shellEnv(), Cwd: top, Cols: 120, Rows: 40,
			Labels: map[string]string{"kind": shellKind, "repo": top}}))
		if err != nil {
			return err
		}
		created = res.Msg.Terminal.Id
		out.Shell = shellOf(res.Msg.Terminal)
		return nil
	})
	if err == nil && created != "" {
		d.bus.PublishShell(&v1.ShellChange{Shell: out.Shell})
	}
	if err != nil && created != "" {
		// The shell started but its record did not commit (the caller went away): a retry with the same mutation id would start a
		// second one, and nobody knows this one, so it ends.
		d.dropShell(created)
	}
	return connect.NewResponse(out), err
}

// dropShell ends a shell nobody will ask about, and forgets it.
func (d *Daemon) dropShell(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = d.rt.term.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: id, Signal: int32(syscall.SIGKILL)}))
}

func (d *Daemon) ListShells(ctx context.Context, _ *connect.Request[v1.ListShellsRequest]) (*connect.Response[v1.ListShellsResponse], error) {
	ts, err := d.shells(ctx)
	if err != nil {
		return nil, err
	}
	out := &v1.ListShellsResponse{}
	for _, t := range ts {
		out.Shells = append(out.Shells, shellOf(t))
	}
	return connect.NewResponse(out), nil
}

func (d *Daemon) CloseShell(ctx context.Context, req *connect.Request[v1.CloseShellRequest]) (*connect.Response[v1.CloseShellResponse], error) {
	out := &v1.CloseShellResponse{}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceCloseShellProcedure, out, func(*store.Tx) error {
		// Looked up here, after a replay of an applied request is answered: a retry of a close that worked is not "no such shell".
		ctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
		defer cancel()
		ts, err := d.shells(ctx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ts, func(t *termv1.Terminal) bool { return t.Id == req.Msg.Id })
		if i < 0 {
			return notFound("no shell %q (`mesh shells`)", req.Msg.Id)
		}
		t := ts[i]
		d.revokeShellTickets(t.Id) // a ticket for a closed shell is refused
		if t.Exited {
			return d.shellExited(ctx, t.Id)
		}
		// As closing a terminal window does: SIGHUP, which the shell passes on to its background jobs. SIGTERM, then SIGKILL, follow
		// if it is still there after a grace period (a shell ignoring HUP); the host's exit watch forgets it.
		_, err = d.rt.term.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: t.Id, Signal: int32(syscall.SIGHUP)}))
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil // it exited and was forgotten meanwhile
		}
		if err != nil {
			return err
		}
		d.goBG(func() {
			select {
			case <-time.After(shellGrace):
			case <-d.stopping:
				return
			}
			bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = d.rt.term.API.KillTerminal(bg, connect.NewRequest(&termv1.KillTerminalRequest{Id: t.Id}))
		})
		return nil
	})
	return connect.NewResponse(out), err
}

// shellExited forgets an exited shell: there is nothing to record, and a client still on its stream was told by the stream.
func (d *Daemon) shellExited(ctx context.Context, id string) error {
	d.revokeShellTickets(id)
	d.rt.mu.Lock()
	delete(d.rt.lastOutput, id) // the output mark the host's watch kept for it
	d.rt.mu.Unlock()
	_, err := d.rt.term.API.AckExit(ctx, connect.NewRequest(&termv1.AckExitRequest{Id: id}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil // forgotten already (the watch and a close both got here): it was told then
	}
	if err == nil {
		d.bus.PublishShell(&v1.ShellChange{Shell: &v1.Shell{Id: id, Exited: true}, Closed: true})
	}
	return err
}

func (d *Daemon) revokeShellTickets(id string) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	for k, t := range d.rt.tickets {
		if t.shellID == id {
			delete(d.rt.tickets, k)
		}
	}
}

// openShellTerminal is OpenTerminal for a shell: a ticket for /v1/shell/<id>, for a device only (an agent never reaches a shell).
func (d *Daemon) openShellTerminal(ctx context.Context, req *v1.OpenTerminalRequest) (*connect.Response[v1.OpenTerminalResponse], error) {
	actor := auth.ActorFrom(ctx)
	if req.Agent != "" {
		return nil, invalid("name an agent or a shell, not both")
	}
	if IsAgent(actor.Class) {
		return nil, denied("an agent opens no project shell")
	}
	ts, err := d.shells(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ts, func(t *termv1.Terminal) bool { return t.Id == req.Shell })
	if i < 0 {
		return nil, notFound("no shell %q (`mesh shells`)", req.Shell)
	}
	path, tk, input, exp := d.issueTicket(actor, ticket{shellID: ts[i].Id, terminalID: ts[i].Id}, req.Input, "/v1/shell/"+ts[i].Id)
	return connect.NewResponse(&v1.OpenTerminalResponse{Path: path, Ticket: tk, Input: input, ExpiresAt: timestamppb.New(exp)}), nil
}
