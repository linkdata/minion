//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRemoteWorkerConfigPassesSocketOptInToPreflightAndJobValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	testsupport.UseShortClaudeTempDir(t, "/tmp")
	testsupport.FakeOnPath(t, "bwrap", "#!/bin/sh\ncase \"$1\" in --help) echo --disable-userns;; esac\n")
	settingsPath := filepath.Join(t.TempDir(), "settings")
	testsupport.FakeOnPath(t, "claude", `#!/bin/sh
previous=
for arg do
  if [ "$previous" = --settings ]; then printf '%s\n' "$arg" >> `+aicli.ShellQuote(settingsPath)+`; fi
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
echo '{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low"]}]}}}'
`)
	selection, err := json.Marshal(map[string]any{
		"version": action.ProtocolVersion, "executor_id": "remote",
		"main":        map[string]string{"provider": "claude", "model": "claude-pinned", "effort": "low"},
		"credentials": map[string]string{"claude_api_key": "fixture-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := action.Job{
		Version: action.ProtocolVersion,
		Identity: action.Identity{JobID: "job", AttemptID: "attempt", ExecutorID: "remote", Repository: "owner/repo",
			RepositoryID: 1, OwnerID: 2, Requester: "writer", RequesterID: 3, Number: 7, SubjectType: "Issue"},
		Command: "comment", Action: command.Comment,
		Source:        workspace.Snapshot{Head: strings.Repeat("a", 40), Repository: []byte("invalid test bundle")},
		Main:          action.RoleSettings{Provider: "claude", Model: "claude-pinned", Effort: "low", Skills: []string{skills.BuiltinAntiPromptInjection}},
		RuntimeBudget: time.Minute, InactivityTimeout: config.DefaultInactivityTimeout,
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	var requestMu sync.Mutex
	var requests []string
	var settingsBeforeJob int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requests = append(requests, r.URL.Path)
		requestMu.Unlock()
		switch r.URL.Path {
		case "/preflight":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(selection)
		case "/next":
			settings, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Error(err)
			}
			requestMu.Lock()
			settingsBeforeJob = len(settings)
			requestMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"job": job, "credentials": action.WorkerCredentials{ClaudeAPIKey: "fixture-key"},
				"stop_grace_ns": int64(time.Second),
			})
		case "/cancel":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.Error(w, "stop after job validation", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	cfg := remoteWorkerConfig{BaseURL: server.URL, ExecutorID: "remote", WorkRoot: t.TempDir()}
	cfg.Claude.AllowAllUnixSockets = true
	code := runRemoteWorkerPreflightAndLoop(t.Context(), cfg, server.Client(), log.New(&logs, "", 0))
	requestMu.Lock()
	seenRequests := append([]string(nil), requests...)
	beforeJob := settingsBeforeJob
	requestMu.Unlock()
	if code != 1 || logs.String() != "remote worker: execution or protocol failure\n" || len(seenRequests) < 3 || seenRequests[0] != "/preflight" || seenRequests[1] != "/next" {
		t.Fatalf("remote preflight or pull failed: code=%d requests=%v logs=%q", code, seenRequests, logs.String())
	}
	settings, err := os.ReadFile(settingsPath)
	if err != nil || beforeJob == 0 || len(settings) <= beforeJob || !strings.Contains(string(settings[:beforeJob]), `"allowAllUnixSockets":true`) ||
		!strings.Contains(string(settings[beforeJob:]), `"allowAllUnixSockets":true`) {
		t.Fatalf("remote preflight or job lost Claude socket opt-in: before=%d settings=%q error=%v", beforeJob, settings, err)
	}
}
