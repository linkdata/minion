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
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/wintest"
	"github.com/linkdata/minion/internal/workspace"
)

func TestNativeWindowsActionMaterializesGitAndRunsCommand(t *testing.T) {
	if helper := os.Getenv("MINION_WINDOWS_MODEL_HELPER"); helper != "" {
		if helper != string(command.Comment) && helper != string(command.Fix) {
			t.Fatalf("unexpected model helper mode %q", helper)
		}
		dir, err := os.Getwd()
		if expected := os.Getenv("MINION_WINDOWS_MODEL_HELPER_DIR"); err != nil || expected == "" || !strings.EqualFold(filepath.Clean(dir), filepath.Clean(expected)) {
			t.Fatalf("unexpected model helper directory %q, expected %q: %v", dir, expected, err)
		}
		if helper == string(command.Fix) {
			if err := os.WriteFile("source.txt", []byte("fixed\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Print("native model answer")
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("reject inherited helper", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command(binary, "-test.run=^TestNativeWindowsActionMaterializesGitAndRunsCommand$/^$")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "MINION_WINDOWS_MODEL_HELPER=fix", "MINION_WINDOWS_MODEL_HELPER_DIR=")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unexpected model helper directory") {
			t.Fatalf("inherited helper result: %v: %s", err, out)
		}
		if _, err := os.Lstat(filepath.Join(dir, "source.txt")); !os.IsNotExist(err) {
			t.Fatalf("inherited helper wrote into work directory: %v", err)
		}
	})
	git, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []command.Action{command.Comment, command.Fix} {
		t.Run(string(action), func(t *testing.T) {
			job := testJob(action)
			calls := 0
			workRoot := t.TempDir()
			runner := Runner{WorkRoot: workRoot, runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
				calls++
				data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
				if err != nil || string(data) != "source\n" {
					t.Fatalf("materialized source=%q error=%v", data, err)
				}
				check := exec.CommandContext(ctx, git, "-C", request.dir, "rev-parse", "HEAD")
				check.Env = testsupport.GitEnvironment(t.TempDir())
				check.Stderr = os.Stderr
				commit, err := check.Output()
				if err != nil || strings.TrimSpace(string(commit)) != job.Source.Head {
					t.Fatalf("materialized Git head=%q error=%v", commit, err)
				}
				cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNativeWindowsActionMaterializesGitAndRunsCommand$")
				cmd.Dir = request.dir
				cmd.Env = append(os.Environ(), "MINION_WINDOWS_MODEL_HELPER="+string(action), "MINION_WINDOWS_MODEL_HELPER_DIR="+request.dir)
				var output bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = os.Stderr
				if err := executil.Run(cmd); err != nil {
					return aicli.Result{}, fmt.Errorf("native model helper: %w; stdout: %q", err, output.String())
				}
				return aicli.Result{Answer: output.String(), UsageComplete: true}, nil
			}}
			result, err := runner.Run(t.Context(), job)
			if err != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "native model answer" || result.Identity != job.Identity || result.SourceRef != job.Source.Head {
				t.Fatalf("native action result=%+v calls=%d error=%v", result, calls, err)
			}
			if action == command.Comment && len(result.Changes) != 0 {
				t.Fatalf("native comment changes=%+v", result.Changes)
			}
			if action == command.Fix && (len(result.Changes) != 1 || result.Changes[0].Path != "source.txt" || result.Changes[0].Mode != workspace.Regular || string(result.Changes[0].Data) != "fixed\n") {
				t.Fatalf("native fix changes=%+v", result.Changes)
			}
			assertEmptyDir(t, workRoot)
		})
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
		if err := wintest.CreateJunction(link, outside); err != nil {
			return aicli.Result{}, err
		}
		return aicli.Result{}, errors.New("fixture model failure")
	}}
	if result, err := runner.Run(t.Context(), job); err == nil || !strings.Contains(err.Error(), "fixture model failure") {
		t.Fatalf("hostile model failure result=%+v error=%v", result, err)
	}
	assertEmptyDir(t, runner.WorkRoot)
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched junction target: %q, %v", data, err)
	}
}
