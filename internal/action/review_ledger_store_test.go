package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

func privateReviewResult(job Job) Result {
	return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Public answer.",
		Calls: []CallUsage{
			{Provider: job.Main.Provider, Role: roleMain, Round: 1},
			{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true},
		},
		Refutations: []Refutation{{Round: 1, Text: "PRIVATE_MAIN_REFUTATION"}},
	}
}

func readPrivateReview(t *testing.T, root string) retainedReview {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "reviews", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("private records=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record retainedReview
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{paths[0]: 0600, filepath.Dir(paths[0]): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("private mode %s: %v error=%v", path, info, err)
		}
	}
	return record
}

func TestPublishRetainsPrivateReviewBeforeGitHubWrites(t *testing.T) {
	for _, test := range []struct {
		name     string
		approved bool
		status   int
	}{{"approved", true, http.StatusCreated}, {"nonblocking", false, http.StatusCreated}, {"publication failure", true, http.StatusBadGateway}} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			job.Identity.Repository = "o/r"
			job.CriticNonblockingRounds = 1
			result := privateReviewResult(job)
			if !test.approved {
				result.Calls[1].Approved = false
			}
			root := t.TempDir()
			var body, journal bytes.Buffer
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record := readPrivateReview(t, root)
				if len(record.Refutations) != 1 || record.Calls[1].Approved != test.approved || record.CriticNonblockingRounds != 1 || record.SourceRef != job.Source.Head {
					t.Errorf("private evidence/policy lost: %+v", record)
				}
				if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r" {
					_, _ = fmt.Fprint(w, publicRepositoryJSON)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments" {
					writes++
					var payload struct{ Body string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					body.WriteString(payload.Body)
					w.WriteHeader(test.status)
					_, _ = fmt.Fprint(w, `{"id":1}`)
					return
				}
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			e := Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), Logger: log.New(&journal, "", 0)}
			request := monitor.Job{ID: job.Identity.JobID, Number: 7, SubjectType: "Issue", Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Source: github.Item{Author: github.User{Login: "writer"}}, Command: command.Command{Name: "comment", Action: command.Comment}}
			publishErr := e.publish(context.Background(), &preparedAction{job: job, request: request}, result)
			if test.status == http.StatusCreated {
				if publishErr != nil {
					t.Fatal(publishErr)
				}
			} else if !errors.Is(publishErr, monitor.ErrPostModelPublication) {
				t.Fatalf("publication failure cause lost: %v", publishErr)
			}
			if writes != 1 || !strings.Contains(body.String(), result.Answer) || strings.Contains(body.String(), "PRIVATE") || strings.Contains(journal.String(), "PRIVATE") {
				t.Fatalf("private evidence leaked or publication lost: writes=%d body=%s journal=%s", writes, &body, &journal)
			}
		})
	}
}

func TestPrivateReviewRetainsPrefixAndRejectsRollback(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	// Returned evidence does not require this provider skill on the orchestrator.
	job.Main.Skills = []string{"https://github.com/example/skills/tree/pinned/skills/uninstalled"}
	result := privateReviewResult(job)
	e := Orchestrator{WorkRoot: t.TempDir()}
	first := result
	first.Calls, first.Refutations = first.Calls[:1], first.Refutations[:1]
	if err := e.retainReviewEvidence(job, first); err != nil {
		t.Fatal(err)
	}
	if err := e.retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.WorkRoot, "reviews", job.Identity.AttemptID+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("identical final evidence rewrote the checkpoint")
	}
	if err := e.retainReviewEvidence(job, first); err == nil {
		t.Fatal("saved evidence was rolled back")
	}
	for _, refs := range [][]Refutation{nil, {{Round: 1, Text: "Replacement interpretation"}}} {
		changed := result
		changed.Refutations = refs
		if err := e.retainReviewEvidence(job, changed); err == nil {
			t.Fatal("saved refutation was removed or rewritten")
		}
	}
	if got := readPrivateReview(t, e.WorkRoot); len(got.Refutations) != 1 || got.Refutations[0].Text != "PRIVATE_MAIN_REFUTATION" {
		t.Fatalf("saved prefix was lost: %+v", got)
	}
}

func TestPrivateReviewBoundaryBeforeStorageOrPublication(t *testing.T) {
	for _, scenario := range []string{"foreign identity", "foreign source", "malformed history", "oversized history", "invalid calls", "invalid answer", "invalid changes", "storage failure", "public root", "symlink root", "grant overlap"} {
		t.Run(scenario, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			result := privateReviewResult(job)
			root := t.TempDir()
			e := Orchestrator{WorkRoot: root}
			safeHistory := false
			switch scenario {
			case "foreign identity":
				result.Identity.AttemptID = "foreign"
			case "foreign source":
				result.SourceRef = strings.Repeat("b", 40)
			case "malformed history":
				result.Refutations[0].Round = 2
			case "oversized history":
				result.Refutations[0].Text = strings.Repeat("x", maxOutputBytes+1)
			case "invalid calls":
				result.Calls[0].Tokens.Input = -1
			case "invalid answer":
				result.Answer, safeHistory = "", true
			case "invalid changes":
				result.Changes, safeHistory = []workspace.Change{{Path: "../escape", Delete: true}}, true
			case "storage failure":
				root = filepath.Join(t.TempDir(), "blocked")
				e.WorkRoot = root
				if err := os.WriteFile(root, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public root":
				if err := os.MkdirAll(filepath.Join(root, "reviews"), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink root":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "reviews")); err != nil {
					t.Fatal(err)
				}
			case "grant overlap":
				e.Runner.Claude.ReadOnlyDirs = []string{filepath.Dir(root)}
			}
			if err := e.publish(context.Background(), &preparedAction{job: job}, result); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("unsafe publication was not stopped: %v", err)
			}
			if safeHistory {
				if got := readPrivateReview(t, root); len(got.Refutations) != 1 {
					t.Fatal("invalid public output erased valid private history")
				}
			} else if paths, err := filepath.Glob(filepath.Join(root, "reviews", "*.json")); err != nil || len(paths) != 0 {
				t.Fatalf("untrusted/unretained history written: %v error=%v", paths, err)
			}
			switch scenario {
			case "foreign identity", "foreign source", "malformed history", "oversized history", "invalid calls":
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("untrusted evidence touched storage: %v", err)
				}
			}
		})
	}
}

func TestPerformCheckpointsPrivateReviewBeforeLaterCalls(t *testing.T) {
	for _, outcome := range []string{"provider failure", "panic", "storage failure", "provider and storage failure", "initial storage failure"} {
		t.Run(outcome, func(t *testing.T) {
			if os.Geteuid() == 0 && (outcome == "storage failure" || outcome == "provider and storage failure") {
				t.Skip("root bypasses read-only directory permissions")
			}
			gitFixtureRemote(t)
			root := t.TempDir()
			if outcome == "initial storage failure" {
				if err := os.WriteFile(filepath.Join(root, "reviews"), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "reviews"), 0700) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !serveIssueFixture(t, w, r) {
					t.Errorf("unexpected publication/request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			failure := errors.New("provider stopped")
			calls := 0
			e := attemptFixture(&Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), Main: "codex", Adversarial: "claude", CriticNonblockingRounds: new(int),
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Runner: Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if calls > 1 {
						record := readPrivateReview(t, root)
						if len(record.Calls) != calls-1 || len(record.Refutations) != 1 {
							t.Fatalf("earlier round was not durable before dispatch: %+v", record)
						}
					}
					if calls == 1 {
						return aicli.Result{Answer: proposalJSON("Public answer", "PRIVATE_FIRST_REFUTATION")}, nil
					}
					if calls == 2 {
						if outcome == "storage failure" {
							if err := os.Chmod(filepath.Join(root, "reviews"), 0500); err != nil {
								t.Fatal(err)
							}
						}
						return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"PRIVATE_CRITIC_FINDING"}`}, nil
					}
					if outcome == "panic" {
						panic("simulated interruption")
					}
					if outcome == "provider and storage failure" {
						if err := os.Chmod(filepath.Join(root, "reviews"), 0500); err != nil {
							t.Fatal(err)
						}
					}
					return aicli.Result{}, failure
				}},
			})
			request := usageJob()
			request.Command, request.Repository.Actions = command.Command{Name: "comment", Action: command.Comment}, []string{"comment"}
			panicked := false
			var runErr error
			func() {
				defer func() { panicked = recover() != nil }()
				runErr = e.perform(testActionContext(t), request)
			}()
			if outcome == "storage failure" || outcome == "provider and storage failure" {
				if err := os.Chmod(filepath.Join(root, "reviews"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if outcome == "panic" {
				if !panicked || calls != 3 {
					t.Fatalf("interruption not exercised: panic=%t calls=%d", panicked, calls)
				}
			} else if outcome == "initial storage failure" {
				if !errors.Is(runErr, monitor.ErrTerminalJob) || calls != 0 {
					t.Fatalf("initial storage failure reached a model: calls=%d error=%v", calls, runErr)
				}
				return
			} else if outcome == "storage failure" {
				if !errors.Is(runErr, monitor.ErrTerminalJob) || !errors.Is(runErr, os.ErrPermission) || calls != 2 {
					t.Fatalf("checkpoint failure allowed another call: calls=%d error=%v", calls, runErr)
				}
				if got := readPrivateReview(t, root); len(got.Refutations) != 1 {
					t.Fatal("checkpoint failure destroyed the previous durable prefix")
				}
				return
			} else if !errors.Is(runErr, failure) || calls != 3 {
				t.Fatalf("original provider failure lost: calls=%d error=%v", calls, runErr)
			}
			if outcome == "provider and storage failure" && (!errors.Is(runErr, os.ErrPermission) || !errors.Is(runErr, monitor.ErrTerminalJob)) {
				t.Fatalf("retention failure lost joined causes: %v", runErr)
			}
			wantCalls := 2
			if outcome == "provider failure" {
				wantCalls = 3
			}
			record := readPrivateReview(t, root)
			if len(record.Refutations) != 1 || record.Refutations[0].Text != "PRIVATE_FIRST_REFUTATION" || len(record.Calls) != wantCalls || !record.Calls[1].HasBlockers {
				t.Fatalf("failed/interrupted action lost earlier evidence: %+v", record)
			}
		})
	}
}
