package daemon

import "testing"

// isDirectTask reads the rule bin/mesh's DIRECT_RE does (tests/test_mesh.py has the same table).
func TestIsDirectTaskReadsTheRuleBinMeshDoes(t *testing.T) {
	for body, want := range map[string]bool{
		"Direct task (no plan): fix it":                     true,
		"Direct task (no plan)\n\nFix it.":                  true,
		"Direct task: fix it":                               true,
		"Direct task:fix it":                                true,
		"Direct task. Fix it.":                              true,
		"  Direct task\n\nFix it.":                          true,
		"Direct task\r\n\r\nFix it.":                        true,
		"Direct task":                                       true,
		"Direct tasks are what we send often: plan the doc": false,
		"Direct task list: plan the doc":                    false,
		"Direct task force, plan it":                        false,
		"Direct task.md should be restructured: plan it":    false,
		"Plan the Direct task feature":                      false,
		"":                                                  false,
	} {
		if got := isDirectTask(body); got != want {
			t.Errorf("isDirectTask(%q) = %v, want %v", body, got, want)
		}
	}
}
