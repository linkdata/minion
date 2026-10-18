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
			return "", nil
		}
		path = home + string(os.PathSeparator) + dirName
	}
	resolved, err := ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", envVar, err)
	}
	return resolved, nil
}
