package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
)

func writeProtocol(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSectionsSplitByHeadingOutsideFencesAndRootSteps(t *testing.T) {
	dir := writeProtocol(t, map[string]string{
		"ORCHESTRATE.md": "# Orchestrate\n\nintro\n\n## Modes\n\nmodes text\n\n### Tiers\n\n```\n## not a heading\n```\n\ntiers text\n\n## Root\n\n1. **Start:** begin.\n2. **Find the scope:** scope.\n   more of two\n3. **Size:** size.\n\n## Questions and reviews\n\nq text\n",
		"PROTOCOL.md":    "# Protocol\n\n1. rule one\n\n## Rules for everyone\n\nrules\n",
		"ROOT_CARD.md":   "## Root card\n\nthe card\n",
	})
	names, parts := Sections(dir)
	for _, want := range []string{"modes", "tiers", "root", "root.1", "root.2", "root.3", "questions-and-reviews", "rules-for-everyone", "root-card", "protocol"} {
		if _, ok := parts[want]; !ok {
			t.Errorf("no part %q in %v", want, names)
		}
	}
	if !strings.Contains(parts["tiers"], "## not a heading") || !strings.Contains(parts["tiers"], "tiers text") || strings.Contains(parts["tiers"], "## Root") {
		t.Errorf("tiers: %q", parts["tiers"])
	}
	if got := parts["root.2"]; got != "## Root, step 2\n\n2. **Find the scope:** scope.\n   more of two" {
		t.Errorf("root.2: %q", got)
	}
	if parts["root-card"] != "## Root card\n\nthe card" || !strings.HasPrefix(parts["protocol"], "# Protocol") {
		t.Errorf("card %q protocol %q", parts["root-card"], parts["protocol"])
	}
}

func TestANumberedListInsideARootStepIsNotTheNextStep(t *testing.T) {
	dir := writeProtocol(t, map[string]string{
		"ORCHESTRATE.md": "## Root\n\n1. **Start:** go:\n1. a nested item\n2. **Next:** two.\n3. **Last:** three.\n",
	})
	_, parts := Sections(dir)
	if !strings.Contains(parts["root.1"], "a nested item") || strings.Contains(parts["root.2"], "nested") || !strings.HasPrefix(parts["root.2"], "## Root, step 2\n\n2. **Next:**") {
		t.Errorf("root.1 %q root.2 %q", parts["root.1"], parts["root.2"])
	}
}

func TestTheDaemonsProtocolHasTheStepsTheBriefPointsTo(t *testing.T) {
	_, parts := Sections(filepath.Join("..", "..", "share", "protocol"))
	// `mesh brief` sends a root to step 6 before approving a plan and to step 8 before checking a done.
	if !strings.Contains(parts["root.6"], "Review every plan") || !strings.Contains(parts["root.8"], "Check every `done`") {
		t.Errorf("root.6 %q root.8 %q", firstLine(parts["root.6"]), firstLine(parts["root.8"]))
	}
	for _, want := range []string{"questions-and-reviews", "follow-ups", "root-card", "protocol", "tiers", "sizes", "notes-and-your-own-context"} {
		if _, ok := parts[want]; !ok {
			t.Errorf("no part %q", want)
		}
	}
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

func TestMeshProtocolListsAndPrintsAPart(t *testing.T) {
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "protocol"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ORCHESTRATE.md": "## Root\n\n1. **Start:** go.\n", "PROTOCOL.md": "# P\n"} {
		if err := os.WriteFile(filepath.Join(share, "protocol", name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(k string) string {
		if k == client.ShareEnv {
			return share
		}
		return ""
	}}
	if code := Protocol(env, nil); code != 0 || !strings.Contains(out.String(), "root.1") || !strings.Contains(out.String(), "chars") {
		t.Errorf("list: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Protocol(env, []string{"ORCHESTRATE.root.1"}); code != 0 || !strings.Contains(out.String(), "## Root, step 1") {
		t.Errorf("part: %d %q", code, out.String())
	}
	out.Reset()
	if code := Protocol(env, []string{"nope"}); code != 1 || !strings.Contains(errb.String(), "no protocol part named nope") {
		t.Errorf("unknown: %d %q", code, errb.String())
	}
}
