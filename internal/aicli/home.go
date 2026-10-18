package aicli

import (
	"fmt"
	"os"
)

// HomePath resolves a native override or HOME-relative default without creating it.
// A derived default does not imply that the override should be exported to the CLI.
func HomePath(envVar, dirName string) (string, error) {
	path := os.Getenv(envVar)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve %s home: %w", envVar, err)
		}
		path = home + string(os.PathSeparator) + dirName
	}
	resolved, err := ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", envVar, path, err)
	}
	return resolved, nil
}
