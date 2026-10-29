package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPReviewCannotRetainConfiguredProviderKey(t *testing.T) {
	const key = "fixture-claude-secret"
	owner := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
	owner.owner.Runner.Claude.APIKey = key
	f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
	f.pull(t)
	result := privateReviewResult(owner.job)
	result.Refutations[0].Text = "private fixture-claude-&#x73;ecret"
	sequence := uint64(1)
	f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(nil, nil, "")}, http.StatusOK, sequence)
	sequence++
	for i, call := range result.Calls[:3] {
		f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: owner.call(callMetadata(call))}, http.StatusOK, sequence)
		sequence++
		f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: owner.call(call)}, http.StatusOK, sequence)
		sequence++
		var refs []Refutation
		if i == 2 {
			refs = result.Refutations
		}
		status := http.StatusOK
		if i == 2 {
			status = http.StatusInternalServerError
		}
		ack := f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(result.Calls[:i+1], refs, "")}, status, sequence)
		if i == 2 && (ack.Failure == nil || !errors.Is(ack.Failure.err(), monitor.ErrTerminalJob) || strings.Contains(ack.Failure.err().Error(), key) || strings.Contains(ack.Failure.err().Error(), "&#x73;ecret")) {
			t.Fatalf("key-bearing review was acknowledged or echoed: %+v", ack)
		}
		sequence++
	}
	path := filepath.Join(owner.owner.WorkRoot, "reviews", owner.job.Identity.AttemptID+".json")
	data, err := os.ReadFile(path)
	if err != nil || containsConfiguredKey(data, key) {
		t.Fatalf("rejected review reached durable checkpoint: error=%v", err)
	}
	if saved := readPrivateReview(t, owner.owner.WorkRoot); !slices.Equal(saved.Calls, result.Calls[:2]) || len(saved.Refutations) != 0 {
		t.Fatalf("rejected review replaced accepted prefix: %+v", saved)
	}
	outcome := f.outcome(sequence, errors.New("worker stopped after rejected review"))
	outcome.Result = Result{Version: owner.job.Version, Identity: owner.job.Identity, SourceRef: owner.job.Source.Head, Calls: result.Calls[:3]}
	f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
	got := f.result(t)
	if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "&#x73;ecret") {
		t.Fatalf("rejected review escaped final error: %v", err)
	}
	if err := owner.owner.retainReviewEvidence(owner.job, owner.sink.failureEvidence()); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, owner.owner.WorkRoot); len(saved.Calls) != 3 || len(saved.Refutations) != 0 || saved.CriticStopReason != "" {
		t.Fatalf("failed outcome lost accepted call prefix: %+v", saved)
	}
}

func TestHostileHTTPResultCannotPublishConfiguredProviderKey(t *testing.T) {
	const codexKey = "fixture-codex-secret"
	const claudeKey = "fixture-claude-secret"
	for _, tc := range []struct {
		name, content string
		audit         bool
	}{
		{name: "answer with other provider key", content: "public " + claudeKey},
		{name: "encoded answer key", content: `public fixture-claude-\u0073ecret`},
		{name: "audit mixed safe and HTML-entity artifact", content: "public fixture-codex-&#x73;ecret", audit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.audit {
				installActionTestSkills(t, testAuditSkillURL)
			}
			if tc.audit {
				gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
			} else {
				gitFixtureRemote(t)
			}
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
			name, action, skill := "comment", command.Comment, skills.BuiltinAntiPromptInjection
			if tc.audit {
				name, action, skill = "security-audit", command.SecurityAudit, testAuditSkillURL
			}
			e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotID: 8, BotLogin: "bot",
				Runner:   Runner{Codex: config.CodexConfig{APIKey: codexKey}, Claude: config.ClaudeConfig{APIKey: claudeKey}},
				Commands: map[string]config.CommandMapping{name: {Action: string(action), Skills: []string{skill}}},
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				if tc.audit {
					job.Source.BaseFiles = nil // The issue audit has no base; preserve that across the HTTP JSON round trip.
				}
				admitFixtureAssignment(t, checkpoints, job)
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				before := f.call(1)
				f.ack(t, "/before", before, http.StatusOK, 1)
				call := before.Checkpoint.Call
				call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 2}, true
				f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
				outcome := f.outcome(3, nil)
				outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Safe result", Calls: []CallUsage{call}}
				if tc.audit {
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), outcome.Result.Answer)
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "architecture.md" {
							outcome.Result.Artifacts[i].Data = []byte(tc.content)
						}
					}
				} else {
					outcome.Result.Answer = tc.content
				}
				if err := outcome.Result.Validate(job); err != nil {
					t.Fatalf("hostile result was not otherwise publishable: %v", err)
				}
				if ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 3); ack.Failure == nil {
					t.Fatal("hostile result was acknowledged")
				}
				got := f.result(t)
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: name, Action: action}
			request.Repository.Actions = []string{name}
			selectFixtureRequest(t, e, &request, false)
			err := e.Execute(t.Context(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "configured provider key") || writes.Load() != 0 {
				t.Fatalf("hostile result crossed publication boundary: error=%v writes=%d", err, writes.Load())
			}
			records, err := usage.ReadAll(usageDir)
			if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].Tokens != (usage.Tokens{Input: 3, Output: 2}) {
				t.Fatalf("failure usage evidence missing: records=%+v error=%v", records, err)
			}
			if tc.audit {
				if paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "complete-*")); err != nil || len(paths) != 0 {
					t.Fatalf("hostile result retained as complete audit: paths=%v error=%v", paths, err)
				}
				paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "incomplete-*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("private incomplete audit evidence missing: paths=%v error=%v", paths, err)
				}
				if marker, err := os.ReadFile(filepath.Join(paths[0], "INCOMPLETE.txt")); err != nil || !strings.Contains(string(marker), "must not be published") {
					t.Fatalf("private incomplete audit marker missing: %q error=%v", marker, err)
				}
				files, err := os.ReadDir(paths[0])
				wantFiles := len(auditArtifacts)
				if err != nil || len(files) != wantFiles {
					t.Fatalf("safe partial audit evidence lost: files=%v error=%v want=%d", files, err, wantFiles)
				}
				for _, file := range files {
					data, err := os.ReadFile(filepath.Join(paths[0], file.Name()))
					if err != nil || containsConfiguredKey(data, codexKey) || containsConfiguredKey(data, claudeKey) {
						t.Fatalf("private audit retained a configured key in %q: %v", file.Name(), err)
					}
				}
				if _, err := os.Stat(filepath.Join(paths[0], "REPORT.md")); err != nil {
					t.Fatalf("safe report was lost: %v", err)
				}
				if _, err := os.Stat(filepath.Join(paths[0], "architecture.md")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("safe/unsafe architecture retention differs: %v", err)
				}
			}
		})
	}
}

func TestHostileHTTPFixEntityKeyCannotPublish(t *testing.T) {
	const key = "fixture-secret"
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
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
	usageDir := filepath.Join(t.TempDir(), "usage")
	e := &Orchestrator{
		WorkRoot: t.TempDir(), Store: openState(t, time.Now()), Usage: &usage.Store{Dir: usageDir},
		BotID:  8,
		GitHub: testGitHub(server, "token"), BotLogin: "bot", Runner: Runner{Codex: config.CodexConfig{APIKey: key}},
		Commands: map[string]config.CommandMapping{"fix": {Action: string(command.Fix), Skills: []string{skills.BuiltinAntiPromptInjection}}},
	}
	e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		job.Source.BaseFiles = nil // The issue fix has no base across the HTTP JSON round trip.
		admitFixtureAssignment(t, checkpoints, job)
		f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
		f.pull(t)
		before := f.call(1)
		f.ack(t, "/before", before, http.StatusOK, 1)
		call := before.Checkpoint.Call
		call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 2}, true
		f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Safe response.", Calls: []CallUsage{call},
			Changes: []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("fixture-&#x73;ecret\n")}}}
		if err := result.Validate(job); err != nil {
			t.Fatalf("hostile Fix did not reach publication gate: %v", err)
		}
		outcome := f.outcome(3, nil)
		outcome.Result = result
		if ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 3); ack.Failure == nil {
			t.Fatal("hostile result was acknowledged")
		}
		got := f.result(t)
		return got.result, got.err
	})
	request := usageJob()
	request.Source.UpdatedAt = time.Now()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	request.Repository.Actions = []string{"fix"}
	selectFixtureRequest(t, e, &request, false)
	err := e.Execute(testActionContext(t), request)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "configured provider key") || writes.Load() != 0 {
		t.Fatalf("entity-encoded key crossed publication boundary: error=%v writes=%d", err, writes.Load())
	}
	refs, refErr := exec.Command("git", "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/minion/").Output()
	if refErr != nil || len(refs) != 0 {
		t.Fatalf("hostile Fix pushed a branch: refs=%q error=%v", refs, refErr)
	}
	records, err := usage.ReadAll(usageDir)
	if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].Tokens != (usage.Tokens{Input: 3, Output: 2}) {
		t.Fatalf("private failed-usage evidence missing: records=%+v error=%v", records, err)
	}
}

func TestHostileHTTPFixNULStrippedRefutationCannotPublish(t *testing.T) {
	const private = "foo\x00bar is a private review conclusion that must not enter a Fix."
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "foo", Mode: workspace.Regular, Data: []byte("bar is a private review conclusion that must not enter a Fix.\n")})
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
	workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
	e := &Orchestrator{
		WorkRoot: workRoot, Store: openState(t, time.Now()), Usage: &usage.Store{Dir: usageDir},
		BotID:  8,
		GitHub: testGitHub(server, "token"), BotLogin: "bot", Adversarial: "claude",
		Commands: map[string]config.CommandMapping{"fix": {Action: string(command.Fix), Skills: []string{skills.BuiltinAntiPromptInjection}}},
	}
	e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		job.Source.BaseFiles = nil // The issue fix has no base across the HTTP JSON round trip.
		admitFixtureAssignment(t, checkpoints, job)
		f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
		f.pull(t)
		result := privateReviewResult(job)
		result.Refutations[0].Text = private
		result.Changes = []workspace.Change{{Path: "foo", Mode: workspace.Regular, Data: []byte(strings.ReplaceAll(private, "\x00", ""))}}
		for i := range result.Calls {
			result.Calls[i].Tokens = usage.Tokens{Input: int64(i + 1), Output: 1}
			result.Calls[i].Complete = true
		}
		sequence := uint64(1)
		binding := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
		sequence++
		for i, call := range result.Calls {
			binding.Call = callMetadata(call)
			f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
			sequence++
			binding.Call = call
			f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
			sequence++
			review.Calls = result.Calls[:i+1]
			if call.Round >= 2 {
				review.Refutations = result.Refutations
			}
			f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
			sequence++
		}
		review.CriticStopReason = result.CriticStopReason
		f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
		sequence++
		if err := result.Validate(job); err != nil {
			t.Fatalf("hostile Fix did not reach publication gate: %v", err)
		}
		outcome := f.outcome(sequence, nil)
		outcome.Result = result
		if ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, sequence); ack.Failure == nil {
			t.Fatal("hostile result was acknowledged")
		}
		got := f.result(t)
		return got.result, got.err
	})
	request := usageJob()
	request.Source.UpdatedAt = time.Now()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	request.Repository.Actions = []string{"fix"}
	selectFixtureRequest(t, e, &request, false)
	err := e.Execute(testActionContext(t), request)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "publication contains private review evidence") || writes.Load() != 0 {
		t.Fatalf("NUL-containing private refutation crossed publication boundary: error=%v writes=%d", err, writes.Load())
	}
	refs, refErr := exec.Command("git", "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/minion/").Output()
	if refErr != nil || len(refs) != 0 {
		t.Fatalf("hostile Fix pushed a branch: refs=%q error=%v", refs, refErr)
	}
	if review := readPrivateReview(t, workRoot); len(review.Refutations) != 1 || review.Refutations[0].Text != private {
		t.Fatalf("private review evidence missing: %+v", review.Refutations)
	}
	records, readErr := usage.ReadAll(usageDir)
	if readErr != nil || len(records) != 1 || records[0].Status != "failed" {
		t.Fatalf("private failed-usage evidence missing: records=%+v error=%v", records, readErr)
	}
}

func TestRejectConfiguredKeyPublicationFields(t *testing.T) {
	const key = "fixture-secret"
	for _, tc := range []struct {
		name   string
		result Result
	}{
		{name: "fix data", result: Result{Changes: []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixture-\x00secret")}}}},
		{name: "fix path", result: Result{Changes: []workspace.Change{{Path: "fixture-secret", Mode: workspace.Regular}}}},
		{name: "audit JSON data", result: Result{Artifacts: []workspace.File{{Path: "findings.json", Mode: workspace.Regular, Data: []byte(`{"value":"fixture-\u0073ecret"}`)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rejectConfiguredKeyPublication(publication{artifacts: tc.result.Artifacts}, tc.result.Changes, "", key); err == nil || !strings.Contains(err.Error(), "configured provider key") {
				t.Fatalf("secret reached publication boundary: %v", err)
			}
		})
	}
}

func TestFixedAuditArtifactPathDoesNotCountAsConfiguredKey(t *testing.T) {
	published := publication{artifacts: []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte("safe report")}}}
	if err := rejectConfiguredKeyPublication(published, nil, "REPORT.md"); err != nil {
		t.Fatalf("fixed audit artifact path blocked safe data: %v", err)
	}
}

func TestChangedFixUnusedSummaryDoesNotRejectConfiguredKey(t *testing.T) {
	const key = "fixture-secret"
	changes := []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("safe change\n")}}
	published, err := projectPublication(Job{Action: command.Fix}, Result{Answer: key, Changes: changes})
	if err != nil || published.answer != "" {
		t.Fatalf("changed Fix retained unused summary: publication=%+v error=%v", published, err)
	}
	if err := rejectConfiguredKeyPublication(published, changes, key); err != nil {
		t.Fatalf("unused summary blocked safe change: %v", err)
	}
	changes[0].Data = []byte(key)
	if err := rejectConfiguredKeyPublication(published, changes, key); err == nil {
		t.Fatal("configured key in published change was accepted")
	}
}

func TestIncompleteAuditRetainsMarkerWhenEveryArtifactContainsKey(t *testing.T) {
	const key = "fixture-secret"
	job := testJob(command.SecurityAudit)
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
		Artifacts: []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte(key)}},
	}
	root := t.TempDir()
	e := Orchestrator{WorkRoot: root, Runner: Runner{Codex: config.CodexConfig{APIKey: key}}}
	if err := e.retainIncompleteAudit(&preparedAction{job: job}, result); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "audits", "incomplete-*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("incomplete audit marker missing: paths=%v error=%v", paths, err)
	}
	files, err := os.ReadDir(paths[0])
	if err != nil || len(files) != 1 || files[0].Name() != "INCOMPLETE.txt" {
		t.Fatalf("unsafe artifact retained: files=%v error=%v", files, err)
	}
}
