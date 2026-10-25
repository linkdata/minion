//go:build !windows

package workspace

import "path/filepath"

func validPlatformPath(string) bool { return true }

func canonicalPath(path string) (string, error) { return filepath.EvalSymlinks(path) }
