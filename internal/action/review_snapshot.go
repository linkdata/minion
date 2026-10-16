package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

type reviewSnapshot struct {
	source, outputDir string
	changes           []workspace.Change
	artifacts         []workspace.File
}

// Freeze outside the main model's writable grants so background work cannot change the critic's input.
func freezeReviewSnapshot(ctx context.Context, workRoot string, job Job, source, outputDir string, index *fixIndex) (snapshot *reviewSnapshot, resultErr error) {
	snapshot = &reviewSnapshot{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, snapshot.close())
			snapshot = nil
		}
	}()
	var err error
	if snapshot.changes, err = reviewChanges(ctx, job, source, index); err != nil {
		return snapshot, err
	}
	if snapshot.source, err = os.MkdirTemp(workRoot, "minion-review-"); err != nil {
		return snapshot, err
	}
	if err := workspace.Materialize(snapshot.source, job.Source.Files); err != nil {
		return snapshot, err
	}
	if err := workspace.Apply(snapshot.source, snapshot.changes); err != nil {
		return snapshot, err
	}
	if outputDir != "" {
		if snapshot.artifacts, err = readReviewArtifacts(outputDir); err != nil {
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

func (s *reviewSnapshot) verify(ctx context.Context, job Job, source, outputDir string, index *fixIndex) error {
	changes, err := reviewChanges(ctx, job, source, index)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(s.changes, changes) {
		return errors.New("candidate source changed during critic review")
	}
	if outputDir != "" {
		files, err := readReviewArtifacts(outputDir)
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
	for _, dir := range []string{s.source, s.outputDir} {
		if dir != "" {
			err = errors.Join(err, cleanup.RemoveAll(dir))
		}
	}
	return err
}

func reviewChanges(ctx context.Context, job Job, source string, index *fixIndex) ([]workspace.Change, error) {
	if job.Action == command.Fix {
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
	changes, err := workspace.Changes(ctx, source, job.Source.Files)
	if err != nil {
		return nil, err
	}
	if len(changes) != 0 {
		return nil, errors.New("read-only action modified supplied source")
	}
	return nil, nil
}

func readReviewArtifacts(outputDir string) ([]workspace.File, error) {
	var files []workspace.File
	for _, name := range auditArtifacts {
		data, err := readAuditArtifact(filepath.Join(outputDir, name), auditArtifactLimit(name), auditArtifactMayBeEmpty(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("freeze audit %s: %w", name, err)
		}
		files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
	}
	return files, nil
}
