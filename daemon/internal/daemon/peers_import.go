package daemon

import (
	"path/filepath"
	"strings"
)

// sameDir reports whether two paths are one folder.
func sameDir(a, b string) bool {
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea != nil || eb != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), filepath.Clean(dir)+string(filepath.Separator))
}
