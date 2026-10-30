package action

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func TestStatusContractRoundTripAndIndependentSequences(t *testing.T) {
	job := testJob(command.Review)
	job.Critic = &RoleSettings{Provider: "claude"}
	job.CriticMaxRounds = 3
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	status := Status{
		Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, ProgressSequence: 7, ProgressAge: time.Second, LogSequence: 3,
		WorkerState: "busy", Phase: "critic", Round: 2, ReviewPhase: reviewVerification,
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Status
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != status {
		t.Fatalf("status round trip changed: %#v", decoded)
	}
	if err := decoded.Validate(job); err != nil {
		t.Fatalf("status round trip rejected: %v", err)
	}
	// Heartbeats and logs can advance without claiming new execution progress.
	decoded.Sequence++
	decoded.LogSequence++
	decoded.ProgressAge += time.Second
	if err := decoded.Validate(job); err != nil {
		t.Fatalf("independent status sequences: %v", err)
	}
	decoded.ReviewPhase = reviewFull
	if err := decoded.Validate(job); err != nil {
		t.Fatalf("full review status: %v", err)
	}
}

func TestStatusRejectsForeignAndMalformedObservations(t *testing.T) {
	job := testJob(command.Comment)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, WorkerState: "busy", Phase: "main", Round: 1}
	if err := status.Validate(job); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, want string
		change     func(*Status)
	}{
		{"version", "version, identity or source", func(s *Status) { s.Version++ }},
		{"job", "version, identity or source", func(s *Status) { s.Identity.JobID = "other" }},
		{"attempt", "version, identity or source", func(s *Status) { s.Identity.AttemptID = "other" }},
		{"executor", "version, identity or source", func(s *Status) { s.Identity.ExecutorID = "other" }},
		{"repository", "version, identity or source", func(s *Status) { s.Identity.RepositoryID++ }},
		{"owner", "version, identity or source", func(s *Status) { s.Identity.OwnerID++ }},
		{"requester", "version, identity or source", func(s *Status) { s.Identity.RequesterID++ }},
		{"source", "version, identity or source", func(s *Status) { s.SourceRef = strings.Repeat("b", 40) }},
		{"missing status sequence", "sequence or progress age", func(s *Status) { s.Sequence = 0 }},
		{"negative progress age", "sequence or progress age", func(s *Status) { s.ProgressAge = -1 }},
		{"oversized error", "status error", func(s *Status) { s.Error = strings.Repeat("x", httpFailureMessageLimit+1) }},
		{"unknown worker state", "worker state", func(s *Status) { s.WorkerState = "idle" }},
		{"ready before cleanup", "terminal worker state", func(s *Status) { s.WorkerState = "ready" }},
		{"unknown phase", "status phase", func(s *Status) { s.Phase = "waiting" }},
		{"main without round", "phase or round", func(s *Status) { s.Round = 0 }},
		{"main with review phase", "phase or round", func(s *Status) { s.ReviewPhase = reviewFull }},
		{"critic without critic", "review phase or round", func(s *Status) { s.Phase = "critic"; s.ReviewPhase = reviewFull }},
		{"prepare with round", "phase or round", func(s *Status) { s.Phase = "prepare" }},
		{"upload with review phase", "phase or round", func(s *Status) { s.Phase = "upload"; s.Round = 0; s.ReviewPhase = reviewFull }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := status
			test.change(&changed)
			if err := changed.Validate(job); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("status validation=%v, want %q", err, test.want)
			}
		})
	}
	for _, phase := range []string{"prepare", "upload", "cleanup"} {
		idle := status
		idle.Phase, idle.Round = phase, 0
		if err := idle.Validate(job); err != nil {
			t.Fatalf("valid %s status: %v", phase, err)
		}
	}
	status.Phase, status.Round, status.WorkerState = "cleanup", 0, "unclean"
	status.ProgressAge = config.MaxActionRuntime + time.Second // Cleanup may outlive the action budget.
	if err := status.Validate(job); err != nil {
		t.Fatalf("unclean cleanup status: %v", err)
	}
}

func TestStatusReviewPhaseFollowsConfiguredRounds(t *testing.T) {
	job := testJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	job.CriticMaxRounds = 3
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, WorkerState: "busy", Phase: "critic", Round: 1, ReviewPhase: reviewFull}
	if err := status.Validate(job); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		round int
		phase string
	}{
		{"first-round verification", 1, reviewVerification},
		{"unknown review phase", 2, "focused"},
		{"round above limit", 4, reviewFull},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := status
			changed.Round, changed.ReviewPhase = test.round, test.phase
			if err := changed.Validate(job); err == nil || !strings.Contains(err.Error(), "review phase or round") {
				t.Fatalf("status validation=%v", err)
			}
		})
	}
}
