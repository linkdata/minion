package action

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestInvalidResultKeepsOnlyValidatedEvidence(t *testing.T) {
	for _, scenario := range []string{"invalid calls", "invalid answer", "foreign identity", "invalid diagnostics", "invalid artifact"} {
		t.Run(scenario, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.SecurityAudit))
			result := completeAuditResult(job)
			result.Calls = append(result.Calls, CallUsage{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true})
			result.Diagnostics = []Diagnostic{{Code: "cleanup_failed", Message: "PRIVATE cleanup details"}}
			switch scenario {
			case "invalid calls":
				result.Calls[0].Tokens.Input = -1
			case "invalid answer":
				result.Answer = ""
			case "foreign identity":
				result.Identity.AttemptID = "FOREIGN\nforged journal line"
			case "invalid diagnostics":
				result.Diagnostics[0].Code = "bad\nforged journal line"
			case "invalid artifact":
				result.Artifacts[0].Path = "../escape"
			}
			var journal bytes.Buffer
			root := t.TempDir()
			e := Orchestrator{WorkRoot: root, Logger: log.New(&journal, "", 0)}
			err := e.publish(context.Background(), &preparedAction{job: job}, result)
			if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("invalid result was not terminal: %v", err)
			}
			text := journal.String()
			if strings.Contains(text, "PRIVATE") || strings.Contains(text, "forged") || strings.Contains(text, "FOREIGN") {
				t.Fatalf("unvalidated/private text reached journal: %s", text)
			}
			if got, want := strings.Contains(text, "code=cleanup_failed"), scenario != "invalid diagnostics"; got != want {
				t.Fatalf("validated cleanup diagnostic present=%t want=%t: %s", got, want, text)
			}
			if got, want := strings.Contains(text, "role=critic round=1 review_phase=full"), scenario != "invalid calls" && scenario != "foreign identity"; got != want {
				t.Fatalf("validated phase metadata present=%t want=%t: %s", got, want, text)
			}
			if !strings.Contains(text, "attempt="+job.Identity.AttemptID) {
				t.Fatalf("accepted attempt identity missing: %s", text)
			}
			entries, readErr := os.ReadDir(filepath.Join(root, "audits"))
			if scenario == "foreign identity" || scenario == "invalid artifact" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("unsafe artifacts retained: %v, %v", entries, readErr)
				}
				return
			}
			if readErr != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "incomplete-") {
				t.Fatalf("safe partial artifacts lost: %v, %v", entries, readErr)
			}
			dir := filepath.Join(root, "audits", entries[0].Name())
			if _, err := os.Stat(filepath.Join(dir, "INCOMPLETE.txt")); err != nil {
				t.Fatal(err)
			}
			if report, err := os.ReadFile(filepath.Join(dir, "REPORT.md")); err != nil || string(report) != "Reviewed audit report." {
				t.Fatalf("safe partial report changed: %q,%v", report, err)
			}
		})
	}
}

func TestJobContextPreservesQuotedNUL(t *testing.T) {
	for _, field := range []string{"title", "description", "notes"} {
		t.Run(field, func(t *testing.T) {
			job := testJob(command.Comment)
			switch field {
			case "title":
				job.Title = "before\x00after"
			case "description":
				job.Description = "before\x00after"
			case "notes":
				job.Notes = "before\x00after"
			}
			if err := job.Validate(); err != nil {
				t.Fatalf("safe quoted context rejected: %v", err)
			}
			prompt, err := actionPrompt(context.Background(), job, "/source", "/context", "", "")
			if err != nil || strings.ContainsRune(prompt, 0) || !strings.Contains(prompt, `before\x00after`) {
				t.Fatalf("context did not stay escaped: %q,%v", prompt, err)
			}
		})
	}
}
