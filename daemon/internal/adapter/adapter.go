// Package adapter holds the agent CLI adapters. An adapter is a CliAdapterService (the plugin interface in
// proto/agentmesh/adapter/v1): the built-ins implement it in process, and a process plugin will serve the
// same service from its own binary. The daemon knows CLIs only through it.
package adapter

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/inputbox"
)

// ReadOnly reports whether a role may not change code: an asker or a reviewer.
func ReadOnly(role agentmeshv1.AgentRole) bool {
	return role == agentmeshv1.AgentRole_AGENT_ROLE_ASKER || role == agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER
}

// Adapter is one CLI's adapter.
type Adapter = adapterv1connect.CliAdapterServiceHandler

// Registry is the adapters by kind.
type Registry struct {
	byKind map[string]Adapter
	about  map[string]*adapterv1.DescribeResponse
}

// NewRegistry registers adapters under the kind each describes.
func NewRegistry(ctx context.Context, adapters ...Adapter) (*Registry, error) {
	r := &Registry{byKind: map[string]Adapter{}, about: map[string]*adapterv1.DescribeResponse{}}
	for _, a := range adapters {
		res, err := a.Describe(ctx, connect.NewRequest(&adapterv1.DescribeRequest{}))
		if err != nil {
			return nil, err
		}
		kind := res.Msg.Kind
		if kind == "" {
			return nil, fmt.Errorf("an adapter describes no kind")
		}
		if _, dup := r.byKind[kind]; dup {
			return nil, fmt.Errorf("two adapters for %q", kind)
		}
		r.byKind[kind], r.about[kind] = a, res.Msg
	}
	return r, nil
}

// Get returns the adapter of a kind.
func (r *Registry) Get(kind string) (Adapter, error) {
	a, ok := r.byKind[kind]
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("no adapter for %q (have %s)", kind, strings.Join(r.Kinds(), ", ")))
	}
	return a, nil
}

// Kinds lists the registered kinds.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasCapability reports whether an adapter kind advertised a capability in Describe.
func (r *Registry) HasCapability(kind, capability string) bool {
	d := r.about[kind]
	if d == nil {
		return false
	}
	for _, c := range d.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// ModelEffort is the effort a model's name fixes by itself (an agy id such as `gemini-4-neon-low`), or "": the
// adapter has no DescribeModel, or the name fixes none.
func (r *Registry) ModelEffort(ctx context.Context, kind, model string) string {
	a, ok := r.byKind[kind]
	if !ok || model == "" {
		return ""
	}
	res, err := a.DescribeModel(ctx, connect.NewRequest(&adapterv1.DescribeModelRequest{Model: model}))
	if err != nil {
		return ""
	}
	return res.Msg.Effort
}

// ShellQuote quotes argv for a POSIX shell, the way hook commands are run.
func ShellQuote(argv ...string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
		}) < 0 {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// ReadInputBox answers a ReadInputBoxRequest from a CLI's input-box profile: what the adapters share.
func ReadInputBox(req *adapterv1.ReadInputBoxRequest, p inputbox.Profile) *adapterv1.ReadInputBoxResponse {
	r := inputbox.Read(req.Screen, p, req.Token)
	out := &adapterv1.ReadInputBoxResponse{Text: r.Text, TokenAbove: int32(r.TokenAbove)}
	switch r.State {
	case inputbox.Empty:
		out.Box = adapterv1.InputBox_INPUT_BOX_EMPTY
	case inputbox.Typed:
		out.Box = adapterv1.InputBox_INPUT_BOX_TYPED
	case inputbox.NoBox:
		out.Box = adapterv1.InputBox_INPUT_BOX_NONE
	default:
		out.Box = adapterv1.InputBox_INPUT_BOX_UNCLEAR
	}
	return out
}
