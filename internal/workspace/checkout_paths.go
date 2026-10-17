package workspace

import "strings"

// ValidCheckoutPath applies Git's protected metadata names before any blob fetch.
// Git's NTFS checks treat backslashes as separators; its HFS checks do not.
func ValidCheckoutPath(name string) bool {
	if !ValidSourcePath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if gitMetadataAlias(part) {
			return false
		}
		for _, ntfsPart := range strings.Split(part, "\\") {
			if ntfsGitMetadataAlias(ntfsPart) {
				return false
			}
		}
	}
	return true
}

func ntfsGitMetadataAlias(part string) bool {
	name, _, _ := strings.Cut(part, ":")
	name = strings.TrimRight(name, " .")
	return strings.EqualFold(name, ".git") || strings.EqualFold(name, "git~1")
}
