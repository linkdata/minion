package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workroot"
)

func TestCronPassSkipsPreflightWhenStateLocked(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	var workRoot string
	for _, candidate := range []string{os.Getenv("XDG_RUNTIME_DIR"), "/dev/shm"} {
		if candidate != "" && workroot.Check(candidate, statePath, 1<<62) == nil {
			workRoot = candidate
			break
		}
	}
	if workRoot == "" {
		t.Skip("startup requires a separate mounted work root")
	}

	cfg := config.Config{
		BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot, WorkRootMaxBytes: 1 << 62,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(statePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-config", configPath}
	if code := run(); code != 0 {
		t.Fatalf("locked cron pass returned %d; want clean skip before AI and GitHub preflight", code)
	}
}
