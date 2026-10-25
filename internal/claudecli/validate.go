package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

const initializeRequest = "{\"type\":\"control_request\",\"request_id\":\"minion-initialize\",\"request\":{\"subtype\":\"initialize\"}}\n"

// Validate checks Claude's prerequisites and resolves model settings.
//
// It requires authenticated CLI state. Model discovery passes Run's sandbox
// and tool options to the CLI's initialization protocol. Fresh initialization
// and missing-session lookup check Run's invocation shapes without inference;
// help checks the optional effort flag. Claude owns sandbox enforcement.
//
// APIKey overrides environment credentials and stored login. Without it,
// environment API/OAuth tokens are rejected. Authenticated proxies are rejected.
func Validate(ctx context.Context, settings config.ClaudeConfig) (validated config.ClaudeConfig, resultErr error) {
	defer func() { resultErr = aicli.RedactError(resultErr, settings.APIKey) }()
	validated = settings
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
	probe := func(args []string, input string) ([]byte, error) {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return aicli.Probe(probeCtx, binary, "", args, dir, env, input, settings.APIKey)
	}
	authArgs := []string{"--safe-mode", "--setting-sources="}
	if authDir != "" {
		authArgs = append(authArgs, "--settings", policy)
	}
	auth, err := probe(append(authArgs, "auth", "status", "--json"), "")
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
	data, err := probe(args, initializeRequest)
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
		if err := validateInvocationCapabilities(probe, dir, policy, validated); err != nil {
			return validated, err
		}
		return validated, nil
	}
	return validated, fmt.Errorf("claude model %q is absent from the installed CLI catalogue", settings.Model)
}

// Probe the fresh invocation and resume's session lookup without a model turn.
// An initialize-only fresh call does not create a resumable conversation, so
// resume can prove argument parsing and lookup, not session continuation.
func validateInvocationCapabilities(probe func([]string, string) ([]byte, error), dir, policy string, settings config.ClaudeConfig) error {
	if settings.Effort != "" {
		help, err := probe([]string{"--help"}, "")
		if err != nil {
			return fmt.Errorf("claude effort capability check: %w", err)
		}
		if !helpOptionDeclared(help, "--effort") {
			return errors.New("claude effort capability check: --effort is unavailable")
		}
	}
	freshID, err := conversationID()
	if err != nil {
		return err
	}
	args := append(arguments(policy), "--no-session-persistence", "--session-id", freshID, "--add-dir", dir, "--model", settings.Model)
	if settings.Effort != "" {
		args = append(args, "--effort", settings.Effort)
	}
	args = append(args, "--json-schema", `{"type":"object"}`, "--input-format", "stream-json")
	data, err := probe(args, initializeRequest)
	if err != nil {
		return fmt.Errorf("claude fresh invocation capability check: %w", err)
	}
	if _, err := initializationModels(data); err != nil {
		return fmt.Errorf("claude fresh invocation did not acknowledge initialization: %w", err)
	}
	if hasAssistantEvent(data) {
		return errors.New("claude fresh invocation emitted a model turn")
	}
	invalid := append(slices.Clone(args), "--minion-unsupported-capability-probe")
	if _, err := probe(invalid, initializeRequest); !cliRejected(err) || !strings.Contains(err.Error(), "--minion-unsupported-capability-probe") {
		return fmt.Errorf("claude fresh invocation did not reject an unknown option: %v", err)
	}
	resumeID, err := conversationID()
	if err != nil {
		return err
	}
	resume := append(arguments(policy), "--resume", resumeID, "--add-dir", dir, "--model", settings.Model)
	if settings.Effort != "" {
		resume = append(resume, "--effort", settings.Effort)
	}
	resume = append(resume, "--json-schema", `{"type":"object"}`, "--input-format", "stream-json")
	data, err = probe(resume, initializeRequest)
	if !cliRejected(err) || !strings.Contains(err.Error(), resumeID) || !isEmptySessionLookup(data) {
		return fmt.Errorf("claude resume invocation did not reach empty-session lookup: %v", err)
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

func cliRejected(err error) bool {
	var exitErr *exec.ExitError
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, aicli.ErrOutputLimit) && errors.As(err, &exitErr) && exitErr.ExitCode() > 0
}

func hasAssistantEvent(data []byte) bool {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "assistant" {
			return true
		}
	}
	return false
}

func isEmptySessionLookup(data []byte) bool {
	found := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type         string  `json:"type"`
			IsError      bool    `json:"is_error"`
			NumTurns     int     `json:"num_turns"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			Usage        struct {
				InputTokens              int64 `json:"input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type == "assistant" {
			return false
		}
		if event.Type == "result" {
			if found || !event.IsError || event.NumTurns != 0 || event.TotalCostUSD != 0 ||
				event.Usage.InputTokens != 0 || event.Usage.OutputTokens != 0 || event.Usage.CacheCreationInputTokens != 0 || event.Usage.CacheReadInputTokens != 0 {
				return false
			}
			found = true
		}
	}
	return found
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
