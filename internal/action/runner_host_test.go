package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRunnerWorkRootIsolation(t *testing.T) {
	for _, kind := range []string{"main read", "critic read", "main skill", "critic skill", "system read"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			runner := Runner{}
			job := portableJob(command.Comment)
			job.Critic = &RoleSettings{Provider: "claude"}
			switch kind {
			case "main read":
				runner.Codex.ReadOnlyDirs = []string{root}
			case "critic read":
				runner.Claude.ReadOnlyDirs = []string{root}
			case "main skill", "critic skill":
				skill := filepath.Join(work, "skill", "SKILL.md")
				writeIndexFixture(t, work, "skill/SKILL.md", []byte("trusted skill"))
				if kind == "main skill" {
					runner.Codex.Skills = []string{skill}
				} else {
					runner.Claude.Skills = []string{skill}
				}
			case "system read":
				work = "/usr"
			}
			if err := runner.checkWorkRootIsolation(work, job); err == nil || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("isolation=%v", err)
			}
		})
	}
	runner := Runner{WorkRoot: "/usr", runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran in public system directory")
		return aicli.Result{}, nil
	}}
	if _, err := runner.Run(context.Background(), portableJob(command.Comment)); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root overlaps") {
		t.Fatalf("unsafe root=%v", err)
	}
	runner = Runner{}
	if _, err := runner.Run(context.Background(), portableJob(command.Comment)); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root is required") {
		t.Fatalf("missing root=%v", err)
	}
}

func TestRunnerMaterializationFailureIsTerminalBeforeModels(t *testing.T) {
	job := portableJob(command.Comment)
	parts := make([]string, 16)
	for i := range parts {
		parts[i] = strings.Repeat("a", 255)
	}
	job.Source.Files[0].Path = strings.Join(parts, "/")
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran despite unrepresentable source")
		return aicli.Result{}, nil
	}}
	if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "materialize source") {
		t.Fatalf("materialize=%v", err)
	}
}

func TestRunnerEnforcesSuppliedRuntimeBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job := portableJob(command.Comment)
	job.RuntimeBudget = 100 * time.Millisecond
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, _ modelRun) (aicli.Result, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > job.RuntimeBudget {
			t.Fatalf("model context did not apply the supplied %s budget: deadline=%v present=%t", job.RuntimeBudget, deadline, ok)
		}
		<-ctx.Done()
		return aicli.Result{}, ctx.Err()
	}}
	result, err := runner.Run(parent, job)
	if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "" {
		t.Fatalf("runtime budget: result=%+v calls=%d parent=%v error=%v", result, calls, parent.Err(), err)
	}
}
