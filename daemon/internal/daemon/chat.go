package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The chat view of an agent: its CLI's record of the conversation, read for display, and a prompt typed into the
// running CLI as the user would. The agent stays one process on one conversation: the view only reads its record,
// and the composer types into its terminal, which is what the user's own typing is.

// maxPrompt bounds what one prompt may hold.
const maxPrompt = 64 << 10

// ReadTranscript reads a page of an agent's conversation through its adapter. An adapter that can't (its CLI keeps no
// record we read) answers `supported: false`, which the app shows as terminal only; a conversation that hasn't
// started has no record yet and reads as empty.
func (d *Daemon) ReadTranscript(ctx context.Context, req *connect.Request[v1.ReadTranscriptRequest]) (*connect.Response[v1.ReadTranscriptResponse], error) {
	actor := auth.ActorFrom(ctx)
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil || !d.rt.adapters.HasCapability(a.Kind, claude.CapTranscript) {
		return connect.NewResponse(&v1.ReadTranscriptResponse{}), nil
	}
	out := &v1.ReadTranscriptResponse{Supported: true, SessionId: a.SessionId, AtStart: true}
	if a.SessionId == "" {
		return connect.NewResponse(out), nil
	}
	res, err := ad.ReadTranscript(ctx, connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: a.SessionId, Cwd: a.Worktree, Env: d.cliBaseEnv(), Offset: req.Msg.Offset, Backward: req.Msg.Backward, MaxBytes: req.Msg.MaxBytes}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return connect.NewResponse(&v1.ReadTranscriptResponse{}), nil
	}
	if err != nil {
		return nil, err
	}
	out.Entries, out.StartOffset, out.EndOffset, out.AtStart = res.Msg.Entries, res.Msg.StartOffset, res.Msg.EndOffset, res.Msg.AtStart
	return connect.NewResponse(out), nil
}

// PromptAgent types a prompt into an agent's terminal as the user: the chat view's composer. It is the user's own
// words, so only a device with the `terminal.input` right may (the same right as typing in the terminal view), and
// never an agent. It goes only where typing is safe: not into a draft someone left in the box or while they type, and
// not at a prompt the agent is waiting on (the line would answer it). Typing can't be undone, so the request's
// mutation id is honoured: a retry of a prompt already typed gets its answer instead of typing it again, and a
// request past its expiry types nothing.
func (d *Daemon) PromptAgent(ctx context.Context, req *connect.Request[v1.PromptAgentRequest]) (*connect.Response[v1.PromptAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	if !d.reg.Has("terminal.input", actor.Class) {
		return nil, denied("%s may not type into a terminal", actor.Class)
	}
	text := CleanPrompt(req.Msg.Text)
	if strings.TrimSpace(text) == "" {
		return nil, invalid("the prompt is empty")
	}
	if len(text) > maxPrompt {
		return nil, invalid("the prompt is longer than %d bytes", maxPrompt)
	}
	mid := req.Msg.Meta.GetMutationId()
	typed := func() (*connect.Response[v1.PromptAgentResponse], error) {
		if mid == "" {
			return nil, nil
		}
		proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid)
		switch {
		case err == nil && proc != agentmeshv1connect.MeshServicePromptAgentProcedure:
			return nil, reusedMutation(mid, proc)
		case err == nil:
			out := &v1.PromptAgentResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
		return nil, nil
	}
	if res, err := typed(); res != nil || err != nil {
		return res, err
	}
	if err := expired(req.Msg.Meta, d.now()); err != nil {
		return nil, err
	}
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	precondition := func(format string, args ...any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
	}
	if a.TerminalId == "" || d.rt.term == nil {
		return nil, precondition("%s has no terminal", a.Name)
	}
	switch a.State {
	case v1.AgentState_AGENT_STATE_EXITED, v1.AgentState_AGENT_STATE_RETIRED:
		return nil, precondition("%s is %s: its terminal is gone (a message or ResumeAgent brings it back)", a.Name, strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_")))
	case v1.AgentState_AGENT_STATE_NEEDS_INPUT:
		return nil, precondition("%s waits at a prompt in its terminal: answer it there, a typed line would be its answer", a.Name)
	}
	// With a doorbell, a reopen or a stop in flight the agent's terminal is not ours to type into.
	mu := d.agentLock(a.Id)
	mu.Lock()
	defer mu.Unlock()
	// A twin of this request that held the lock first has typed and recorded by now.
	if res, err := typed(); res != nil || err != nil {
		return res, err
	}
	quiet, err := d.quietToType(ctx, a)
	if err != nil {
		return nil, err
	}
	if !quiet {
		return nil, precondition("%s's input box holds a draft or someone is typing in its terminal: finish or clear it, then send again", a.Name)
	}
	if err := d.typePrompt(ctx, a, text); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("typing into %s's terminal: %w", a.Name, err))
	}
	// Recorded even if the request expired or its caller left while typing: the prompt is in, and a retry must
	// find that. The expiry was checked before typing.
	meta := proto.CloneOf(req.Msg.Meta)
	if meta != nil {
		meta.ExpiresAt = nil
	}
	out := &v1.PromptAgentResponse{}
	if _, err := d.write(context.WithoutCancel(ctx), meta, agentmeshv1connect.MeshServicePromptAgentProcedure, out,
		func(*store.Tx) error { return nil }); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// typePrompt types a prompt and submits it. A prompt of several lines, or with a tab (a typed Tab is a keypress:
// completion or a mode toggle, not text), is one bracketed paste where the CLI takes one; otherwise newlines and
// tabs become spaces (a typed newline would submit the first line alone).
func (d *Daemon) typePrompt(ctx context.Context, a *v1.Agent, text string) error {
	if !strings.ContainsAny(text, "\n\t") {
		return d.typeLine(ctx, a.TerminalId, text)
	}
	if !d.rt.adapters.HasCapability(a.Kind, claude.CapBracketedPaste) {
		flat := strings.ReplaceAll(text, "\t", " ")
		if strings.Contains(flat, "\n") {
			flat = strings.Join(strings.Fields(flat), " ")
		}
		return d.typeLine(ctx, a.TerminalId, flat)
	}
	if _, err := d.rt.term.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
		Data: []byte("\x1b[200~" + text + "\x1b[201~")})); err != nil {
		return err
	}
	time.Sleep(d.rt.cfg.DeliveryDelay)
	err := d.pressEnter(ctx, a.TerminalId)
	if err != nil && writeDidNotHappen(err) {
		// As typeLine does: the Enter certainly never went in, so take the paste back out (Ctrl-U) rather than
		// leave a draft that blocks the retry and every doorbell. The box clears the line the cursor is on, as far
		// as the CLI's Ctrl-U goes.
		d.rt.term.API.WriteTerminal(context.WithoutCancel(ctx), connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
			Data: []byte("\x15")}))
	}
	return err
}

// CleanPrompt makes a prompt safe to type: line endings are \n, and every other control character (an Escape
// especially, which could end a bracketed paste and send keystrokes) is dropped, as is DEL. Tabs stay.
func CleanPrompt(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (!unicode.IsControl(r) && r != 0x7f) {
			return r
		}
		return -1
	}, s)
}
