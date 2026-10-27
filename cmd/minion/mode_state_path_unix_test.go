//go:build linux || darwin

package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/state"
)

func TestSelectedModeSyncsStatePathBeforeWorkerStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the search-only directory check")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "search-only")
	dir := filepath.Join(parent, "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	initial, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatalf("ordinary state open under searchable ancestor: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	var output bytes.Buffer
	code := runOrchestrateMode(t.Context(), &action.Orchestrator{}, nil, store, false,
		log.New(&output, "", 0), "", time.Second)
	if code != 1 || !strings.Contains(output.String(), "sync selected state path:") {
		t.Fatalf("selected mode started without path sync: code=%d log=%q", code, output.String())
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncPathAncestors(); err != nil {
		t.Fatalf("selected state path remained unavailable after access returned: %v", err)
	}
}
