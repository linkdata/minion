package action

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/workspace"
)

type reviewSnapshot struct {
	source, outputDir string
	base              []workspace.File
	ownedSource       bool
	changes           []workspace.Change
	artifacts         []workspace.File
	repository        *gitwork.Repository
	refresh           func(context.Context) error
}

// Freeze outside the main model's writable grants. The caller owns the partial snapshot even on failure.
func freezeReviewSnapshot(ctx context.Context, workRoot string, job Job, source, outputDir string, index *gitwork.OfflineIndex) (*reviewSnapshot, error) {
	s := &reviewSnapshot{base: job.Source.Files}
	// Bind the action once; refresh only frozen bytes between model calls.
	s.refresh = func(ctx context.Context) error {
		var err error
		if job.Action == command.Fix {
			if s.changes, err = index.Changes(ctx); err != nil {
				return err
			}
			if err := job.validateChanges(s.changes); err != nil {
				return err
			}
			if s.source, err = resetReviewDirectory(s.source, workRoot, "minion-review-"); err != nil {
				return err
			}
			s.ownedSource = true
			if err := workspace.MaterializeChanges(s.source, job.Source.Files, s.changes); err != nil {
				return err
			}
			if s.repository, err = gitwork.MaterializeRepository(ctx, s.source, job.Source); err != nil {
				return err
			}
		} else {
			s.source = source
		}
		if outputDir != "" {
			if s.artifacts, _, err = readAuditArtifacts(outputDir, false); err != nil {
				return err
			}
			if s.outputDir, err = resetReviewDirectory(s.outputDir, workRoot, ".minion-review-reports-"); err != nil {
				return err
			}
			if err := workspace.Materialize(s.outputDir, s.artifacts); err != nil {
				return err
			}
		}
		return nil
	}
	return s, s.refresh(ctx)
}

func resetReviewDirectory(path, workRoot, prefix string) (string, error) {
	if path == "" {
		return os.MkdirTemp(workRoot, prefix)
	}
	if err := cleanup.RemoveAll(path); err != nil {
		return path, err
	}
	return path, os.Mkdir(path, 0700)
}

func (s *reviewSnapshot) verify(ctx context.Context) error {
	if s.ownedSource {
		if err := workspace.CheckChangesUnchanged(ctx, s.source, s.base, s.changes); err != nil {
			return fmt.Errorf("frozen candidate source changed during critic review: %w", err)
		}
	}
	if s.repository != nil {
		if err := s.repository.Verify(ctx); err != nil {
			return fmt.Errorf("frozen candidate repository changed during critic review: %w", err)
		}
	}
	if s.outputDir != "" {
		if err := workspace.CheckUnchanged(ctx, s.outputDir, s.artifacts); err != nil {
			return fmt.Errorf("frozen candidate reports changed during critic review: %w", err)
		}
	}
	return nil
}

func (s *reviewSnapshot) close() error {
	var err error
	if s.ownedSource {
		err = cleanup.RemoveAll(s.source)
	}
	if s.outputDir != "" {
		err = errors.Join(err, cleanup.RemoveAll(s.outputDir))
	}
	return err
}
