package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunnerRequiresExistingWorkRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	runner := Runner{WorkRoot: missing, runModel: forbidModel(t)}
	if result, err := runner.Run(t.Context(), testJob(command.Comment)); !errors.Is(err, os.ErrNotExist) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 {
		t.Fatalf("missing trusted root recreated or used: %+v,%v", result, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing trusted root was created: %v", err)
	}
	runner = Runner{}
	if _, err := runner.Run(t.Context(), testJob(command.Comment)); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root is required") {
		t.Fatalf("missing root=%v", err)
	}
}

func TestRunnerRejectsCanceledContextBeforePreparation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-be-created")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := (&Runner{WorkRoot: root}).Run(ctx, testJob(command.Comment))
	if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 {
		t.Fatalf("canceled dispatch: %+v, %v", result, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled dispatch prepared its work root: %v", err)
	}
}

func TestRunnerReturnsParentCancellationAfterFinalReview(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := withClaudeCritic(testJob(command.Comment))
	root := t.TempDir()
	calls := 0
	runner := Runner{
		WorkRoot: root,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			calls++
			if request.role == roleMain {
				return aicli.Result{Answer: proposalJSON("Done.", "")}, nil
			}
			return aicli.Result{Answer: approvedCritiqueJSON}, nil
		},
		recordReview: func(receipts []CallUsage, _ []Refutation, stopReason string) error {
			if len(receipts) == 2 && stopReason == "approved" {
				cancel() // After the last model and repository verification, before Run returns.
			}
			return nil
		},
	}
	result, err := runner.Run(ctx, job)
	if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || calls != 2 || len(result.Calls) != 2 || result.CriticStopReason != "approved" {
		t.Fatalf("canceled after final review: result=%+v calls=%d error=%v", result, calls, err)
	}
	assertEmptyDir(t, root)
}

func TestRunnerPreparationFailuresAreTerminalBeforeModels(t *testing.T) {
	longPath := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", 16), "/")
	sourceFailure := func(job *Job) { job.Source.Files[0].Path = longPath }
	contextFailure := func(job *Job) { job.Source.BaseFiles[0].Path = longPath }
	if runtime.GOOS == "windows" {
		sourceFailure = func(job *Job) {
			job.Source.Files = append(job.Source.Files, workspace.File{Path: "SOURCE.txt", Mode: workspace.Regular})
		}
		contextFailure = func(job *Job) {
			job.Source.BaseFiles = append(job.Source.BaseFiles, workspace.File{Path: "DELETED.txt", Mode: workspace.Regular})
		}
	}
	for _, test := range []struct {
		name   string
		action command.Action
		mutate func(*Job)
		want   string
	}{
		{"source materialization", command.Comment, sourceFailure, "materialize source"},
		{"repository materialization", command.Comment, func(job *Job) { job.Source.Files[0].Data = []byte("forged\n") }, "materialize repository"},
		{"context materialization", command.Review, contextFailure, "materialize context"},
		{"missing audit task skill", command.SecurityAudit, func(job *Job) { job.Main.Skills = []string{skills.BuiltinAntiPromptInjection} }, "action prompt: security audit requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(test.action)
			test.mutate(&job)
			runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
			if runtime.GOOS == "windows" && (test.name == "source materialization" || test.name == "context materialization") {
				probe := filepath.Join(runner.WorkRoot, "case-probe")
				if err := os.WriteFile(probe, nil, 0600); err != nil {
					t.Fatal(err)
				}
				_, caseErr := os.Stat(filepath.Join(runner.WorkRoot, "CASE-PROBE"))
				if err := os.Remove(probe); err != nil {
					t.Fatal(err)
				}
				if errors.Is(caseErr, os.ErrNotExist) {
					t.Skip("case alias fixture requires a case-insensitive work root")
				}
				if caseErr != nil {
					t.Fatal(caseErr)
				}
			}
			if result, err := runner.Run(t.Context(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), test.want) || len(result.Calls) != 0 {
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
			if action != command.SecurityAudit {
				t.Parallel()
			}
			parent, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	result, err := runner.Run(t.Context(), job)
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
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "omitted source path") || len(result.Calls) != 1 || len(result.Changes) != 0 {
		t.Fatalf("omitted write must not rerun the model: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("incomplete result must retain valid diagnostics: %v", err)
	}
}
