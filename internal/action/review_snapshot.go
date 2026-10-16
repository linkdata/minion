package action

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

type reviewSnapshot struct {
	source, outputDir string
	ownedSource       bool
	changes           []workspace.Change
	artifacts         []workspace.File
}

// Freeze outside the main model's writable grants so background work cannot change the critic's input.
func freezeReviewSnapshot(ctx context.Context, workRoot string, job Job, source, outputDir string, index *fixIndex) (snapshot *reviewSnapshot, resultErr error) {
	snapshot = &reviewSnapshot{source: source}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, snapshot.close())
			snapshot = nil
		}
	}()
	var err error
	if job.Action == command.Fix {
		if snapshot.changes, err = reviewChanges(ctx, job, index); err != nil {
			return snapshot, err
		}
		if snapshot.source, err = os.MkdirTemp(workRoot, "minion-review-"); err != nil {
			return snapshot, err
		}
		snapshot.ownedSource = true
		if err := workspace.Materialize(snapshot.source, job.Source.Files); err != nil {
			return snapshot, err
		}
		if err := workspace.Apply(snapshot.source, snapshot.changes); err != nil {
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

func (s *reviewSnapshot) verify(ctx context.Context, job Job, outputDir string, index *fixIndex) error {
	if job.Action == command.Fix {
		changes, err := reviewChanges(ctx, job, index)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(s.changes, changes) {
			return errors.New("candidate source changed during critic review")
		}
	}
	if outputDir != "" {
		files, _, err := readAuditArtifacts(outputDir, auditForReview)
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

func reviewChanges(ctx context.Context, job Job, index *fixIndex) ([]workspace.Change, error) {
	if index == nil {
		return nil, errors.New("fix critic snapshot requires the offline index")
	}
	changes, err := index.changes(ctx)
	if err != nil {
		return nil, err
	}
	if err := job.validateChanges(changes); err != nil {
		return nil, err
	}
	return changes, nil
}
