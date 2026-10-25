//go:build linux

package main

import (
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

func TestRunSelectedLocalWorkerPreflightsBeforeOpeningSocket(t *testing.T) {
	selection := action.WorkerSelection{
		Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Main:     action.RoleSettings{Provider: "codex", Model: "test-model", Effort: "high"},
	}
	err := runSelectedLocalWorker(t.Context(), filepath.Join(t.TempDir(), "missing.sock"), action.Runner{}, selection)
	if err == nil || err.Error() != "worker work root unavailable" {
		t.Fatalf("preflight must fail before socket dial: %v", err)
	}
}
