// Package aicli shares AI invocation results, bounded output and trusted host-command helpers.
package aicli

import (
	"errors"

	"github.com/linkdata/minion/internal/usage"
)

// ErrOutputLimit means a run exceeded its bounded output allowance.
var ErrOutputLimit = errors.New("AI output limit exceeded")

// Result contains the answer and accounted token usage from one invocation.
type Result struct {
	Answer        string
	Tokens        usage.Tokens
	UsageComplete bool
}

// StreamEvent is one fully emitted provider event. NativeID is a root session
// or helper agent ID. An empty ID marks a gap that prevents coverage proof.
type StreamEvent struct {
	NativeID string
	Kind     string
	Text     string
}

// PreparedInput is the exact direct invocation Minion constructed before Start.
// It does not prove the CLI consumed the input or started successfully.
type PreparedInput struct {
	NativeID string
	Mode     string
	Path     string
	Args     []string
	Dir      string
	Stdin    string
	Schema   string
}
