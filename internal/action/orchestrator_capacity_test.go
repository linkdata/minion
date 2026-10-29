package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

type readinessExecutor struct {
	err   error
	calls int
}

func (*readinessExecutor) Execute(context.Context, Job, Checkpoints) (Result, error) {
	panic("capacity test must not execute a Job")
}

func (e *readinessExecutor) WaitCapacity(context.Context) error {
	e.calls++
	return e.err
}

func TestOrchestratorWaitCapacityRequiresReadyExecutor(t *testing.T) {
	for _, test := range []struct {
		name     string
		executor Executor
	}{
		{name: "missing executor"},
		{name: "executor without readiness", executor: executorFunc(func(context.Context, Job, Checkpoints) (Result, error) {
			panic("capacity test must not execute a Job")
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := (&Orchestrator{Executor: test.executor}).WaitCapacity(t.Context()); !errors.Is(err, monitor.ErrAcceptanceState) {
				t.Fatalf("unavailable readiness was accepted: %v", err)
			}
		})
	}
}

func TestOrchestratorWaitCapacityDelegatesWithoutExposingSecrets(t *testing.T) {
	const secret = "selected-worker-secret"
	cause := errors.New("worker unavailable")
	worker := &readinessExecutor{err: fmt.Errorf("%w: %s", cause, secret)}
	owner := &Orchestrator{Executor: worker, Runner: Runner{Codex: config.CodexConfig{APIKey: secret}}}
	err := owner.WaitCapacity(t.Context())
	if worker.calls != 1 || !errors.Is(err, cause) || strings.Contains(err.Error(), secret) {
		t.Fatalf("readiness was bypassed or leaked credentials: calls=%d error=%v", worker.calls, err)
	}
}
