package codexcli

import (
	"bufio"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/executil"
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
	settings = captureAmbientNativeCredentials(settings)
	defer func() { resultErr = redactCodexError(resultErr, settings) }()
	home, err := HomePath()
	if err != nil {
		return settings, err
	}
	if err := rejectGlobalInstructions(home); err != nil {
		return settings, err
	}
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
			if err := validateExecCapabilities(ctx, settings); err != nil {
				return settings, fmt.Errorf("cannot validate Codex settings: %w", err)
			}
			return settings, nil
		}
	}
	return settings, fmt.Errorf("codex.reasoning_effort %q is not supported for model %q; supported efforts: %s", effort, selected.Model, strings.Join(efforts, ", "))
}

// Check selected config and both exec parsers without starting an inference turn.
func validateExecCapabilities(ctx context.Context, settings config.CodexConfig) (err error) {
	binary, err := aicli.ExecutablePath("codex")
	if err != nil {
		return err
	}
	home, err := HomePath()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "minion-codex-exec-check-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	schema := filepath.Join(dir, "output-schema.json")
	env := childEnvironment(os.Environ(), dir, dir, home, true)
	probe := func(args []string) ([]byte, error) {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return aicli.Probe(probeCtx, binary, "codex", args, dir, env, "", settings.APIKey)
	}
	// exec --help parses flags but does not load -c settings. App-server
	// startup checks that the resolved model/effort and both sandbox modes parse
	// under strict config with no request. The catalogue checks model support.
	if settings.ReasoningEffort != "" {
		for _, writable := range []bool{false, true} {
			args := append(appServerArgs(dir, writable), "-c", "model="+strconv.Quote(settings.Model), "-c", "model_reasoning_effort="+strconv.Quote(settings.ReasoningEffort))
			if _, probeErr := probe(args); probeErr != nil {
				return fmt.Errorf("Codex selected effort config capability check (writable=%t): %w", writable, probeErr) //nolint:staticcheck // -- Codex is a proper noun.
			}
			const unknownKey = "minion_unsupported_capability_probe"
			invalid := append(append([]string(nil), args...), "-c", unknownKey+"=true")
			if _, probeErr := probe(invalid); !capabilityRejected(probeErr, unknownKey) {
				return fmt.Errorf("Codex selected effort config capability check (writable=%t) did not reject an unknown key: %v", writable, probeErr) //nolint:staticcheck // -- Codex is a proper noun.
			}
		}
	}
	for _, command := range []struct {
		name string
		args []string
	}{
		{name: "exec", args: []string{"exec"}},
		{name: "exec resume", args: []string{"exec", "resume"}},
	} {
		var fullArgs []string
		for _, writable := range []bool{false, true} {
			args := append(append([]string(nil), command.args...),
				"--json", "--ignore-user-config", "--ignore-rules", "--strict-config", "--skip-git-repo-check",
			)
			args = append(args, sandboxArgs(dir, "", writable)...)
			args = append(args, disabledFeatureArgs()...)
			args = append(args, execConfigArgs()...)
			args = append(args, "--model", settings.Model)
			if settings.ReasoningEffort != "" {
				args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(settings.ReasoningEffort))
			}
			args = append(args, "--output-schema", schema, "--help")
			_, probeErr := probe(args)
			if probeErr != nil {
				return fmt.Errorf("Codex %s capability check (writable=%t): %w", command.name, writable, probeErr) //nolint:staticcheck // -- Codex is a proper noun.
			}
			fullArgs = args
		}
		const unknownOption = "--minion-unsupported-capability-probe"
		invalid := append(append([]string(nil), fullArgs[:len(fullArgs)-1]...), unknownOption, "--help")
		_, probeErr := probe(invalid)
		if !capabilityRejected(probeErr, unknownOption) {
			return fmt.Errorf("Codex %s capability check did not reject an unknown option: %v", command.name, probeErr) //nolint:staticcheck // -- Codex is a proper noun.
		}
	}
	return nil
}

func capabilityRejected(err error, sentinel string) bool {
	var exitErr *exec.ExitError
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, aicli.ErrOutputLimit) && errors.As(err, &exitErr) && exitErr.ExitCode() > 0 && strings.Contains(err.Error(), sentinel)
}

func disabledFeatureArgs() []string {
	var args []string
	for _, feature := range []string{
		"apps", "browser_use", "browser_use_external", "browser_use_full_cdp_access",
		"computer_use", "image_generation", "plugins", "remote_plugin",
		"shell_snapshot", "skill_mcp_dependency_install", "memories", "hooks", "skill_search",
	} {
		args = append(args, "--disable", feature)
	}
	return args
}

func appServerArgs(dir string, writable bool) []string {
	args := append([]string{"app-server", "--strict-config", "--listen", "stdio://", "-c", `model_provider="openai"`}, disabledFeatureArgs()...)
	args = append(args, execConfigArgs()...)
	return append(args, sandboxArgs(dir, "", writable)...)
}

func listModels(ctx context.Context, settings config.CodexConfig) (models []modelInfo, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	binary, err := aicli.ExecutablePath("codex")
	if err != nil {
		return nil, err
	}
	home, err := HomePath()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "minion-codex-check-")
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir))
	}()
	// app-server has no --ignore-user-config flag. Select exec's built-in
	// provider explicitly and avoid repository config by using a temporary cwd.
	// Startup validates writable sandbox config without starting an inference turn.
	args := appServerArgs(dir, true)
	if settings.APIKey != "" {
		args = append(args, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`)
	}
	if settings.Model != "" {
		args = append(args, "-c", "model="+strconv.Quote(settings.Model))
	}
	if settings.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(settings.ReasoningEffort))
	}
	// #nosec G204 -- The canonical installed executable receives fixed arguments without a shell.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Args[0] = "codex"
	cmd.Dir = dir
	cmd.Env = nativeCredentialEnvironment(os.Environ(), dir, dir, home, settings)
	diagnostics := bounded.Tail{}
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
	executil.Prepare(cmd)
	var directStopSent atomic.Bool
	cancelCommand := cmd.Cancel
	cmd.Cancel = func() error {
		err := cancelCommand()
		if err == nil {
			directStopSent.Store(true)
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stopReader := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer func() {
		stopReader()
		contextErr := ctx.Err()
		cancel()
		if waitErr := cmd.Wait(); !expectedDiscoveryStop(waitErr, directStopSent.Load()) {
			err = errors.Join(err, waitErr)
		}
		if contextErr != nil && !errors.Is(err, contextErr) {
			err = errors.Join(contextErr, err)
		}
		if err != nil {
			if contextErr != nil {
				err = fmt.Errorf("Codex model discovery: %w", err) //nolint:staticcheck // -- Codex is a proper noun.
			} else if detail := redactCodexTail(&diagnostics, settings); detail != "" {
				err = fmt.Errorf("%w: %q", err, detail)
			}
		}
	}()
	encoder := json.NewEncoder(stdin)
	// Catalogue limits count the logical line after ScanLines removes CR/LF.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), aicli.MaxEventBytes+2)
	call := func(id int, method string, params any, result any) error {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for scanner.Scan() {
			if len(scanner.Bytes()) > aicli.MaxEventBytes {
				return fmt.Errorf("%w: Codex catalogue event exceeded the size limit", aicli.ErrOutputLimit)
			}
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
				return fmt.Errorf("Codex %s: %q", method, bounded.Text(redactCodexText(response.Error.Message, settings))) //nolint:staticcheck // -- Codex is a proper noun.
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
			if errors.Is(err, bufio.ErrTooLong) {
				return fmt.Errorf("%w: Codex catalogue event exceeded the size limit", aicli.ErrOutputLimit)
			}
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

// The app-server is deliberately canceled after its last catalogue response.
// Keep unexpected exits and pipe-drain failures reported by Cmd.Wait even when cancellation is expected.
func expectedDiscoveryStop(err error, directStopSent bool) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if !expectedDiscoveryStop(cause, directStopSent) {
				return false
			}
		}
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	if !directStopSent {
		return false
	}
	if runtime.GOOS == "windows" {
		return exit.ExitCode() == 1 // os.Process.Kill uses TerminateProcess with exit code 1.
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && (status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGKILL)
}
