package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
)

func TestRunnerCreditsGitImportCommandsBeforeModel(t *testing.T) {
	root := t.TempDir()
	gitEvents := 0
	modelStarted := false
	var callbackErr error
	runner := Runner{WorkRoot: root, statusActivity: func() {
		paths, err := filepath.Glob(filepath.Join(root, "minion-*"))
		if err != nil || len(paths) != 1 {
			callbackErr = fmt.Errorf("source workspace: paths=%v error=%v", paths, err)
			return
		}
		if _, err := os.Stat(filepath.Join(paths[0], ".git", "HEAD")); err == nil {
			gitEvents++
		}
	}, runModel: func(context.Context, modelRun) (aicli.Result, error) {
		modelStarted = true
		return aicli.Result{Answer: proposalJSON("Done.", "")}, nil
	}}
	if _, err := runner.Run(t.Context(), testJob(command.Comment)); err != nil || callbackErr != nil || !modelStarted || gitEvents < 6 {
		t.Fatalf("Git import missed progress: model=%t events=%d callback=%v error=%v", modelStarted, gitEvents, callbackErr, err)
	}
}
