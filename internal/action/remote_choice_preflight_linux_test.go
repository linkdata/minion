//go:build linux

package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestRemoteRolePreflightBeforePull(t *testing.T) {
	for _, tc := range []struct {
		name          string
		defaultEffort string
		mainChoice    bool
		criticChoice  bool
		supportMedium bool
		wantError     bool
	}{
		{name: "unsupported default", defaultEffort: "medium", wantError: true},
		{name: "unsupported main choice", defaultEffort: "high", mainChoice: true, wantError: true},
		{name: "unsupported critic choice", defaultEffort: "high", criticChoice: true, wantError: true},
		{name: "supported choice", defaultEffort: "high", mainChoice: true, supportMedium: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
				t.Setenv(key, "")
			}
			script := preflightCodexCLI
			if tc.supportMedium {
				script = strings.Replace(script, `"supportedReasoningEfforts":[{"reasoningEffort":"high"}]`,
					`"supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"medium"}]`, 1)
			}
			fakeOnPath(t, "codex", script)
			certificate, leaf := testRemoteCertificate(t)
			token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
			commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
			cfg := config.Config{Main: "codex", Commands: commands,
				Codex:          config.CodexConfig{Model: "main-1", ReasoningEffort: tc.defaultEffort},
				Repositories:   []config.Repository{{Name: "o/r", Actions: []string{"inspect"}}},
				RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:0", Token: token}}
			if tc.mainChoice || tc.criticChoice {
				cfg.Admin = &config.AdminConfig{UserModelChoices: &config.UserModelChoices{}}
				if tc.mainChoice {
					cfg.Admin.UserModelChoices.Main = []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}
				}
				if tc.criticChoice {
					cfg.Adversarial = "codex"
					cfg.Admin.UserModelChoices.Critic = []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}
				}
			}
			owner := &Orchestrator{Main: "codex", Adversarial: cfg.Adversarial, Commands: commands,
				CodexModel: "main-1", CodexEffort: tc.defaultEffort}
			_, endpoint, err := owner.AttachRemoteWorker(cfg, certificate)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = endpoint.Close() })
			origin := "https://" + endpoint.Addr().String()
			client, err := NewLocalExecutionClient(LocalEndpointAccess{BaseURL: origin, Certificate: leaf.Raw, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			pullBody := fmt.Sprintf(`{"version":%d,"executor_id":"remote"}`, ProtocolVersion)
			post := func(path string, tokenClient *http.Client) *http.Response {
				t.Helper()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, strings.NewReader(pullBody))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				response, err := tokenClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			unauthorized, err := NewLocalExecutionClient(LocalEndpointAccess{BaseURL: origin, Certificate: leaf.Raw,
				Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unauthorized.CloseIdleConnections)
			response := post("/preflight", unauthorized)
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthorized preflight status=%d", response.StatusCode)
			}
			_ = response.Body.Close()
			response = post("/next", client)
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("pull skipped required preflight: status=%d", response.StatusCode)
			}
			_ = response.Body.Close()
			err = PreflightRemoteChoices(ctx, client, origin, "remote", false)
			if tc.wantError {
				if err == nil || err.Error() != preflightCodexInvocationUnsupported {
					t.Fatalf("unsupported role was offered: %v", err)
				}
				readyCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
				if err := owner.WaitCapacity(readyCtx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("failed worker exposed capacity: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("supported remote choices failed preflight: %v", err)
			}
			pullCtx, stopPull := context.WithCancel(ctx)
			defer stopPull()
			pullDone := make(chan error, 1)
			go func() {
				request, err := http.NewRequestWithContext(pullCtx, http.MethodPost, origin+"/next", strings.NewReader(pullBody))
				if err == nil {
					request.Header.Set("Content-Type", "application/json")
					var response *http.Response
					response, err = client.Do(request)
					if err == nil {
						_, err = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
					}
				}
				pullDone <- err
			}()
			if err := owner.WaitCapacity(ctx); err != nil {
				t.Fatalf("validated worker did not hold a pull: %v", err)
			}
			stopPull()
			<-pullDone
		})
	}
}

func TestRemoteChoicePreflightBoundsSlowCLIProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(key, "")
	}
	marker := filepath.Join(t.TempDir(), "login-started")
	script := strings.Replace(preflightCodexCLI, "'login status') exit 0 ;;",
		"'login status') printf started > "+shellQuote(marker)+"; exec /bin/sleep 5 ;;", 1)
	if script == preflightCodexCLI {
		t.Fatal("Codex login probe fixture did not change")
	}
	fakeOnPath(t, "codex", script)
	selection := remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
		Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	data, err := httpEncode(selection, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/preflight" {
			t.Fatalf("remote worker pulled before CLI preflight: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(data)), Request: request}, nil
	})}
	start := time.Now()
	err = preflightRemoteChoices(t.Context(), client, "https://worker.invalid", "remote", false, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || t.Context().Err() != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("slow CLI probe escaped remote preflight deadline: elapsed=%s error=%v", time.Since(start), err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("slow CLI probe was not reached: %v", err)
	}
}

func TestRemoteClaudeSocketOptInReachesPreflightAndModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	testsupport.UseShortClaudeTempDir(t, "/tmp")
	fakeOnPath(t, "bwrap", "#!/bin/sh\ncase \"$1\" in --help) echo --disable-userns;; esac\n")
	settingsPath := filepath.Join(t.TempDir(), "settings")
	modelMarker := filepath.Join(t.TempDir(), "model")
	fakeOnPath(t, "claude", `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.318 (Claude Code)'; exit 0; fi
previous=
for arg do
  if [ "$previous" = --settings ]; then printf '%s\n' "$arg" >> `+shellQuote(settingsPath)+`; fi
  previous=$arg
done
case " $* " in
  *' auth status --json '*) echo '{"loggedIn":true}'; exit 0 ;;
  *' --minion-unsupported-capability-probe '*) echo 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;;
  *' --resume '*)
    previous=
    for arg do
      if [ "$previous" = --resume ]; then id=$arg; break; fi
      previous=$arg
    done
    echo '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}'
    echo "No conversation found with session ID: $id" >&2
    exit 1 ;;
esac
cat >/dev/null
case " $* " in
  *' --no-session-persistence '*)
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low"]}]}}}' ;;
  *)
    printf called > `+shellQuote(modelMarker)+`
    previous=
    for arg do
      if [ "$previous" = --session-id ]; then id=$arg; break; fi
      previous=$arg
    done
    printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$id"
    printf '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","session_id":"%s","result":"Done.","usage":{"input_tokens":3,"output_tokens":2},"modelUsage":{"fixture":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}\n' "$id" ;;
esac
`)
	selection := remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
		Main:        RoleSettings{Provider: "claude", Model: "claude-pinned", Effort: "low"},
		Credentials: WorkerCredentials{ClaudeAPIKey: "fixture-key"}}
	data, err := httpEncode(selection, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/preflight" {
			t.Fatalf("unexpected remote request: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(data)), Request: request}, nil
	})}
	if err := PreflightRemoteChoices(t.Context(), client, "https://worker.invalid", "remote", true); err != nil {
		t.Fatalf("remote Claude preflight: %v (%v)", err, errors.Unwrap(err))
	}
	preflightSettings, err := os.ReadFile(settingsPath)
	if err != nil || len(preflightSettings) == 0 {
		t.Fatalf("Claude preflight did not pass settings: %v", err)
	}
	runner := NewRemoteRunner(t.TempDir())
	runner.Claude.APIKey, runner.Claude.AllowAllUnixSockets = selection.Credentials.ClaudeAPIKey, true
	var session modelSession
	t.Cleanup(func() {
		if err := session.close(); err != nil {
			t.Error(err)
		}
	})
	_, err = runner.runNative(t.Context(), modelRun{session: &session, provider: "claude", dir: t.TempDir(), prompt: "Test model call", model: "claude-pinned", effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(modelMarker); err != nil {
		t.Fatalf("Claude model was not called: %v", err)
	}
	content, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) <= len(preflightSettings) {
		t.Fatal("Claude model did not receive settings after remote preflight")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if !strings.Contains(line, `"allowAllUnixSockets":true`) {
			t.Fatalf("remote preflight or model lost Unix socket opt-in: %s", line)
		}
	}
}
