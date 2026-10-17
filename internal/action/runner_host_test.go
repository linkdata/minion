package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || parent.Err() != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "" {
		t.Fatalf("runtime budget: result=%+v calls=%d parent=%v error=%v", result, calls, parent.Err(), err)
	}
}

func TestRunnerContextMaterializationFailureIsTerminalBeforeModels(t *testing.T) {
	job := portableJob(command.Review)
	job.Source.BaseFiles[0].Path = strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", 16), "/")
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran despite unrepresentable context")
		return aicli.Result{}, nil
	}}
	if result, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "materialize context") || len(result.Calls) != 0 {
		t.Fatalf("context materialization result=%+v error=%v", result, err)
	}
}

func TestRunnerMissingAuditTaskSkillIsTerminalBeforeModels(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran without the audit task skill")
		return aicli.Result{}, nil
	}}
	if result, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "action prompt: security audit requires") || len(result.Calls) != 0 {
		t.Fatalf("audit task result=%+v error=%v", result, err)
	}
}

func TestRunnerReadOnlyCriticReusesSourceAndCanaryGuardsExport(t *testing.T) {
	for _, action := range []command.Action{command.Review, command.Comment} {
		for _, mutate := range []bool{false, true} {
			t.Run(string(action)+"/mutate="+strconv.FormatBool(mutate), func(t *testing.T) {
				job := portableJob(action)
				job.Critic = &RoleSettings{Provider: "claude"}
				var source string
				calls := 0
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if request.role == "main" {
						source = request.dir
						return aicli.Result{Answer: proposalJSON("reviewed answer", "private notes")}, nil
					}
					if request.dir != source || request.writable || request.outputDir != "" {
						t.Fatalf("read-only critic source/grants=%+v", request)
					}
					if mutate {
						if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("mutate\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
				}}
				result, err := runner.Run(context.Background(), job)
				if mutate {
					if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "read-only action modified") || result.Answer != "" {
						t.Fatalf("canary accepted changed source: %+v,%v", result, err)
					}
				} else if err != nil || result.Answer != "reviewed answer" {
					t.Fatalf("read-only review failed: %+v,%v", result, err)
				}
				if calls != 2 {
					t.Fatalf("model calls=%d", calls)
				}
			})
		}
	}
}

func TestRunnerMissingHostSkillsRemainRetryableBeforeModels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, provider := range []string{"codex", "claude"} {
		for _, role := range []string{"main", "critic"} {
			t.Run(provider+"/"+role, func(t *testing.T) {
				job := portableJob(command.Comment)
				settings := RoleSettings{Provider: provider, Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}}
				if role == "main" {
					job.Main = settings
				} else {
					job.Critic = &settings
					if provider == "codex" {
						job.Main.Provider = "claude"
					}
				}
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
					t.Fatal("model ran despite failed host preflight")
					return aicli.Result{}, nil
				}}
				result, err := runner.Run(context.Background(), job)
				if err == nil || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "not installed") || len(result.Calls) != 0 {
					t.Fatalf("host preflight result=%+v error=%v", result, err)
				}
			})
		}
	}
}
