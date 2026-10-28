package action

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type usageCheckpoint struct {
	record           usage.Record
	previousComplete bool
	save             func() error
	logf             func(string, ...any)
}

func (c *usageCheckpoint) beforeCall(call CallUsage) error {
	calls := &c.record.CodexCalls
	if call.Provider == "claude" {
		calls = &c.record.ClaudeCalls
	}
	c.previousComplete = c.record.UsageComplete
	*calls++
	c.record.UsageComplete = false
	if err := c.save(); err != nil {
		*calls--
		c.record.UsageComplete = c.previousComplete
		return err
	}
	return nil
}

func (c *usageCheckpoint) afterCall(call CallUsage) error {
	tokens, valid := c.record.Tokens.AddChecked(call.Tokens)
	c.record.Tokens = tokens
	c.record.UsageComplete = c.previousComplete && call.Complete && valid
	c.record.UsageSource = executorClaimOrigin
	if !valid {
		c.logf("action usage counters invalid or overflowed; retaining previous reported totals for attempt %s", c.record.ID)
	}
	if err := c.save(); err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	return nil
}

// trackUsage adds accounting checkpoints to this attempt-scoped receiver.
func (e *Orchestrator) trackUsage(job monitor.Job) (func(error) error, error) {
	startedAt := time.Now().UTC()
	if job.AttemptID != "" {
		previous, found, err := e.Usage.Load(job.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("load action usage %s: %w", job.AttemptID, err)
		}
		if found {
			if previous.JobID != job.ID || previous.CodexCalls != 0 || previous.ClaudeCalls != 0 {
				return nil, fmt.Errorf("%w: attempt %s already has model usage or a different job", monitor.ErrTerminalJob, job.AttemptID)
			}
			startedAt = previous.StartedAt
		}
	}
	checkpoint := &usageCheckpoint{logf: e.logf, record: usage.Record{
		ID: e.attemptID, JobID: job.ID, Repo: job.Repository.Name,
		User: job.Source.Author.Login, Action: cmp.Or(job.Command.Name, string(job.Command.Action)), Number: job.Number,
		SourceURL: sourceURL(job), Provider: e.mainProvider(), CriticProvider: e.Adversarial,
		StartedAt: startedAt, Status: "running", UsageComplete: true,
	}}
	record := &checkpoint.record
	main := e.roleSettings(record.Provider, nil)
	record.Model, record.Effort = cmp.Or(main.Model, "default"), main.Effort
	if record.CriticProvider != "" {
		critic := e.roleSettings(record.CriticProvider, nil)
		record.CriticModel, record.CriticEffort = cmp.Or(critic.Model, "default"), critic.Effort
	}
	checkpoint.save = func() error {
		if err := e.Usage.Save(checkpoint.record); err != nil {
			return fmt.Errorf("save action usage %s: %w", record.ID, err)
		}
		return nil
	}
	if err := checkpoint.save(); err != nil {
		return nil, err
	}
	e.usageCheckpoint = checkpoint
	finish := func(actionErr error) error {
		finished := time.Now().UTC()
		record.FinishedAt = &finished
		record.Status = "succeeded"
		if actionErr != nil {
			record.Status = "failed"
		}
		if err := checkpoint.save(); err != nil {
			if actionErr == nil {
				e.logf("final action usage checkpoint failed: %q", err)
				return nil
			}
			return errors.Join(actionErr, err)
		}
		return actionErr
	}
	return finish, nil
}

// CompleteTerminalUsage reconciles an existing attempt before terminal state is removed.
func (e *Orchestrator) CompleteTerminalUsage(job monitor.Job, outcome string, at time.Time) {
	if e.Usage == nil || job.AttemptID == "" {
		return
	}
	record, found, err := e.Usage.Load(job.AttemptID)
	if err != nil {
		e.logf("load recovered action usage checkpoint failed: %q", err)
		return
	}
	if !found {
		return
	}
	if record.JobID != job.ID {
		e.logf("recovered action usage attempt %s belongs to another job", job.AttemptID)
		return
	}
	status := "failed"
	if outcome == "succeeded" {
		status = "succeeded"
	}
	if record.Status == status {
		return
	}
	finished := at.UTC()
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	if finished.Before(record.StartedAt) {
		finished = record.StartedAt
	}
	if record.FinishedAt != nil && finished.Before(*record.FinishedAt) {
		finished = *record.FinishedAt
	}
	record.FinishedAt, record.Status = &finished, status
	if err := e.Usage.Save(record); err != nil {
		e.logf("final recovered action usage checkpoint failed: %q", err)
	}
}
