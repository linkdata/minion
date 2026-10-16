package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
