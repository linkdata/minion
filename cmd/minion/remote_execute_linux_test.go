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

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestRemoteWorkerConfigPassesSocketOptInToPreflight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	root, err := os.MkdirTemp("/tmp", "mc-remote-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("TMPDIR", root)
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
	var requestMu sync.Mutex
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requests = append(requests, r.URL.Path)
		requestMu.Unlock()
		if r.URL.Path == "/preflight" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(selection)
			return
		}
		http.Error(w, "stop after preflight", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	cfg := remoteWorkerConfig{BaseURL: server.URL, ExecutorID: "remote", WorkRoot: t.TempDir()}
	cfg.Claude.AllowAllUnixSockets = true
	code := runRemoteWorkerPreflightAndLoop(t.Context(), cfg, server.Client(), log.New(&logs, "", 0))
	requestMu.Lock()
	seenRequests := append([]string(nil), requests...)
	requestMu.Unlock()
	if code != 1 || logs.String() != "remote worker: execution or protocol failure\n" || len(seenRequests) != 2 || seenRequests[0] != "/preflight" || seenRequests[1] != "/next" {
		t.Fatalf("remote preflight or pull failed: code=%d requests=%v logs=%q", code, seenRequests, logs.String())
	}
	settings, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(settings), `"allowAllUnixSockets":true`) {
		t.Fatalf("remote worker preflight lost Claude socket opt-in: settings=%q error=%v", settings, err)
	}
}
