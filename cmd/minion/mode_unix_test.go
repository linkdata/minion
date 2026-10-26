//go:build !windows

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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestOrchestrateModeRejectsAliasedWorkRootOverlap(t *testing.T) {
	base := t.TempDir()
	workRoot := filepath.Join(base, "work")
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(workRoot, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", workRoot)
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	code := runOrchestrateMode(t.Context(), &action.Orchestrator{WorkRoot: alias}, &monitor.Monitor{}, &state.Store{}, false, logger, "", time.Second)
	if code != 1 || !strings.Contains(logs.String(), "overlaps the model work root") || strings.Contains(logs.String(), "start selected worker") {
		t.Fatalf("aliased work root was not rejected before worker startup: code=%d log=%q", code, logs.String())
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runtime directory was not cleaned up: entries=%v error=%v", entries, err)
	}
}
