package action

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

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
	if err := selection.Credentials.validate(); err != nil {
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
	preflightCtx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || !workspace.ValidIdentitySegment(executorID) || client == nil {
		return errors.New("invalid remote preflight endpoint")
	}
	request, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: executorID}, httpControlBytes)
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
	if err := validateRemoteChoices(selection); err != nil {
		return err
	}
	runner := Runner{
		Codex: config.CodexConfig{APIKey: selection.Credentials.CodexAPIKey, NativeAPIKey: selection.Credentials.CodexNativeAPIKey,
			NativeAccessToken: selection.Credentials.CodexAccessToken},
		Claude: config.ClaudeConfig{APIKey: selection.Credentials.ClaudeAPIKey, AllowAllUnixSockets: allowAllUnixSockets},
	}
	if message, err := checkOfferedRolesCLI(preflightCtx, &runner, offeredPreflightRoles(selection.Main, selection.MainChoices, selection.Critic, selection.CriticChoices)); err != nil {
		return preflightError{message: message, cause: redactActionError(err, runner.secretValues()...)}
	}
	return preflightCtx.Err()
}
