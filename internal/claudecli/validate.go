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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

// Validate checks Claude's prerequisites and resolves model settings.
//
// It requires Linux, Claude Code 2.1.302 or later, authenticated CLI state,
// bubblewrap, and socat. Model discovery uses the CLI's initialization protocol
// without an inference turn. These checks do not execute a sandboxed command;
// deployments must validate runtime isolation.
//
// APIKey overrides environment credentials and stored login. Without it,
// environment API/OAuth tokens are rejected. Authenticated proxies are rejected.
func Validate(ctx context.Context, settings config.ClaudeConfig) (validated config.ClaudeConfig, resultErr error) {
	defer func() { resultErr = aicli.RedactError(resultErr, settings.APIKey) }()
	validated = settings
	if runtime.GOOS != "linux" {
		return validated, errors.New("claude isolation currently requires Linux")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "minion-claude-check-")
	if err != nil {
		return validated, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	authDir, err := authentication(filepath.Dir(dir), settings.APIKey)
	if err != nil {
		return validated, err
	}
	if authDir != "" {
		defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(authDir)) }()
	}
	binary, err := executable("claude", "")
	if err != nil {
		return validated, err
	}
	env, err := childEnvironment(os.Environ(), dir, dir, authDir)
	if err != nil {
		return validated, err
	}
	version, err := probe(ctx, binary, []string{"--version"}, dir, env, "")
	if err != nil {
		return validated, err
	}
	if !supportedVersion(string(version)) {
		return validated, errors.New("claude isolation requires Claude Code 2.1.302 or later")
	}
	for _, name := range []string{"bwrap", "socat"} {
		if _, err := executable(name, ""); err != nil {
			return validated, fmt.Errorf("claude sandbox dependency %s: %w", name, err)
		}
	}
	policy, err := sandboxSettings(dir, dir, false, "", settings, nil, authDir)
	if err != nil {
		return validated, err
	}
	authArgs := []string{"--safe-mode", "--setting-sources", ""}
	if authDir != "" {
		authArgs = append(authArgs, "--settings", policy)
	}
	auth, err := probe(ctx, binary, append(authArgs, "auth", "status", "--json"), dir, env, "")
	if err != nil {
		return validated, fmt.Errorf("claude authentication: %w", err)
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if json.Unmarshal(auth, &status) != nil || !status.LoggedIn {
		return validated, errors.New("claude is not authenticated; configure claude.api_key or run claude auth login")
	}
	args := append(arguments(policy), "--input-format", "stream-json")
	data, err := probe(ctx, binary, args, dir, env, "{\"type\":\"control_request\",\"request_id\":\"minion-initialize\",\"request\":{\"subtype\":\"initialize\"}}\n")
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
		if validated.Effort == "" {
			return validated, nil
		}
		for _, effort := range model.Efforts {
			if validated.Effort == effort {
				return validated, nil
			}
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

func supportedVersion(version string) bool {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) != 3 {
		return false
	}
	minimum := []int{2, 1, 302}
	numbers := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		numbers[i] = n
	}
	for i, n := range numbers {
		if n != minimum[i] {
			return n > minimum[i]
		}
	}
	return true
}

type probeOutput struct {
	data   bytes.Buffer
	cancel context.CancelFunc
}

func (b *probeOutput) Write(p []byte) (int, error) {
	if len(p) > maxEventBytes-b.data.Len() {
		b.cancel()
		return 0, aicli.ErrOutputLimit
	}
	return b.data.Write(p)
}

func probe(ctx context.Context, binary string, args []string, dir string, env []string, input string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// #nosec G204 -- Callers pass trusted installed executables and fixed argument shapes without a shell.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(input)
	cmd.WaitDelay = 2 * time.Second
	configureCancellation(cmd)
	output := probeOutput{cancel: cancel}
	var diagnostics tailBuffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	return output.data.Bytes(), nil
}
