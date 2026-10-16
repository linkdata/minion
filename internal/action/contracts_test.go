package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestResultRejectsChangesOverlappingOmittedSource(t *testing.T) {
	for _, name := range []string{"private/link", "PRIVATE/link", "private/link/file", "private"} {
		t.Run(strings.ReplaceAll(name, "/", "-"), func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Source.Omitted = []string{"private/link"}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
				Changes: []workspace.Change{{Path: name, Mode: workspace.Regular, Data: []byte("replacement")}}}
			if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "omitted source path") {
				t.Fatalf("change %q validation=%v", name, err)
			}
		})
	}
}

func TestRunnerOmittedSourceWriteIsTerminal(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Omitted = []string{"hidden"}
	runner := Runner{WorkRoot: t.TempDir(), RunCodex: func(_ context.Context, dir, prompt string, _ bool, _ string) (codexcli.Result, error) {
		if !strings.Contains(prompt, "Do not write omitted paths") {
			t.Fatal("prompt permits writing omitted source")
		}
		if err := os.WriteFile(filepath.Join(dir, "hidden"), []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		return codexcli.Result{Answer: "Done."}, nil
	}}
	if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "omitted source path") {
		t.Fatalf("omitted write must not rerun the model: %v", err)
	}
}

func TestResultValidatesChangedPathsAgainstOriginalSource(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "Foo/stay", Mode: workspace.Regular}}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
		Changes: []workspace.Change{{Path: "foo/new", Mode: workspace.Regular}}}
	if err := result.Validate(job); err == nil {
		t.Fatal("accepted directory case collision against unchanged source")
	}
	result.Changes = append([]workspace.Change{{Path: "Foo/stay", Delete: true}}, result.Changes...)
	if err := result.Validate(job); err != nil {
		t.Fatalf("rejected directory case rename: %v", err)
	}
}
