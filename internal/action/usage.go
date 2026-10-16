package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

// trackUsage gives each action attempt its own record and runner wrapper.
func (e *Orchestrator) trackUsage(job monitor.Job) (*Orchestrator, func(error) error, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, nil, fmt.Errorf("create usage ID: %w", err)
	}
	name := job.Command.Name
	if name == "" {
		name = string(job.Command.Action)
	}
	record := usage.Record{
		ID: hex.EncodeToString(id[:]), JobID: job.ID, Repo: job.Repository.Name,
		User: job.Source.Author.Login, Action: name, Number: job.Number,
		SourceURL: sourceURL(job), Provider: e.mainProvider(), CriticProvider: e.Adversarial,
		StartedAt: time.Now().UTC(), Status: "running", UsageComplete: true,
	}
	main := e.roleSettings(record.Provider, nil)
	record.Model, record.Effort = main.Model, main.Effort
	if record.Model == "" {
		record.Model = "default"
	}
	if record.CriticProvider != "" {
		critic := e.roleSettings(record.CriticProvider, nil)
		record.CriticModel, record.CriticEffort = critic.Model, critic.Effort
		if record.CriticModel == "" {
			record.CriticModel = "default"
		}
	}
	save := func() error {
		if err := e.Usage.Save(record); err != nil {
			return fmt.Errorf("save action usage %s: %w", record.ID, err)
		}
		return nil
	}
	if err := save(); err != nil {
		return nil, nil, err
	}
	tracked := *e
	observe := tracked.Runner.aroundModel
	tracked.Runner.aroundModel = func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		model := request.model
		if model == "" {
			model = "default"
		}
		if request.role == "main" {
			record.Provider, record.Model, record.Effort = request.provider, model, request.effort
		} else if request.role == "critic" {
			record.CriticProvider, record.CriticModel, record.CriticEffort = request.provider, model, request.effort
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
		var result aicli.Result
		var err error
		if observe != nil {
			result, err = observe(ctx, request, next)
		} else {
			result, err = next(ctx, request)
		}
		record.Tokens.Add(result.Tokens)
		record.UsageComplete = complete && result.UsageComplete
		return result, errors.Join(err, save())
	}
	finish := func(actionErr error) error {
		finished := time.Now().UTC()
		record.FinishedAt = &finished
		record.Status = "succeeded"
		if actionErr != nil {
			record.Status = "failed"
		}
		return errors.Join(actionErr, save())
	}
	return &tracked, finish, nil
}
