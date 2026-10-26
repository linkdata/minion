package main

import (
	"os"
	"strings"

	"github.com/linkdata/minion/internal/action"
)

// selectedWorkerEnv filters known GitHub tokens and unused AI auth variables.
func selectedWorkerEnv(startup action.WorkerStartup) []string {
	codexEnv := startup.CodexAPIKey == "" && (startup.Selection.Main.Provider == "codex" ||
		startup.Selection.Critic != nil && startup.Selection.Critic.Provider == "codex")
	parent := os.Environ()
	env := make([]string, 0, len(parent))
	for _, entry := range parent {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN":
			continue
		case "CODEX_API_KEY", "CODEX_ACCESS_TOKEN":
			if !codexEnv {
				continue
			}
		}
		env = append(env, entry)
	}
	return env
}
