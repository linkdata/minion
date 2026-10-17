package gitwork

import (
	"fmt"
	"strings"
)

func repositoryURL(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validRepositorySegment(parts[0]) || !validRepositorySegment(parts[1]) {
		return "", fmt.Errorf("invalid repository %q: want owner/name", repo)
	}
	return "https://github.com/" + parts[0] + "/" + parts[1] + ".git", nil
}

func validRepositorySegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") == ""
}

// validateBranch applies Git's refname restrictions before the value is used
// in a command argument or refspec. A branch must never be an option or a ref
// expression such as @{...}.
func validateBranch(branch string) error {
	if branch == "" || branch == "@" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "//") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return fmt.Errorf("invalid branch %q", branch)
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("invalid branch %q", branch)
		}
	}
	for i := 0; i < len(branch); i++ {
		c := branch[i]
		if c <= ' ' || c == 0x7f || strings.ContainsRune("~^:?*[\\", rune(c)) {
			return fmt.Errorf("invalid branch %q", branch)
		}
	}
	return nil
}
