package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workroot"
)

func TestExecuteCriticAdmissionUsesControlledWorkRoot(t *testing.T) {
	for _, critic := range []bool{false, true} {
		for _, unsafe := range []bool{false, true} {
			t.Run(fmt.Sprintf("critic_%t/unsafe_%t", critic, unsafe), func(t *testing.T) {
				t.Setenv("PATH", gitOnlyPath(t))
				gitFixtureRemote(t)
				root, other := filepath.Join(t.TempDir(), "controlled"), t.TempDir()
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				root, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				failure := errors.New("provider stopped")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !serveIssueFixture(t, w, r) {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				e := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}, Runner: Runner{WorkRoot: other, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if filepath.Dir(request.dir) != root {
						t.Fatalf("runner ignored orchestrator placement: %s", request.dir)
					}
					return aicli.Result{}, failure
				}}}
				if critic {
					e.Adversarial = "claude"
				}
				if unsafe {
					e.Runner.Codex.ReadOnlyDirs = []string{"/"}
				}
				request := usageJob()
				request.Command = command.Command{Name: "comment", Action: command.Comment}
				request.Repository.Actions = []string{"comment"}
				selectFixtureRunner(t, e, &request)
				err = e.Execute(t.Context(), request)
				if unsafe {
					if errors.Is(err, workroot.ErrUnsafe) || !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "model evidence directories must be selected by Minion") || calls != 0 {
						t.Fatalf("admission classification: calls=%d error=%v", calls, err)
					}
					if _, err := os.Stat(filepath.Join(root, "reviews")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused admission wrote private evidence: %v", err)
					}
				} else if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("safe controlled root did not dispatch: calls=%d error=%v", calls, err)
				}
				entries, readErr := os.ReadDir(root)
				wantEntries := []string{"worker-diagnostics"}
				if !unsafe && critic {
					wantEntries = append([]string{"reviews"}, wantEntries...)
				}
				if readErr != nil || len(entries) != len(wantEntries) {
					t.Fatalf("controlled root retained execution paths: %v,%v", entries, readErr)
				}
				for i, name := range wantEntries {
					if entries[i].Name() != name || !entries[i].IsDir() {
						t.Fatalf("controlled root retained execution paths: %v", entries)
					}
				}
				otherEntries, readErr := os.ReadDir(other)
				if readErr != nil || len(otherEntries) != 0 {
					t.Fatalf("unused Runner root changed: %v,%v", otherEntries, readErr)
				}
			})
		}
	}
}

func TestReviewEvidenceInitialCheckpointIsNotTerminalBeforeModels(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	checkpoints := 0
	runner := Runner{WorkRoot: t.TempDir(), recordReview: func(receipts []CallUsage, refs []Refutation, stopReason string) error {
		checkpoints++
		if len(receipts) != 0 || len(refs) != 0 || stopReason != "" {
			t.Fatalf("initial checkpoint carried model evidence: calls=%+v refutations=%+v stop=%q", receipts, refs, stopReason)
		}
		return &os.PathError{Op: "write", Path: "private-review", Err: syscall.ENOSPC}
	}, runModel: forbidModel(t)}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, monitor.ErrTerminalJob) || checkpoints != 1 || len(result.Calls) != 0 || len(result.Refutations) != 0 || result.Answer != "" || result.CriticStopReason != "" {
		t.Fatalf("initial checkpoint policy: checkpoints=%d result=%+v error=%v", checkpoints, result, err)
	}
}

func TestPrivateReviewRejectsChangedSavedCheckpoint(t *testing.T) {
	for _, kind := range []string{"corrupt", "identity", "source", "policy", "max rounds", "consecutive rounds", "stop reason", "calls", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			e := Orchestrator{WorkRoot: t.TempDir()}
			if err := e.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.WorkRoot, "reviews", job.Identity.AttemptID+".json")
			saved := readPrivateReview(t, e.WorkRoot)
			outside := filepath.Join(t.TempDir(), "outside")
			data := []byte("{")
			switch kind {
			case "identity":
				saved.Identity.RequesterID++
			case "source":
				saved.SourceRef = strings.Repeat("b", 40)
			case "policy":
				saved.CriticNonblockingRounds++
			case "max rounds":
				saved.CriticMaxRounds++
			case "consecutive rounds":
				saved.CriticConsecutiveNoBlockerRounds++
			case "stop reason":
				saved.CriticStopReason = "nonblocking"
			case "calls":
				saved.Calls[1].HasBlockers, saved.Calls[1].NoBlockers = false, true
			}
			if kind != "corrupt" {
				var err error
				data, err = json.Marshal(saved)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" || kind == "hardlink" {
				writeFixtureFile(t, filepath.Dir(outside), filepath.Base(outside), data)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if kind == "hardlink" {
					link = os.Link
				}
				if err := link(outside, path); err != nil {
					if kind == "symlink" && runtime.GOOS == "windows" {
						t.Skipf("Windows symlink fixture is unavailable: %v", err)
					}
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			var outsideBefore os.FileInfo
			if kind == "symlink" || kind == "hardlink" {
				outsideBefore, err = os.Stat(outside)
				if err != nil {
					t.Fatal(err)
				}
			}
			err = e.retainReviewEvidence(job, result)
			if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("tampered checkpoint accepted: %v", err)
			}
			if kind == "corrupt" && !strings.Contains(err.Error(), "saved review evidence is invalid") {
				t.Fatalf("corruption lost: %v", err)
			}
			if kind == "identity" || kind == "source" || kind == "policy" || kind == "max rounds" || kind == "calls" {
				if !strings.Contains(err.Error(), "changed or discarded a saved prefix") {
					t.Fatalf("checkpoint binding lost: %v", err)
				}
			}
			if kind == "hardlink" && !strings.Contains(err.Error(), "exactly one hard link") {
				t.Fatalf("checkpoint hardlink lost single-link cause: %v", err)
			}
			if after, statErr := os.Lstat(path); statErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("refused checkpoint was replaced or rewritten: %v", statErr)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, data) {
				t.Fatalf("refused checkpoint bytes changed: %q,%v", got, readErr)
			}
			if kind == "symlink" && runtime.GOOS != "windows" {
				if !errors.Is(err, syscall.ELOOP) {
					t.Fatalf("checkpoint symlink lost no-follow cause: %v", err)
				}
			}
			if kind == "symlink" || kind == "hardlink" {
				if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, data) {
					t.Fatalf("outside checkpoint touched: %q,%v", got, err)
				}
				if after, err := os.Stat(outside); err != nil || !os.SameFile(outsideBefore, after) || !outsideBefore.ModTime().Equal(after.ModTime()) {
					t.Fatalf("outside checkpoint replaced or rewritten: %v", err)
				}
			}
		})
	}
}
