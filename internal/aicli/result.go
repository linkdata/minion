// Package aicli shares AI invocation results, bounded output and trusted host-command helpers.
package aicli

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/usage"
)

// ErrOutputLimit means a run exceeded its bounded output allowance.
var ErrOutputLimit = errors.New("AI output limit exceeded")

// Result contains the answer and accounted token usage from one invocation.
type Result struct {
	Answer        string
	Tokens        usage.Tokens
	UsageComplete bool
	Diagnostics   []ProviderDiagnostic
}

// ProviderDiagnostic records a private native observation, not a completion verdict.
type ProviderDiagnostic struct {
	Code    string
	Message string
}

// AppendNativeDiagnostic retains recent native observations and marks lost ones.
// Two slots remain available for the final stderr tail and observed structured output.
func AppendNativeDiagnostic(diagnostics []ProviderDiagnostic, code, message string) []ProviderDiagnostic {
	const nativeLimit = 61
	entry := ProviderDiagnostic{Code: code, Message: DiagnosticText(message)}
	if len(diagnostics) < nativeLimit {
		return append(diagnostics, entry)
	}
	if len(diagnostics) == nativeLimit {
		diagnostics = append([]ProviderDiagnostic{{Code: "diagnostics_truncated"}}, diagnostics[1:]...)
		return append(diagnostics, entry)
	}
	copy(diagnostics[1:], diagnostics[2:])
	diagnostics[len(diagnostics)-1] = entry
	return diagnostics
}

// DiagnosticText bounds a redacted observation and keeps it valid UTF-8.
func DiagnosticText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= bounded.DiagnosticBytes {
		return text
	}
	cut := bounded.DiagnosticBytes - len("...")
	for !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// StreamEvent is one provider event or a metadata-only content_missing marker.
// NativeID is a root session or helper agent ID. Both an empty ID and
// content_missing prevent coverage proof.
type StreamEvent struct {
	NativeID string
	Kind     string
	Text     string
}

// PreparedInput holds selected direct CLI input fields Minion constructed before Start.
// It omits environment variables and does not prove the CLI consumed the input.
type PreparedInput struct {
	NativeID string
	Mode     string
	Path     string
	Args     []string
	Dir      string
	Stdin    string
	Schema   string
}
