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

func loadPrivateReview(root string) (retainedReview, error) {
	paths, err := filepath.Glob(filepath.Join(root, "reviews", "*.json"))
	if err != nil || len(paths) != 1 {
		return retainedReview{}, fmt.Errorf("private records=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		return retainedReview{}, err
	}
	var record retainedReview
	if err := json.Unmarshal(data, &record); err != nil {
		return retainedReview{}, err
	}
	for path, want := range map[string]os.FileMode{paths[0]: 0600, filepath.Dir(paths[0]): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			return retainedReview{}, fmt.Errorf("private mode %s: %v error=%v", path, info, err)
		}
	}
	return record, nil
}

func readPrivateReview(t *testing.T, root string) retainedReview {
	t.Helper()
	record, err := loadPrivateReview(root)
	if err != nil {
		t.Fatal(err)
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
			job := withClaudeCritic(testJob(command.Comment))
			job.Identity.Repository = "o/r"
			job.CriticNonblockingRounds = 1
			result := privateReviewResult(job)
			if !test.approved {
				result.Calls[len(result.Calls)-1].Approved = false
				result.CriticStopReason = "nonblocking"
			}
			root := t.TempDir()
			var body, journal bytes.Buffer
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record, err := loadPrivateReview(root)
				if err != nil {
					t.Error(err)
					http.Error(w, "private review unavailable", http.StatusInternalServerError)
					return
				}
				if record.ReviewClaimOrigin != "executor_reported" || len(record.Refutations) != 1 || len(record.Calls) == 0 || record.Calls[len(record.Calls)-1].Approved != test.approved || !record.Calls[len(record.Calls)-1].ReviewComplete || record.CriticStopReason != result.CriticStopReason || record.CriticNonblockingRounds != 1 || record.CriticConsecutiveNoBlockerRounds != job.CriticConsecutiveNoBlockerRounds || record.SourceRef != job.Source.Head {
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
			if writes != 1 || !strings.Contains(body.String(), result.Answer) || !strings.Contains(journal.String(), "call_claim_origin=executor_reported") || strings.Contains(body.String(), "PRIVATE") || strings.Contains(journal.String(), "PRIVATE") {
				t.Fatalf("private evidence leaked or publication lost: writes=%d body=%s journal=%s", writes, &body, &journal)
			}
		})
	}
}

func TestPrivateReviewRetainsPrefixAndRejectsRollback(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	// Returned evidence does not require this provider skill on the orchestrator.
	job.Main.Skills = []string{"https://github.com/example/skills/tree/pinned/skills/uninstalled"}
	result := privateReviewResult(job)
	e := Orchestrator{WorkRoot: t.TempDir()}
	first := result
	first.CriticStopReason = ""
	first.Calls, first.Refutations = first.Calls[:3], first.Refutations[:1]
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
	for _, refs := range [][]Refutation{nil, {{Round: 2, Text: "Replacement interpretation"}}} {
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

func TestPrivateReviewRejectsRewrittenCoverage(t *testing.T) {
	for _, reviewComplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved review complete=%t", reviewComplete), func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			result.Calls, result.Refutations = result.Calls[:2], nil
			result.Answer, result.CriticStopReason = "", ""
			result.Calls[1].ReviewComplete = reviewComplete
			if err := result.validate(job, false); err != nil {
				t.Fatalf("invalid saved prefix: %v", err)
			}
			orchestrator := Orchestrator{WorkRoot: t.TempDir()}
			if err := orchestrator.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(orchestrator.WorkRoot, "reviews", job.Identity.AttemptID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := result
			changed.Calls = append([]CallUsage(nil), result.Calls...)
			changed.Calls[1].ReviewComplete = !reviewComplete
			if err := changed.validate(job, false); err != nil {
				t.Fatalf("invalid replacement prefix: %v", err)
			}
			if err := orchestrator.retainReviewEvidence(job, changed); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "changed or discarded a saved prefix") {
				t.Fatalf("saved coverage was rewritten: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || readPrivateReview(t, orchestrator.WorkRoot).Calls[1].ReviewComplete != reviewComplete {
				t.Fatalf("rejected coverage update changed retained evidence: %s error=%v", after, err)
			}
		})
	}
}

func TestPrivateReviewRecordsDecisionAfterVerdictCheckpoint(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	orchestrator := Orchestrator{WorkRoot: t.TempDir()}
	initial := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
	if err := orchestrator.retainReviewEvidence(job, initial); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, orchestrator.WorkRoot); saved.ReviewClaimOrigin != "" {
		t.Fatalf("metadata-only checkpoint claimed a worker verdict: %+v", saved)
	}
	beforeDecision := result
	beforeDecision.CriticStopReason = ""
	if err := orchestrator.retainReviewEvidence(job, beforeDecision); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, orchestrator.WorkRoot); saved.ReviewClaimOrigin != "executor_reported" || saved.CriticStopReason != "" || !saved.Calls[len(saved.Calls)-1].Approved || !saved.Calls[len(saved.Calls)-1].ReviewComplete {
		t.Fatalf("verdict checkpoint fabricated or lost evidence: %+v", saved)
	}
	if err := orchestrator.retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, orchestrator.WorkRoot); saved.ReviewClaimOrigin != "executor_reported" || saved.CriticStopReason != "approved" {
		t.Fatalf("decision with unchanged call prefix was not persisted: %+v", saved)
	}
	for _, reason := range []string{"", "nonblocking"} {
		changed := result
		changed.CriticStopReason = reason
		if err := orchestrator.retainReviewEvidence(job, changed); err == nil {
			t.Fatalf("saved decision changed from approved to %q", reason)
		}
	}
	if saved := readPrivateReview(t, orchestrator.WorkRoot); saved.CriticStopReason != "approved" {
		t.Fatalf("saved decision changed after rejected update: %+v", saved)
	}
}

func TestPrivateReviewContentDigestExtendsDecisionOnce(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	e := Orchestrator{WorkRoot: t.TempDir()}
	digest, err := contentDigest(result.Answer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveReviewEvidence(e.WorkRoot, job, result, digest, nil); err == nil {
		t.Fatal("review content digest created evidence without a saved decision")
	}
	if err := e.retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	if err := saveReviewEvidence(e.WorkRoot, job, result, digest, nil); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, e.WorkRoot); saved.ContentDigest != digest {
		t.Fatalf("decision digest was not retained: %+v", saved)
	}
	path := filepath.Join(e.WorkRoot, "reviews", job.Identity.AttemptID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.retainReviewEvidence(job, result); err != nil {
		t.Fatalf("final result retention discarded its acknowledged digest: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("identical result rewrote the content digest: %v", err)
	}
	if err := saveReviewEvidence(e.WorkRoot, job, result, strings.Repeat("b", 64), nil); err == nil {
		t.Fatal("review evidence replaced its content digest")
	}
}

func TestPrivateReviewBoundaryBeforeStorageOrPublication(t *testing.T) {
	for _, scenario := range []string{"foreign identity", "foreign source", "malformed history", "oversized history", "invalid calls", "invalid answer", "read-only source changes", "storage failure", "public root", "symlink root"} {
		t.Run(scenario, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
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
				result.Refutations[0].Round = 3
			case "oversized history":
				result.Refutations[0].Text = strings.Repeat("x", maxOutputBytes+1)
			case "invalid calls":
				result.Calls[0].Tokens.Input = -1
			case "invalid answer":
				result.Answer, safeHistory = "", true
			case "read-only source changes":
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
			}
			if err := e.publish(context.Background(), &preparedAction{job: job}, result); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("unsafe publication was not stopped: %v", err)
			}
			if scenario == "storage failure" {
				if data, err := os.ReadFile(root); err != nil || string(data) != "blocked" {
					t.Fatalf("blocking state file changed: %q,%v", data, err)
				}
				return
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
			e := attemptFixture(&Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), Main: "codex", Adversarial: "claude", CriticStopping: config.CriticStopping{NonblockingAfterRound: new(int)},
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Runner: Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if calls > 1 {
						record := readPrivateReview(t, root)
						wantRefs := 0
						if calls > 3 {
							wantRefs = 1
						}
						if len(record.Calls) != calls-1 || len(record.Refutations) != wantRefs {
							t.Fatalf("earlier round was not durable before dispatch: %+v", record)
						}
					}
					if calls == 1 {
						return aicli.Result{Answer: proposalJSON("Public answer", "")}, nil
					}
					if calls == 2 {
						return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"PRIVATE_CRITIC_FINDING"}`}, nil
					}
					if calls == 3 {
						return aicli.Result{Answer: proposalJSON("Public answer", "PRIVATE_LATER_REFUTATION")}, nil
					}
					if outcome == "storage failure" {
						if err := os.Chmod(filepath.Join(root, "reviews"), 0500); err != nil {
							t.Fatal(err)
						}
						return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"PRIVATE_CRITIC_FINDING"}`}, nil
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
				if !panicked || calls != 4 {
					t.Fatalf("interruption not exercised: panic=%t calls=%d", panicked, calls)
				}
			} else if outcome == "initial storage failure" {
				if !errors.Is(runErr, monitor.ErrTerminalJob) || calls != 0 {
					t.Fatalf("initial storage failure reached a model: calls=%d error=%v", calls, runErr)
				}
				return
			} else if outcome == "storage failure" {
				if !errors.Is(runErr, monitor.ErrTerminalJob) || !errors.Is(runErr, os.ErrPermission) || calls != 4 {
					t.Fatalf("checkpoint failure allowed another call: calls=%d error=%v", calls, runErr)
				}
				if got := readPrivateReview(t, root); len(got.Refutations) != 1 || len(got.Calls) != 3 {
					t.Fatalf("checkpoint failure destroyed the previous durable prefix: %+v", got)
				}
				return
			} else if !errors.Is(runErr, failure) || calls != 4 {
				t.Fatalf("original provider failure lost: calls=%d error=%v", calls, runErr)
			}
			if outcome == "provider and storage failure" && (!errors.Is(runErr, os.ErrPermission) || !errors.Is(runErr, monitor.ErrTerminalJob)) {
				t.Fatalf("retention failure lost joined causes: %v", runErr)
			}
			if outcome == "provider failure" && errors.Is(runErr, monitor.ErrTerminalJob) {
				t.Fatalf("retained provider outage became terminal: %v", runErr)
			}
			wantCalls := 3
			if outcome == "provider failure" {
				wantCalls = 4
			}
			record := readPrivateReview(t, root)
			if len(record.Refutations) != 1 || record.Refutations[0].Text != "PRIVATE_LATER_REFUTATION" || len(record.Calls) != wantCalls || !record.Calls[1].HasBlockers || !record.Calls[1].ReviewComplete {
				t.Fatalf("failed/interrupted action lost earlier evidence: %+v", record)
			}
		})
	}
}
