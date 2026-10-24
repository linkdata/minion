package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

// Validate checks Claude's prerequisites and resolves model settings.
//
// It requires Linux or macOS, the CLI options used by Run, authenticated CLI
// state, and bubblewrap/socat on Linux. Model discovery uses the CLI's initialization
// protocol without an inference turn. These checks do not execute a sandboxed command;
// deployments must validate runtime isolation.
//
// APIKey overrides environment credentials and stored login. Without it,
// environment API/OAuth tokens are rejected. Authenticated proxies are rejected.
func Validate(ctx context.Context, settings config.ClaudeConfig) (validated config.ClaudeConfig, resultErr error) {
	defer func() { resultErr = aicli.RedactError(resultErr, settings.APIKey) }()
	validated = settings
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return validated, errors.New("claude isolation requires Linux or macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "minion-claude-check-")
	if err != nil {
		return validated, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, os.RemoveAll(dir))
	}()
	authDir, err := authentication(filepath.Dir(dir), settings.APIKey)
	if authDir != "" {
		defer func() {
			resultErr = errors.Join(resultErr, os.RemoveAll(authDir))
		}()
	}
	if err != nil {
		return validated, err
	}
	binary, err := aicli.ExecutablePath("claude")
	if err != nil {
		return validated, err
	}
	configDir, _, err := configDirectory(authDir)
	if err != nil {
		return validated, err
	}
	env, err := childEnvironment(os.Environ(), dir, dir, authDir, configDir)
	if err != nil {
		return validated, err
	}
	settings.ReadOnlyDirs = nil
	policy, err := sandboxSettings(dir, dir, false, "", settings, nil, authDir)
	if err != nil {
		return validated, err
	}
	help, err := aicli.Probe(ctx, binary, "", []string{"--help"}, dir, env, "", settings.APIKey)
	if err != nil {
		return validated, fmt.Errorf("claude capabilities: %w", err)
	}
	for _, option := range append([]string{"--session-id", "--resume", "--add-dir", "--model", "--effort", "--json-schema", "--no-session-persistence", "--input-format"}, arguments(policy)...) {
		if strings.HasPrefix(option, "-") {
			option = strings.TrimSuffix(option, "=")
			if !helpSupportsOption(help, option) {
				return validated, fmt.Errorf("claude help does not list required option %s", option)
			}
		}
	}
	if runtime.GOOS == "linux" {
		for _, name := range []string{"bwrap", "socat"} {
			if _, err := aicli.ExecutablePath(name); err != nil {
				return validated, fmt.Errorf("claude sandbox dependency %s: %w", name, err)
			}
		}
	}
	authArgs := []string{"--safe-mode", "--setting-sources="}
	if authDir != "" {
		authArgs = append(authArgs, "--settings", policy)
	}
	auth, err := aicli.Probe(ctx, binary, "", append(authArgs, "auth", "status", "--json"), dir, env, "", settings.APIKey)
	if err != nil {
		return validated, fmt.Errorf("claude authentication: %w", err)
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if json.Unmarshal(auth, &status) != nil || !status.LoggedIn {
		return validated, errors.New("claude is not authenticated; configure claude.api_key or run claude auth login")
	}
	args := append(arguments(policy), "--no-session-persistence", "--input-format", "stream-json", "--json-schema", `{"type":"object"}`)
	data, err := aicli.Probe(ctx, binary, "", args, dir, env, "{\"type\":\"control_request\",\"request_id\":\"minion-initialize\",\"request\":{\"subtype\":\"initialize\"}}\n", settings.APIKey)
	if err != nil {
		return validated, fmt.Errorf("claude model discovery: %w", err)
	}
	models, err := initializationModels(data)
	if err != nil {
		return validated, err
	}
	for _, model := range models {
		if settings.Model == "" {
			if model.Value != "default" {
				continue
			}
		} else if settings.Model != model.Value && settings.Model != model.ResolvedModel {
			continue
		}
		if model.ResolvedModel == "" {
			return validated, errors.New("claude model catalogue omitted the resolved model")
		}
		validated.Model = model.ResolvedModel
		if validated.Effort == "" || slices.Contains(model.Efforts, validated.Effort) {
			return validated, nil
		}
		return validated, fmt.Errorf("claude effort %q is unsupported for model %q", validated.Effort, validated.Model)
	}
	return validated, fmt.Errorf("claude model %q is absent from the installed CLI catalogue", settings.Model)
}

type modelInfo struct {
	Value         string   `json:"value"`
	ResolvedModel string   `json:"resolvedModel"`
	Efforts       []string `json:"supportedEffortLevels"`
}

func initializationModels(data []byte) ([]modelInfo, error) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var message struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					Models []modelInfo `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal(line, &message); err != nil {
			return nil, err
		}
		if message.Type == "control_response" && message.Response.RequestID == "minion-initialize" {
			if message.Response.Subtype != "success" || len(message.Response.Response.Models) == 0 {
				return nil, errors.New("claude initialization did not return a model catalogue")
			}
			return message.Response.Response.Models, nil
		}
	}
	return nil, errors.New("claude did not acknowledge initialization")
}

func helpSupportsOption(help []byte, option string) bool {
	for _, line := range strings.Split(string(help), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "-") {
				break
			}
			field, _, _ = strings.Cut(strings.TrimSuffix(field, ","), "=")
			if field == option {
				return true
			}
		}
	}
	return false
}
