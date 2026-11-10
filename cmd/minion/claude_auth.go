package main

import "os"

func claudeRunnerKey(configured string) string {
	if configured != "" {
		return configured
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}
