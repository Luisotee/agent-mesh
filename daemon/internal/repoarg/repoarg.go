// Package repoarg is the rule for what a `--repo` argument is, shared by the CLI (which resolves paths against
// the caller's cwd) and the daemon (which looks a bare name up in `repos.dir`).
package repoarg

import "strings"

// IsBareName reports whether arg is a bare repo name: no slash, not `.` or `..`, no leading `~` or `-`, and not a
// URL (`:` or `@`). Anything else is a path (or an option, or a URL) and is never looked up by name.
func IsBareName(arg string) bool {
	return arg != "" && arg != "." && arg != ".." && !strings.Contains(arg, "/") &&
		!strings.HasPrefix(arg, "~") && !strings.HasPrefix(arg, "-") && !strings.ContainsAny(arg, ":@")
}
