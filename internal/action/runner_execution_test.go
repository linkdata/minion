package action

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type runnerExecutionCheckpoints struct {
	before func(CallCheckpoint) error
	after  func(CallCheckpoint) error
	review func(ReviewCheckpoint) error
}

func (c runnerExecutionCheckpoints) BeforeCall(value CallCheckpoint) error {
	if c.before != nil {
		return c.before(value)
	}
	return nil
}

func (c runnerExecutionCheckpoints) AfterCall(value CallCheckpoint) error {
	if c.after != nil {
		return c.after(value)
	}
	return nil
}

func (c runnerExecutionCheckpoints) Review(value ReviewCheckpoint) error {
	if c.review != nil {
		return c.review(value)
	}
	return nil
}

func TestRunnerExecuteRequiresCheckpoints(t *testing.T) {
	job := testJob(command.Comment)
	runner := Runner{runModel: forbidModel(t)}
	result, err := runner.Execute(t.Context(), job, nil)
	if !errors.Is(err, monitor.ErrTerminalJob) || !result.matchesJob(job) || len(result.Calls) != 0 || result.Answer != "" {
		t.Fatalf("missing required checkpoints were accepted: result=%+v error=%v", result, err)
	}
}

func TestRunnerExecuteCheckpointsInsideObserverAndSeparatesDecision(t *testing.T) {
	requireActionProcessGroup(t)
	job := withClaudeCritic(testJob(command.Comment))
	tokens := usage.Tokens{Input: 11, CachedInput: 3, Output: 2}
	var events []string
	var calls []CallUsage
	runner := Runner{
		WorkRoot: t.TempDir(),
		recordReview: func([]CallUsage, []Refutation, string) error {
			t.Fatal("Execute used the optional direct-Run review hook")
			return nil
		},
		aroundModel: func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
			events = append(events, "observe-before/"+request.role)
			call, err := next(ctx, request)
			events = append(events, "observe-after/"+request.role)
			return call, err
		},
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			events = append(events, "native/"+request.role)
			answer := approvedCritiqueJSON
			if request.role == roleMain {
				answer = proposalJSON("Complete answer", "")
			}
			return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, nil
		},
	}
	checkpoints := runnerExecutionCheckpoints{
		before: func(value CallCheckpoint) error {
			if value.Version != ProtocolVersion || value.Identity != job.Identity || value.SourceRef != job.Source.Head {
				t.Fatalf("before-call binding changed: %+v", value)
			}
			role, phase := job.Main, ""
			if value.Call.Role == roleCritic {
				role, phase = *job.Critic, reviewFull
			}
			want := CallUsage{Provider: role.Provider, Role: value.Call.Role, Round: 1, ReviewPhase: phase, Model: role.Model, Effort: role.Effort}
			if value.Call != want {
				t.Fatalf("before-call facts contain unpinned metadata or returned evidence: got=%+v want=%+v", value.Call, want)
			}
			events = append(events, "before/"+value.Call.Role)
			return nil
		},
		after: func(value CallCheckpoint) error {
			if value.Version != ProtocolVersion || value.Identity != job.Identity || value.SourceRef != job.Source.Head || value.Call.Tokens != tokens || !value.Call.Complete {
				t.Fatalf("after-call lost binding or returned usage: %+v", value)
			}
			critic := value.Call.Role == roleCritic
			if value.Call.Approved != critic || value.Call.NoBlockers != critic || value.Call.HasBlockers || value.Call.ReviewComplete != critic {
				t.Fatalf("after-call lost parsed verdict claims: %+v", value.Call)
			}
			calls = append(calls, value.Call)
			events = append(events, "after/"+value.Call.Role)
			return nil
		},
		review: func(value ReviewCheckpoint) error {
			if value.Version != ProtocolVersion || value.Identity != job.Identity || value.SourceRef != job.Source.Head || !slices.Equal(value.Calls, calls) || len(value.Refutations) != 0 {
				t.Fatalf("review checkpoint changed the captured prefix: %+v", value)
			}
			events = append(events, fmt.Sprintf("review/%d/%s", len(value.Calls), value.CriticStopReason))
			return nil
		},
	}
	result, err := runner.Execute(t.Context(), job, checkpoints)
	wantEvents := []string{
		"review/0/", "observe-before/main", "before/main", "native/main", "after/main", "observe-after/main", "review/1/",
		"observe-before/critic", "before/critic", "native/critic", "after/critic", "observe-after/critic", "review/2/", "review/2/approved",
	}
	if err != nil || result.Answer != "Complete answer" || result.CriticStopReason != "approved" || !slices.Equal(result.Calls, calls) || !slices.Equal(events, wantEvents) {
		t.Fatalf("execution changed checkpoint order: result=%+v error=%v events=%v", result, err, events)
	}
}

func TestRunnerExecuteCallCheckpointFailurePreservesReturnedFacts(t *testing.T) {
	requireActionProcessGroup(t)
	checkpointErr, nativeErr := errors.New("checkpoint failed"), errors.New("native failed")
	for _, test := range []struct {
		name, role string
		before     bool
		nativeErr  error
		wantCalls  int
	}{
		{name: "before main", role: roleMain, before: true},
		{name: "before critic", role: roleCritic, before: true, wantCalls: 1},
		{name: "after main", role: roleMain, wantCalls: 1},
		{name: "after critic", role: roleCritic, wantCalls: 2},
		{name: "after failed critic", role: roleCritic, nativeErr: nativeErr, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			tokens := usage.Tokens{Input: 13, Output: 4}
			var captured []CallUsage
			nativeCalls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				nativeCalls++
				answer := approvedCritiqueJSON
				if request.role == roleMain {
					answer = proposalJSON("Candidate", "")
				}
				var err error
				if request.role == test.role {
					err = test.nativeErr
				}
				return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, err
			}}
			checkpoints := runnerExecutionCheckpoints{
				before: func(value CallCheckpoint) error {
					if test.before && value.Call.Role == test.role {
						return checkpointErr
					}
					return nil
				},
				after: func(value CallCheckpoint) error {
					captured = append(captured, value.Call)
					if !test.before && value.Call.Role == test.role {
						return checkpointErr
					}
					return nil
				},
			}
			result, err := runner.Execute(t.Context(), job, checkpoints)
			if !errors.Is(err, checkpointErr) || errors.Is(err, monitor.ErrTerminalJob) == test.before || nativeCalls != test.wantCalls || len(result.Calls) != test.wantCalls || !slices.Equal(result.Calls, captured) || result.Answer != "" || result.CriticStopReason != "" {
				t.Fatalf("checkpoint failure continued or erased paid evidence: result=%+v captured=%+v calls=%d error=%v", result, captured, nativeCalls, err)
			}
			if test.nativeErr != nil && !errors.Is(err, test.nativeErr) {
				t.Fatalf("checkpoint failure erased native error: %v", err)
			}
			if !test.before {
				last := result.Calls[len(result.Calls)-1]
				parsed := test.role == roleCritic && test.nativeErr == nil
				if last.Tokens != tokens || !last.Complete || last.Approved != parsed || last.NoBlockers != parsed || last.HasBlockers || last.ReviewComplete != parsed {
					t.Fatalf("post-call failure changed raw usage/verdict: %+v", last)
				}
			}
		})
	}
}

func TestRunnerExecuteStopsAfterPaidReviewCheckpointFailure(t *testing.T) {
	requireActionProcessGroup(t)
	job := withClaudeCritic(testJob(command.Comment))
	reviewErr := errors.New("paid review checkpoint failed")
	tokens := usage.Tokens{Input: 23, CachedInput: 7, Output: 4}
	var captured []CallUsage
	var reviewed []int
	nativeCalls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		nativeCalls++
		if nativeCalls != 1 || request.role != roleMain {
			t.Fatal("critic dispatched after paid review checkpoint failure")
		}
		return aicli.Result{Answer: proposalJSON("Candidate answer", ""), Tokens: tokens, UsageComplete: true}, nil
	}}
	result, err := runner.Execute(t.Context(), job, runnerExecutionCheckpoints{
		after: func(value CallCheckpoint) error {
			captured = append(captured, value.Call)
			return nil
		},
		review: func(value ReviewCheckpoint) error {
			reviewed = append(reviewed, len(value.Calls))
			if value.CriticStopReason != "" {
				t.Fatal("review decision reached after paid review checkpoint failure")
			}
			if len(value.Calls) == 0 {
				return nil
			}
			if len(value.Calls) != 1 || !slices.Equal(value.Calls, captured) {
				t.Fatalf("review checkpoint lost the paid main receipt: %+v", value)
			}
			return reviewErr
		},
	})
	if !errors.Is(err, reviewErr) || !errors.Is(err, monitor.ErrTerminalJob) || nativeCalls != 1 || !slices.Equal(reviewed, []int{0, 1}) || len(result.Calls) != 1 || !slices.Equal(result.Calls, captured) || result.Answer != "" || result.CriticStopReason != "" {
		t.Fatalf("paid review checkpoint failure continued or erased evidence: result=%+v captured=%+v calls=%d reviews=%v error=%v", result, captured, nativeCalls, reviewed, err)
	}
	if call := result.Calls[0]; call.Role != roleMain || call.Tokens != tokens || !call.Complete {
		t.Fatalf("paid review checkpoint failure changed the returned receipt: %+v", call)
	}
}

func TestRunnerExecuteAfterCallRunsAfterNativeCancellation(t *testing.T) {
	requireActionProcessGroup(t)
	job := testJob(command.Comment)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tokens := usage.Tokens{Input: 17, CachedInput: 2, Output: 3}
	var captured []CallUsage
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		cancel()
		return aicli.Result{Tokens: tokens}, context.Canceled
	}}
	result, err := runner.Execute(ctx, job, runnerExecutionCheckpoints{after: func(value CallCheckpoint) error {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("fixture did not cancel before AfterCall")
		}
		captured = append(captured, value.Call)
		return nil
	}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || len(captured) != 1 || !slices.Equal(result.Calls, captured) || captured[0].Tokens != tokens || captured[0].Complete || result.Answer != "" {
		t.Fatalf("cancellation discarded returned paid facts: result=%+v captured=%+v error=%v", result, captured, err)
	}
}

func TestRunnerExecuteObserverCancellationSkipsCallCheckpoints(t *testing.T) {
	requireActionProcessGroup(t)
	job := testJob(command.Comment)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := false
	runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t), aroundModel: func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		observed = true
		cancel()
		return next(ctx, request)
	}}
	result, err := runner.Execute(ctx, job, runnerExecutionCheckpoints{
		before: func(CallCheckpoint) error {
			t.Fatal("BeforeCall ran after observer cancellation")
			return nil
		},
		after: func(CallCheckpoint) error {
			t.Fatal("AfterCall ran without native dispatch")
			return nil
		},
	})
	if !observed || !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 || result.Answer != "" || result.CriticStopReason != "" {
		t.Fatalf("observer cancellation crossed the call checkpoint boundary: result=%+v observed=%t error=%v", result, observed, err)
	}
}
