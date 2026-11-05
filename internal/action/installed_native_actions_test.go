package action

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

func TestInstalledNativeActions(t *testing.T) {
	if os.Getenv("MINION_LIVE_NATIVE_ACTIONS") != "1" {
		t.Skip("set MINION_LIVE_NATIVE_ACTIONS=1 to run authenticated CLI actions")
	}

	roles := []struct{ provider, model, effort string }{
		{"codex", os.Getenv("MINION_LIVE_CODEX_MODEL"), os.Getenv("MINION_LIVE_CODEX_EFFORT")},
		{"claude", os.Getenv("MINION_LIVE_CLAUDE_MODEL"), os.Getenv("MINION_LIVE_CLAUDE_EFFORT")},
	}
	for _, role := range roles {
		for _, action := range []command.Action{command.Review, command.Fix} {
			t.Run(role.provider+"/"+string(action), func(t *testing.T) {
				if role.model == "" || role.effort == "" {
					t.Fatalf("set MINION_LIVE_%s_MODEL and MINION_LIVE_%s_EFFORT to resolved installed-CLI choices", strings.ToUpper(role.provider), strings.ToUpper(role.provider))
				}
				job := testJob(action)
				job.Main.Provider, job.Main.Model, job.Main.Effort = role.provider, role.model, role.effort
				job.Title, job.Notes = "Native CLI smoke test", ""
				job.RuntimeBudget, job.InactivityTimeout = 5*time.Minute, 2*time.Minute
				if action == command.Review {
					job.Description = "Read source.txt using the CLI's shell tool, then review the empty comparison between identical commits."
					job.Source.BaseFiles, job.Source.Diff = nil, nil
				} else {
					job.Description = "Replace source.txt with exactly fixed followed by one newline. Change no other files."
				}

				workRoot := t.TempDir()
				t.Cleanup(func() { assertEmptyDir(t, workRoot) })
				runner := NewRemoteRunner(workRoot)
				if role.provider == "claude" {
					runner.Claude.APIKey = os.Getenv("MINION_LIVE_CLAUDE_API_KEY")
					if runner.Claude.APIKey == "" {
						t.Fatal("set MINION_LIVE_CLAUDE_API_KEY so native transcripts stay in test-owned scratch")
					}
					if runtime.GOOS != "windows" {
						useShortClaudeTempDir(t, "/var/tmp")
					}
				}
				var successfulClaudeTool atomic.Bool
				if role.provider == "claude" && action == command.Review {
					runner.eventSink = func(record attemptRecord) {
						if record.EventSource != "claude.tool_result" {
							return
						}
						// Claude permits only Bash here; successful results may omit is_error.
						var toolResult struct {
							Type    string `json:"type"`
							IsError bool   `json:"is_error"`
						}
						if json.Unmarshal([]byte(record.Text), &toolResult) == nil && toolResult.Type == "tool_result" && !toolResult.IsError {
							successfulClaudeTool.Store(true)
						}
					}
				}
				result, err := runner.Run(t.Context(), job)
				if err != nil {
					t.Fatal(err)
				}
				if err := result.Validate(job); err != nil {
					t.Fatalf("returned result is invalid: %v", err)
				}
				if len(result.Calls) != 1 {
					t.Fatalf("model calls = %d, want one", len(result.Calls))
				}
				call := result.Calls[0]
				if call.Provider != role.provider || call.Role != roleMain || call.Round != 1 || call.ReviewPhase != "" || call.Tokens.Input <= 0 || call.Tokens.Output <= 0 {
					t.Fatalf("invalid main call receipt: %+v", call)
				}
				if strings.TrimSpace(result.Answer) == "" {
					t.Fatal("native CLI returned a blank answer")
				}
				if action == command.Review {
					if len(result.Changes) != 0 || len(result.Artifacts) != 0 {
						t.Fatal("read-only review returned changes or artifacts")
					}
					if role.provider == "claude" && !successfulClaudeTool.Load() {
						t.Fatal("Claude review completed without a successful sandboxed Bash result")
					}
					return
				}
				if len(result.Changes) != 1 || result.Changes[0].Path != "source.txt" || result.Changes[0].Mode != workspace.Regular || result.Changes[0].Delete || string(result.Changes[0].Data) != "fixed\n" {
					t.Fatal("fix did not return only source.txt with the expected mode and contents")
				}
			})
		}
	}
}
