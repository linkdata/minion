package action

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

// Existing call-order fixtures use generic private evidence; ledger contract
// tests below specify their evidence independently.
func reviewLedgerFixture(calls []CallUsage) []ReviewEntry {
	var entries []ReviewEntry
	for _, call := range calls {
		entry := ReviewEntry{Role: call.Role, Round: call.Round, ReviewPhase: call.ReviewPhase}
		if call.Role == roleCritic {
			if !call.NoBlockers && !call.HasBlockers {
				break
			}
			entry.Approved, entry.HasBlockers = call.Approved, call.HasBlockers
			if !call.Approved {
				entry.Feedback = "Unresolved finding."
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestReviewLedgerRequiresParsedCallPrefix(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	calls := []CallUsage{
		{Provider: job.Main.Provider, Role: roleMain, Round: 1},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true},
	}
	ledger := []ReviewEntry{
		{Role: roleMain, Round: 1, Notes: "Main assertion, not verified acceptance."},
		{Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true},
	}
	for length := 0; length <= len(ledger); length++ {
		if err := validateReviewLedger(job, calls, ledger[:length], false); err != nil {
			t.Fatalf("valid prefix %d refused: %v", length, err)
		}
		if err := validateReviewLedger(job, calls, ledger[:length], true); (err == nil) != (length == len(calls)) {
			t.Fatalf("complete prefix %d validation=%v", length, err)
		}
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: calls}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "review ledger") {
		t.Fatalf("complete result accepted missing review evidence: %v", err)
	}
	result.ReviewLedger = ledger
	if err := result.Validate(job); err != nil {
		t.Fatalf("complete result rejected matching evidence: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]ReviewEntry) []ReviewEntry
	}{
		{"role", func(entries []ReviewEntry) []ReviewEntry { entries[0].Role = roleCritic; return entries }},
		{"round", func(entries []ReviewEntry) []ReviewEntry { entries[0].Round++; return entries }},
		{"phase", func(entries []ReviewEntry) []ReviewEntry { entries[1].ReviewPhase = reviewVerification; return entries }},
		{"skipped main", func(entries []ReviewEntry) []ReviewEntry { return entries[1:] }},
		{"extra entry", func(entries []ReviewEntry) []ReviewEntry { return append(entries, entries[0]) }},
		{"main approval", func(entries []ReviewEntry) []ReviewEntry { entries[0].Approved = true; return entries }},
		{"main blockers", func(entries []ReviewEntry) []ReviewEntry { entries[0].HasBlockers = true; return entries }},
		{"main feedback", func(entries []ReviewEntry) []ReviewEntry { entries[0].Feedback = "finding"; return entries }},
		{"critic notes", func(entries []ReviewEntry) []ReviewEntry { entries[1].Notes = "author assertion"; return entries }},
		{"forged rejection", func(entries []ReviewEntry) []ReviewEntry {
			entries[1].Approved = false
			entries[1].Feedback = "finding"
			return entries
		}},
		{"forged blockers", func(entries []ReviewEntry) []ReviewEntry { entries[1].HasBlockers = true; return entries }},
		{"approval with feedback", func(entries []ReviewEntry) []ReviewEntry { entries[1].Feedback = "remaining finding"; return entries }},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := test.mutate(append([]ReviewEntry(nil), ledger...))
			result.Calls, result.ReviewLedger = calls, entries
			if err := result.validate(job, false); err == nil {
				t.Fatal("accepted invalid or skipped review evidence")
			}
		})
	}
	job.Critic, job.CriticMaxRounds = nil, 0
	if err := validateReviewLedger(job, calls[:1], nil, true); err != nil {
		t.Fatalf("no-critic action requires ledger: %v", err)
	}
	if err := validateReviewLedger(job, calls[:1], ledger[:1], false); err == nil {
		t.Fatal("accepted review ledger without a critic")
	}
}

func TestReviewLedgerRetainsNonblockingAndRejectsUnknownVerdicts(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 1, 1
	main := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1}
	for _, test := range []struct {
		name                 string
		approved, noBlockers bool
		hasBlockers          bool
		feedback             string
		valid                bool
	}{
		{name: "approved", approved: true, noBlockers: true, valid: true},
		{name: "accepted nonblocking is not approval", noBlockers: true, feedback: "Nonblocking simplification.", valid: true},
		{name: "blocking rejection", hasBlockers: true, feedback: "Blocking defect.", valid: true},
		{name: "unparsed", feedback: "Invented finding."},
		{name: "contradictory receipt", noBlockers: true, hasBlockers: true, feedback: "Finding."},
		{name: "empty rejection", noBlockers: true},
		{name: "whitespace rejection", noBlockers: true, feedback: " \n\t"},
		{name: "approval with blockers", approved: true, hasBlockers: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: test.approved, NoBlockers: test.noBlockers, HasBlockers: test.hasBlockers}
			entries := []ReviewEntry{{Role: roleMain, Round: 1}, {Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: test.approved, HasBlockers: test.hasBlockers, Feedback: test.feedback}}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{main, call}, ReviewLedger: entries}
			if err := result.validate(job, false); (err == nil) != test.valid {
				t.Fatalf("verdict ledger validation=%v want valid=%t", err, test.valid)
			}
			if !test.approved && test.noBlockers && test.valid {
				result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: []CallUsage{main, call}, ReviewLedger: entries}
				if err := result.Validate(job); err != nil || result.ReviewLedger[1].Approved {
					t.Fatalf("threshold promoted a nonblocking verdict to approval: %+v, %v", result.ReviewLedger, err)
				}
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var wire struct {
					ReviewLedger []map[string]json.RawMessage `json:"review_ledger"`
				}
				if err := json.Unmarshal(data, &wire); err != nil || len(wire.ReviewLedger) != 2 || string(wire.ReviewLedger[1]["approved"]) != "false" || string(wire.ReviewLedger[1]["has_blockers"]) != "false" {
					t.Fatalf("false critic verdict omitted: %s,%v", data, err)
				}
				var decoded Result
				if err := json.Unmarshal(data, &decoded); err != nil || decoded.Validate(job) != nil || !reflect.DeepEqual(decoded.ReviewLedger, entries) {
					t.Fatalf("nonblocking private wire evidence changed: %s,%v", data, err)
				}

			}
		})
	}
	// A failed or unparsed final critic call is not evidence in the ledger.
	unknown := CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull}
	if err := validateReviewLedger(job, []CallUsage{main, unknown}, []ReviewEntry{{Role: roleMain, Round: 1}}, false); err != nil {
		t.Fatalf("unparsed final call cannot retain prior parsed evidence: %v", err)
	}
}

func TestReviewLedgerBoundsPrivateTextAndPreservesJSONControls(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	calls := []CallUsage{
		{Provider: job.Main.Provider, Role: roleMain, Round: 1},
		{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true},
	}
	for _, field := range []struct {
		name  string
		limit int
		set   func([]ReviewEntry, string)
	}{
		{"notes", maxOutputBytes, func(entries []ReviewEntry, text string) { entries[0].Notes = text }},
		{"feedback", maxCriticOutputBytes, func(entries []ReviewEntry, text string) { entries[1].Feedback = text }},
	} {
		for _, test := range []struct {
			name, text string
			valid      bool
		}{
			{"exact bytes", strings.Repeat("😀", field.limit/4), true},
			{"one byte over", strings.Repeat("x", field.limit+1), false},
			{"invalid UTF8", string([]byte{0xff}), false},
			{"private controls", "private\x00\x1b\r\n\t/../evidence\u2028😀", true},
		} {
			t.Run(field.name+"/"+test.name, func(t *testing.T) {
				entries := []ReviewEntry{{Role: roleMain, Round: 1}, {Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true, Feedback: "Finding."}}
				field.set(entries, test.text)
				if err := validateReviewLedger(job, calls, entries, false); (err == nil) != test.valid {
					t.Fatalf("private text validation=%v want valid=%t", err, test.valid)
				}
				if test.valid {
					result := Result{ReviewLedger: entries}
					data, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					var decoded Result
					if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded.ReviewLedger, entries) || validateReviewLedger(job, calls, decoded.ReviewLedger, false) != nil {
						t.Fatalf("JSON transport changed private evidence: %v", err)
					}
				}
			})
		}
	}
}

func TestReviewLedgerUsesExistingRoundCountBound(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	for _, rounds := range []int{1, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = rounds
		var calls []CallUsage
		for round := 1; round <= rounds; round++ {
			calls = append(calls, CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: round})
			if round > 1 {
				calls = append(calls, CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: round, ReviewPhase: reviewVerification, Approved: true, NoBlockers: true})
			}
			calls = append(calls, CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: round, ReviewPhase: reviewFull, Approved: round == rounds, NoBlockers: round == rounds, HasBlockers: round != rounds})
		}
		entries := reviewLedgerFixture(calls)
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: calls, ReviewLedger: entries}
		if err := result.Validate(job); err != nil || len(entries) != 3*rounds-1 {
			t.Fatalf("maximum ledger refused: entries=%d err=%v", len(entries), err)
		}
		calls = append(calls, CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: rounds + 1})
		entries = append(entries, ReviewEntry{Role: roleMain, Round: rounds + 1})
		result.Calls, result.ReviewLedger = calls, entries
		if err := result.validate(job, false); err == nil {
			t.Fatal("ledger exceeded configured call/round bound")
		}
	}
}
