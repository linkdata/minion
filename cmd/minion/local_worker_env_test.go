package main

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/action"
)

func TestSelectedWorkerEnv(t *testing.T) {
	seeded := map[string]string{
		"GH_TOKEN": "parent-gh", "GITHUB_TOKEN": "parent-github", "Gh_ToKeN": "mixed-gh", "gItHuB_tOkEn": "mixed-github",
		"GH_ENTERPRISE_TOKEN": "parent-enterprise-gh", "GITHUB_ENTERPRISE_TOKEN": "parent-enterprise-github",
		"CODEX_API_KEY": "codex-key", "CODEX_ACCESS_TOKEN": "codex-token", "OPENAI_API_KEY": "openai-key",
		"ANTHROPIC_API_KEY": "claude-key", "ANTHROPIC_AUTH_TOKEN": "claude-auth", "CLAUDE_CODE_OAUTH_TOKEN": "claude-token",
		"MINION_WORKER_ENV_ORDINARY": "keep-me",
	}
	for name, value := range seeded {
		t.Setenv(name, value)
	}
	codex := action.WorkerSelection{Main: action.RoleSettings{Provider: "codex"}}
	claude := action.WorkerSelection{Main: action.RoleSettings{Provider: "claude"}}
	both := action.WorkerSelection{Main: action.RoleSettings{Provider: "codex"}, Critic: &action.RoleSettings{Provider: "claude"}}
	for _, tc := range []struct {
		name     string
		startup  action.WorkerStartup
		codexEnv bool
	}{
		{name: "Codex env login", startup: action.WorkerStartup{Selection: codex}, codexEnv: true},
		{name: "Codex configured key", startup: action.WorkerStartup{Selection: codex, CodexAPIKey: "configured"}},
		{name: "Claude login", startup: action.WorkerStartup{Selection: claude}},
		{name: "Claude configured key", startup: action.WorkerStartup{Selection: claude, ClaudeAPIKey: "configured"}},
		{name: "both providers", startup: action.WorkerStartup{Selection: both}, codexEnv: true},
		{name: "both configured", startup: action.WorkerStartup{Selection: both, CodexAPIKey: "configured", ClaudeAPIKey: "configured"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(map[string]string)
			for _, entry := range selectedWorkerEnv(tc.startup) {
				key, value, _ := strings.Cut(entry, "=")
				key = strings.ToUpper(key)
				if key == "GH_TOKEN" || key == "GITHUB_TOKEN" || key == "GH_ENTERPRISE_TOKEN" || key == "GITHUB_ENTERPRISE_TOKEN" || key == "OPENAI_API_KEY" || key == "ANTHROPIC_AUTH_TOKEN" || key == "ANTHROPIC_API_KEY" || key == "CLAUDE_CODE_OAUTH_TOKEN" {
					t.Fatalf("unused credential reached child environment: %q", key)
				}
				got[key] = value
			}
			if got["MINION_WORKER_ENV_ORDINARY"] != "keep-me" {
				t.Fatalf("ordinary environment changed: %q", got["MINION_WORKER_ENV_ORDINARY"])
			}
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
				value, present := got[key]
				if present != tc.codexEnv {
					t.Fatalf("%s present = %v, want %v", key, present, tc.codexEnv)
				}
				if present && value != seeded[key] {
					t.Fatalf("%s value changed", key)
				}
			}
		})
	}
}
