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
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestStatusUsesConfiguredRoundBudget(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	for _, limit := range []int{config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = limit
		rounds := limit
		status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseCritic, Round: rounds, ReviewPhase: "full"}
		if err := status.Validate(job); err != nil {
			t.Fatalf("valid round %d: %v", rounds, err)
		}
		status.Round++
		if err := status.Validate(job); err == nil {
			t.Fatal("accepted status beyond configured rounds")
		}
	}
}

func TestStatusDistinguishesReviewPhases(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	for _, tt := range []struct {
		name        string
		phase       StatusPhase
		round       int
		reviewPhase string
		valid       bool
	}{
		{"initial full", PhaseCritic, 1, "full", true},
		{"revised full", PhaseCritic, 2, "full", true},
		{"finding verification", PhaseCritic, 2, "verification", true},
		{"main", PhaseMain, 1, "", true},
		{"missing critic phase", PhaseCritic, 1, "", false},
		{"unknown critic phase", PhaseCritic, 1, "focused", false},
		{"initial verification", PhaseCritic, 1, "verification", false},
		{"unstarted review", PhaseCritic, 0, "full", false},
		{"review phase on main", PhaseMain, 1, "full", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: tt.phase, Round: tt.round, ReviewPhase: tt.reviewPhase}
			if err := status.Validate(job); (err == nil) != tt.valid {
				t.Fatalf("status validation=%v; want valid=%v", err, tt.valid)
			}
		})
	}
	job.Critic = nil
	status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseCritic, Round: 1, ReviewPhase: "full"}
	if err := status.Validate(job); err == nil || !strings.Contains(err.Error(), "review phase") {
		t.Fatalf("allowed critic status without a critic: %v", err)
	}
}

func TestFixJobRejectsUnsupportedLayoutBeforeExecution(t *testing.T) {
	for _, paths := range [][]string{{"Docs/a", "docs/b"}, {"ſ", "s"}, {"µ/a", "μ/b"}, {`back\slash`}} {
		job := portableJob(command.Fix)
		for _, name := range paths {
			job.Source.Files = append(job.Source.Files, workspace.File{Path: name, Mode: workspace.Regular})
		}
		if err := job.Validate(); err == nil {
			t.Fatalf("accepted unsupported writable source paths %q", paths)
		}
		job.Action = command.Comment
		if err := job.Validate(); err != nil {
			t.Fatalf("read-only source paths %q should remain supported: %v", paths, err)
		}
	}
}

func TestFixJobIncludesOmittedPathsInUpfrontFoldedLayout(t *testing.T) {
	for _, names := range [][2]string{{"link/a", "Link"}, {"LINK", "link/b"}, {"ſ/a", "s"}, {"Docs/a", "docs/b"}, {"File", "file"}} {
		t.Run(names[0]+"-"+names[1], func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Source.Files = []workspace.File{{Path: names[0], Mode: workspace.Regular}}
			job.Source.Omitted = []string{names[1]}
			if err := workspace.ValidateSnapshot(job.Source); err != nil {
				t.Fatalf("test does not describe a valid exact source: %v", err)
			}
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				t.Fatal("model ran with a folded file/omission collision")
				return aicli.Result{}, nil
			}}
			if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || (!strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "duplicate source path")) {
				t.Fatalf("unsupported fix source was not refused before execution: %v", err)
			}
			job.Action = command.Comment
			if err := job.Validate(); err != nil {
				t.Fatalf("read-only exact source must remain supported: %v", err)
			}
		})
	}
}

func TestRunnerFixCanReturnEmptyAnswerWithoutCritic(t *testing.T) {
	job := portableJob(command.Fix)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		writeIndexFixture(t, request.dir, "source.txt", []byte("fixed source"))
		return aicli.Result{}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed source" {
		t.Fatalf("valid fix was lost because its summary was empty: %+v,%v", result, err)
	}
	for _, test := range []struct {
		action command.Action
		critic bool
		valid  bool
	}{{command.Fix, false, true}, {command.Fix, true, false}, {command.Comment, false, false}, {command.Review, false, false}, {command.SecurityAudit, false, false}} {
		job := portableJob(test.action)
		if test.critic {
			job.Critic = &RoleSettings{Provider: "claude"}
		}
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
		if err := result.Validate(job); (err == nil) != test.valid || (!test.valid && !strings.Contains(err.Error(), "action answer")) {
			t.Fatalf("empty %s answer with critic=%t: %v", test.action, test.critic, err)
		}
	}
}

func TestResultRejectsChangesOverlappingOmittedSource(t *testing.T) {
	for _, name := range []string{"private/link", "PRIVATE/link", "private/link/file", "private"} {
		t.Run(strings.ReplaceAll(name, "/", "-"), func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Source.Omitted = []string{"private/link"}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
				Changes: []workspace.Change{{Path: name, Mode: workspace.Regular, Data: []byte("replacement")}}}
			if err := result.Validate(job); err == nil || (!strings.Contains(err.Error(), "omitted source path") && !strings.Contains(err.Error(), "collision")) {
				t.Fatalf("change %q validation=%v", name, err)
			}
		})
	}
}

func TestJobChangeValidationFoldsOmittedUnicodePaths(t *testing.T) {
	for _, pair := range [][2]string{{"ſ", "s"}, {"µ", "μ"}, {"ς", "σ"}, {"ϐ", "β"}, {"K", "k"}} {
		job := portableJob(command.Fix)
		job.Source.Omitted = []string{pair[0] + "/link"}
		changes := []workspace.Change{{Path: pair[1] + "/link/file", Mode: workspace.Regular}}
		if err := job.validateChanges(changes); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("omitted %q vs changed %q: %v", pair[0], pair[1], err)
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

func TestResultValidatesChangedPathsAgainstOriginalSource(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "Foo/stay", Mode: workspace.Regular}}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
		Changes: []workspace.Change{{Path: "foo/new", Mode: workspace.Regular}}, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	if err := result.Validate(job); err == nil {
		t.Fatal("accepted directory case collision against unchanged source")
	}
	result.Changes = append([]workspace.Change{{Path: "Foo/stay", Delete: true}}, result.Changes...)
	if err := result.Validate(job); err != nil {
		t.Fatalf("rejected directory case rename: %v", err)
	}
}

func TestResultCallUsageRequiresOrderedReviewPhases(t *testing.T) {
	job := portableJob(command.Comment)
	job.Main.Model, job.Main.Effort = "main-model", "high"
	job.Critic = &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "medium"}
	main := func(round int) CallUsage {
		return CallUsage{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: "main", Round: round, Complete: true}
	}
	critic := func(round int, phase string) CallUsage {
		return CallUsage{Provider: job.Critic.Provider, Model: job.Critic.Model, Effort: job.Critic.Effort, Role: "critic", Round: round, ReviewPhase: phase, Complete: true}
	}
	for _, test := range []struct {
		name              string
		calls             []CallUsage
		complete, partial bool
	}{
		{"no review calls", nil, false, true},
		{"first full review", []CallUsage{main(1), critic(1, "full")}, true, true},
		{"revised full review", []CallUsage{main(1), critic(1, "full"), main(2), critic(2, "verification"), critic(2, "full")}, true, true},
		{"rejected verification then revision", []CallUsage{main(1), critic(1, "full"), main(2), critic(2, "verification"), main(3), critic(3, "verification"), critic(3, "full")}, true, true},
		{"interrupted first main", []CallUsage{main(1)}, false, true},
		{"interrupted revision", []CallUsage{main(1), critic(1, "full"), main(2)}, false, true},
		{"interrupted verification", []CallUsage{main(1), critic(1, "full"), main(2), critic(2, "verification")}, false, true},
		{"critic first", []CallUsage{critic(1, "full")}, false, false},
		{"missing first full", []CallUsage{main(1), critic(1, "verification")}, false, false},
		{"missing revision verification", []CallUsage{main(1), critic(1, "full"), main(2), critic(2, "full")}, false, false},
		{"repeated full", []CallUsage{main(1), critic(1, "full"), critic(1, "full")}, false, false},
		{"main before critic", []CallUsage{main(1), main(2)}, false, false},
		{"skipped round", []CallUsage{main(1), critic(1, "full"), main(3)}, false, false},
		{"critic round mismatch", []CallUsage{main(1), critic(2, "full")}, false, false},
		{"unknown phase", []CallUsage{main(1), critic(1, "focused")}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: test.calls}
			if err := result.Validate(job); (err == nil) != test.complete || (len(test.calls) == 0 && !strings.Contains(err.Error(), "final full review")) {
				t.Fatalf("complete validation=%v, want valid=%v", err, test.complete)
			}
			if err := result.validate(job, false); (err == nil) != test.partial {
				t.Fatalf("interrupted validation=%v, want valid=%v", err, test.partial)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*CallUsage)
	}{
		{"main phase", func(call *CallUsage) { call.ReviewPhase = "full" }},
		{"provider attribution", func(call *CallUsage) { call.Provider = "claude" }},
		{"model attribution", func(call *CallUsage) { call.Model = "different-model" }},
		{"effort attribution", func(call *CallUsage) { call.Effort = "low" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := main(1)
			test.mutate(&call)
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: []CallUsage{call, critic(1, "full")}}
			if err := result.validate(job, false); err == nil {
				t.Fatal("accepted malformed or incorrectly attributed call")
			}
		})
	}
}

func TestResultCallUsageRejectsCriticsAndRepeatedMainWithoutCritic(t *testing.T) {
	job := portableJob(command.Comment)
	main := CallUsage{Provider: job.Main.Provider, Role: "main", Round: 1}
	for _, calls := range [][]CallUsage{
		{{Provider: "claude", Role: "critic", Round: 1, ReviewPhase: "full"}},
		{main, {Provider: "claude", Role: "critic", Round: 1, ReviewPhase: "full"}},
		{main, {Provider: job.Main.Provider, Role: "main", Round: 2}},
		{main, main},
	} {
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: calls}
		for _, complete := range []bool{false, true} {
			if err := result.validate(job, complete); err == nil || (!strings.Contains(err.Error(), "model call")) {
				t.Fatalf("accepted unexpected no-critic calls (complete=%t): %+v,%v", complete, calls, err)
			}
		}
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: []CallUsage{main}}
	if err := result.Validate(job); err != nil {
		t.Fatalf("refused one valid main call without critic: %v", err)
	}
	result.Calls = nil
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "requires one main call") {
		t.Fatalf("accepted complete result without an executed main call: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("refused an interrupted result before main dispatch: %v", err)
	}
}

func TestResultCallUsageHonorsConfiguredRoundLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		limit  int
		rounds int
	}{
		{"explicit", 2, 2}, {"configured default", config.DefaultCriticMaxRounds, config.DefaultCriticMaxRounds}, {"maximum", config.MaxCriticMaxRounds, config.MaxCriticMaxRounds},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, test.limit
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer."}
			for round := 1; round <= test.rounds+1; round++ {
				result.Calls = append(result.Calls, CallUsage{Provider: job.Main.Provider, Role: "main", Round: round})
				if round > 1 {
					result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", Round: round, ReviewPhase: "verification"})
				}
				result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", Round: round, ReviewPhase: "full"})
				if err := result.Validate(job); (err == nil) != (round <= test.rounds) {
					t.Fatalf("round=%d limit=%d result validation=%v", round, test.limit, err)
				}
			}
		})
	}
	job := portableJob(command.Comment)
	for _, invalid := range []int{-1, config.MaxCriticMaxRounds + 1} {
		job.CriticMaxRounds = invalid
		if err := job.Validate(); err == nil {
			t.Fatalf("accepted invalid critic round limit %d", invalid)
		}
	}
}

func TestCompleteAuditRequiresNonemptyCoreArtifacts(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	for _, empty := range auditArtifacts {
		if auditArtifactMayBeEmpty(empty) {
			continue
		}
		var files []workspace.File
		for _, name := range auditArtifacts {
			data := []byte("placeholder")
			if name == empty {
				data = nil
			}
			files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
		}
		if err := validateResultArtifacts(job, files, true); err == nil || !strings.Contains(err.Error(), "empty action report artifact") {
			t.Fatalf("empty %s=%v", empty, err)
		}
		if err := validateResultArtifacts(job, files, false); err != nil {
			t.Fatalf("partial %s=%v", empty, err)
		}
	}
}
