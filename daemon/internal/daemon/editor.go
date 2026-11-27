package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// OpenInEditor opens a path of an agent's worktree in the editor the host's `editor.command` names. It runs a program on the
// host's own machine, so only the host's own device may ask (a paired phone or another computer's app would open an
// editor on a screen nobody is at), and `editor.command` itself is host-only (`hostOnlySettings`): an agent can't pick what runs.
// The command is split on spaces and run with no shell, the path as its one extra argument, which is absolute (so it can't be
// read as an option) and is resolved through symlinks: it must be the worktree or something inside it.
func (d *Daemon) OpenInEditor(ctx context.Context, req *connect.Request[v1.OpenInEditorRequest]) (*connect.Response[v1.OpenInEditorResponse], error) {
	if err := requireHostDevice(ctx); err != nil {
		return nil, err
	}
	a, err := d.findAgentCrossRun(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if a.Worktree == "" || a.Peer {
		return nil, failedPrecondition("%s has no worktree of its own to open", a.Name)
	}
	target, err := insideWorktree(a.Worktree, req.Msg.Path)
	if err != nil {
		return nil, err
	}
	argv := strings.Fields(d.editorCommand(ctx))
	if len(argv) == 0 {
		return nil, failedPrecondition("no editor is set: give `editor.command` a program such as `code`")
	}
	// PATH is the one meshd resolved from the login shell at start, so an editor installed for the user is found.
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, failedPrecondition("the editor %q isn't on this host's PATH (`editor.command`): %v", argv[0], err)
	}
	out := &v1.OpenInEditorResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceOpenInEditorProcedure, out, func(*store.Tx) error {
		cmd := exec.Command(bin, append(argv[1:], target)...)
		cmd.Dir = a.Worktree
		if err := cmd.Start(); err != nil {
			return failedPrecondition("couldn't start %s: %v", argv[0], err)
		}
		go func() { _ = cmd.Wait() }() // an editor that stays is not waited for; this only reaps it
		out.Opened = target
		return nil
	})
	return connect.NewResponse(out), err
}

// insideWorktree resolves a path asked for (relative to the worktree, or absolute; empty is the worktree) through
// symlinks and says it is the worktree or inside it, as an absolute path.
func insideWorktree(worktree, asked string) (string, error) {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", failedPrecondition("its worktree isn't on disk (archived?): %v", err)
	}
	p := strings.TrimSpace(asked)
	switch {
	case p == "":
		p = root
	case !filepath.IsAbs(p):
		p = filepath.Join(root, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", notFound("%s: %v", asked, err)
	}
	if !insideDir(root, real) {
		return "", denied("%s is outside the agent's worktree", asked)
	}
	return real, nil
}

// editorCommand is the `editor.command` setting.
func (d *Daemon) editorCommand(ctx context.Context) string {
	v, err := d.Setting(d.store.Read(ctx), "editor.command")
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
