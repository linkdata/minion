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
	modelSettings := func(provider string) (model, effort string) {
		model, effort = e.Codex.Model, e.Codex.ReasoningEffort
		if provider == "claude" {
			model, effort = e.Claude.Model, e.Claude.Effort
		}
		if model == "" {
			model = "default"
		}
		return
	}
	record := usage.Record{
		ID: hex.EncodeToString(id[:]), JobID: job.ID, Repo: job.Repository.Name,
		User: job.Source.Author.Login, Action: name, Number: job.Number,
		SourceURL: sourceURL(job), Provider: e.mainProvider(), CriticProvider: e.Adversarial,
		StartedAt: time.Now().UTC(), Status: "running", UsageComplete: true,
	}
	record.Model, record.Effort = modelSettings(record.Provider)
	if record.CriticProvider != "" {
		record.CriticModel, record.CriticEffort = modelSettings(record.CriticProvider)
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
	tracked.aroundModel = func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
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
