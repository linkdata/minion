package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

// Validate checks Claude's prerequisites and resolves model settings.
//
// It requires authenticated CLI state. Model discovery passes Run's sandbox
// and tool options to the CLI's initialization protocol. Help advertises the
// remaining fresh and resumed invocation options. Neither starts inference.
// Claude owns sandbox enforcement.
//
// APIKey overrides environment credentials and stored login. Without it,
// environment API/OAuth tokens are rejected. Authenticated proxies are rejected.
func Validate(ctx context.Context, settings config.ClaudeConfig) (validated config.ClaudeConfig, resultErr error) {
	defer func() { resultErr = aicli.RedactError(resultErr, settings.APIKey) }()
	validated = settings
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
	policy, err := sandboxSettings(dir, "", authDir)
	if err != nil {
		return validated, err
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
		if validated.Effort != "" && !slices.Contains(model.Efforts, validated.Effort) {
			return validated, fmt.Errorf("claude effort %q is unsupported for model %q", validated.Effort, validated.Model)
		}
		if err := validateInvocationCapabilities(ctx, binary, dir, env, validated); err != nil {
			return validated, err
		}
		return validated, nil
	}
	return validated, fmt.Errorf("claude model %q is absent from the installed CLI catalogue", settings.Model)
}

// Initialization checks common Run flags; standalone help lists the rest.
func validateInvocationCapabilities(ctx context.Context, binary, dir string, env []string, settings config.ClaudeConfig) error {
	help, err := aicli.Probe(ctx, binary, "", []string{"--help"}, dir, env, "", settings.APIKey)
	if err != nil {
		return fmt.Errorf("claude invocation capability check: %w", err)
	}
	required := []string{"--session-id", "--resume", "--add-dir", "--model"}
	if settings.Effort != "" {
		required = append(required, "--effort")
	}
	for _, option := range required {
		if !helpOptionDeclared(help, option) {
			return fmt.Errorf("claude invocation capability check: %s is unavailable", option)
		}
	}
	return nil
}

func helpOptionDeclared(help []byte, option string) bool {
	for _, line := range bytes.Split(help, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) > 0 && fields[0] == option ||
			len(fields) > 1 && strings.HasSuffix(fields[0], ",") && fields[1] == option {
			return true
		}
	}
	return false
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
