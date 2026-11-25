package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strings"
)

// scrubber makes a captured payload safe to commit: the user's home, the capture's repo, session ids and
// anything that names a person or a host are replaced with stable stand-ins, the same one for the same value in
// every file, so the fixtures still say which hooks belong to the same session. What it can't place it refuses:
// a name or an email left over is an error for the person capturing to look at, not a silent keep.
type scrubber struct {
	pairs   [][2]string
	uuids   map[string]string
	order   []string
	secrets []string
}

var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// Claude Code's scratch folder is named for the capturing user's uid.
	uidRE   = regexp.MustCompile(`/tmp/claude-[0-9]+/`)
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z0-9.-]+`)
)

// dashed is how Claude Code names a project's folder under ~/.claude/projects: the path with / and . as -.
func dashed(p string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(p)
}

// newScrubber takes the capture's root folder (everything it made is under it: the repo, the state dir with the
// agent's worktree), the repo, and the home.
func newScrubber(home, root string, repos ...string) *scrubber {
	s := &scrubber{uuids: map[string]string{}}
	for _, r := range repos {
		if r != "" {
			s.pairs = append(s.pairs, [2]string{dashed(r), "-work-repo"}, [2]string{r, "/work/repo"})
		}
	}
	if root != "" {
		s.pairs = append(s.pairs, [2]string{dashed(root), "-work"}, [2]string{root, "/work"})
	}
	if home != "" {
		s.pairs = append(s.pairs, [2]string{dashed(home), "-home-user"}, [2]string{home, "/home/user"})
	}
	s.sortPairs()
	if u, err := user.Current(); err == nil && u.Username != "" {
		s.secrets = append(s.secrets, u.Username)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		s.secrets = append(s.secrets, h)
	}
	return s
}

// sortPairs puts the longest first, so a repo inside the home is replaced before the home is.
func (s *scrubber) sortPairs() {
	sort.SliceStable(s.pairs, func(i, j int) bool { return len(s.pairs[i][0]) > len(s.pairs[j][0]) })
}

// alsoPath replaces another spelling of a path (the unresolved one, when the resolved one was given) with the same
// stand-in as its sibling.
func (s *scrubber) alsoPath(path, standIn string) {
	if path != "" {
		s.pairs = append(s.pairs, [2]string{dashed(path), dashed(standIn)}, [2]string{path, standIn})
		s.sortPairs()
	}
}

// scrub returns the payload with its stand-ins, indented. It is an error when the payload isn't JSON, or a user
// name, a host name or an email address survives.
func (s *scrubber) scrub(raw []byte) ([]byte, error) {
	text := string(raw)
	for _, p := range s.pairs {
		text = strings.ReplaceAll(text, p[0], p[1])
	}
	text = uidRE.ReplaceAllString(text, "/tmp/claude-user/")
	text = uuidRE.ReplaceAllStringFunc(text, func(id string) string {
		id = strings.ToLower(id)
		if fake, ok := s.uuids[id]; ok {
			return fake
		}
		fake := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(s.uuids)+1)
		s.uuids[id] = fake
		s.order = append(s.order, id)
		return fake
	})
	for _, name := range s.secrets {
		if len(name) >= 3 && strings.Contains(strings.ToLower(text), strings.ToLower(name)) {
			return nil, fmt.Errorf("%q is still in the payload: add what it appears in to the scrubber, or edit the capture", name)
		}
	}
	if m := emailRE.FindString(text); m != "" {
		return nil, fmt.Errorf("an email address is in the payload: %q", m)
	}
	text, err := summaryOut(text)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(text), "", "  "); err != nil {
		return nil, fmt.Errorf("not JSON after scrubbing: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// summaryOut replaces a long model-written message (a /compact's summary comes back as a subagent's last message,
// thousands of characters of the model's paraphrase of everything in its context, the account's own instructions
// included) with a stand-in: it is not what the adapter reads, and it is the one field that can carry the
// account's private text.
func summaryOut(text string) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return "", fmt.Errorf("not JSON: %w", err)
	}
	m, _ := obj["last_assistant_message"].(string)
	if len(m) <= longMessage {
		return text, nil
	}
	obj["last_assistant_message"] = "(a conversation summary: left out, it paraphrases the account's own instructions)"
	b, err := json.Marshal(obj)
	return string(b), err
}

// longMessage is the length over which a last_assistant_message is taken for a summary.
const longMessage = 600
