package aicli

import (
	"errors"
	"fmt"
	"os"
)

// ResolveSystemReadPaths requires the first path and omits missing optional paths.
func ResolveSystemReadPaths(paths []string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	for i, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if i != 0 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("system read path %q: %w", path, err)
		}
		absolute, err := ResolvePath(path)
		if err != nil {
			return nil, fmt.Errorf("system read path %q: %w", path, err)
		}
		resolved = append(resolved, absolute)
	}
	return resolved, nil
}
