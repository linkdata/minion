// Package testtemp normalizes the default temporary directory for anchored-path tests.
package testtemp

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Main keeps Darwin's symlinked /var temporary path out of ordinary fixtures.
func Main(m *testing.M) {
	if runtime.GOOS == "darwin" {
		root, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			log.Fatal(err)
		}
		for _, key := range []string{"TMPDIR", "GOTMPDIR"} {
			if err := os.Setenv(key, root); err != nil {
				log.Fatal(err)
			}
		}
	}
	os.Exit(m.Run())
}
