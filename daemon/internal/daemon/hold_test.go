package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func holdPR(t *testing.T, w *prWorld, url string, release bool) (*v1.HoldPullRequestResponse, error) {
	t.Helper()
	res, err := w.device.Mesh.HoldPullRequest(context.Background(), connect.NewRequest(&v1.HoldPullRequestRequest{Meta: meta(""), Url: url, Release: release}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestTheUserCanHoldAPullRequestsMergeAndReleaseIt(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	url := "https://github.com/acme/demo/pull/12"
	if rows := w.prs("", false); len(rows) != 1 || len(rows[0].PullRequests) != 1 || rows[0].PullRequests[0].Held {
		t.Fatalf("before the hold: %v", rows)
	}

	res, err := holdPR(t, w, url, false)
	if err != nil || len(res.PullRequests) != 1 || !res.PullRequests[0].Held {
		t.Fatalf("hold: %v, %v", res, err)
	}
	if rows := w.prs("", false); !rows[0].PullRequests[0].Held {
		t.Errorf("ListPullRequests doesn't show the hold: %v", rows[0].PullRequests)
	}
	// A hold shows in the event log (as a change with nothing but the flag), and a client holding the PR gets it.
	ch := w.changes()
	last := ch[len(ch)-1]
	if !last.PullRequest.Held || last.FromState != "open" || last.PullRequest.State != "open" {
		t.Errorf("last change %v", last)
	}

	// GitHub is read again (state and checks change): the hold stays, it is the user's, not GitHub's.
	w.setList(ghPR(12, "OPEN", w.branchTip, false, run("COMPLETED", "SUCCESS")))
	w.advance(3 * time.Minute)
	if rows := w.prs("", false); !rows[0].PullRequests[0].Held || rows[0].PullRequests[0].Checks != "pass" {
		t.Errorf("after a read of GitHub: %v", rows[0].PullRequests)
	}

	// The brief the root reads says so, in words that tell it not to merge.
	brief, err := w.as(w.rootTok).GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil || !strings.Contains(brief.Msg.Text, "HELD by the user: do not merge") {
		t.Errorf("the brief: %v\n%v", err, brief)
	}

	// Releasing it, twice, is fine.
	for range 2 {
		if res, err := holdPR(t, w, url, true); err != nil || res.PullRequests[0].Held {
			t.Fatalf("release: %v, %v", res, err)
		}
	}
	if rows := w.prs("", false); rows[0].PullRequests[0].Held {
		t.Errorf("still held: %v", rows[0].PullRequests)
	}
}

func TestHoldingAPullRequestTheHostDoesNotListIsRefused(t *testing.T) {
	w := startPR(t)
	if _, err := holdPR(t, w, "https://github.com/acme/demo/pull/99", false); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("got %v, want not found", err)
	}
	if _, err := holdPR(t, w, " ", false); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an empty url: %v", err)
	}
}

func TestOnlyTheUserOrTheRootMayHold(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	w.prs("", false)
	_, err := w.as(w.workTok).HoldPullRequest(context.Background(), connect.NewRequest(&v1.HoldPullRequestRequest{Meta: meta(""), Url: "https://github.com/acme/demo/pull/12"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a worker holding its own PR: %v", err)
	}
	if _, err := w.as(w.rootTok).HoldPullRequest(context.Background(), connect.NewRequest(&v1.HoldPullRequestRequest{Meta: meta(""), Url: "https://github.com/acme/demo/pull/12"})); err != nil {
		t.Errorf("the root: %v", err)
	}
}
