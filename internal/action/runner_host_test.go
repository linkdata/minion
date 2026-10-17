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
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunnerWorkRootIsolation(t *testing.T) {
	for _, kind := range []string{"main read", "critic read", "main skill", "critic skill", "system read", "codex binary"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			runner := Runner{}
			job := withClaudeCritic(portableJob(command.Comment))
			switch kind {
			case "main read":
				runner.Codex.ReadOnlyDirs = []string{root}
			case "critic read":
				runner.Claude.ReadOnlyDirs = []string{root}
			case "main skill", "critic skill":
				skill := filepath.Join(work, "skill", "SKILL.md")
				writeFixtureFile(t, work, "skill/SKILL.md", []byte("trusted skill"))
				if kind == "main skill" {
					runner.Codex.Skills = []string{skill}
				} else {
					runner.Claude.Skills = []string{skill}
				}
			case "system read":
				work = "/usr"
			case "codex binary":
				writeFixtureFile(t, work, "codex", []byte("#!/bin/sh\nexit 0\n"))
				if err := os.Chmod(filepath.Join(work, "codex"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", work+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			if err := runner.CheckWorkRootIsolation(work, job.Main.Provider, job.Critic.Provider); err == nil || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("isolation=%v", err)
			}
		})
	}
	runner := Runner{WorkRoot: "/usr", runModel: forbidModel(t)}
	if _, err := runner.Run(context.Background(), portableJob(command.Comment)); !errors.Is(err, workroot.ErrUnsafe) || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root overlaps") {
		t.Fatalf("unsafe root=%v", err)
	}
	runner = Runner{}
	if _, err := runner.Run(context.Background(), portableJob(command.Comment)); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "work root is required") {
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
	result, err := (&Runner{WorkRoot: root}).Run(ctx, portableJob(command.Comment))
	if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
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
		{"context materialization", command.Review, func(job *Job) { job.Source.BaseFiles[0].Path = longPath }, "materialize context"},
		{"missing audit task skill", command.SecurityAudit, func(*Job) {}, "action prompt: security audit requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := portableJob(test.action)
			test.mutate(&job)
			runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
			if result, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), test.want) || len(result.Calls) != 0 {
				t.Fatalf("preparation result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunnerEnforcesSuppliedRuntimeBudget(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.SecurityAudit} {
		t.Run(string(action), func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			job := portableJob(action)
			job.RuntimeBudget = 100 * time.Millisecond
			if action == command.SecurityAudit {
				installActionTestSkills(t, testAuditSkillURL)
				job.Main.Skills = []string{testAuditSkillURL}
				fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
			}
			calls := 0
			var source, reports string
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > job.RuntimeBudget {
					t.Fatalf("model context did not apply the supplied %s budget: deadline=%v present=%t", job.RuntimeBudget, deadline, ok)
				}
				source, reports = request.dir, request.outputDir
				if reports != "" {
					if err := os.WriteFile(filepath.Join(reports, "REPORT.md"), []byte("partial audit"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				<-ctx.Done()
				return aicli.Result{}, ctx.Err()
			}}
			result, err := runner.Run(parent, job)
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || parent.Err() != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "" {
				t.Fatalf("runtime budget: result=%+v calls=%d parent=%v error=%v", result, calls, parent.Err(), err)
			}
			if action == command.SecurityAudit {
				if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "audit_incomplete" || len(result.Artifacts) != 1 || result.Artifacts[0].Path != "REPORT.md" || string(result.Artifacts[0].Data) != "partial audit" {
					t.Fatalf("expired audit lost final diagnostic or pre-cleanup artifact: %+v", result)
				}
			}
			for _, dir := range []string{source, reports} {
				if dir != "" {
					if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("execution workspace retained after partial collection: %q, %v", dir, err)
					}
				}
			}
		})
	}
}

func TestRunnerReadOnlyCriticReusesSourceAndCanaryGuardsExport(t *testing.T) {
	for _, action := range []command.Action{command.Review, command.Comment} {
		for _, mutate := range []bool{false, true} {
			t.Run(string(action)+"/mutate="+strconv.FormatBool(mutate), func(t *testing.T) {
				job := withClaudeCritic(portableJob(action))
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
					return aicli.Result{Answer: approvedCritiqueJSON}, nil
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
		t.Run(provider, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Main = RoleSettings{Provider: provider, Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}}
			runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
			result, err := runner.Run(context.Background(), job)
			if err == nil || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "not installed") || len(result.Calls) != 0 {
				t.Fatalf("host preflight result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunnerFixCanReturnEmptyAnswerWithoutCritic(t *testing.T) {
	job := portableJob(command.Fix)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		writeFixtureFile(t, request.dir, "source.txt", []byte("fixed source"))
		return aicli.Result{}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed source" {
		t.Fatalf("valid fix was lost because its summary was empty: %+v,%v", result, err)
	}
	for _, test := range []struct {
		action  command.Action
		critic  bool
		changed bool
		valid   bool
	}{{command.Fix, false, true, true}, {command.Fix, false, false, false}, {command.Fix, true, false, false}, {command.Comment, false, false, false}, {command.Review, false, false, false}, {command.SecurityAudit, false, false, false}} {
		job := portableJob(test.action)
		if test.critic {
			job = withClaudeCritic(job)
		}
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
		if test.critic {
			result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true})
			result.ReviewLedger = reviewLedgerFixture(result.Calls)
		}
		if test.changed {
			result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixed source")}}
		}
		if err := result.Validate(job); (err == nil) != test.valid || (!test.valid && !strings.Contains(err.Error(), "action answer")) {
			t.Fatalf("empty %s answer with critic=%t: %v", test.action, test.critic, err)
		}
	}
}

func TestRunnerOmittedSourceWriteIsTerminal(t *testing.T) {
	job := portableJob(command.Fix)
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
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "omitted source path") {
		t.Fatalf("omitted write must not rerun the model: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("incomplete result must retain valid diagnostics: %v", err)
	}
}

func TestWorkRootIsolationRejectsResolvedCodexTarget(t *testing.T) {
	root := t.TempDir()
	work, bin := filepath.Join(root, "work"), filepath.Join(root, "bin")
	for _, directory := range []string{work, bin} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(work, "actual-codex")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{}
	if err := runner.CheckWorkRootIsolation(work, "codex"); err == nil || !strings.Contains(err.Error(), "work root overlaps") || !strings.Contains(err.Error(), want) {
		t.Fatalf("outside symlink hid executable inside work root: %v", err)
	}
	if err := runner.CheckWorkRootIsolation(work, "claude"); err != nil {
		t.Fatalf("unused Codex executable affected Claude admission: %v", err)
	}
}
