package action

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/linkdata/minion/internal/config"
)

const workerStartupBytes = 8 << 20

// WorkerStartup is trusted parent input to one selected local worker.
// GitHub credentials stay with the parent.
type WorkerStartup struct {
	Selection    WorkerSelection `json:"selection"`
	WorkRoot     string          `json:"work_root"`
	SocketPath   string          `json:"socket_path"`
	CodexAPIKey  string          `json:"codex_api_key,omitempty"`
	ClaudeAPIKey string          `json:"claude_api_key,omitempty"`
}

func (s WorkerStartup) validate() error {
	if s.WorkRoot == "" || s.SocketPath == "" || !filepath.IsAbs(s.WorkRoot) || !filepath.IsAbs(s.SocketPath) ||
		filepath.Clean(s.WorkRoot) != s.WorkRoot || filepath.Clean(s.SocketPath) != s.SocketPath {
		return errors.New("worker startup requires clean absolute work root and socket paths")
	}
	if _, err := s.runner(nil); err != nil {
		return fmt.Errorf("worker startup selection: %w", err)
	}
	selected := func(provider string) bool {
		return s.Selection.Main.Provider == provider || s.Selection.Critic != nil && s.Selection.Critic.Provider == provider
	}
	if s.CodexAPIKey != "" && !selected("codex") || s.ClaudeAPIKey != "" && !selected("claude") {
		return errors.New("worker startup contains a key for an unselected provider")
	}
	return nil
}

func (s WorkerStartup) runner(call modelCall) (*Runner, error) {
	return NewSelectedRunner(Runner{
		WorkRoot: s.WorkRoot,
		Codex:    config.CodexConfig{APIKey: s.CodexAPIKey},
		Claude:   config.ClaudeConfig{APIKey: s.ClaudeAPIKey},
		runModel: call,
	}, s.Selection)
}

// EncodeWorkerStartup prepares bounded input for the direct child's stdin.
func EncodeWorkerStartup(startup WorkerStartup) ([]byte, error) {
	if err := startup.validate(); err != nil {
		return nil, err
	}
	return httpEncode(startup, workerStartupBytes)
}

func decodeWorkerStartup(input io.Reader) (WorkerStartup, error) {
	if input == nil {
		return WorkerStartup{}, errors.New("worker startup input is missing")
	}
	var startup WorkerStartup
	if err := httpDecode(input, workerStartupBytes, &startup); err != nil {
		return WorkerStartup{}, fmt.Errorf("decode worker startup: %w", err)
	}
	if err := startup.validate(); err != nil {
		return WorkerStartup{}, err
	}
	return startup, nil
}
