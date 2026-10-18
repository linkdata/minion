package action

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestRefutationsRequireOrderedMainRoundsButPermitEmptyOrSparseMemory(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 3
	calls := []CallUsage{
		{Provider: job.Main.Provider, Role: roleMain, Round: 1},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true},
		{Provider: job.Main.Provider, Role: roleMain, Round: 2},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, HasBlockers: true},
		{Provider: job.Main.Provider, Role: roleMain, Round: 3},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 3, ReviewPhase: reviewVerification, Approved: true, NoBlockers: true},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 3, ReviewPhase: reviewFull, Approved: true, NoBlockers: true},
	}
	for _, refs := range [][]Refutation{nil, {{Round: 2, Text: "One refutation; implemented changes are intentionally absent."}}, {{Round: 2, Text: "Earlier refutation."}, {Round: 3, Text: "Independent later refutation."}}} {
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: calls, Refutations: refs}
		if err := result.Validate(job); err != nil {
			t.Fatalf("empty/sparse memory refused: %+v: %v", refs, err)
		}
	}
	for _, refs := range [][]Refutation{
		{{Round: 1, Text: "Preemptive refutation"}}, {{Round: 0, Text: "Refutation"}}, {{Round: -1, Text: "Refutation"}}, {{Round: 4, Text: "No corresponding main call"}},
		{{Round: 2, Text: "First"}, {Round: 2, Text: "Duplicate"}}, {{Round: 3, Text: "Later"}, {Round: 1, Text: "Earlier"}},
	} {
		if err := validateRefutations(job, calls, refs); err == nil {
			t.Fatalf("accepted unordered/unassociated refutations: %+v", refs)
		}
	}
	// A critic receipt for round2 cannot stand in for a missing main-round receipt.
	if err := validateRefutations(job, []CallUsage{calls[0], calls[3]}, []Refutation{{Round: 2, Text: "Invented main evidence"}}); err == nil {
		t.Fatal("critic round accepted as author refutation")
	}
	job.Critic = nil
	job.CriticMaxRounds = 0
	if err := validateRefutations(job, calls[:1], nil); err != nil {
		t.Fatal(err)
	}
	if err := validateRefutations(job, calls[:1], []Refutation{{Round: 1, Text: "No critic"}}); err == nil {
		t.Fatal("accepted refutation memory without critic")
	}
}

func TestRefutationsBoundPrivateTextAndPreserveJSONControls(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	calls := privateReviewResult(job).Calls[:3]
	for _, test := range []struct {
		name, text string
		valid      bool
	}{
		{"exact cap", strings.Repeat("x", maxOutputBytes), true}, {"over cap", strings.Repeat("x", maxOutputBytes+1), false},
		{"empty", "", false}, {"whitespace", " \n\t", false}, {"invalid UTF8", string([]byte{0xff}), false}, {"control characters", "Private refutation\x00\n\t\"quoted\"\\path", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			refs := []Refutation{{Round: 2, Text: test.text}}
			if err := validateRefutations(job, calls, refs); (err == nil) != test.valid {
				t.Fatalf("refutation validation=%v valid=%t", err, test.valid)
			}
			if test.valid {
				result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: calls, Refutations: refs}
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var decoded Result
				if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded.Refutations, refs) || decoded.validate(job, false) != nil {
					t.Fatalf("private wire text changed: %v", err)
				}
			}
		})
	}
}
