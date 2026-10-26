package main

import (
	"strings"
	"testing"
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
	got := make(map[string]string)
	for _, entry := range selectedWorkerEnv() {
		key, value, _ := strings.Cut(entry, "=")
		got[strings.ToUpper(key)] = value
	}
	if got["MINION_WORKER_ENV_ORDINARY"] != "keep-me" {
		t.Fatalf("ordinary environment changed: %q", got["MINION_WORKER_ENV_ORDINARY"])
	}
	for key := range seeded {
		if key != "MINION_WORKER_ENV_ORDINARY" {
			if _, present := got[strings.ToUpper(key)]; present {
				t.Fatalf("credential reached child environment: %q", key)
			}
		}
	}
}
