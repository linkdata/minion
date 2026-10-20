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
	"github.com/linkdata/minion/internal/workroot"
)

func TestRunnerWorkRootIsolation(t *testing.T) {
	for _, kind := range []string{"main skill", "critic skill", "system read"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			runner := Runner{}
			job := withClaudeCritic(testJob(command.Comment))
			switch kind {
			case "main skill", "critic skill":
				skill := filepath.Join(work, "skill", "SKILL.md")
				writeFixtureFile(t, work, "skill/SKILL.md", []byte("trusted skill"))
				if kind == "main skill" {
					runner.Codex.Skills = []string{skill}
				} else {
					runner.Claude.Skills = []string{skill}
				}
			case "system read":
				t.Setenv("PATH", t.TempDir())
				work = "/usr"
			}
			if err := runner.CheckWorkRootIsolation(work, job.Main.Provider, job.Critic.Provider); err == nil || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("isolation=%v", err)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing")
	runner := Runner{WorkRoot: missing, runModel: forbidModel(t)}
	if result, err := runner.Run(context.Background(), testJob(command.Comment)); !errors.Is(err, os.ErrNotExist) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 {
		t.Fatalf("missing trusted root recreated or used: %+v,%v", result, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing trusted root was created: %v", err)
	}
	runner = Runner{}
	if _, err := runner.Run(context.Background(), testJob(command.Comment)); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root is required") {
		t.Fatalf("missing root=%v", err)
	}
}

func TestWorkRootIsolationRejectsUnknownProvider(t *testing.T) {
	if err := (&Runner{}).CheckWorkRootIsolation(t.TempDir(), "unknown"); err == nil || !strings.Contains(err.Error(), "unsupported AI provider") {
		t.Fatalf("unknown provider mixed filesystem grants: %v", err)
	}
}

func TestRunnerRejectsCanceledContextBeforePreparation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-be-created")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := (&Runner{WorkRoot: root}).Run(ctx, testJob(command.Comment))
	if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 {
		t.Fatalf("canceled dispatch: %+v, %v", result, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled dispatch prepared its work root: %v", err)
	}
}

func TestRunnerPreparationFailuresAreTerminalBeforeModels(t *testing.T) {
	longPath := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", 16), "/")
	for _, test := range []struct {
		name   string
		action command.Action
		mutate func(*Job)
		want   string
	}{
		{"source materialization", command.Comment, func(job *Job) { job.Source.Files[0].Path = longPath }, "materialize source"},
		{"repository materialization", command.Comment, func(job *Job) { job.Source.Files[0].Data = []byte("forged\n") }, "materialize repository"},
		{"context materialization", command.Review, func(job *Job) { job.Source.BaseFiles[0].Path = longPath }, "materialize context"},
		{"missing audit task skill", command.SecurityAudit, func(*Job) {}, "action prompt: security audit requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(test.action)
			test.mutate(&job)
			runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
			if result, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), test.want) || len(result.Calls) != 0 {
				t.Fatalf("preparation result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunnerEnforcesSuppliedRuntimeBudget(t *testing.T) {
	for _, test := range []struct {
		name        string
		action      command.Action
		lateSuccess bool
	}{
		{"comment cancellation", command.Comment, false},
		{"audit cancellation", command.SecurityAudit, false},
		{"late successful comment", command.Comment, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := test.action
			parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			job := testJob(action)
			if action == command.SecurityAudit {
				job = auditRunnerJob(t)
			}
			job.RuntimeBudget = 2 * time.Second
			calls := 0
			var reports string
			root := t.TempDir()
			runner := Runner{WorkRoot: root, runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > job.RuntimeBudget {
					t.Fatalf("model context did not apply the supplied %s budget: deadline=%v present=%t", job.RuntimeBudget, deadline, ok)
				}
				reports = request.outputDir
				if reports != "" {
					if err := os.WriteFile(filepath.Join(reports, "REPORT.md"), []byte("partial audit"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				<-ctx.Done()
				if test.lateSuccess {
					return aicli.Result{Answer: "Completed after the deadline."}, nil
				}
				return aicli.Result{}, ctx.Err()
			}}
			result, err := runner.Run(parent, job)
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || parent.Err() != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "" || len(result.Changes) != 0 {
				t.Fatalf("runtime budget: result=%+v calls=%d parent=%v error=%v", result, calls, parent.Err(), err)
			}
			if action == command.SecurityAudit {
				if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "audit_incomplete" || len(result.Artifacts) != 1 || result.Artifacts[0].Path != "REPORT.md" || string(result.Artifacts[0].Data) != "partial audit" {
					t.Fatalf("expired audit lost final diagnostic or pre-cleanup artifact: %+v", result)
				}
			}
			assertEmptyDir(t, root)
		})
	}
}

func TestRunnerFixCanReturnEmptyAnswerWithoutCritic(t *testing.T) {
	job := testJob(command.Fix)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		writeFixtureFile(t, request.dir, "source.txt", []byte("fixed source"))
		return aicli.Result{}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed source" {
		t.Fatalf("valid fix was lost because its summary was empty: %+v,%v", result, err)
	}
}

func TestRunnerOmittedSourceWriteIsTerminal(t *testing.T) {
	job := testJob(command.Fix)
	job.Source.Omitted = []string{"hidden"}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if !strings.Contains(request.prompt, "Do not create, change or delete omitted paths or anything beneath them") {
			t.Fatal("prompt permits writing omitted source")
		}
		if err := os.WriteFile(filepath.Join(request.dir, "hidden"), []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Answer: "Done."}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "omitted source path") || len(result.Calls) != 1 || len(result.Changes) != 0 {
		t.Fatalf("omitted write must not rerun the model: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("incomplete result must retain valid diagnostics: %v", err)
	}
}

func TestWorkRootIsolationRejectsResolvedExecutableTarget(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			work, bin := filepath.Join(root, "work"), filepath.Join(root, "bin")
			for _, directory := range []string{work, bin} {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(work, "actual-"+provider)
			if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(bin, provider)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			want, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			runner := Runner{}
			if err := runner.CheckWorkRootIsolation(work, provider); err == nil || !strings.Contains(err.Error(), "work root overlaps") || !strings.Contains(err.Error(), want) {
				t.Fatalf("outside symlink hid executable inside work root: %v", err)
			}
			other := "claude"
			if provider == "claude" {
				other = "codex"
			}
			if err := runner.CheckWorkRootIsolation(work, other); err != nil {
				t.Fatalf("unused %s executable affected %s admission: %v", provider, other, err)
			}
			job := testJob(command.Comment)
			job.Main.Provider = other
			job.Critic = &RoleSettings{Provider: provider}
			job.CriticMaxRounds = 1
			runner.WorkRoot, runner.runModel = work, forbidModel(t)
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "work root overlaps") || !strings.Contains(err.Error(), want) || len(result.Calls) != 0 {
				t.Fatalf("critic executable escaped Run admission: calls=%d error=%v", len(result.Calls), err)
			}
		})
	}
}
