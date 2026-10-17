package codexcli

import "github.com/linkdata/minion/internal/aicli"

// ExecutablePath resolves the exact host file granted to sandboxed commands.
func ExecutablePath() (string, error) { return aicli.ExecutablePath("codex") }
