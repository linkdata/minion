package action

import (
	"context"
	"errors"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestCanceledSelectedAuditDoesNotSaveUnavailableValidatorResult(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	f, store, _ := selectedHTTPResultFixtureForJob(t, job, nil)
	outcome := completeHTTPResult(t, f)
	audit := completeAuditResult(f.job)
	outcome.Result.Answer, outcome.Result.Artifacts = audit.Answer, audit.Artifacts
	if err := outcome.Result.Validate(f.job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // The parent has canceled before the completed result reaches validation.
	sink := f.tx.sink.(*executionCheckpoints)
	sink.assignmentContext = ctx
	err := sink.ReceiveCompletedResult(outcome.Result)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errAcceptedResultSavedForRetry) || errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("canceled validator became a publication retry: %v", err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("canceled validator saved a publishable result: present=%t error=%v", present, err)
	}
}
