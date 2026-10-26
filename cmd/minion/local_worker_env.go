package main

import (
	"os"
	"strings"
)

// selectedWorkerEnv filters known GitHub tokens and ambient AI auth variables.
func selectedWorkerEnv() []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent))
	for _, entry := range parent {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN":
			continue
		}
		env = append(env, entry)
	}
	return env
}
