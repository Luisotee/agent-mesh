package copilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func TestListCommandsHasTheBuiltInsAndTheSkillsOfTheUserAndTheWorktree(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	for p, body := range map[string]string{
		filepath.Join(home, ".copilot", "skills", "mine", "SKILL.md"):  "---\nname: mine\ndescription: user skill\n---\n",
		filepath.Join(cwd, ".github", "skills", "ship", "SKILL.md"):    "---\nname: ship\ndescription: project skill\n---\n",
		filepath.Join(cwd, ".claude", "skills", "review2", "SKILL.md"): "---\nname: review2\ndescription: from claude's folder\n---\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := New().ListCommands(context.Background(), connect.NewRequest(&adapterv1.ListCommandsRequest{Cwd: cwd, Env: map[string]string{"HOME": home}}))
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, c := range res.Msg.Commands {
		if _, dup := src[c.Name]; dup {
			t.Errorf("%s listed twice", c.Name)
		}
		src[c.Name] = c.Source
	}
	if src["mine"] != "user" || src["ship"] != "project" || src["review2"] != "project" || src["model"] != "builtin" || src["exit"] != "builtin" {
		t.Errorf("sources: mine %q ship %q review2 %q model %q", src["mine"], src["ship"], src["review2"], src["model"])
	}
}

func TestTheBuiltInTableHasNoRepeatedNameOrEmptyDescription(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range builtinCommands {
		if c.Name == "" || c.Desc == "" || seen[c.Name] {
			t.Errorf("bad or repeated entry %q", c.Name)
		}
		seen[c.Name] = true
	}
}
