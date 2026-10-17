package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/processgroup"
)

type modelInfo struct {
	Model                     string `json:"model"`
	IsDefault                 bool   `json:"isDefault"`
	DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

// Validate resolves model and reasoning settings using the installed Codex catalogue.
//
// Omitted settings are resolved and pinned for this process. Discovery does not
// start an inference turn. Temporary-directory cleanup failures return an error.
// A nonempty APIKey in settings selects API authentication for discovery without
// changing the stored CLI login.
func Validate(ctx context.Context, settings config.CodexConfig) (resolved config.CodexConfig, resultErr error) {
	defer func() { resultErr = aicli.RedactError(resultErr, settings.APIKey) }()
	models, err := listModels(ctx, settings)
	if err != nil {
		return settings, fmt.Errorf("cannot validate Codex settings: %w", err)
	}
	var selected *modelInfo
	var names []string
	for i := range models {
		model := &models[i]
		if model.Model == "" {
			return settings, errors.New("Codex model catalogue contains an unnamed model") //nolint:staticcheck // -- Codex is a proper noun.
		}
		names = append(names, model.Model)
		if model.Model == settings.Model || (settings.Model == "" && model.IsDefault) {
			selected = model
		}
	}
	if selected == nil {
		if settings.Model == "" {
			return settings, errors.New("Codex model catalogue did not identify a default model") //nolint:staticcheck // -- Codex is a proper noun.
		}
		return settings, fmt.Errorf("codex.model %q is not recognized by installed Codex; supported models: %s", settings.Model, strings.Join(names, ", "))
	}
	effort := settings.ReasoningEffort
	if effort == "" {
		effort = selected.DefaultReasoningEffort
	}
	var efforts []string
	for _, supported := range selected.SupportedReasoningEfforts {
		efforts = append(efforts, supported.ReasoningEffort)
		if effort != "" && supported.ReasoningEffort == effort {
			settings.Model = selected.Model
			settings.ReasoningEffort = effort
			return settings, nil
		}
	}
	return settings, fmt.Errorf("codex.reasoning_effort %q is not supported for model %q; supported efforts: %s", effort, selected.Model, strings.Join(efforts, ", "))
}

func listModels(ctx context.Context, settings config.CodexConfig) (models []modelInfo, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "minion-codex-check-")
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir))
	}()
	// app-server has no --ignore-user-config flag. Select exec's built-in
	// provider explicitly and avoid repository config by using a temporary cwd.
	args := []string{"app-server", "--strict-config", "--listen", "stdio://", "-c", `model_provider="openai"`}
	if settings.APIKey != "" {
		args = append(args, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`)
	}
	if settings.Model != "" {
		args = append(args, "-c", "model="+strconv.Quote(settings.Model))
	}
	if settings.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(settings.ReasoningEffort))
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = dir
	cmd.Env = childEnvironment(os.Environ(), dir, dir, settings.APIKey != "")
	cmd.WaitDelay = 2 * time.Second
	processgroup.Configure(cmd)
	diagnostics := bounded.Tail{Limit: maxDiagnosticBytes}
	cmd.Stderr = &diagnostics
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		// Wait also closes the pipe after a started process exits.
		if closeErr := stdin.Close(); !errors.Is(closeErr, os.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		contextErr := ctx.Err()
		cancel()
		_ = cmd.Wait()
		if err != nil {
			if contextErr != nil {
				err = fmt.Errorf("Codex model discovery: %w", contextErr) //nolint:staticcheck // -- Codex is a proper noun.
			} else if detail := strings.TrimSpace(diagnostics.String()); detail != "" {
				err = fmt.Errorf("%w: %s", err, detail)
			}
		}
	}()
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	call := func(id int, method string, params any, result any) error {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for scanner.Scan() {
			var response struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				return fmt.Errorf("decode Codex %s response: %w", method, err)
			}
			if response.ID == nil {
				continue
			}
			if *response.ID != id {
				return fmt.Errorf("unexpected Codex response ID %d during %s", *response.ID, method)
			}
			if response.Error != nil {
				return fmt.Errorf("Codex %s: %s", method, response.Error.Message) //nolint:staticcheck // -- Codex is a proper noun.
			}
			if len(response.Result) == 0 || string(response.Result) == "null" {
				return fmt.Errorf("Codex %s returned no result", method) //nolint:staticcheck // -- Codex is a proper noun.
			}
			if result != nil {
				return json.Unmarshal(response.Result, result)
			}
			return nil
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("Codex exited before replying to %s", method) //nolint:staticcheck // -- Codex is a proper noun.
	}
	if err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "minion", "version": "0.1.0"}}, nil); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if settings.APIKey != "" {
		var result struct {
			Type string `json:"type"`
		}
		if err := call(0, "account/login/start", map[string]string{"type": "apiKey", "apiKey": settings.APIKey}, &result); err != nil {
			return nil, err
		}
		if result.Type != "apiKey" {
			return nil, errors.New("codex did not select configured API key authentication")
		}
	}
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{"includeHidden": true, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Data       []modelInfo `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := call(page+2, "model/list", params, &result); err != nil {
			return nil, err
		}
		models = append(models, result.Data...)
		if result.NextCursor == "" {
			if len(models) == 0 {
				return nil, errors.New("Codex returned an empty model catalogue") //nolint:staticcheck // -- Codex is a proper noun.
			}
			return models, nil
		}
		if result.NextCursor == cursor {
			return nil, errors.New("Codex model catalogue repeated a page cursor") //nolint:staticcheck // -- Codex is a proper noun.
		}
		cursor = result.NextCursor
	}
	return nil, errors.New("too many Codex model catalogue pages")
}
