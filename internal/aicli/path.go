package aicli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/workspace"
)

// SafePath excludes relative lookup and directories the model can write.
func SafePath(value string, writable ...string) string {
	var blocked []string
	for _, root := range writable {
		if root == "" {
			continue
		}
		resolved, err := ResolvePath(root)
		if err != nil {
			continue
		}
		blocked = append(blocked, resolved)
	}
	var safe []string
	appendSafe := func(value string) {
		if !filepath.IsAbs(value) {
			return
		}
		resolved, err := ResolvePath(value)
		if err != nil {
			return
		}
		for _, root := range blocked {
			if workspace.Within(root, resolved) {
				return
			}
		}
		safe = append(safe, resolved)
	}
	for _, dir := range filepath.SplitList(value) {
		appendSafe(dir)
	}
	if len(safe) == 0 {
		for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
			appendSafe(dir)
		}
	}
	if len(safe) == 0 {
		return "/dev/null" // No command lookup is safer than a writable fallback.
	}
	return strings.Join(safe, string(os.PathListSeparator))
}
