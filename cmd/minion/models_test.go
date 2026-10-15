package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestCheckModelsRequiresOnlySelectedProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(key, "")
	}
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

func TestConfiguredCodexKeySkipsAmbientLogin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	binary := filepath.Join(dir, "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf called > \"$0.called\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkCodexCLI(context.Background(), config.CodexConfig{APIKey: "dummy-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checked ambient login despite configured key: %v", err)
	}
	if err := checkCodexCLI(context.Background(), config.CodexConfig{}); err == nil {
		t.Fatal("accepted failed ambient login")
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
