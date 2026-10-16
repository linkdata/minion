package action

import (
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func TestFixResultIncludesOmittedDirectorySpellingsInFinalLayout(t *testing.T) {
	for _, test := range []struct {
		name, omitted string
		change        workspace.Change
		want          string
	}{
		{"same directory", "dir/link", workspace.Change{Path: "dir/new", Mode: workspace.Regular}, ""},
		{"directory spelling", "dir/link", workspace.Change{Path: "Dir/new", Mode: workspace.Regular}, "directory spelling collision"},
		{"unicode directory spelling", "ſ/link", workspace.Change{Path: "s/new", Mode: workspace.Regular}, "directory spelling collision"},
		{"replace omission", "dir/link", workspace.Change{Path: "dir/link", Mode: workspace.Regular}, "omitted source path"},
		{"delete omission", "dir/link", workspace.Change{Path: "DIR/LINK", Delete: true}, "omitted source path"},
		{"write descendant", "dir/link", workspace.Change{Path: "DIR/link/new", Mode: workspace.Regular}, "collision"},
		{"replace ancestor", "dir/link", workspace.Change{Path: "DIR", Mode: workspace.Regular}, "collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Source.Omitted = []string{test.omitted}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid test source layout: %v", err)
			}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
				Changes: []workspace.Change{test.change}, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
			err := result.Validate(job)
			if test.want == "" {
				if err != nil {
					t.Fatalf("valid sibling change refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("omission layout validation=%v want%q", err, test.want)
			}
		})
	}
}

func TestJobRequiresExplicitCriticBudgetAndDefaultBranchFix(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	for _, rounds := range []int{0, -1, config.MaxCriticMaxRounds + 1} {
		job.CriticMaxRounds = rounds
		if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "critic round limit") {
			t.Fatalf("invalid critic budget %d validation=%v", rounds, err)
		}
	}
	for _, rounds := range []int{1, config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = rounds
		if err := job.Validate(); err != nil {
			t.Fatalf("explicit critic budget %d validation=%v", rounds, err)
		}
	}
	job = portableJob(command.Fix)
	job.Source.Base, job.Source.MergeBase = strings.Repeat("b", 40), strings.Repeat("c", 40)
	if err := workspace.ValidateSnapshot(job.Source); err != nil {
		t.Fatalf("invalid comparison fixture: %v", err)
	}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "fix requires an issue without a pull request comparison") {
		t.Fatalf("accepted fix on a PR-comparison snapshot: %v", err)
	}
}

func TestAuditResultAnswerMustMatchTransferredReport(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	const report = "Reviewed audit report."
	metadata := []byte(fmt.Sprintf(`{"run_id":"contract-test","repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.Identity.Repository, sourceIdentity(job), job.Source.Head, auditTestOwners))
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: report,
		Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	for _, name := range auditArtifacts {
		data := []byte("content")
		switch name {
		case "run-metadata.json":
			data = metadata
		case "REPORT.md":
			data = []byte(report)
		}
		result.Artifacts = append(result.Artifacts, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("invalid complete audit fixture: %v", err)
	}
	for _, answer := range []string{"Different unreviewed text.", report + "\n"} {
		result.Answer = answer
		if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "answer differs from REPORT.md") {
			t.Fatalf("mismatched audit answer validation=%v", err)
		}
		if err := result.validate(job, false); err != nil {
			t.Fatalf("incomplete audit diagnostics cannot be retained: %v", err)
		}
	}
}

func TestStatusRequiresMeaningfulStatePhasePairs(t *testing.T) {
	job := portableJob(command.Comment)
	for _, test := range []struct {
		state StatusState
		phase StatusPhase
		round int
		valid bool
	}{
		{StatusReady, PhasePreparing, 0, true},
		{StatusRunning, PhasePreparing, 0, true},
		{StatusRunning, PhaseMain, 1, true},
		{StatusRunning, PhaseCollecting, 1, true},
		{StatusRunning, PhaseUploading, 1, true},
		{StatusStopping, PhaseCleanup, 0, true},
		{StatusCompleted, PhaseFinished, 1, true},
		{StatusFailed, PhaseFinished, 0, true},
		{StatusUnavailable, PhaseFinished, 0, true},
		{StatusReady, PhaseMain, 1, false},
		{StatusReady, PhasePreparing, 1, false},
		{StatusRunning, PhaseFinished, 1, false},
		{StatusStopping, PhaseFinished, 1, false},
		{StatusCompleted, PhaseMain, 1, false},
		{StatusFailed, PhaseMain, 1, false},
		{StatusUnavailable, PhaseCleanup, 1, false},
		{StatusCompleted, PhaseFinished, 0, false},
		{StatusRunning, PhasePreparing, 1, false},
		{StatusRunning, PhaseMain, 0, false},
		{StatusRunning, PhaseCollecting, 0, false},
		{StatusRunning, PhaseUploading, 0, false},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", test.state, test.phase, test.round), func(t *testing.T) {
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: test.state, Phase: test.phase, Round: test.round}
			err := status.Validate(job)
			if (err == nil) != test.valid || (err != nil && !strings.Contains(err.Error(), "state and phase")) {
				t.Fatalf("status pair validation=%v want valid=%t", err, test.valid)
			}
		})
	}
}

func TestResultAndStatusShareDiagnosticBounds(t *testing.T) {
	job := portableJob(command.Comment)
	tooMany := make([]Diagnostic, 101)
	for i := range tooMany {
		tooMany[i] = Diagnostic{Code: "code", Message: "message"}
	}
	for _, test := range []struct {
		name        string
		diagnostics []Diagnostic
		want        string
	}{
		{"valid", []Diagnostic{{Code: "incomplete_usage", Message: "Provider omitted usage."}}, ""},
		{"maximum count", tooMany[:100], ""},
		{"maximum message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", 8<<10)}}, ""},
		{"empty code", []Diagnostic{{Code: "", Message: "message"}}, "invalid action diagnostic"},
		{"unsafe code", []Diagnostic{{Code: "../private", Message: "message"}}, "invalid action diagnostic"},
		{"nul message", []Diagnostic{{Code: "code", Message: "nul\x00message"}}, "invalid action diagnostic"},
		{"invalid UTF8 message", []Diagnostic{{Code: "code", Message: string([]byte{0xff})}}, "invalid action diagnostic"},
		{"oversized message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", (8<<10)+1)}}, "invalid action diagnostic"},
		{"too many", tooMany, "too many action diagnostics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.",
				Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}, Diagnostics: test.diagnostics}
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseMain, Round: 1, Diagnostics: test.diagnostics}
			for name, err := range map[string]error{"result": result.Validate(job), "status": status.Validate(job)} {
				if (err == nil) != (test.want == "") || (err != nil && !strings.Contains(err.Error(), test.want)) {
					t.Fatalf("%s diagnostics validation=%v want%q", name, err, test.want)
				}
			}
		})
	}
}
