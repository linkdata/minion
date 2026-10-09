package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

// trackUsage gives each action attempt its own record and runner wrapper.
func (e *Executor) trackUsage(job monitor.Job) (*Executor, func(error) error, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, nil, fmt.Errorf("create usage ID: %w", err)
	}
	record := usage.Record{
		ID: hex.EncodeToString(id[:]), JobID: job.ID, Repo: job.Repository.Name,
		User: job.Source.Author.Login, Action: string(job.Command.Action), Number: job.Number,
		SourceURL: sourceURL(job), Model: e.Codex.Model, Effort: e.Codex.ReasoningEffort,
		StartedAt: time.Now().UTC(), Status: "running", UsageComplete: true,
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
	run := e.RunCodex
	if run == nil {
		run = func(ctx context.Context, dir, prompt string, writable bool, outputDir string) (codexcli.Result, error) {
			return codexcli.Run(ctx, dir, prompt, writable, outputDir, e.Codex)
		}
	}
	tracked := *e
	tracked.RunCodex = func(ctx context.Context, dir, prompt string, writable bool, outputDir string) (codexcli.Result, error) {
		complete := record.UsageComplete
		record.CodexCalls++
		record.UsageComplete = false
		if err := save(); err != nil {
			record.CodexCalls--
			record.UsageComplete = complete
			return codexcli.Result{}, err
		}
		result, err := run(ctx, dir, prompt, writable, outputDir)
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
