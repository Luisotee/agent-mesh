// Package cli is meshctl's command line, derived from the action registry at run time: one command per
// action (its `cli` name), one flag per request field, the response printed as JSON. Nothing here names an
// action, so an action added to the schemas is a command with no code.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// Env is what the CLI reads from its environment and files; tests replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

// OSEnv is the process's own environment.
func OSEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1 // the daemon refused or failed the call
	ExitUsage = 2
)

// Main runs one command and returns its exit code. hand holds the hand-written commands (hook, version),
// dispatched before the registry is even loaded: a hook must work, and exit 0, whatever else fails.
func Main(args []string, env Env, hand map[string]func([]string) int) int {
	global := flag.NewFlagSet("meshctl", flag.ContinueOnError)
	global.SetOutput(env.Stderr)
	state := global.String("state", env.Getenv(client.StateEnv), "the daemon's state dir (or $"+client.StateEnv+")")
	// --json is also a command's own flag; given here, before the command, it is that command's.
	whole := global.String("json", "", "the whole request as JSON, for the command that follows")
	global.Usage = func() {
		fmt.Fprintln(env.Stderr, "usage: meshctl [--state <dir>] [--json <request>] <command> [flags]; meshctl help")
	}
	if err := global.Parse(args); err != nil {
		return ExitUsage
	}
	rest := global.Args()
	if len(rest) > 0 {
		if h, ok := hand[rest[0]]; ok {
			return h(rest[1:])
		}
	}
	reg, err := registry.Mesh()
	if err != nil {
		fmt.Fprintln(env.Stderr, "meshctl:", err)
		return ExitError
	}
	if len(rest) == 0 || rest[0] == "help" {
		if len(rest) > 1 {
			if a := find(reg, rest[1]); a != nil {
				newCommand(a, env, *state).usage(env.Stdout)
				return ExitOK
			}
			if _, ok := hand[rest[1]]; ok {
				fmt.Fprintf(env.Stdout, "meshctl %s is hand-written; run it without arguments for its usage\n", rest[1])
				return ExitOK
			}
			fmt.Fprintf(env.Stderr, "meshctl help: unknown command %q\n", rest[1])
			return ExitUsage
		}
		usage(env.Stdout, reg, hand)
		return ExitOK
	}
	a := find(reg, rest[0])
	if a == nil {
		fmt.Fprintf(env.Stderr, "meshctl: unknown command %q (meshctl help lists them)\n", rest[0])
		return ExitUsage
	}
	cmd := newCommand(a, env, *state)
	cmd.whole = *whole
	req, err := cmd.parse(rest[1:])
	if errors.Is(err, flag.ErrHelp) {
		cmd.usage(env.Stdout)
		return ExitOK
	}
	if err != nil {
		// Said once: the flag package's own message is off, so a bad flag isn't reported twice with the full usage.
		fmt.Fprintf(env.Stderr, "meshctl %s: %v (meshctl help %s lists the flags)\n", a.CLI, err, a.CLI)
		return ExitUsage
	}
	if cmd.state == "" {
		fmt.Fprintln(env.Stderr, "meshctl: give --state or $"+client.StateEnv)
		return ExitUsage
	}
	// An agent acts as itself (its token in the environment); anyone else as the host's local device.
	tok := env.Getenv(client.TokenEnv)
	if tok == "" {
		if tok, err = fsutil.ReadSecret(statedir.Paths{Dir: cmd.state}.DeviceToken()); err != nil {
			fmt.Fprintf(env.Stderr, "meshctl: no %s and no device token: %v\n", client.TokenEnv, err)
			return ExitError
		}
	}
	conn, err := client.Dial(cmd.state, tok, client.Options{})
	if err != nil {
		fmt.Fprintln(env.Stderr, "meshctl:", err)
		return ExitError
	}
	if err := call(context.Background(), conn, a, req, env.Stdout); err != nil {
		fmt.Fprintf(env.Stderr, "meshctl %s: %v\n", a.CLI, err)
		return ExitError
	}
	return ExitOK
}

func find(reg *registry.Registry, name string) *registry.Action {
	for _, a := range reg.Actions() {
		if a.CLI == name {
			return a
		}
	}
	return nil
}

func usage(w io.Writer, reg *registry.Registry, hand map[string]func([]string) int) {
	fmt.Fprintln(w, "usage: meshctl [--state <dir>] [--json <request>] <command> [flags]   (meshctl help <command> for its flags)")
	fmt.Fprintln(w, "\ncommands:")
	for _, a := range reg.Actions() {
		fmt.Fprintf(w, "  %-14s %s\n", a.CLI, a.Summary)
	}
	names := make([]string, 0, len(hand))
	for n := range hand {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-14s\n", n)
	}
}

// command is one action's flag set over a dynamic request message.
type command struct {
	action *registry.Action
	flags  *flag.FlagSet
	req    *dynamicpb.Message
	env    Env
	state  string
	whole  string
	// The field flags, in the order given: applied after --json, so they override it wherever they stand. A list
	// field's first flag replaces the list --json gave; the next ones add to it.
	pending   []func()
	listSeen  map[protoreflect.FieldNumber]bool
	stdinRead bool
	// meta: the write's mutation id and expiry, filled in unless given.
	mutationID string
	expiresIn  time.Duration
}

// FlagName is a field's flag: its proto name in kebab case.
func FlagName(f protoreflect.FieldDescriptor) string {
	return strings.ReplaceAll(string(f.Name()), "_", "-")
}

// fieldFlag sets one field. A bool field is a switch: --peek means true, --peek=false false.
type fieldFlag struct {
	c *command
	f protoreflect.FieldDescriptor
}

func (v fieldFlag) String() string { return "" }
func (v fieldFlag) IsBoolFlag() bool {
	return v.f != nil && v.f.Kind() == protoreflect.BoolKind && !v.f.IsList()
}
func (v fieldFlag) Set(s string) error {
	// Parsed once, now (so a bad value is reported at its flag, and `@-` reads stdin once), applied after --json.
	val, err := v.c.value(v.f, s)
	if err != nil {
		return err
	}
	v.c.pending = append(v.c.pending, func() { v.c.set(v.f, val) })
	return nil
}

func newCommand(a *registry.Action, env Env, state string) *command {
	c := &command{action: a, req: dynamicpb.NewMessage(a.Method.Input()), env: env, state: state, listSeen: map[protoreflect.FieldNumber]bool{},
		flags: flag.NewFlagSet("meshctl "+a.CLI, flag.ContinueOnError)}
	// The flag package would print its error and the usage itself; Main prints the error once instead.
	c.flags.SetOutput(io.Discard)
	c.flags.Usage = func() {}
	c.flags.StringVar(&c.state, "state", state, "the daemon's state dir (or $"+client.StateEnv+")")
	c.flags.StringVar(&c.whole, "json", "", "the whole request as JSON; the other flags override its fields (a repeated field's first flag replaces its list)")
	fields := a.Method.Input().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if a.Write && f.Name() == "meta" && f.Kind() == protoreflect.MessageKind {
			c.flags.StringVar(&c.mutationID, "mutation-id", "", "the write's id: a retry with the same id runs once (default: a new one)")
			c.flags.DurationVar(&c.expiresIn, "expires-in", 0, "refuse the write if it reaches the daemon later than this")
			continue
		}
		c.flags.Var(fieldFlag{c: c, f: f}, FlagName(f), describe(f))
	}
	return c
}

// usage prints the command's summary and its flags.
func (c *command) usage(w io.Writer) {
	fmt.Fprintf(w, "usage: meshctl %s [flags]\n  %s\n", c.action.CLI, c.action.Summary)
	c.flags.SetOutput(w)
	c.flags.PrintDefaults()
	c.flags.SetOutput(io.Discard)
}

func describe(f protoreflect.FieldDescriptor) string {
	var kind string
	switch {
	case f.Enum() != nil:
		vals := f.Enum().Values()
		names := make([]string, 0, vals.Len())
		for i := 1; i < vals.Len(); i++ {
			names = append(names, enumShort(f.Enum(), vals.Get(i)))
		}
		kind = "one of " + strings.Join(names, ", ")
	case isValue(f):
		kind = "a JSON value; bare text that is not JSON is a string"
	case f.Kind() == protoreflect.MessageKind:
		kind = "JSON " + string(f.Message().Name())
	case f.Kind() == protoreflect.BoolKind:
		kind = "a switch"
	case f.Kind() == protoreflect.StringKind || f.Kind() == protoreflect.BytesKind:
		kind = "text; @file reads a file, @- stdin, @@ starts it with a literal @"
	default:
		kind = f.Kind().String()
	}
	if f.IsList() {
		kind += " (repeatable)"
	}
	return kind
}

// enumShort is an enum value's name without its type prefix, lowercased: AGENT_ROLE_WORKER → worker.
func enumShort(e protoreflect.EnumDescriptor, v protoreflect.EnumValueDescriptor) string {
	prefix := strings.ToUpper(strings.ReplaceAll(registry.Kebab(string(e.Name())), "-", "_")) + "_"
	return strings.ToLower(strings.TrimPrefix(string(v.Name()), prefix))
}

// atHint is appended to a failed `@` read: the rule and its escape.
const atHint = "an argument starting with @ reads a file (@- reads stdin); start it with @@ for a literal @"

// text is a text flag's value: `@-` is stdin (once), `@<path>` a file, `@@…` a literal `@…`; a bare `@` and
// anything not starting with `@` is itself.
func (c *command) text(v string) (string, error) {
	if len(v) < 2 || v[0] != '@' {
		return v, nil
	}
	if rest, ok := strings.CutPrefix(v, "@@"); ok {
		return "@" + rest, nil
	}
	if v == "@-" {
		if c.stdinRead {
			return "", errors.New("stdin (@-) is given for two fields: it can be read once")
		}
		c.stdinRead = true
		b, err := io.ReadAll(c.env.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(v[1:])
	if err != nil {
		return "", fmt.Errorf("%w (%s)", err, atHint)
	}
	return string(b), nil
}

// isValue reports whether a field is a google.protobuf.Value, which holds any JSON.
func isValue(f protoreflect.FieldDescriptor) bool {
	return f.Kind() == protoreflect.MessageKind && f.Message().FullName() == "google.protobuf.Value"
}

func (c *command) value(f protoreflect.FieldDescriptor, v string) (protoreflect.Value, error) {
	switch f.Kind() {
	case protoreflect.StringKind:
		s, err := c.text(v)
		return protoreflect.ValueOfString(s), err
	case protoreflect.BytesKind:
		s, err := c.text(v)
		return protoreflect.ValueOfBytes([]byte(s)), err
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(v)
		return protoreflect.ValueOfBool(b), err
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(v, 10, 32)
		return protoreflect.ValueOfInt32(int32(n)), err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(v, 10, 64)
		return protoreflect.ValueOfInt64(n), err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(v, 10, 32)
		return protoreflect.ValueOfUint32(uint32(n)), err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(v, 10, 64)
		return protoreflect.ValueOfUint64(n), err
	case protoreflect.EnumKind:
		vals := f.Enum().Values()
		for i := 0; i < vals.Len(); i++ {
			ev := vals.Get(i)
			if strings.EqualFold(v, string(ev.Name())) || strings.EqualFold(v, enumShort(f.Enum(), ev)) {
				return protoreflect.ValueOfEnum(ev.Number()), nil
			}
		}
		return protoreflect.Value{}, fmt.Errorf("%q is not %s", v, describe(f))
	case protoreflect.MessageKind:
		m := dynamicpb.NewMessage(f.Message())
		if err := protojson.Unmarshal([]byte(v), m); err != nil {
			// Text that starts like JSON (a list, an object, a quoted string) is a JSON typo, not a string.
			if !isValue(f) || strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{") || strings.HasPrefix(v, `"`) {
				return protoreflect.Value{}, err
			}
			// A google.protobuf.Value is any JSON; text that isn't JSON is the string it spells (a setting's
			// `sonnet`, no quotes needed; `'"5"'` is the string 5, and `5` the number).
			s, terr := c.text(v)
			if terr != nil {
				return protoreflect.Value{}, terr
			}
			quoted, _ := json.Marshal(s)
			m = dynamicpb.NewMessage(f.Message())
			if err := protojson.Unmarshal(quoted, m); err != nil {
				return protoreflect.Value{}, err
			}
		}
		return protoreflect.ValueOfMessage(m), nil
	}
	return protoreflect.Value{}, fmt.Errorf("fields of kind %s can only be set with --json", f.Kind())
}

func (c *command) set(f protoreflect.FieldDescriptor, val protoreflect.Value) {
	if f.IsList() {
		if !c.listSeen[f.Number()] {
			c.listSeen[f.Number()] = true
			c.req.Clear(f)
		}
		c.req.Mutable(f).List().Append(val)
	} else {
		c.req.Set(f, val)
	}
}

// parse reads the flags into the request: --json first, then every field flag in the order given, so the
// flags override the JSON wherever they stand.
func (c *command) parse(args []string) (*dynamicpb.Message, error) {
	if err := c.flags.Parse(args); err != nil {
		return nil, err
	}
	if c.flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: every field is a flag (meshctl help %s)", c.flags.Arg(0), c.action.CLI)
	}
	if c.whole != "" {
		if err := protojson.Unmarshal([]byte(c.whole), c.req); err != nil {
			return nil, fmt.Errorf("--json: %w", err)
		}
	}
	for _, apply := range c.pending {
		apply()
	}
	if c.action.Write {
		f := c.action.Method.Input().Fields().ByName("meta")
		meta := c.req.Mutable(f).Message()
		mfs := meta.Descriptor().Fields()
		switch {
		case c.mutationID != "":
			meta.Set(mfs.ByName("mutation_id"), protoreflect.ValueOfString(c.mutationID))
		case !meta.Has(mfs.ByName("mutation_id")):
			meta.Set(mfs.ByName("mutation_id"), protoreflect.ValueOfString(uuid.NewString()))
		}
		if c.expiresIn > 0 {
			ts := timestamppb.New(time.Now().Add(c.expiresIn))
			meta.Set(mfs.ByName("expires_at"), protoreflect.ValueOfMessage(ts.ProtoReflect()))
		}
	}
	return c.req, nil
}

// initDynamic makes connect allocate dynamic messages of the method's types.
func initDynamic(spec connect.Spec, msg any) error {
	m, ok := msg.(*dynamicpb.Message)
	if !ok {
		return nil
	}
	md, ok := spec.Schema.(protoreflect.MethodDescriptor)
	if !ok {
		return fmt.Errorf("no method schema for %s", spec.Procedure)
	}
	if spec.IsClient {
		*m = *dynamicpb.NewMessage(md.Output())
	} else {
		*m = *dynamicpb.NewMessage(md.Input())
	}
	return nil
}

var (
	pretty  = protojson.MarshalOptions{Multiline: true, Indent: "  "}
	compact = protojson.MarshalOptions{}
)

// call runs the action. A unary response is printed as indented JSON; a stream prints one JSON object per
// line as it arrives, until the daemon ends it.
func call(ctx context.Context, conn *client.Conn, a *registry.Action, req *dynamicpb.Message, out io.Writer) error {
	c := connect.NewClient[dynamicpb.Message, dynamicpb.Message](conn.HTTP, client.BaseURL+a.Procedure,
		connect.WithSchema(a.Method), connect.WithResponseInitializer(initDynamic),
		connect.WithInterceptors(auth.ClientToken(conn.Token)))
	if a.Streaming() {
		stream, err := c.CallServerStream(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		defer stream.Close()
		for stream.Receive() {
			b, err := compact.Marshal(stream.Msg())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", b)
		}
		return stream.Err()
	}
	res, err := c.CallUnary(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	b, err := pretty.Marshal(res.Msg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\n", b)
	return nil
}

// Request builds an action's request from flags, for tests.
func Request(a *registry.Action, args []string) (proto.Message, error) {
	m, err := newCommand(a, Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }}, "").parse(args)
	if err != nil {
		return nil, err
	}
	return m, nil
}
