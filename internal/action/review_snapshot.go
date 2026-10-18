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
}

// Freeze outside the main model's writable grants. The caller owns the partial snapshot even on failure.
func freezeReviewSnapshot(ctx context.Context, workRoot string, job Job, source, outputDir string, index *gitwork.OfflineIndex) (*reviewSnapshot, error) {
	snapshot := &reviewSnapshot{}
	return snapshot, snapshot.refresh(ctx, workRoot, job, source, outputDir, index)
}

// Refresh frozen bytes at stable private paths only between model calls.
func (s *reviewSnapshot) refresh(ctx context.Context, workRoot string, job Job, source, outputDir string, index *gitwork.OfflineIndex) error {
	s.base = job.Source.Files
	var err error
	if job.Action == command.Fix {
		if s.changes, err = index.Changes(ctx); err != nil {
			return err
		}
		if err := job.validateChanges(s.changes); err != nil {
			return err
		}
		if !s.ownedSource {
			if s.source, err = os.MkdirTemp(workRoot, "minion-review-"); err != nil {
				return err
			}
			s.ownedSource = true
		} else {
			if err := cleanup.RemoveAll(s.source); err != nil {
				return err
			}
			if err := os.Mkdir(s.source, 0700); err != nil {
				return err
			}
		}
		if err := workspace.MaterializeChanges(s.source, job.Source.Files, s.changes); err != nil {
			return err
		}
	} else {
		s.source = source
	}
	if outputDir != "" {
		if s.artifacts, _, err = readAuditArtifacts(outputDir, false); err != nil {
			return err
		}
		if s.outputDir == "" {
			if s.outputDir, err = os.MkdirTemp(workRoot, ".minion-review-reports-"); err != nil {
				return err
			}
		} else {
			if err := cleanup.RemoveAll(s.outputDir); err != nil {
				return err
			}
			if err := os.Mkdir(s.outputDir, 0700); err != nil {
				return err
			}
		}
		if err := workspace.Materialize(s.outputDir, s.artifacts); err != nil {
			return err
		}
	}
	return nil
}

func (s *reviewSnapshot) verify(ctx context.Context) error {
	if s.ownedSource {
		if err := workspace.CheckChangesUnchanged(ctx, s.source, s.base, s.changes); err != nil {
			return fmt.Errorf("frozen candidate source changed during critic review: %w", err)
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
