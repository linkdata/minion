package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
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

func TestOrchestratorWaitCapacityDelegatesWithoutExposingSecrets(t *testing.T) {
	if err := (&Orchestrator{}).WaitCapacity(t.Context()); err != nil {
		t.Fatalf("orchestrator without an executor required readiness: %v", err)
	}
	const secret = "selected-worker-secret"
	cause := errors.New("worker unavailable")
	worker := &readinessExecutor{err: fmt.Errorf("%w: %s", cause, secret)}
	owner := &Orchestrator{Executor: worker, Runner: Runner{Codex: config.CodexConfig{APIKey: secret}}}
	err := owner.WaitCapacity(t.Context())
	if worker.calls != 1 || !errors.Is(err, cause) || strings.Contains(err.Error(), secret) {
		t.Fatalf("readiness was bypassed or leaked credentials: calls=%d error=%v", worker.calls, err)
	}
}
