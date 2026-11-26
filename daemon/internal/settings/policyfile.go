package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A policy file is bin/mesh's `~/.config/agent-mesh.json` (design §21): the policy leaves, nested by their dots.
// meshd imports it once into the settings store (docs: daemon/README.md, "Settings"); this is the pure part,
// which maps a file's keys onto the host's leaves and checks every value against its rule.

// PolicyValue is one leaf an import would set.
type PolicyValue struct {
	Key   string
	Value any
	// From is the file's own key when it isn't Key (`repos_dir` for `repos.dir`), else empty.
	From string
}

// PolicySkip is a key of the file an import leaves alone, with why.
type PolicySkip struct {
	Key string
	Why string
}

// policyRenames are bin/mesh leaves the host keeps under another key.
var policyRenames = map[string]string{
	"peers_dir": "peers.dir",
	"repos_dir": "repos.dir",
}

// policyUnused are bin/mesh leaves nothing in the daemon reads: they belong to herdr's panes, `mesh watch` and
// the `--bg` views. A subtree is named by its first key.
var policyUnused = map[string]string{
	"hang_after":    "not used by the daemon (it has no watch; `accountability.*` and `caps.agents` are its guards)",
	"held_wait":     "not used by the daemon (it has no watch)",
	"side_panes":    "not used by the daemon (it has no herdr panes)",
	"watch.timeout": "not used by the daemon (it has no watch)",
	"models":        "not used by the daemon (it has no herdr panes or background views; models are `tiers.<kind>.<tier>.*`)",
}

// copilotModelKey is the old leaf that stood in for every built-in Copilot model: an import writes it onto each
// `tiers.copilot.<tier>.model` the file doesn't name itself, as bin/mesh read it (`CopilotCli.default_model`).
const copilotModelKey = "copilot_model"

func unusedWhy(leaf string) (string, bool) {
	for key, why := range policyUnused {
		if leaf == key || strings.HasPrefix(leaf, key+".") {
			return why, true
		}
	}
	return "", false
}

func unusedParent(leaf string) bool {
	for key := range policyUnused {
		if strings.HasPrefix(key, leaf+".") {
			return true
		}
	}
	return false
}

// ParsePolicy reads a policy file. The error is only for text that isn't a JSON object; a key that can't be
// imported (unknown, wrong type, unused by the daemon) is a skip, and the rest still imports. Keys starting with
// `_` are comments. The values come back in key order.
func ParsePolicy(s *Schema, data []byte) ([]PolicyValue, []PolicySkip, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, nil, fmt.Errorf("not valid JSON: more than one value")
	}
	top, ok := root.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("expected a JSON object")
	}
	var vals []PolicyValue
	var skips []PolicySkip
	seen := map[string]string{} // leaf -> the file's key that set it
	var copilot any
	var copilotSet bool

	var walk func(node map[string]any, prefix string)
	walk = func(node map[string]any, prefix string) {
		keys := make([]string, 0, len(node))
		for k := range node {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasPrefix(k, "_") {
				continue
			}
			leaf, v := prefix+k, node[k]
			if why, ok := unusedWhy(leaf); ok {
				skips = append(skips, PolicySkip{leaf, why})
				continue
			}
			if leaf == copilotModelKey {
				copilot, copilotSet = v, true
				continue
			}
			target := leaf
			if to, ok := policyRenames[leaf]; ok {
				target = to
			}
			if l, ok := s.Lookup(target); ok {
				checked, err := Check(l, v)
				if err != nil {
					skips = append(skips, PolicySkip{leaf, err.Error()})
					continue
				}
				if prev, dup := seen[target]; dup {
					skips = append(skips, PolicySkip{leaf, "already given as " + prev})
					continue
				}
				seen[target] = leaf
				pv := PolicyValue{Key: target, Value: checked}
				if target != leaf {
					pv.From = leaf
				}
				vals = append(vals, pv)
				continue
			}
			if m, ok := v.(map[string]any); ok && (len(s.Keys(target+".")) > 0 || unusedParent(leaf)) {
				walk(m, leaf+".")
				continue
			}
			skips = append(skips, PolicySkip{leaf, "no such setting"})
		}
	}
	walk(top, "")

	if copilotSet {
		model, ok := copilot.(string)
		if !ok || model == "" {
			skips = append(skips, PolicySkip{copilotModelKey, "must be a non-empty string"})
		} else {
			for _, key := range s.Keys("tiers.copilot.") {
				if !strings.HasSuffix(key, ".model") {
					continue
				}
				if _, named := seen[key]; named {
					continue
				}
				seen[key] = copilotModelKey
				vals = append(vals, PolicyValue{Key: key, Value: []any{model}, From: copilotModelKey})
			}
		}
	}
	sort.SliceStable(vals, func(i, j int) bool { return vals[i].Key < vals[j].Key })
	sort.SliceStable(skips, func(i, j int) bool { return skips[i].Key < skips[j].Key })
	return vals, skips, nil
}
