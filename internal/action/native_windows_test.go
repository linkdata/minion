//go:build windows

package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/executil"
)

func TestNativeWindowsActionMaterializesGitAndRunsCommand(t *testing.T) {
	if os.Getenv("MINION_WINDOWS_MODEL_HELPER") == "1" {
		fmt.Print("native model answer")
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	git, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
		calls++
		data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
		if err != nil || string(data) != "source\n" {
			t.Fatalf("materialized source=%q error=%v", data, err)
		}
		check := exec.CommandContext(ctx, git, "-C", request.dir, "rev-parse", "HEAD")
		commit, err := check.Output()
		if err != nil || strings.TrimSpace(string(commit)) != job.Source.Head {
			t.Fatalf("materialized Git head=%q error=%v", commit, err)
		}
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNativeWindowsActionMaterializesGitAndRunsCommand$")
		cmd.Dir = request.dir
		cmd.Env = append(os.Environ(), "MINION_WINDOWS_MODEL_HELPER=1")
		var output bytes.Buffer
		cmd.Stdout = &output
		if err := executil.Run(cmd); err != nil {
			return aicli.Result{}, err
		}
		return aicli.Result{Answer: output.String(), UsageComplete: true}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || calls != 1 || strings.TrimSpace(result.Answer) != "native model answer" {
		t.Fatalf("native action result=%q calls=%d error=%v", result.Answer, calls, err)
	}
	if entries, err := os.ReadDir(runner.WorkRoot); err != nil || len(entries) != 0 {
		t.Fatalf("completed native action left workspaces: %v, %v", entries, err)
	}
}

func TestNativeWindowsRunnerCleansJunctionAfterModelFailure(t *testing.T) {
	job := testJob(command.Comment)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		link := filepath.Join(request.dir, "linked")
		if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			return aicli.Result{}, fmt.Errorf("create model junction: %w: %s", err, output)
		}
		return aicli.Result{}, errors.New("fixture model failure")
	}}
	if result, err := runner.Run(t.Context(), job); err == nil || !strings.Contains(err.Error(), "fixture model failure") {
		t.Fatalf("hostile model failure result=%+v error=%v", result, err)
	}
	if entries, err := os.ReadDir(runner.WorkRoot); err != nil || len(entries) != 0 {
		t.Fatalf("rejected native action left workspaces: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched junction target: %q, %v", data, err)
	}
}
