package claude

// The adapter against real Claude Code: testdata/<version>/ holds the raw hook payloads of one real session,
// scrubbed (daemon/scripts/capture-claude-hooks.sh makes them, by hand: never a test), numbered in the order they
// fired, with sequence.json saying which scenario each belongs to. Every version with fixtures is decoded, and
// the whole sequence is replayed through the reducer. A Claude Code that changes a payload fails here instead of
// quietly changing what an agent's state means.

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
	ClaudeVersion string        `json:"claude_version"`
	Steps         []fixtureStep `json:"steps"`
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

// A version is verified when it has fixtures, and has fixtures when it is listed: the two can't drift.
func TestVerifiedVersionsAreTheOnesWithFixtures(t *testing.T) {
	want := append([]string(nil), VerifiedVersions...)
	sort.Strings(want)
	if got := fixtureVersions(t); !reflect.DeepEqual(got, want) {
		t.Errorf("testdata has fixtures for %v, VerifiedVersions lists %v", got, want)
	}
	for _, v := range VerifiedVersions {
		seq := readSequence(t, v)
		if seq.ClaudeVersion != v {
			t.Errorf("%s/sequence.json says it was captured from %s", v, seq.ClaudeVersion)
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

// kinds names the neutral events of a decode, in order.
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
		case *adapterv1.AgentEvent_NeedsInput:
			out = append(out, "needs_input")
		case *adapterv1.AgentEvent_TurnEnded:
			out = append(out, "turn_ended")
		case *adapterv1.AgentEvent_Compacting:
			out = append(out, "compacting")
		case *adapterv1.AgentEvent_SessionEnded:
			out = append(out, "session_ended:"+k.SessionEnded.Reason)
		case *adapterv1.AgentEvent_DecodeError:
			out = append(out, "DECODE_ERROR:"+k.DecodeError.Error)
		default:
			out = append(out, "?")
		}
	}
	return out
}

// What each captured hook must become, by file (the real session of 2.1.332; another version's fixtures bring
// their own table when they are added).
var expected2_1_332 = map[string][]string{
	"01-SessionStart.json":     {"session_started:startup"},
	"02-UserPromptSubmit.json": {"turn_started"},
	"03-PreToolUse.json":       {"activity:Read"},
	"04-PostToolUse.json":      {"activity:Read"},
	"05-Stop.json":             {"turn_ended"},
	"06-UserPromptSubmit.json": {"turn_started"},
	"07-PreToolUse.json":       {"activity:Write"},
	"08-Notification.json":     {"needs_input"},
	"09-PostToolUse.json":      {"activity:Write"},
	"10-Stop.json":             {"turn_ended"},
	"11-PreCompact.json":       {"compacting"},
	"12-SubagentStop.json":     nil, // a compaction's own subagent: the agent's turn goes on
	"13-SessionStart.json":     {"session_started:compact"},
	"14-SessionEnd.json":       {"session_ended:clear"},
	"15-SessionStart.json":     {"session_started:clear"},
	"16-SessionEnd.json":       {"session_ended:other"}, // the old process, stopped for the reopen
	"17-SessionStart.json":     {"session_started:resume"},
	"18-UserPromptSubmit.json": {"turn_started"},
	"19-PreToolUse.json":       {"activity:Bash"},
	"20-Notification.json":     {"needs_input"},
	"21-PreToolUse.json":       {"activity:Bash"},
	"22-Notification.json":     {"needs_input"},
	"23-Stop.json":             {"turn_ended"},
	"24-SessionEnd.json":       {"session_ended:prompt_input_exit"},
}

// What the agent shows after each scenario of the session, from the reducer (state, how many prompts wait).
// endState is what the agent is after a scenario's payloads have been replayed.
type endState struct {
	state v1.AgentState
	needs int32
}

var after2_1_332 = map[string]endState{
	"startup":                              {v1.AgentState_AGENT_STATE_IDLE, 0},
	"a turn with a tool call":              {v1.AgentState_AGENT_STATE_IDLE, 0},
	"a permission prompt":                  {v1.AgentState_AGENT_STATE_NEEDS_INPUT, 1},
	"the prompt accepted, the turn ending": {v1.AgentState_AGENT_STATE_IDLE, 0},
	"/compact":                             {v1.AgentState_AGENT_STATE_IDLE, 0},
	"/clear":                               {v1.AgentState_AGENT_STATE_IDLE, 0},
	"a reopen (claude --resume)":           {v1.AgentState_AGENT_STATE_IDLE, 0},
	"/exit":                                {v1.AgentState_AGENT_STATE_IDLE, 0}, // the process exits after it, which the host reports
}

func TestEveryFixtureOfEveryVersionDecodesAndTheSessionReplaysThroughTheReducer(t *testing.T) {
	tables := map[string]map[string][]string{"2.1.332": expected2_1_332}
	afterTables := map[string]map[string]endState{"2.1.332": after2_1_332}
	for _, version := range fixtureVersions(t) {
		t.Run(version, func(t *testing.T) {
			table, ok := tables[version]
			after, ok2 := afterTables[version]
			if !ok || !ok2 {
				t.Fatalf("fixtures for %s have no expectations: add its tables to fixtures_test.go", version)
			}
			seq := readSequence(t, version)
			// sequence.json lists every payload once, in order.
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
				t.Fatalf("sequence.json lists %v, the folder holds %v", listed, files)
			}

			a := New()
			state := reducer.Start(time.Now())
			session := "" // the conversation the daemon knows: from the first SessionStart on, as reportHook passes it
			for _, st := range seq.Steps {
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
						AgentId: "a1", SessionId: session, EventName: name, Payload: raw, FiredAt: timestamppb.Now()}))
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
					// The state mid-scenario, where it says something the end of the scenario doesn't (the file
					// names are 2.1.332's: a version with other payloads gets its own checks).
					if version != "2.1.332" {
						continue
					}
					switch f {
					case "11-PreCompact.json", "12-SubagentStop.json":
						if state.State != v1.AgentState_AGENT_STATE_WORKING || state.Detail != "compacting" {
							t.Errorf("%s: a compaction shows as %v %q", f, state.State, state.Detail)
						}
					case "13-SessionStart.json":
						if state.State != v1.AgentState_AGENT_STATE_IDLE {
							t.Errorf("%s: the compacted session isn't idle again: %v", f, state.State)
						}
					case "20-Notification.json", "22-Notification.json":
						if state.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
							t.Errorf("%s: a permission prompt shows as %v", f, state.State)
						}
					}
				}
				want, ok := after[st.Scenario]
				if !ok {
					t.Errorf("scenario %q has no expected end state for %s", st.Scenario, version)
					continue
				}
				if state.State != want.state || state.NeedsInput != want.needs {
					t.Errorf("after %q the agent is %v with %d waiting, want %v with %d", st.Scenario, state.State, state.NeedsInput,
						want.state, want.needs)
				}
			}
		})
	}
}

// The questions a reopen rests on, answered by the real session: `claude --resume <id>` starts a session with the
// same id, so the daemon's record of the conversation stays valid, and the killed process's SessionEnd arrives
// before the resumed one's SessionStart, under the same id.
func TestARealResumeKeepsTheSessionIDAndTheKilledProcessEndsFirst(t *testing.T) {
	id := func(file string) string {
		var p struct {
			SessionID string `json:"session_id"`
			Source    string `json:"source"`
		}
		b, err := os.ReadFile(filepath.Join("testdata", "2.1.332", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		return p.SessionID + "/" + p.Source
	}
	cleared, ended, resumed := id("15-SessionStart.json"), id("16-SessionEnd.json"), id("17-SessionStart.json")
	cs, es, rs := strings.Split(cleared, "/")[0], strings.Split(ended, "/")[0], strings.Split(resumed, "/")[0]
	if cs != es || cs != rs {
		t.Errorf("the session id changed across the resume: %s, %s, %s", cs, es, rs)
	}
	if !strings.HasSuffix(resumed, "/resume") {
		t.Errorf("the resumed session's source: %s", resumed)
	}
}
