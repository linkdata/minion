package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/workspace"
)

type reviewSnapshot struct {
	source, outputDir string
	liveOutputDir     string
	base              []workspace.File
	index             *gitwork.OfflineIndex
	ownedSource       bool
	changes           []workspace.Change
	artifacts         []workspace.File
}

// Freeze outside the main model's writable grants so background work cannot change the critic's input.
func freezeReviewSnapshot(ctx context.Context, workRoot string, job Job, source, outputDir string, index *gitwork.OfflineIndex) (snapshot *reviewSnapshot, resultErr error) {
	snapshot = &reviewSnapshot{source: source, liveOutputDir: outputDir, base: job.Source.Files, index: index}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, snapshot.close())
			snapshot = nil
		}
	}()
	var err error
	if job.Action == command.Fix {
		if snapshot.changes, err = index.Changes(ctx); err != nil {
			return snapshot, err
		}
		if err := job.validateChanges(snapshot.changes); err != nil {
			return snapshot, err
		}
		if snapshot.source, err = os.MkdirTemp(workRoot, "minion-review-"); err != nil {
			return snapshot, err
		}
		snapshot.ownedSource = true
		if err := workspace.MaterializeChanges(snapshot.source, job.Source.Files, snapshot.changes); err != nil {
			return snapshot, err
		}
	}
	if outputDir != "" {
		if snapshot.artifacts, _, err = readAuditArtifacts(outputDir, auditForReview); err != nil {
			return snapshot, err
		}
		if snapshot.outputDir, err = os.MkdirTemp(workRoot, ".minion-review-reports-"); err != nil {
			return snapshot, err
		}
		if err := workspace.Materialize(snapshot.outputDir, snapshot.artifacts); err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func (s *reviewSnapshot) verify(ctx context.Context) error {
	if s.ownedSource {
		if err := workspace.CheckChangesUnchanged(ctx, s.source, s.base, s.changes); err != nil {
			return fmt.Errorf("frozen candidate source changed during critic review: %w", err)
		}
		changes, err := s.index.Changes(ctx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(s.changes, changes) {
			return errors.New("candidate source changed during critic review")
		}
	}
	if s.outputDir != "" {
		if err := workspace.CheckUnchanged(ctx, s.outputDir, s.artifacts); err != nil {
			return fmt.Errorf("frozen candidate reports changed during critic review: %w", err)
		}
		files, _, err := readAuditArtifacts(s.liveOutputDir, auditForReview)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(s.artifacts, files) {
			return errors.New("candidate reports changed during critic review")
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
