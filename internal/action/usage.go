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
	record usage.Record
	save   func() error
	logf   func(string, ...any)
}

func usageCall(ordinal int, call CallUsage) usage.Call {
	return usage.Call{Ordinal: ordinal, Provider: call.Provider, Model: cmp.Or(call.Model, "default"), Effort: call.Effort,
		Role: call.Role, Round: call.Round, ReviewPhase: call.ReviewPhase, Tokens: call.Tokens,
		EstimatedCostUSD:      call.EstimatedCostUSD,
		EstimatedCostComplete: call.EstimatedCostComplete,
		UsageCoverage:         call.UsageCoverage, Complete: call.Complete}
}

func (c *usageCheckpoint) beforeCall(ordinal int, call CallUsage) error {
	entry := usageCall(ordinal, call)
	entry.StartedAt = time.Now().UTC()
	previous := c.record
	added, err := c.record.BeginCall(entry)
	if err != nil || !added {
		return err
	}
	if err := c.save(); err != nil {
		c.record = previous
		return err
	}
	return nil
}

func (c *usageCheckpoint) discardBeforeCall(ordinal int) error {
	if err := c.record.DiscardPendingCall(ordinal); err != nil {
		return err
	}
	if len(c.record.Calls) == 0 {
		c.record.ExecutorID, c.record.SourceRef = "", ""
	}
	return c.save()
}

func (c *usageCheckpoint) afterCall(ordinal int, call CallUsage) error {
	entry := usageCall(ordinal, call)
	finished := time.Now().UTC()
	entry.FinishedAt = &finished
	_, valid := c.record.Tokens.AddChecked(call.Tokens)
	added, err := c.record.FinishCall(entry)
	if err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	if added && !valid {
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
		RepositoryID: job.Repository.ID, User: job.Source.Author.Login, RequesterID: job.Source.Author.ID,
		Action: cmp.Or(job.Command.Name, string(job.Command.Action)), Number: job.Number,
		SourceURL: sourceURL(job), Provider: e.mainProvider(), CriticProvider: e.Adversarial,
		StartedAt: startedAt, Status: "running", UsageCoverage: usage.Coverage{Scope: "attempt"}, UsageComplete: true,
	}}
	record := &checkpoint.record
	main := e.mainRoleSettings(nil)
	record.Model, record.Effort = cmp.Or(main.Model, "default"), main.Effort
	if record.CriticProvider != "" {
		critic := e.criticRoleSettings()
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
		saveFinal := func() error {
			if e.finalUsageEntered != nil {
				e.finalUsageEntered()
			}
			latest, found, err := e.Usage.Load(record.ID)
			if err != nil {
				return fmt.Errorf("load final action usage %s: %w", record.ID, err)
			}
			if !found || latest.JobID != record.JobID {
				return fmt.Errorf("final action usage %s belongs to another attempt", record.ID)
			}
			if len(latest.Calls) > len(record.Calls) {
				last := latest.Calls[len(latest.Calls)-1]
				if last.FinishedAt == nil {
					// A failed BeforeCall save may have replaced the file without sending an ACK.
					if err := latest.DiscardPendingCall(last.Ordinal); err != nil {
						return fmt.Errorf("discard unacknowledged final usage call %s: %w", record.ID, err)
					}
					if len(latest.Calls) == 0 {
						latest.ExecutorID, latest.SourceRef = "", ""
					}
				}
			}
			for _, call := range record.Calls {
				if call.FinishedAt == nil {
					continue
				}
				if _, err := latest.FinishCall(call); err != nil {
					return fmt.Errorf("reconcile final action usage %s: %w", record.ID, err)
				}
			}
			*record = latest
			finished := time.Now().UTC()
			if finished.Before(record.StartedAt) {
				finished = record.StartedAt
			}
			if record.FinishedAt != nil && finished.Before(*record.FinishedAt) {
				finished = *record.FinishedAt
			}
			record.FinishedAt = &finished
			record.Status = "succeeded"
			if actionErr != nil {
				record.Status = "failed"
			}
			if e.finalUsageSave != nil {
				if err := e.finalUsageSave(*record); err != nil {
					return fmt.Errorf("save action usage %s: %w", record.ID, err)
				}
				return nil
			}
			return checkpoint.save()
		}
		var err error
		if e.Store != nil {
			if e.finalUsageWaiting != nil {
				e.finalUsageWaiting()
			}
			err = e.Store.WithPrivateRecords(saveFinal)
		} else {
			err = saveFinal()
		}
		if err != nil {
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
	if e.Store != nil {
		_ = e.Store.WithPrivateRecords(func() error {
			e.completeTerminalUsage(job, outcome, at)
			return nil
		})
		return
	}
	e.completeTerminalUsage(job, outcome, at)
}

func (e *Orchestrator) completeTerminalUsage(job monitor.Job, outcome string, at time.Time) {
	if e.Usage == nil || job.AttemptID == "" {
		return
	}
	record, found, err := e.Usage.Load(job.AttemptID)
	if err != nil {
		e.logf("load action usage checkpoint failed: %q", err)
		return
	}
	if !found {
		return
	}
	if record.JobID != job.ID {
		e.logf("action usage attempt %s belongs to another job", job.AttemptID)
		return
	}
	if !setTerminalUsage(&record, outcome, at) {
		return
	}
	if err := e.Usage.Save(record); err != nil {
		e.logf("final action usage checkpoint failed: %q", err)
	}
}

func setTerminalUsage(record *usage.Record, outcome string, at time.Time) bool {
	status := "failed"
	if outcome == "succeeded" {
		status = "succeeded"
	}
	if record.Status == status {
		return false
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
	return true
}
