package action

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/claudecli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func validateRemoteChoices(selection remoteChoicePreflight) error {
	if selection.Main.Provider == "" {
		return errors.New("remote preflight has no selected main role")
	}
	if len(selection.MainChoices) > config.MaxUserModelChoicesPerRole || len(selection.CriticChoices) > config.MaxUserModelChoicesPerRole {
		return errors.New("remote preflight has too many choices")
	}
	for _, selected := range []struct {
		name    string
		role    *RoleSettings
		choices []config.RoleModelConfig
	}{
		{name: "main", role: &selection.Main, choices: selection.MainChoices},
		{name: "critic", role: selection.Critic, choices: selection.CriticChoices},
	} {
		if selected.role == nil {
			if len(selected.choices) != 0 {
				return errors.New("remote critic choices require a critic")
			}
			continue
		}
		if err := selected.role.Validate(); err != nil {
			return fmt.Errorf("remote %s role: %w", selected.name, err)
		}
		if !(config.RoleModelConfig{Model: selected.role.Model, Effort: selected.role.Effort}).ValidChoice() {
			return fmt.Errorf("remote %s role requires an explicit pair", selected.name)
		}
		for _, choice := range selected.choices {
			if !choice.ValidChoice() {
				return fmt.Errorf("remote %s choice requires an explicit pair", selected.name)
			}
		}
		if err := validateRoleChoices(selected.name, *selected.role, selected.choices); err != nil {
			return err
		}
	}
	if err := selection.Credentials.Validate(); err != nil {
		return err
	}
	selected := func(provider string) bool {
		return selection.Main.Provider == provider || selection.Critic != nil && selection.Critic.Provider == provider
	}
	if !selected("codex") && (selection.Credentials.CodexAPIKey != "" || selection.Credentials.CodexNativeAPIKey != "" || selection.Credentials.CodexAccessToken != "") ||
		!selected("claude") && selection.Credentials.ClaudeAPIKey != "" {
		return errors.New("remote preflight contains unselected provider credentials")
	}
	return nil
}

// Leave time for the worker to open its first pull before the orchestrator's two-minute wait expires.
const remoteChoicePreflightTimeout = 90 * time.Second

func auditSupportedOnHost(ctx context.Context, timeout time.Duration) bool {
	if !(config.CommandMapping{Action: string(command.SecurityAudit)}).SupportedOnHost() {
		return false
	}
	probeCtx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	binary, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		return false
	}
	dir, err := os.MkdirTemp("", ".minion-audit-preflight-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	_, err = auditNodePermissionFlag(probeCtx, dir, binary)
	return err == nil
}

// PreflightRemoteChoices checks configured role pairs before a remote worker opens its first pull.
func PreflightRemoteChoices(ctx context.Context, client *http.Client, origin, executorID string, allowAllUnixSockets bool) error {
	err := preflightRemoteChoices(ctx, client, origin, executorID, allowAllUnixSockets, remoteChoicePreflightTimeout)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return preflightError{message: "remote selection preflight timed out", cause: err}
	}
	if _, safe := err.(preflightError); safe {
		return err
	}
	return preflightError{message: "remote selection preflight unavailable", cause: err}
}

func preflightRemoteChoices(ctx context.Context, client *http.Client, origin, executorID string, allowAllUnixSockets bool, timeout time.Duration) error {
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || !workspace.ValidIdentitySegment(executorID) || client == nil {
		return errors.New("invalid remote preflight endpoint")
	}
	preflightCtx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	request, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: executorID,
		AuditHostSupported: auditSupportedOnHost(preflightCtx, httpControlTimeout)}, httpControlBytes)
	if err != nil {
		return err
	}
	transport := httpExecutionClient{client: client, base: u}
	var selection remoteChoicePreflight
	fetchCtx, stopFetch := context.WithTimeout(preflightCtx, httpControlTimeout)
	err = transport.post(fetchCtx, "preflight", request, httpControlBytes, &selection, httpBinding{}, 0)
	stopFetch()
	if err != nil {
		return err
	}
	if selection.Version != ProtocolVersion || selection.ExecutorID != executorID {
		return errors.New("remote preflight version or executor differs")
	}
	return validateRemoteSelection(preflightCtx, selection, allowAllUnixSockets)
}

func validateRemoteSelection(ctx context.Context, selection remoteChoicePreflight, allowAllUnixSockets bool) error {
	if err := validateRemoteChoices(selection); err != nil {
		return err
	}
	runner := Runner{
		Codex: config.CodexConfig{APIKey: selection.Credentials.CodexAPIKey, NativeAPIKey: selection.Credentials.CodexNativeAPIKey,
			NativeAccessToken: selection.Credentials.CodexAccessToken},
		Claude: config.ClaudeConfig{APIKey: selection.Credentials.ClaudeAPIKey, AllowAllUnixSockets: allowAllUnixSockets},
	}
	roles := offeredPreflightRoles(selection.Main, selection.MainChoices, selection.Critic, selection.CriticChoices)
	for _, role := range roles {
		if role.Provider == "claude" {
			if _, err := claudecli.CheckHost(ctx, allowAllUnixSockets); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The short host probe deadline must not look like the whole preflight deadline.
				return preflightError{message: "remote Claude host check failed: " + redactActionText(err.Error(), runner.secretValues()...)}
			}
			break
		}
	}
	if message, err := checkOfferedRolesCLI(ctx, &runner, roles); err != nil {
		return preflightError{message: message, cause: redactActionError(err, runner.secretValues()...)}
	}
	return ctx.Err()
}
