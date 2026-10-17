package workspace

import (
	"path/filepath"
	"strings"
)

// ValidIdentitySegment accepts one bounded action or repository identifier.
func ValidIdentitySegment(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 100 &&
		strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// Within compares canonical absolute paths; callers resolve symlinks first.
func Within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && filepath.IsLocal(rel)
}
