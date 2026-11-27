package daemon

import (
	"context"
	"slices"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
)

// The composer's helpers (desktop/CLAUDE.md): the commands an agent's CLI accepts at its prompt, for the `/`
// completion, and the prompt Claude Code suggests next. Both read; neither types anything.

// ListAgentCommands asks the agent's adapter for the commands its CLI accepts here (the worktree decides which
// project commands and skills load). An adapter that can't say answers `supported: false`.
func (d *Daemon) ListAgentCommands(ctx context.Context, req *connect.Request[v1.ListAgentCommandsRequest]) (*connect.Response[v1.ListAgentCommandsResponse], error) {
	a, err := d.FindAgent(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return connect.NewResponse(&v1.ListAgentCommandsResponse{}), nil
	}
	lean, err := d.leanForCommands(ctx, a)
	if err != nil {
		return nil, err
	}
	res, err := ad.ListCommands(ctx, connect.NewRequest(&adapterv1.ListCommandsRequest{
		Cwd: a.Worktree, Env: d.agentEnv(a, ""), Binary: d.rt.cfg.Binaries[a.Kind], ReadOnly: adapter.ReadOnly(a.Role), Lean: lean}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return connect.NewResponse(&v1.ListAgentCommandsResponse{}), nil
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListAgentCommandsResponse{Supported: true, Commands: res.Msg.Commands}), nil
}

// GetPromptSuggestion reads the grey text of the agent's empty input box when its adapter can tell a suggestion
// from the CLI's fixed hint (`ReadInputBoxResponse.ghost`), and only while the agent sits at its prompt.
func (d *Daemon) GetPromptSuggestion(ctx context.Context, req *connect.Request[v1.GetPromptSuggestionRequest]) (*connect.Response[v1.GetPromptSuggestionResponse], error) {
	a, err := d.FindAgent(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	out := &v1.GetPromptSuggestionResponse{}
	if a.TerminalId == "" || d.rt.term == nil || !takesDoorbell(a.State) {
		return connect.NewResponse(out), nil
	}
	if r := d.readInputBox(ctx, a, ""); r.GetBox() == adapterv1.InputBox_INPUT_BOX_EMPTY {
		out.Text = r.Ghost
	}
	return connect.NewResponse(out), nil
}

// leanForCommands is the part of a Claude agent's lean launch that changes which commands it has: the plugins
// turned off and the claude.ai skills (a hidden skill can still be typed, so those stay in the list). nil for another
// CLI or with `claude.lean` off.
func (d *Daemon) leanForCommands(ctx context.Context, a *v1.Agent) (*adapterv1.LeanLaunch, error) {
	if a.Kind != claude.Kind || !d.leanOn(ctx) {
		return nil, nil
	}
	r := d.store.Read(ctx)
	plugins, err := d.settingStrings(r, "claude.plugins_off")
	if err != nil {
		return nil, err
	}
	keepSkills, err := d.Setting(r, "claude.claudeai_skills")
	if err != nil {
		return nil, err
	}
	return &adapterv1.LeanLaunch{
		OffPlugins:         filterAddedPlugins(plugins, a.AddedTools),
		HideClaudeaiSkills: keepSkills != true && !slices.Contains(a.AddedTools, claude.ClaudeAISkills),
	}, nil
}
