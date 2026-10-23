package action

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type executionAdapterFixture struct {
	t     *testing.T
	job   Job
	owner *Orchestrator
	sink  *executionCheckpoints
}

func newExecutionAdapterFixture(t *testing.T, job Job) *executionAdapterFixture {
	t.Helper()
	owner := attemptFixture(&Orchestrator{WorkRoot: t.TempDir(), Main: job.Main.Provider})
	job.Identity.AttemptID = owner.attemptID
	if job.Critic != nil {
		owner.Adversarial = job.Critic.Provider
	}
	return &executionAdapterFixture{t: t, job: job, owner: owner, sink: newExecutionCheckpoints(owner, job)}
}

func (f *executionAdapterFixture) call(call CallUsage) CallCheckpoint {
	return CallCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Call: call}
}

func (f *executionAdapterFixture) review(calls []CallUsage, refs []Refutation, reason string) ReviewCheckpoint {
	return ReviewCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Calls: calls, Refutations: refs, CriticStopReason: reason}
}

func (f *executionAdapterFixture) result(calls []CallUsage, refs []Refutation, reason string) Result {
	return Result{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Answer: "Candidate", Calls: calls, Refutations: refs, CriticStopReason: reason}
}

func (f *executionAdapterFixture) ack(call CallUsage) {
	f.t.Helper()
	if err := f.sink.BeforeCall(f.call(callMetadata(call))); err != nil {
		f.t.Fatalf("valid BeforeCall: %v", err)
	}
	if err := f.sink.AfterCall(f.call(call)); err != nil {
		f.t.Fatalf("valid AfterCall: %v", err)
	}
}

func (f *executionAdapterFixture) saveReview(calls []CallUsage, refs []Refutation, reason string) {
	f.t.Helper()
	if err := f.sink.Review(f.review(calls, refs, reason)); err != nil {
		f.t.Fatalf("valid Review: %v", err)
	}
}

func (f *executionAdapterFixture) startUsage() *usage.Store {
	f.t.Helper()
	f.owner.Usage = &usage.Store{Dir: filepath.Join(f.t.TempDir(), "usage")}
	if _, err := f.owner.trackUsage(usageJob()); err != nil {
		f.t.Fatal(err)
	}
	return f.owner.Usage
}

func TestExecutionBeforeCallRejectsForeignOrUnpinnedFacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*CallCheckpoint)
	}{
		{"version", func(f *CallCheckpoint) { f.Version++ }},
		{"identity", func(f *CallCheckpoint) { f.Identity.AttemptID = "foreign" }},
		{"source", func(f *CallCheckpoint) { f.SourceRef = strings.Repeat("b", 40) }},
		{"provider", func(f *CallCheckpoint) { f.Call.Provider = "claude" }},
		{"role", func(f *CallCheckpoint) { f.Call.Role = roleCritic }},
		{"round", func(f *CallCheckpoint) { f.Call.Round = 2 }},
		{"phase", func(f *CallCheckpoint) { f.Call.ReviewPhase = reviewFull }},
		{"model", func(f *CallCheckpoint) { f.Call.Model = "other" }},
		{"effort", func(f *CallCheckpoint) { f.Call.Effort = "low" }},
		{"tokens", func(f *CallCheckpoint) { f.Call.Tokens.Input = 1 }},
		{"accounting", func(f *CallCheckpoint) { f.Call.Complete = true }},
		{"approval", func(f *CallCheckpoint) { f.Call.Approved = true }},
		{"no blockers", func(f *CallCheckpoint) { f.Call.NoBlockers = true }},
		{"blockers", func(f *CallCheckpoint) { f.Call.HasBlockers = true }},
		{"coverage", func(f *CallCheckpoint) { f.Call.ReviewComplete = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			job.Main.Model, job.Main.Effort = "pinned", "high"
			f := newExecutionAdapterFixture(t, job)
			fact := f.call(CallUsage{Provider: "codex", Role: roleMain, Round: 1, Model: "pinned", Effort: "high"})
			test.change(&fact)
			if err := f.sink.BeforeCall(fact); !errors.Is(err, monitor.ErrTerminalJob) || f.sink.pending != nil || len(f.sink.calls) != 0 {
				t.Fatalf("invalid BeforeCall acknowledged: fact=%+v error=%v", fact, err)
			}
		})
	}
}

func TestExecutionCallPairingAndMissingAcknowledgements(t *testing.T) {
	for _, scenario := range []string{"missing callbacks", "missing Before", "duplicate Before", "missing After", "duplicate After", "changed After metadata", "changed Result", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, testJob(command.Comment))
			call := CallUsage{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7}, Complete: true}
			result := f.result([]CallUsage{call}, nil, "")
			var runErr error
			if scenario != "missing callbacks" && scenario != "missing Before" {
				if err := f.sink.BeforeCall(f.call(callMetadata(call))); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "duplicate Before":
				runErr = f.sink.BeforeCall(f.call(callMetadata(call)))
			case "missing After":
				result.Calls = nil
				runErr = errors.New("executor stopped before returning a receipt")
			case "missing callbacks":
			default:
				fact := f.call(call)
				if scenario == "changed After metadata" {
					fact.Call.Model = "other"
				}
				runErr = f.sink.AfterCall(fact)
				if scenario == "duplicate After" {
					runErr = f.sink.AfterCall(fact)
				}
				if scenario == "changed Result" {
					result.Calls[0].Tokens.Input++
				}
			}
			err := f.sink.finish(result, runErr)
			if scenario == "complete" {
				if err != nil || f.owner.Usage != nil {
					t.Fatalf("explicit no-usage acknowledgments failed: %v", err)
				}
			} else if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("missing/mismatched callback did not stop execution: %v", err)
			}
		})
	}
}

func TestExecutionCriticRequiresInitialPrefixAndSeparateDecision(t *testing.T) {
	for _, scenario := range []string{"missing initial", "missing main prefix", "combined receipt and decision", "missing decision", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			calls := []CallUsage{{Provider: "codex", Role: roleMain, Round: 1}, {Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true, ReviewComplete: true}}
			if scenario == "missing initial" {
				if err := f.sink.BeforeCall(f.call(calls[0])); !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("missing initial review was accepted: %v", err)
				}
				return
			}
			f.saveReview(nil, nil, "")
			f.ack(calls[0])
			if scenario == "missing main prefix" {
				if err := f.sink.BeforeCall(f.call(callMetadata(calls[1]))); !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("missing main prefix permitted critic work: %v", err)
				}
				return
			}
			f.saveReview(calls[:1], nil, "")
			f.ack(calls[1])
			if scenario == "combined receipt and decision" {
				if err := f.sink.Review(f.review(calls, nil, "approved")); !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("decision introduced an unsaved receipt prefix: %v", err)
				}
				return
			}
			f.saveReview(calls, nil, "")
			if scenario == "complete" {
				f.saveReview(calls, nil, "approved")
				digest, err := contentDigest("Candidate", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				fact := f.review(calls, nil, "approved")
				fact.ContentDigest = digest
				if err := f.sink.Review(fact); err != nil {
					t.Fatal(err)
				}
			}
			err := f.sink.finish(f.result(calls, nil, "approved"), nil)
			if scenario == "missing decision" {
				if !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("Result substituted for a decision acknowledgment: %v", err)
				}
				return
			}
			saved := readPrivateReview(t, f.owner.WorkRoot)
			if err != nil || f.owner.Usage != nil || saved.CriticStopReason != "approved" || !slices.Equal(saved.Calls, calls) {
				t.Fatalf("nil Usage disabled required critic retention: saved=%+v error=%v", saved, err)
			}
		})
	}
}

func TestExecutionRejectsForeignAfterAndReviewBindings(t *testing.T) {
	for _, method := range []string{"AfterCall", "Review"} {
		for _, field := range []string{"version", "identity", "source"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
				f.saveReview(nil, nil, "")
				path := filepath.Join(f.owner.WorkRoot, "reviews", f.job.Identity.AttemptID+".json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				fact := f.call(CallUsage{Provider: f.job.Main.Provider, Role: roleMain, Round: 1})
				if method == "AfterCall" {
					if err := f.sink.BeforeCall(fact); err != nil {
						t.Fatal(err)
					}
				}
				switch field {
				case "version":
					fact.Version++
				case "identity":
					fact.Identity.AttemptID = "foreign"
				case "source":
					fact.SourceRef = strings.Repeat("b", 40)
				}
				if method == "AfterCall" {
					err = f.sink.AfterCall(fact)
				} else {
					err = f.sink.Review(ReviewCheckpoint{Version: fact.Version, Identity: fact.Identity, SourceRef: fact.SourceRef})
				}
				after, readErr := os.ReadFile(path)
				if !errors.Is(err, monitor.ErrTerminalJob) || readErr != nil || !bytes.Equal(before, after) || len(f.sink.calls) != 0 || f.sink.returned != nil || !f.sink.reviewed || len(f.sink.review.Calls) != 0 {
					t.Fatalf("foreign facts changed checkpoint state: calls=%+v review=%+v error=%v read=%v", f.sink.calls, f.sink.review, err, readErr)
				}
			})
		}
	}
}

func TestExecutionDecisionSaveFailureKeepsAcknowledgedPrefix(t *testing.T) {
	for _, test := range []struct {
		reason   string
		approved bool
	}{{"approved", true}, {"round_cap", false}} {
		t.Run(test.reason, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			if test.reason == "round_cap" {
				job.CriticMaxRounds = 1
			}
			f := newExecutionAdapterFixture(t, job)
			calls := []CallUsage{
				{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 3}, Complete: true},
				{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: usage.Tokens{Input: 5}, Complete: true, Approved: test.approved, NoBlockers: test.approved, HasBlockers: !test.approved, ReviewComplete: true},
			}
			f.saveReview(nil, nil, "")
			f.ack(calls[0])
			f.saveReview(calls[:1], nil, "")
			f.ack(calls[1])
			f.saveReview(calls, nil, "")
			dir := filepath.Join(f.owner.WorkRoot, "reviews")
			path := filepath.Join(dir, f.job.Identity.AttemptID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			saved := readPrivateReview(t, f.owner.WorkRoot)
			if saved.CriticStopReason != "" || !slices.Equal(saved.Calls, calls) {
				t.Fatalf("receipt checkpoint already contained a decision: %+v", saved)
			}
			durableDir := dir + "-durable"
			if err := os.Rename(dir, durableDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			decisionErr := f.sink.Review(f.review(calls, nil, test.reason))
			if !errors.Is(decisionErr, monitor.ErrTerminalJob) || !errors.Is(decisionErr, syscall.ENOTDIR) {
				t.Fatalf("failed decision save was acknowledged: %v", decisionErr)
			}
			after, err := os.ReadFile(filepath.Join(durableDir, filepath.Base(path)))
			if err != nil || !bytes.Equal(before, after) || f.sink.review.CriticStopReason != "" || !slices.Equal(f.sink.review.Calls, calls) || len(f.sink.review.Refutations) != 0 {
				t.Fatalf("failed decision write changed durable/acknowledged prefix: review=%+v error=%v", f.sink.review, err)
			}
			result := f.result(calls, nil, test.reason)
			if err := result.Validate(f.job); err != nil {
				t.Fatalf("fixture must be an otherwise publishable result: %v", err)
			}
			if err := f.sink.finish(result, decisionErr); !errors.Is(err, decisionErr) || !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("failed decision permitted successful execution: %v", err)
			}
			if err := f.sink.finish(result, nil); !errors.Is(err, decisionErr) || !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("ignored failed decision was accepted from a valid Result: %v", err)
			}
		})
	}
}

func TestExecutionReviewAcknowledgmentOwnsRetainedSlices(t *testing.T) {
	for _, mutate := range []string{"calls", "refutations"} {
		t.Run(mutate, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			result := privateReviewResult(f.job)
			f.saveReview(nil, nil, "")
			for i, call := range result.Calls {
				f.ack(call)
				var refs []Refutation
				if call.Round >= 2 {
					refs = result.Refutations
				}
				f.saveReview(result.Calls[:i+1], refs, "")
			}
			f.saveReview(result.Calls, result.Refutations, result.CriticStopReason)
			path := filepath.Join(f.owner.WorkRoot, "reviews", f.job.Identity.AttemptID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mutate == "calls" {
				result.Calls[0].Tokens.Input = 91
			} else {
				result.Refutations[0].Text = "Changed after acknowledgment"
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || f.sink.review.Calls[0].Tokens.Input != 0 || f.sink.review.Refutations[0].Text != "PRIVATE_MAIN_REFUTATION" {
				t.Fatalf("borrowed mutation changed acknowledged/saved evidence: review=%+v error=%v", f.sink.review, err)
			}
			if err := f.sink.finish(result, nil); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("changed Result matched aliased acknowledgment: %v", err)
			}
		})
	}
}

func TestExecutionFailedBeforePreservesOrdinaryErrorButRefusesPaidContinuation(t *testing.T) {
	for _, scenario := range []string{"faithful", "different error", "ignored success", "ignored paid receipt"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, testJob(command.Comment))
			store := f.startUsage()
			original := store.Dir
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			store.Dir = blocked
			call := CallUsage{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7}, Complete: true}
			beforeErr := f.sink.BeforeCall(f.call(callMetadata(call)))
			if beforeErr == nil || errors.Is(beforeErr, monitor.ErrTerminalJob) {
				t.Fatalf("pre-save failure changed classification: %v", beforeErr)
			}
			store.Dir = original
			if err := f.sink.BeforeCall(f.call(callMetadata(call))); !errors.Is(err, beforeErr) {
				t.Fatalf("a later callback erased the first failure: %v", err)
			}
			result, runErr := f.result(nil, nil, ""), beforeErr
			if scenario == "different error" {
				runErr = errors.New("different executor error")
			}
			if scenario == "ignored success" || scenario == "ignored paid receipt" {
				result.Calls = []CallUsage{call}
				if scenario == "ignored success" {
					runErr = nil
				}
			}
			err := f.sink.finish(result, runErr)
			terminal := strings.HasPrefix(scenario, "ignored")
			if !errors.Is(err, beforeErr) || runErr != nil && !errors.Is(err, runErr) || errors.Is(err, monitor.ErrTerminalJob) != terminal || f.owner.usageCheckpoint.record.CodexCalls != 0 || !f.owner.usageCheckpoint.record.UsageComplete {
				t.Fatalf("failed Before state/cause changed: record=%+v error=%v", f.owner.usageCheckpoint.record, err)
			}
		})
	}
}

func TestExecutionFailedAfterAllowsOnlyItsExactCapturedReceipt(t *testing.T) {
	for _, scenario := range []string{"exact", "drop", "change failed receipt", "rewrite acknowledged prefix", "extra paid receipt"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			store := f.startUsage()
			original := store.Dir
			calls := []CallUsage{
				{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 3}, Complete: true},
				{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: usage.Tokens{Input: 7, Output: 2}, Complete: true, Approved: true, NoBlockers: true, ReviewComplete: true},
			}
			f.saveReview(nil, nil, "")
			f.ack(calls[0])
			f.saveReview(calls[:1], nil, "")
			if err := f.sink.BeforeCall(f.call(callMetadata(calls[1]))); err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			store.Dir = blocked
			afterErr := f.sink.AfterCall(f.call(calls[1]))
			if !errors.Is(afterErr, monitor.ErrTerminalJob) || f.sink.returned == nil || *f.sink.returned != calls[1] {
				t.Fatalf("failed After lost its actual captured receipt: returned=%+v error=%v", f.sink.returned, afterErr)
			}
			result := f.result(slices.Clone(calls), nil, "")
			switch scenario {
			case "drop":
				result.Calls = result.Calls[:1]
			case "change failed receipt":
				result.Calls[1].Tokens.Input++
			case "rewrite acknowledged prefix":
				result.Calls[0].Tokens.Input++
			case "extra paid receipt":
				result.Calls = append(result.Calls, CallUsage{Provider: "codex", Role: roleMain, Round: 2})
			}
			runErr := errors.New("different executor error")
			err := f.sink.finish(result, runErr)
			if !errors.Is(err, runErr) || !errors.Is(err, afterErr) || !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), "execution checkpoint contract violated") != (scenario != "exact") {
				t.Fatalf("failed After accepted the wrong prefix or lost a cause: %v", err)
			}
			records, err := usage.ReadAll(original)
			if err != nil || len(records) != 1 || records[0].Tokens != calls[0].Tokens || records[0].UsageComplete || records[0].CodexCalls != 1 || records[0].ClaudeCalls != 1 || f.owner.usageCheckpoint.record.Tokens != (usage.Tokens{Input: 10, Output: 2}) {
				t.Fatalf("failed write claimed returned usage was durable: records=%+v memory=%+v error=%v", records, f.owner.usageCheckpoint.record, err)
			}
		})
	}
}

func TestExecutionPaidReviewFailureJoinsDifferentExecutorError(t *testing.T) {
	f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
	f.saveReview(nil, nil, "")
	call := CallUsage{Provider: "codex", Role: roleMain, Round: 1}
	f.ack(call)
	dir := filepath.Join(f.owner.WorkRoot, "reviews")
	if err := os.Rename(dir, dir+"-durable"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewErr := f.sink.Review(f.review([]CallUsage{call}, nil, ""))
	if !errors.Is(reviewErr, monitor.ErrTerminalJob) {
		t.Fatalf("paid review failure lost terminal policy: %v", reviewErr)
	}
	runErr := errors.New("different executor error")
	if err := f.sink.finish(f.result([]CallUsage{call}, nil, ""), runErr); !errors.Is(err, runErr) || !errors.Is(err, reviewErr) || !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("executor error erased the first paid checkpoint failure: %v", err)
	}
}
