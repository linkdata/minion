package aicli

import (
	"fmt"
	"os"
	"path/filepath"
)

// HomePath resolves a native override or OS user-home default without creating it.
// A derived default does not imply that the override should be exported to the CLI.
func HomePath(envVar, dirName string) (string, error) {
	path := os.Getenv(envVar)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve %s home: %w", envVar, err)
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("resolve %s home: native home must be absolute", envVar)
		}
		path = home + string(os.PathSeparator) + dirName
	}
	resolved, err := ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", envVar, path, err)
	}
	return resolved, nil
}
