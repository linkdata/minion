package action

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunnerMissingProviderIsNotTerminalAndRecordsIncompleteUsage(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("PATH", gitOnlyPath(t))
			job := testJob(command.Comment)
			job.Main.Provider = provider
			result, err := (&Runner{WorkRoot: t.TempDir()}).Run(t.Context(), job)
			if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Calls) != 1 {
				t.Fatalf("missing %s classification/accounting: result=%+v error=%v", provider, result, err)
			}
			if got := result.Calls[0]; got.Provider != provider || got.Role != roleMain || got.Round != 1 || got.Tokens != (usage.Tokens{}) || got.Complete ||
				got.UsageCoverage != (usage.Coverage{Scope: "unknown", PartialReport: true}) {
				t.Fatalf("missing %s call=%+v", provider, got)
			}
		})
	}
}

func TestNativeSameProviderPairWaitsForCapabilityVerification(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		job := withClaudeCritic(testJob(command.Comment))
		job.Main.Provider, job.Critic.Provider = provider, provider
		if err := job.Validate(); err != nil {
			t.Fatalf("portable %s pair rejected: %v", provider, err)
		}
		result, err := (&Runner{WorkRoot: t.TempDir()}).Run(t.Context(), job)
		if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), nativeRolePairUnverified) || len(result.Calls) != 0 {
			t.Fatalf("%s native pair reached model work: result=%+v error=%v", provider, result, err)
		}
	}
}

func TestRunnerMissingGitIsRetryableBeforeModelWork(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := (&Runner{WorkRoot: t.TempDir()}).Run(t.Context(), testJob(command.Comment))
	if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "materialize repository") || len(result.Calls) != 0 || result.Answer != "" {
		t.Fatalf("missing Git misclassified or charged: result=%+v error=%v", result, err)
	}
}
