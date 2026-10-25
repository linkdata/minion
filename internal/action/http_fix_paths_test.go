package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPFixPathsCannotPublish(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
	source := filepath.Join(filepath.Dir(remote), "source")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, name, path, want string) {
		t.Run(name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveFixFlowFixture(t, w, r) || serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			workRoot := t.TempDir()
			traversalTarget := filepath.Join(workRoot, "outside")
			if err := os.WriteFile(traversalTarget, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			e := &Orchestrator{
				WorkRoot: workRoot, Store: openState(t, time.Now()), Usage: &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")},
				GitHub: testGitHub(server, "token"), BotLogin: "bot",
				Commands: map[string]config.CommandMapping{"fix": {Action: string(command.Fix), Skills: []string{skills.BuiltinAntiPromptInjection}}},
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				job.Source.BaseFiles = nil // Empty issue comparison is omitted on the HTTP wire.
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				before := f.call(1)
				f.ack(t, "/before", before, http.StatusOK, 1)
				call := before.Checkpoint.Call
				call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 2}, true
				f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
				result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Safe response.", Calls: []CallUsage{call},
					Changes: []workspace.Change{{Path: path, Mode: workspace.Regular, Data: []byte("hostile\n")}}}
				if err := httpResultShape(result); err != nil {
					t.Fatalf("hostile result failed wire shape: %v", err)
				}
				outcome := f.outcome(3, nil)
				outcome.Result = result
				f.ack(t, "/outcome", outcome, http.StatusOK, 3)
				got := f.result(t)
				if got.err != nil {
					t.Fatalf("receiver rejected shape-valid outcome: %v", got.err)
				}
				return got.result, nil
			})
			request := usageJob()
			request.Source.UpdatedAt = time.Now()
			request.Command = command.Command{Name: "fix", Action: command.Fix}
			request.Repository.Actions = []string{"fix"}
			err := e.Execute(testActionContext(t), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || writes.Load() != 0 {
				t.Fatalf("hostile path reached publication: error=%v writes=%d", err, writes.Load())
			}
			for _, target := range []string{outside, traversalTarget} {
				data, readErr := os.ReadFile(target)
				if readErr != nil || string(data) != "untouched" {
					t.Fatalf("outside sentinel %q changed: %q, %v", target, data, readErr)
				}
			}
			refs, refErr := exec.Command(git, "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/minion/").Output()
			if refErr != nil || len(refs) != 0 {
				t.Fatalf("hostile fix pushed a branch: refs=%q error=%v", refs, refErr)
			}
			entries, readErr := os.ReadDir(workRoot)
			if readErr != nil || len(entries) != 1 || entries[0].Name() != "outside" {
				t.Fatalf("checkout was not cleaned up: %v, %v", entries, readErr)
			}
		})
	}

	run(t, "traversal", "../outside", "unsafe or duplicate changed path")
	run(t, "absolute", outside, "unsafe or duplicate changed path")
	run(t, "Git metadata", ".git/config", "unsafe or duplicate changed path")
	run(t, "Git metadata alias", ".git./config", "unsafe or duplicate changed path")

	t.Run("omitted symlink", func(t *testing.T) {
		linkTarget := t.TempDir()
		if err := os.WriteFile(filepath.Join(linkTarget, "outside"), []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		symlinkOrSkip(t, linkTarget, filepath.Join(source, "link"))
		gitEnv := testsupport.GitEnvironment(t.TempDir())
		for _, args := range [][]string{
			{"-C", source, "add", "--", "link"},
			{"-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "omitted symlink"},
			{"-C", source, "push", remote, "HEAD:refs/heads/main"},
		} {
			cmd := exec.Command(git, args...)
			cmd.Env = gitEnv
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %s: %v", args, output, err)
			}
		}
		run(t, "descendant", "link/outside", "collision")
		data, err := os.ReadFile(filepath.Join(linkTarget, "outside"))
		if err != nil || string(data) != "untouched" {
			t.Fatalf("symlink target changed: %q, %v", data, err)
		}
	})
}
