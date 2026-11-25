package agy

// The adapter against real agy CLI: testdata/<version>/ holds the scrubbed hook payloads of one real session
// (daemon/scripts/capture-agy-hooks.sh makes them, by hand: never a test), numbered in the order they fired,
// with sequence.json saying which scenario each belongs to. Every version with fixtures is decoded, and the
// whole sequence is replayed through the reducer.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/reducer"
)

type fixtureStep struct {
	Scenario string   `json:"scenario"`
	Files    []string `json:"files"`
}

type fixtureSequence struct {
	AgyVersion string        `json:"agy_version"`
	Steps      []fixtureStep `json:"steps"`
}

func fixtureVersions(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestVerifiedVersionsAreTheOnesWithFixtures(t *testing.T) {
	want := append([]string(nil), VerifiedVersions...)
	sort.Strings(want)
	if got := fixtureVersions(t); !reflect.DeepEqual(got, want) {
		t.Errorf("testdata has fixtures for %v, VerifiedVersions lists %v", got, want)
	}
	for _, v := range VerifiedVersions {
		seq := readSequence(t, v)
		if seq.AgyVersion != v {
			t.Errorf("%s/sequence.json says it was captured from %s", v, seq.AgyVersion)
		}
	}
}

func readSequence(t *testing.T, version string) fixtureSequence {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", version, "sequence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seq fixtureSequence
	if err := json.Unmarshal(b, &seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func kinds(evs []*adapterv1.AgentEvent) []string {
	var out []string
	for _, e := range evs {
		switch k := e.Kind.(type) {
		case *adapterv1.AgentEvent_SessionStarted:
			out = append(out, "session_started:"+k.SessionStarted.Source)
		case *adapterv1.AgentEvent_TurnStarted:
			out = append(out, "turn_started")
		case *adapterv1.AgentEvent_Activity:
			out = append(out, "activity:"+k.Activity.Detail)
		case *adapterv1.AgentEvent_TurnEnded:
			out = append(out, "turn_ended")
		case *adapterv1.AgentEvent_RateLimited:
			out = append(out, "rate_limited")
		case *adapterv1.AgentEvent_TurnFailed:
			out = append(out, "turn_failed")
		case *adapterv1.AgentEvent_DecodeError:
			out = append(out, "DECODE_ERROR:"+k.DecodeError.Error)
		default:
			out = append(out, "?")
		}
	}
	return out
}

var expected1_5_17 = map[string][]string{
	"01-SessionStart.json":   {"session_started:turn"},
	"02-PreInvocation.json":  {"turn_started"},
	"03-PreToolUse.json":     {"activity:read_file"},
	"04-PostToolUse.json":    {"activity:read_file"},
	"05-PostInvocation.json": {"activity:thinking"},
	"06-PreInvocation.json":  {"turn_started"},
	"07-PostInvocation.json": {"activity:thinking"},
	"08-Stop.json":           {"turn_ended"},
	"09-PreInvocation.json":  {"turn_started"},
	"10-PreToolUse.json":     {"activity:write_file"},
	"11-PostInvocation.json": {"activity:thinking"},
	"12-PreInvocation.json":  {"turn_started"},
	"13-PostInvocation.json": {"activity:thinking"},
	"14-Stop.json":           {"turn_ended"},
	"15-PreInvocation.json":  {"turn_started"},
	"16-PreToolUse.json":     {"activity:read_file"},
	"17-PostToolUse.json":    {"activity:read_file"},
	"18-PreToolUse.json":     {"activity:run_command"},
	"19-PostToolUse.json":    {"activity:run_command"},
	"20-PostInvocation.json": {"activity:thinking"},
	"21-PreInvocation.json":  {"turn_started"},
	"22-PreToolUse.json":     {"activity:run_command"},
	"23-PostToolUse.json":    {"activity:run_command"},
	"24-PostInvocation.json": {"activity:thinking"},
	"25-PreInvocation.json":  {"turn_started"},
	"26-PreToolUse.json":     {"activity:run_command"},
	"27-PostToolUse.json":    {"activity:run_command"},
	"28-PostInvocation.json": {"activity:thinking"},
	"29-PreInvocation.json":  {"turn_started"},
	"30-PreToolUse.json":     {"activity:run_command"},
	"31-PostToolUse.json":    {"activity:run_command"},
	"32-PostInvocation.json": {"activity:thinking"},
	"33-PreInvocation.json":  {"turn_started"},
	"34-PostInvocation.json": {"activity:thinking"},
	"35-Stop.json":           {"turn_ended"},
}

func TestEveryFixtureOfEveryVersionDecodesAndReplaysThroughTheReducer(t *testing.T) {
	tables := map[string]map[string][]string{"1.5.17": expected1_5_17}
	for _, version := range fixtureVersions(t) {
		t.Run(version, func(t *testing.T) {
			table, ok := tables[version]
			if !ok {
				t.Fatalf("fixtures for %s have no expectations: add a table to fixtures_test.go", version)
			}
			seq := readSequence(t, version)
			files, _ := filepath.Glob(filepath.Join("testdata", version, "[0-9]*.json"))
			var listed []string
			for _, st := range seq.Steps {
				listed = append(listed, st.Files...)
			}
			for i, f := range files {
				files[i] = filepath.Base(f)
			}
			sort.Strings(files)
			if !reflect.DeepEqual(listed, files) {
				t.Fatalf("sequence.json lists %v, folder holds %v", listed, files)
			}

			a := New()
			state := reducer.Start(time.Now())
			session := ""
			for _, st := range seq.Steps {
				// Startup produces 0 hooks in agy; the daemon moves STARTING -> IDLE when output settles.
				if st.Scenario == "startup" && len(st.Files) == 0 {
					state = reducer.Reduce(state, &adapterv1.AgentEvent{
						Time: timestamppb.Now(),
						Kind: &adapterv1.AgentEvent_SessionStarted{SessionStarted: &adapterv1.SessionStarted{Source: "startup"}},
					})
				}
				for _, f := range st.Files {
					raw, err := os.ReadFile(filepath.Join("testdata", version, f))
					if err != nil {
						t.Fatal(err)
					}
					if !json.Valid(raw) {
						t.Fatalf("%s is not JSON", f)
					}
					name := strings.TrimSuffix(strings.SplitN(f, "-", 2)[1], ".json")
					res, err := a.DecodeHook(context.Background(), connect.NewRequest(&adapterv1.DecodeHookRequest{
						AgentId: "a1", SessionId: session, EventName: name, Payload: raw, FiredAt: timestamppb.Now(),
					}))
					if err != nil {
						t.Fatalf("%s: %v", f, err)
					}
					if got := kinds(res.Msg.Events); !reflect.DeepEqual(got, table[f]) {
						t.Errorf("%s decodes to %v, want %v", f, got, table[f])
					}
					for _, ev := range res.Msg.Events {
						state = reducer.Reduce(state, ev)
					}
					session = state.SessionID
				}
				if state.State != v1.AgentState_AGENT_STATE_IDLE {
					t.Errorf("after %q the agent is %v, want IDLE", st.Scenario, state.State)
				}
			}
		})
	}
}

func TestARealAgyReopenKeepsTheConversationIDWithoutSessionStart(t *testing.T) {
	read := func(file string) (convID, model string) {
		var p struct {
			ConversationID string `json:"conversationId"`
			ModelName      string `json:"modelName"`
		}
		b, err := os.ReadFile(filepath.Join("testdata", "1.5.17", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		return p.ConversationID, p.ModelName
	}
	c1, m1 := read("01-SessionStart.json")
	c2, m2 := read("15-PreInvocation.json")
	if c1 == "" || c1 != c2 {
		t.Errorf("conversationId changed across --conversation reopen: %q vs %q", c1, c2)
	}
	if m1 != "gemini-3.8-flash-low" || m2 != "gemini-3.8-flash-medium" {
		t.Errorf("models: %q -> %q", m1, m2)
	}
}
