package action

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

// trackUsage adds a record and runner wrapper to this attempt-scoped receiver.
func (e *Orchestrator) trackUsage(job monitor.Job) (func(error) error, error) {
	name := job.Command.Name
	if name == "" {
		name = string(job.Command.Action)
	}
	record := usage.Record{
		ID: e.attemptID, JobID: job.ID, Repo: job.Repository.Name,
		User: job.Source.Author.Login, Action: name, Number: job.Number,
		SourceURL: sourceURL(job), Provider: e.mainProvider(), CriticProvider: e.Adversarial,
		StartedAt: time.Now().UTC(), Status: "running", UsageComplete: true,
	}
	main := e.roleSettings(record.Provider, nil)
	record.Model, record.Effort = cmp.Or(main.Model, "default"), main.Effort
	if record.CriticProvider != "" {
		critic := e.roleSettings(record.CriticProvider, nil)
		record.CriticModel, record.CriticEffort = cmp.Or(critic.Model, "default"), critic.Effort
	}
	save := func() error {
		if err := e.Usage.Save(record); err != nil {
			return fmt.Errorf("save action usage %s: %w", record.ID, err)
		}
		return nil
	}
	if err := save(); err != nil {
		return nil, err
	}
	e.Runner.aroundModel = func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		if err := ctx.Err(); err != nil {
			return aicli.Result{}, err
		}
		calls := &record.CodexCalls
		if request.provider == "claude" {
			calls = &record.ClaudeCalls
		}
		complete := record.UsageComplete
		*calls++
		record.UsageComplete = false
		if err := save(); err != nil {
			*calls--
			record.UsageComplete = complete
			return aicli.Result{}, err
		}
		result, err := next(ctx, request)
		tokens, valid := record.Tokens.AddChecked(result.Tokens)
		record.Tokens = tokens
		record.UsageComplete = complete && result.UsageComplete && valid
		if !valid {
			e.logf("action usage counters invalid or overflowed; retaining lower bound for attempt %s", record.ID)
		}
		if saveErr := save(); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", monitor.ErrTerminalJob, saveErr))
		}
		return result, err
	}
	finish := func(actionErr error) error {
		finished := time.Now().UTC()
		record.FinishedAt = &finished
		record.Status = "succeeded"
		if actionErr != nil {
			record.Status = "failed"
		}
		if err := save(); err != nil {
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
