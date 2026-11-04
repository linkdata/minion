package action

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestDashboardDetailReadsValidatedRetainedReview(t *testing.T) {
	job := testJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	job.CriticMaxRounds = 3
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		CriticStopReason: "approved",
		Calls: []CallUsage{
			{Provider: "codex", Role: roleMain, Round: 1, Complete: true},
			{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true, ReviewComplete: true, Complete: true},
			{Provider: "codex", Role: roleMain, Round: 2, Complete: true},
			{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, Approved: true, NoBlockers: true, ReviewComplete: true, Complete: true},
			{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewFull, Approved: true, NoBlockers: true, ReviewComplete: true, Complete: true},
		},
		Refutations: []Refutation{{Round: 2, Text: "Main revision addressed the first critic round."}},
	}
	root := t.TempDir()
	if err := saveReviewEvidence(root, job, result, nil); err != nil {
		t.Fatalf("valid review could not be retained: %v", err)
	}
	detail := DashboardDetail{JobID: job.Identity.JobID, AttemptID: job.Identity.AttemptID}
	source := ""
	detail.readReview(root, nil, &source)
	if source != job.Source.Head || !strings.Contains(detail.CriticVerdict, "Round 2 full review: Critic approved") ||
		detail.CriticStop != "Stopped after approval" || len(detail.Refutations) != 1 || detail.Refutations[0].Text != result.Refutations[0].Text {
		t.Fatalf("validated retained review was not projected: %+v", detail)
	}
}

func TestDashboardDetailShowsOriginalCriticCapNotice(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	for _, test := range []struct {
		name, want           string
		verificationBlockers bool
	}{
		{"full approval with verification blockers", "Critic round limit reached (2). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic reported approval, but required verification findings were not cleared. A critic review phase of the final candidate reported blockers.", true},
		{"full review did not approve", "Critic round limit reached (2). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic did not approve. A critic review phase of the final candidate reported blockers.", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := privateReviewResult(job)
			result.CriticStopReason = "round_cap"
			verification := &result.Calls[len(result.Calls)-2]
			verification.Approved, verification.NoBlockers, verification.HasBlockers = false, !test.verificationBlockers, test.verificationBlockers
			full := &result.Calls[len(result.Calls)-1]
			full.Approved, full.NoBlockers, full.HasBlockers = test.verificationBlockers, test.verificationBlockers, !test.verificationBlockers
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid cap fixture: %v", err)
			}
			root := t.TempDir()
			if err := saveReviewEvidence(root, job, result, nil); err != nil {
				t.Fatal(err)
			}
			detail := DashboardDetail{JobID: job.Identity.JobID, AttemptID: job.Identity.AttemptID}
			source := ""
			detail.readReview(root, nil, &source)
			if detail.CriticStop != test.want || source != job.Source.Head {
				t.Fatalf("cap notice=%q, source=%q; want %q", detail.CriticStop, source, test.want)
			}
		})
	}
}
