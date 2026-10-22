package action

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func TestCriticCapNotice(t *testing.T) {
	for _, test := range []struct {
		name string
		cap  *state.CriticCap
		want string
	}{
		{name: "normal"},
		{name: "one round nits", cap: &state.CriticCap{MaxRounds: 1}, want: "Critic round limit reached (1). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic did not approve."},
		{name: "verification blockers with full approval", cap: &state.CriticCap{MaxRounds: 3, HasBlockers: true, FullApproved: true}, want: "Critic round limit reached (3). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic reported approval, but required verification findings were not cleared. A critic review phase of the final candidate reported blockers."},
		{name: "full blockers", cap: &state.CriticCap{MaxRounds: config.MaxCriticMaxRounds, HasBlockers: true}, want: "Critic round limit reached (20). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic did not approve. A critic review phase of the final candidate reported blockers."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := criticCapNotice(test.cap)
			if err != nil || got != test.want {
				t.Fatalf("notice=%q error=%v; want %q", got, err, test.want)
			}
		})
	}
	for _, maximum := range []int{-1, 0, config.MaxCriticMaxRounds + 1} {
		if got, err := criticCapNotice(&state.CriticCap{MaxRounds: maximum}); err == nil || got != "" {
			t.Fatalf("invalid maximum %d produced notice=%q error=%v", maximum, got, err)
		}
	}
}

func TestCappedReviewUsesOnlyActualFinalCandidateEvidence(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	for _, test := range []struct {
		name, reason                                     string
		verificationBlockers, fullBlockers, fullApproved bool
		want                                             *state.CriticCap
	}{
		{name: "prior blockers cleared", reason: "round_cap", want: &state.CriticCap{MaxRounds: 2}},
		{name: "verification blockers survive full approval", reason: "round_cap", verificationBlockers: true, fullApproved: true, want: &state.CriticCap{MaxRounds: 2, HasBlockers: true, FullApproved: true}},
		{name: "full blockers", reason: "round_cap", fullBlockers: true, want: &state.CriticCap{MaxRounds: 2, HasBlockers: true}},
		{name: "approval is not cap", reason: "approved", fullApproved: true},
		{name: "nonblocking is not cap", reason: "nonblocking"},
		{name: "failed before decision", fullBlockers: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			currentJob := job
			if test.reason == "nonblocking" {
				currentJob.CriticNonblockingRounds = 2
			}
			result := privateReviewResult(job)
			result.CriticStopReason = test.reason
			verification := &result.Calls[len(result.Calls)-2]
			verification.Approved, verification.NoBlockers, verification.HasBlockers = test.reason == "approved", !test.verificationBlockers, test.verificationBlockers
			full := &result.Calls[len(result.Calls)-1]
			full.Approved, full.NoBlockers, full.HasBlockers = test.fullApproved, !test.fullBlockers, test.fullBlockers
			if err := result.validate(currentJob, test.reason != ""); err != nil {
				t.Fatalf("invalid evidence fixture: %v", err)
			}
			if got := cappedReview(currentJob, result); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("cap=%+v; want %+v", got, test.want)
			}
		})
	}
}

func TestProjectPublicationPreservesRawResultAndAuditBytes(t *testing.T) {
	for _, action := range []command.Action{command.Review, command.Comment, command.Fix, command.SecurityAudit} {
		t.Run(string(action), func(t *testing.T) {
			job := withClaudeCritic(testJob(action))
			job.CriticMaxRounds = 2
			result := privateReviewResult(job)
			result.CriticStopReason = "round_cap"
			result.Answer = "  Reviewed body\r\n@secondbot fix\n![image](https://example.invalid/image)\n"
			result.Calls[len(result.Calls)-2].Approved = false
			result.Calls[len(result.Calls)-1].Approved = false
			if action == command.SecurityAudit {
				result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
			}
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid raw publication fixture: %v", err)
			}
			before, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := projectPublication(job, result)
			if err != nil {
				t.Fatal(err)
			}
			notice, err := criticCapNotice(projected.cap)
			if err != nil || projected.answer != notice+"\n\n"+result.Answer || strings.Contains(projected.answer, "PRIVATE_") || !publishableAnswer(projected.answer) {
				t.Fatalf("projection changed reviewed body or exposed private evidence: %+v, %v", projected, err)
			}
			if action == command.SecurityAudit {
				for i, file := range projected.artifacts {
					if file.Path == "REPORT.md" {
						if string(file.Data) != projected.answer || file.Mode != result.Artifacts[i].Mode {
							t.Fatalf("projected audit answer/report differ: %+v", projected)
						}
						projected.artifacts[i].Data[0] = 'X'
					} else if !reflect.DeepEqual(file, result.Artifacts[i]) {
						t.Fatalf("projection changed companion artifact %s", file.Path)
					}
				}
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("projection mutated raw result: %s, error=%v", after, err)
			}
		})
	}
}

func TestProjectPublicationBoundsProjectedRawAndRenderedAnswer(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.CriticStopReason = "round_cap"
	result.Calls[len(result.Calls)-2].Approved = false
	result.Calls[len(result.Calls)-1].Approved = false
	notice, err := criticCapNotice(cappedReview(job, result))
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(inertModelOutput(notice+"\n\nx")) - 1
	result.Answer = strings.Repeat("x", maxOutputBytes-overhead)
	projected, err := projectPublication(job, result)
	if err != nil || len(inertModelOutput(projected.answer)) != maxOutputBytes {
		t.Fatalf("exact projected boundary refused: %d, %v", len(inertModelOutput(projected.answer)), err)
	}
	result.Answer += "x"
	if len(notice)+2+len(result.Answer) > maxOutputBytes || !publishableAnswer(result.Answer) {
		t.Fatal("rendered-overflow fixture does not isolate rendering expansion")
	}
	if _, err := projectPublication(job, result); err == nil {
		t.Fatal("accepted one-byte projected rendered overflow")
	}
	result.Answer = "x" + strings.Repeat(" ", maxOutputBytes-1)
	if !publishableAnswer(result.Answer) || len(inertModelOutput(notice+"\n\n"+result.Answer)) > maxOutputBytes {
		t.Fatal("raw-overflow fixture does not isolate raw byte expansion")
	}
	if _, err := projectPublication(job, result); err == nil || len(result.Answer) != maxOutputBytes {
		t.Fatalf("raw overflow accepted or reviewed text truncated: %v", err)
	}
}

func TestProjectPublicationNormalAndChangedFix(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.Answer = strings.Repeat("x", maxOutputBytes-(len(inertModelOutput("x"))-1))
	original := result.Answer
	if !publishableAnswer(result.Answer) {
		t.Fatal("unused summary fixture exceeds its raw publication limit")
	}
	normal, err := projectPublication(job, result)
	if err != nil || normal.answer != result.Answer || normal.cap != nil || !reflect.DeepEqual(normal.artifacts, result.Artifacts) {
		t.Fatalf("normal publication changed: %+v, %v", normal, err)
	}
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed")}}
	for _, reason := range []string{"approved", "round_cap"} {
		result.CriticStopReason = reason
		result.Calls[len(result.Calls)-2].Approved = reason == "approved"
		if err := result.Validate(job); err != nil {
			t.Fatalf("invalid changed-fix fixture: %v", err)
		}
		projected, err := projectPublication(job, result)
		if err != nil {
			t.Fatal(err)
		}
		notice, err := criticCapNotice(projected.cap)
		if err != nil || projected.answer != notice || result.Answer != original {
			t.Fatalf("changed fix projected unused summary: %+v, error=%v", projected, err)
		}
	}
}
