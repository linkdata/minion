package workspace

import (
	"path/filepath"
	"strings"
)

func validPlatformPath(name string) bool {
	if _, err := filepath.Localize(name); err != nil {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if !validWindowsComponent(part) || ntfsGitMetadataAlias(part) {
			return false
		}
	}
	return true
}

func validWindowsComponent(part string) bool {
	_, err := filepath.Localize(part)
	return err == nil && strings.TrimRight(part, " .") == part &&
		!strings.ContainsAny(part, `<>"|?*`) && strings.IndexFunc(part, func(r rune) bool { return r < ' ' }) < 0
}
