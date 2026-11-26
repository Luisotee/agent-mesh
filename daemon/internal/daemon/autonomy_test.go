package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestTheBriefNamesTheReposTheUserMergesOrApprovesPlansForOnlyWhenSet(t *testing.T) {
	w := startPR(t)
	brief := func() string {
		res, err := w.as(w.rootTok).GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Text
	}
	// Nothing set: no autonomy line, so a repo in no list is handled exactly as before.
	if text := brief(); strings.Contains(text, "autonomy") {
		t.Errorf("an autonomy line with nothing set:\n%s", text)
	}
	set := func(key string, vals ...any) {
		t.Helper()
		v, err := structpb.NewValue(vals)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: key, Value: v})); err != nil {
			t.Fatal(err)
		}
	}
	set("repos.user_merges", "/p/shop", "/p/api")
	set("repos.user_approves_plans", "/p/api", "/p/pay")
	set("autonomy.ask_before", "migrations", "deploys")
	text := brief()
	for _, want := range []string{
		"the user's autonomy settings",
		"repo /p/api: the user approves its plans and merges its PRs: don't `mesh approve` a plan there",
		"repo /p/pay: the user approves its plans and merges its PRs",
		"repo /p/shop: the user merges its PRs: never merge one",
		"ask the user before: migrations; deploys",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
	// /p/api is in both lists: it gets the stricter line only.
	if strings.Count(text, "repo /p/api") != 1 {
		t.Errorf("a repo in both lists is named once:\n%s", text)
	}
	// A worker's brief of its run says nothing of it: the settings are the root's to act on.
	res, err := w.as(w.workTok).GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil || strings.Contains(res.Msg.Text, "autonomy settings") {
		t.Errorf("a worker's brief: %v\n%v", err, res)
	}
}
