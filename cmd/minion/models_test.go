package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestCheckModelsRequiresOnlySelectedProvider(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "claude" && runtime.GOOS != "linux" {
				t.Skip("Claude sandbox support requires Linux")
			}
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			for _, name := range []string{"codex", "claude"} {
				body := "#!/bin/sh\nprintf 'called' > \"$0.called\"\nprintf 'fixture preflight failure' >&2\nexit 1\n"
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{Main: provider}
			var output bytes.Buffer
			if err := checkModels(context.Background(), &cfg, log.New(&output, "", 0)); err == nil {
				t.Fatal("preflight accepted a broken CLI")
			}
			for _, name := range []string{"codex", "claude"} {
				_, err := os.Stat(filepath.Join(dir, name+".called"))
				if (err == nil) != (name == provider) {
					t.Fatalf("%s called = %t; selected %s", name, err == nil, provider)
				}
			}
		})
	}
}

func TestLogCommandSkillsReportsUnsupportedWorkflow(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{Main: "claude", Commands: map[string]config.CommandMapping{
		"security": {Action: "security-audit", Skills: []string{"unused"}},
	}}
	logCommandSkills(cfg, log.New(&output, "", 0))
	if !strings.Contains(output.String(), "command security disabled: action security-audit is unsupported with claude as main") {
		t.Fatalf("missing disabled action diagnostic: %s", output.String())
	}
}
