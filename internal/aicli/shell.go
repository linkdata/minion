package aicli

import "strings"

// ShellQuote quotes one literal argument for a POSIX shell.
func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
