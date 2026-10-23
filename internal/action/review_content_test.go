package action

import (
	"errors"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestReviewedContentDigestUsesFrozenCandidate(t *testing.T) {
	fix := testJob(command.Fix)
	change := workspace.Change{Path: "source.txt", Mode: workspace.Regular, Data: []byte("reviewed fix\n")}
	fixDigest, err := reviewedContentDigest(fix, strings.Repeat("x", maxOutputBytes), &reviewSnapshot{changes: []workspace.Change{change}}, "")
	if err != nil {
		t.Fatal(err)
	}
	wantFix, err := contentDigest("", []workspace.Change{change}, nil)
	if err != nil || fixDigest != wantFix {
		t.Fatalf("changed fix digest did not cover frozen changes and discarded answer: got=%q want=%q error=%v", fixDigest, wantFix, err)
	}

	audit := testJob(command.SecurityAudit)
	raw := auditArtifactsFixture(audit, sourceIdentity(audit), "Reviewed report.")
	auditDigest, err := reviewedContentDigest(audit, "Unused proposal answer", &reviewSnapshot{artifacts: raw}, "/installed/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeAuditArtifacts(raw, audit, "/installed/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	wantAudit, err := contentDigest("Reviewed report.", nil, normalized)
	if err != nil || auditDigest != wantAudit {
		t.Fatalf("audit digest did not cover reviewed report and normalized artifacts: got=%q want=%q error=%v", auditDigest, wantAudit, err)
	}
}

func TestExecutionCriticRequiresReviewedContentDigest(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Fix, command.SecurityAudit} {
		t.Run(string(action), func(t *testing.T) {
			f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(action)))
			calls := []CallUsage{
				{Provider: f.job.Main.Provider, Role: roleMain, Round: 1},
				{Provider: f.job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true, ReviewComplete: true},
			}
			result := f.result(calls, nil, "approved")
			switch action {
			case command.Fix:
				result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("reviewed fix\n")}}
			case command.SecurityAudit:
				result.Answer = "Reviewed report."
				result.Artifacts = auditArtifactsFixture(f.job, sourceIdentity(f.job), result.Answer)
			}
			if err := result.Validate(f.job); err != nil {
				t.Fatalf("fixture result is invalid: %v", err)
			}
			f.saveReview(nil, nil, "")
			f.ack(calls[0])
			f.saveReview(calls[:1], nil, "")
			f.ack(calls[1])
			f.saveReview(calls, nil, "")
			f.saveReview(calls, nil, "approved")
			if err := f.sink.finish(result, nil); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("successful result without reviewed content digest passed: %v", err)
			}
			digest, err := contentDigest(result.Answer, result.Changes, result.Artifacts)
			if err != nil {
				t.Fatal(err)
			}
			fact := f.review(calls, nil, "approved")
			fact.ContentDigest = digest
			if err := f.sink.Review(fact); err != nil {
				t.Fatal(err)
			}
			if err := f.sink.finish(result, nil); err != nil {
				t.Fatalf("unchanged reviewed content was rejected: %v", err)
			}
			altered := result
			switch action {
			case command.Comment:
				altered.Answer = "Forged answer"
			case command.Fix:
				altered.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("forged fix\n")}}
			case command.SecurityAudit:
				altered.Artifacts = append([]workspace.File(nil), result.Artifacts...)
				altered.Artifacts[1].Data = []byte("Forged architecture\n")
			}
			if err := altered.Validate(f.job); err != nil {
				t.Fatalf("altered content must remain contract-valid: %v", err)
			}
			if err := f.sink.finish(altered, nil); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("altered result matched frozen content checkpoint: %v", err)
			}
		})
	}
}

func TestReviewContentDigestShape(t *testing.T) {
	valid := strings.Repeat("a", 64)
	for _, digest := range []string{valid[:63], valid + "a", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		if err := httpReviewShape(ReviewCheckpoint{ContentDigest: digest}); !errors.Is(err, errHTTPShape) {
			t.Fatalf("invalid digest %q passed HTTP shape: %v", digest, err)
		}
	}
	if err := httpReviewShape(ReviewCheckpoint{ContentDigest: valid}); err != nil {
		t.Fatalf("valid content digest rejected: %v", err)
	}
}

func TestContentDigestCanonicalizesOmittedEmptySlices(t *testing.T) {
	withoutSlices, err := contentDigest("Reviewed answer", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withEmptySlices, err := contentDigest("Reviewed answer", []workspace.Change{}, []workspace.File{})
	if err != nil || withEmptySlices != withoutSlices {
		t.Fatalf("omitempty changed digest across the worker wire: got=%q want=%q error=%v", withEmptySlices, withoutSlices, err)
	}
}
