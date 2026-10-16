package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestStatusUsesConfiguredRoundBudget(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	for _, limit := range []int{0, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = limit
		rounds := limit
		if rounds == 0 {
			rounds = config.DefaultCriticMaxRounds
		}
		status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseCritic, Round: rounds}
		if err := status.Validate(job); err != nil {
			t.Fatalf("valid round %d: %v", rounds, err)
		}
		status.Round++
		if err := status.Validate(job); err == nil {
			t.Fatal("accepted status beyond configured rounds")
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
			if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "omitted source path") {
				t.Fatalf("change %q validation=%v", name, err)
			}
		})
	}
}

func TestRunnerOmittedSourceWriteIsTerminal(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Omitted = []string{"hidden"}
	runner := Runner{WorkRoot: t.TempDir(), RunCodex: func(_ context.Context, dir, prompt string, _ bool, _ string) (codexcli.Result, error) {
		if !strings.Contains(prompt, "Do not create, change or delete omitted paths or anything beneath them") {
			t.Fatal("prompt permits writing omitted source")
		}
		if err := os.WriteFile(filepath.Join(dir, "hidden"), []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		return codexcli.Result{Answer: "Done."}, nil
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
		Changes: []workspace.Change{{Path: "foo/new", Mode: workspace.Regular}}}
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
			if err := result.Validate(job); (err == nil) != test.complete {
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

func TestResultCallUsageHonorsConfiguredAndDefaultRoundLimits(t *testing.T) {
	if config.DefaultCriticMaxRounds != 5 || config.MaxCriticMaxRounds != 20 {
		t.Fatalf("critic configuration changed: default=%d max=%d", config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds)
	}
	for _, test := range []struct {
		name   string
		limit  int
		rounds int
	}{
		{"explicit", 2, 2}, {"default", 0, 5}, {"maximum", 20, 20},
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
