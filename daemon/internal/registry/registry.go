// Package registry reads the action registry out of the compiled protobuf descriptors: every RPC of a registry
// service with its allowed actor classes, whether it writes, and its CLI name. Nothing about an action is
// written twice: the server's permission checks, the CLI's commands and the tests all come from here.
package registry

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Action is one RPC of the registry.
type Action struct {
	// Procedure is the Connect procedure path, "/agentmesh.v1.MeshService/Hello".
	Procedure string
	Method    protoreflect.MethodDescriptor
	Actors    map[v1.ActorClass]bool
	Write     bool
	CLI       string
	Summary   string
}

// Streaming reports whether the action streams its responses.
func (a *Action) Streaming() bool { return a.Method.IsStreamingServer() }

// Registry is the set of actions and named permissions.
type Registry struct {
	actions map[string]*Action
	ordered []*Action
	perms   map[string]map[v1.ActorClass]bool
}

// New builds the registry from services. Every method must carry an `action` option naming at least one actor
// class, and CLI names must be unique.
func New(services ...protoreflect.ServiceDescriptor) (*Registry, error) {
	r := &Registry{actions: map[string]*Action{}, perms: map[string]map[v1.ActorClass]bool{}}
	clis := map[string]string{}
	for _, svc := range services {
		sopts, _ := svc.Options().(*descriptorpb.ServiceOptions)
		if sopts != nil {
			for _, p := range proto.GetExtension(sopts, v1.E_Permissions).([]*v1.PermissionOptions) {
				if _, dup := r.perms[p.Name]; dup {
					return nil, fmt.Errorf("permission %q defined twice", p.Name)
				}
				r.perms[p.Name] = classSet(p.Actors)
			}
		}
		methods := svc.Methods()
		for i := 0; i < methods.Len(); i++ {
			m := methods.Get(i)
			proc := "/" + string(svc.FullName()) + "/" + string(m.Name())
			mopts, _ := m.Options().(*descriptorpb.MethodOptions)
			if mopts == nil || !proto.HasExtension(mopts, v1.E_Action) {
				return nil, fmt.Errorf("%s has no (agentmesh.v1.action) option", proc)
			}
			opt := proto.GetExtension(mopts, v1.E_Action).(*v1.ActionOptions)
			if len(opt.Actors) == 0 {
				return nil, fmt.Errorf("%s allows no actor class", proc)
			}
			for _, c := range opt.Actors {
				if c == v1.ActorClass_ACTOR_CLASS_UNSPECIFIED {
					return nil, fmt.Errorf("%s lists ACTOR_CLASS_UNSPECIFIED", proc)
				}
			}
			cli := opt.Cli
			if cli == "" {
				cli = Kebab(string(m.Name()))
			}
			if other, dup := clis[cli]; dup {
				return nil, fmt.Errorf("CLI name %q used by %s and %s", cli, other, proc)
			}
			clis[cli] = proc
			a := &Action{Procedure: proc, Method: m, Actors: classSet(opt.Actors), Write: opt.Write, CLI: cli,
				Summary: opt.Summary}
			r.actions[proc] = a
			r.ordered = append(r.ordered, a)
		}
	}
	return r, nil
}

func classSet(cs []v1.ActorClass) map[v1.ActorClass]bool {
	out := map[v1.ActorClass]bool{}
	for _, c := range cs {
		out[c] = true
	}
	return out
}

// Get returns the action at a procedure path, or nil.
func (r *Registry) Get(procedure string) *Action { return r.actions[procedure] }

// Actions returns every action, in declaration order.
func (r *Registry) Actions() []*Action { return r.ordered }

// Allowed reports whether an actor class may call a procedure. An unknown procedure is never allowed.
func (r *Registry) Allowed(procedure string, c v1.ActorClass) bool {
	a := r.actions[procedure]
	return a != nil && a.Actors[c]
}

// Has reports whether an actor class holds a named permission.
func (r *Registry) Has(permission string, c v1.ActorClass) bool { return r.perms[permission][c] }

// Permissions returns the named permissions, sorted.
func (r *Registry) Permissions() []string {
	out := make([]string, 0, len(r.perms))
	for p := range r.perms {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// For lists what an actor class may do: its actions' procedure paths and its permissions, sorted.
func (r *Registry) For(c v1.ActorClass) (actions, permissions []string) {
	for _, a := range r.ordered {
		if a.Actors[c] {
			actions = append(actions, a.Procedure)
		}
	}
	for _, p := range r.Permissions() {
		if r.perms[p][c] {
			permissions = append(permissions, p)
		}
	}
	sort.Strings(actions)
	return actions, permissions
}

// Kebab turns "SpawnAgent" into "spawn-agent".
func Kebab(name string) string {
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Mesh returns the registry of MeshService, the host's public API.
func Mesh() (*Registry, error) {
	return New(v1.File_agentmesh_v1_mesh_proto.Services().ByName("MeshService"))
}
