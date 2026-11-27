package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
)

func TestAReportHeaderNamesTheTaskItAnswers(t *testing.T) {
	ts := timestamppb.New(time.Date(2026, 11, 26, 4, 12, 19, 0, time.UTC))
	var out bytes.Buffer
	renderInbox(&v1.ReadInboxResponse{Messages: []*v1.Message{
		{Id: "m9", Re: "m3", Type: v1.MessageType_MESSAGE_TYPE_DONE, From: &v1.Actor{Name: "w"}, CreatedAt: ts, Body: "ok"},
	}}, &out)
	if want := "── done from w at 2026-11-26T04:12:19Z (m9, on task m3) ──\nok\n"; out.String() != want {
		t.Errorf("header:\n%s\nwant\n%s", out.String(), want)
	}
}

func TestAskAndAsksHaveATextForm(t *testing.T) {
	round := &v1.Ask{Id: "a1", Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_FINISHED, CallerName: "lead",
		Pr: "https://github.com/o/r/pull/7", PrNumber: 7, Round: 2, Size: "large", AnswersPath: "/run/reports/a1.md",
		Members: []*v1.AskMember{
			{Name: "r-claude", Kind: "claude", How: "fresh", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED},
			{Name: "r-agy", Kind: "agy", How: "fresh", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_SETTLED, Line: "failed"},
			{Name: "r-copilot", Kind: "copilot", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED, Line: "not installed"},
		},
		Verdicts: []*v1.Verdict{{Reviewer: "r-claude", Agree: 3, Other: 1, Disagree: 0, Blocking: 1, Judge: "author"}}}
	var out bytes.Buffer
	renderAsks(&v1.ListAsksResponse{Asks: []*v1.Ask{round}}, &out)
	want := "a1  review  finished  PR #7 round 2 (large)  by lead  1/2 answered  verdicts (agree/other/disagree): r-claude 3/1/0 (1 blocking) by author\n" +
		"    r-claude answered, r-agy settled, r-copilot not_asked\n    /run/reports/a1.md\n"
	if out.String() != want {
		t.Errorf("asks:\n%s\nwant\n%s", out.String(), want)
	}
	// A relay step is no round; a finished round whose file could not be written points at the JSON.
	out.Reset()
	step := &v1.Ask{Id: "a2", Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_FINISHED, Collected: true,
		Pr: "https://github.com/o/r/pull/7", PrNumber: 7, Round: 1, RelayStep: 1, Size: "large"}
	renderAsks(&v1.ListAsksResponse{Asks: []*v1.Ask{step}}, &out)
	if s := out.String(); !strings.Contains(s, "PR #7 relay step 1 (large)") || strings.Contains(s, "round") ||
		!strings.Contains(s, "mesh asks --format json") {
		t.Errorf("a relay step with no answers file:\n%s", s)
	}
	out.Reset()
	renderAsks(&v1.ListAsksResponse{}, &out)
	if out.String() != "no asks\n" {
		t.Errorf("no asks: %q", out.String())
	}
	out.Reset()
	round.Status = v1.AskStatus_ASK_STATUS_OPEN
	renderAsk(&v1.AskResponse{Ask: round, Notes: []string{"a worktree stayed on an old commit"}, Hint: "round 2 of 2"}, &out)
	for _, w := range []string{"a1  review  open  PR #7 round 2 (large)", "  r-agy (agy, fresh): settled: failed",
		"  r-copilot (copilot): not_asked: not installed", "answers: /run/reports/a1.md", "note: a worktree stayed", "round 2 of 2"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("ask lacks %q:\n%s", w, out.String())
		}
	}
	out.Reset()
	renderAsk(&v1.AskResponse{Agents: []*v1.Agent{{Name: "r-ask", Kind: "claude"}}, Message: &v1.Message{Id: "m5"}}, &out)
	if want := "asked r-ask (claude)\ntask m5\n"; out.String() != want {
		t.Errorf("one agent: %q", out.String())
	}
	if !hasText("ask") || !hasText("asks") {
		t.Error("ask and asks have no --format")
	}
}

func TestTheUsageLinePointsAtTheHowToMap(t *testing.T) {
	reg, err := registry.New()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	usage(&out, reg, nil)
	if !strings.Contains(out.String(), "mesh protocol how-do-i") {
		t.Errorf("usage lacks the map:\n%s", out.String())
	}
}
