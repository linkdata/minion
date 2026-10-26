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
	Selection         WorkerSelection     `json:"selection"`
	WorkRoot          string              `json:"work_root"`
	Endpoint          LocalEndpointAccess `json:"endpoint"`
	CodexAPIKey       string              `json:"codex_api_key,omitempty"`
	CodexNativeAPIKey string              `json:"codex_native_api_key,omitempty"`
	CodexAccessToken  string              `json:"codex_access_token,omitempty"`
	ClaudeAPIKey      string              `json:"claude_api_key,omitempty"`
}

func (s WorkerStartup) validate() error {
	if s.WorkRoot == "" || !filepath.IsAbs(s.WorkRoot) {
		return errors.New("worker startup requires an absolute work root")
	}
	if err := s.Endpoint.validate(); err != nil {
		return fmt.Errorf("worker startup endpoint: %w", err)
	}
	if _, err := s.runner(nil); err != nil {
		return fmt.Errorf("worker startup selection: %w", err)
	}
	credentials := WorkerCredentials{CodexAPIKey: s.CodexAPIKey, CodexNativeAPIKey: s.CodexNativeAPIKey, CodexAccessToken: s.CodexAccessToken, ClaudeAPIKey: s.ClaudeAPIKey}
	if err := credentials.validate(); err != nil {
		return err
	}
	selected := func(provider string) bool {
		return s.Selection.Main.Provider == provider || s.Selection.Critic != nil && s.Selection.Critic.Provider == provider
	}
	if (s.CodexAPIKey != "" || s.CodexNativeAPIKey != "" || s.CodexAccessToken != "") && !selected("codex") || s.ClaudeAPIKey != "" && !selected("claude") {
		return errors.New("worker startup contains a key for an unselected provider")
	}
	return nil
}

func (s WorkerStartup) runner(call modelCall) (*Runner, error) {
	return NewSelectedRunner(Runner{
		WorkRoot: s.WorkRoot,
		Codex:    config.CodexConfig{APIKey: s.CodexAPIKey, NativeAPIKey: s.CodexNativeAPIKey, NativeAccessToken: s.CodexAccessToken},
		Claude:   config.ClaudeConfig{APIKey: s.ClaudeAPIKey},
		runModel: call,
	}, s.Selection)
}

// EncodeWorkerStartup prepares bounded input for the direct child's stdin.
func EncodeWorkerStartup(startup WorkerStartup) ([]byte, error) {
	remaining := workerStartupBytes
	add := func(value string) bool {
		if len(value) > remaining {
			return false
		}
		remaining -= len(value)
		return true
	}
	addRole := func(role RoleSettings) bool {
		if !add(role.Provider) || !add(role.Model) || !add(role.Effort) {
			return false
		}
		for _, skill := range role.Skills {
			if !add(skill) {
				return false
			}
		}
		for _, skill := range role.AlwaysOnSkills {
			if !add(skill) {
				return false
			}
		}
		return true
	}
	if !add(startup.WorkRoot) || !add(startup.Endpoint.BaseURL) || !add(string(startup.Endpoint.Certificate)) || !add(startup.Endpoint.Token) || !add(startup.CodexAPIKey) || !add(startup.CodexNativeAPIKey) || !add(startup.CodexAccessToken) || !add(startup.ClaudeAPIKey) || !addRole(startup.Selection.Main) {
		return nil, errHTTPBodyLimit
	}
	if startup.Selection.Critic != nil && !addRole(*startup.Selection.Critic) {
		return nil, errHTTPBodyLimit
	}
	for name, mapping := range startup.Selection.Commands {
		if !add(name) || !add(mapping.Action) {
			return nil, errHTTPBodyLimit
		}
		for _, skill := range mapping.Skills {
			if !add(skill) {
				return nil, errHTTPBodyLimit
			}
		}
	}
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
