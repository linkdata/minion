// Package testtemp normalizes the default temporary directory for anchored-path tests.
package testtemp

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Main keeps Unix test sockets below Claude's 56-byte temp-root limit.
func Main(m *testing.M) {
	if runtime.GOOS == "windows" {
		os.Exit(m.Run())
	}
	root, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		log.Fatal(err)
	}
	scratch := ""
	if len(root) > 56 {
		scratch, err = os.MkdirTemp("/tmp", "mc-test-")
		if err != nil {
			log.Fatal(err)
		}
		root, err = filepath.EvalSymlinks(scratch)
		if err != nil || len(root) > 56 {
			_ = os.RemoveAll(scratch)
			log.Fatalf("short test temp root: %q: %v", root, err)
		}
	}
	for _, key := range []string{"TMPDIR", "GOTMPDIR"} {
		if err := os.Setenv(key, root); err != nil {
			if scratch != "" {
				_ = os.RemoveAll(scratch)
			}
			log.Fatal(err)
		}
	}
	code := m.Run()
	if scratch != "" {
		if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
			log.Printf("test temp root %q not empty: %v, %v", scratch, entries, err)
			code = 1
		}
		if err := os.RemoveAll(scratch); err != nil {
			log.Print(err)
			code = 1
		}
	}
	os.Exit(code)
}
