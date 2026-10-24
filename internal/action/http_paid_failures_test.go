package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

// Drop the paid receipt before any HTTP request reaches the parent handler.
type httpDropPaidAfter struct {
	http.RoundTripper
	cut *atomic.Bool
}

func (c httpDropPaidAfter) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/after" && c.cut.CompareAndSwap(false, true) {
		_ = request.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return c.RoundTripper.RoundTrip(request)
}

// The actual sink has returned before the server tries to write this ACK.
type httpLosePaidACK struct {
	http.ResponseWriter
	cut *atomic.Bool
}

func (w httpLosePaidACK) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w httpLosePaidACK) Write([]byte) (int, error) {
	connection, _, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return 0, err
	}
	w.cut.Store(true)
	return 0, errors.Join(io.ErrClosedPipe, connection.Close())
}

func TestHTTPPaidDisconnectsKeepRawFactsSeparateFromSavedCheckpoints(t *testing.T) {
	for _, stage := range []string{"After request", "After ACK", "critic prefix ACK", "approved decision ACK", "capped decision ACK"} {
		t.Run(stage, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.CriticMaxRounds = 1
			f := newExecutionAdapterFixture(t, job)
			store := f.startUsage()
			var cut atomic.Bool
			var deliveredAfter atomic.Int32
			var deliveredReview atomic.Int32
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			transport := newHTTPServerTest(t, ctx, f.job, f.sink, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					loseACK := false
					if r.URL.Path == "/after" {
						deliveredAfter.Add(1)
						loseACK = stage == "After ACK"
					}
					if r.URL.Path == "/review" {
						deliveredReview.Add(1)
					}
					if r.URL.Path == "/review" && stage != "After request" && stage != "After ACK" {
						var review httpReview
						if err := httpDecode(r.Body, httpReviewBytes, &review); err != nil {
							t.Error(err)
							http.Error(w, "invalid review fixture", http.StatusBadRequest)
							return
						}
						loseACK = len(review.Checkpoint.Calls) == 2 && ((stage == "critic prefix ACK" && review.Checkpoint.CriticStopReason == "") || (stage != "critic prefix ACK" && review.Checkpoint.CriticStopReason != ""))
						body, err := httpEncode(review, httpReviewBytes)
						if err != nil {
							t.Error(err)
							http.Error(w, "invalid review fixture", http.StatusBadRequest)
							return
						}
						_ = r.Body.Close()
						r.Body = io.NopCloser(bytes.NewReader(body))
					}
					if loseACK {
						w = httpLosePaidACK{ResponseWriter: w, cut: &cut}
					}
					next.ServeHTTP(w, r)
				})
			})
			client := transport.server.Client()
			if stage == "After request" {
				client.Transport = httpDropPaidAfter{RoundTripper: client.Transport, cut: &cut}
			}
			mainTokens := usage.Tokens{Input: 7, Output: 2}
			criticTokens := usage.Tokens{Input: 11, CachedInput: 1, Output: 3}
			var native atomic.Int32
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				index := native.Add(1)
				if index == 1 && request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate", ""), Tokens: mainTokens, UsageComplete: true}, nil
				}
				if index == 2 && request.role == roleCritic {
					answer := approvedCritiqueJSON
					if stage == "capped decision ACK" {
						answer = criticFeedbackJSON(true, "PRIVATE_FINAL_BLOCKER")
					}
					return aicli.Result{Answer: answer, Tokens: criticTokens, UsageComplete: true}, nil
				}
				return aicli.Result{}, fmt.Errorf("paid work repeated after %s: call %d role=%s", stage, index, request.role)
			}}
			workerErr := RunHTTP(ctx, client, transport.server.URL, f.job.Identity.ExecutorID, &runner)
			got := transport.result(t)
			finalErr := f.sink.finish(got.result, got.err)
			wantCalls := 2
			if stage == "After request" || stage == "After ACK" {
				wantCalls = 1
			}
			if !cut.Load() || int(native.Load()) != wantCalls || len(got.result.Calls) != wantCalls || !got.result.matchesJob(f.job) || !errors.Is(workerErr, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || finalErr == nil || errors.Is(finalErr, monitor.ErrTerminalJob) != (stage == "After request") || got.result.Answer != "" {
				t.Fatalf("paid disconnect lost raw outcome, repeated work or passed publication gate: calls=%d raw=%+v worker=%v parent=%v finish=%v", native.Load(), got.result, workerErr, got.err, finalErr)
			}
			wantAfter, wantReviews := wantCalls, 1
			if stage == "After request" {
				wantAfter = 0
			} else if stage == "critic prefix ACK" {
				wantReviews = 3
			} else if wantCalls == 2 {
				wantReviews = 4
			}
			if int(deliveredAfter.Load()) != wantAfter || int(deliveredReview.Load()) != wantReviews {
				t.Fatalf("paid callbacks were retried or skipped after disconnect: after=%d review=%d", deliveredAfter.Load(), deliveredReview.Load())
			}
			main := got.result.Calls[0]
			if main.Provider != f.job.Main.Provider || main.Role != roleMain || main.Round != 1 || main.Tokens != mainTokens || !main.Complete {
				t.Fatalf("main paid receipt changed: %+v", main)
			}
			if wantCalls == 2 {
				critic := got.result.Calls[1]
				capped := stage == "capped decision ACK"
				if critic.Provider != f.job.Critic.Provider || critic.Role != roleCritic || critic.Round != 1 || critic.ReviewPhase != reviewFull || critic.Tokens != criticTokens || !critic.Complete || !critic.ReviewComplete || critic.Approved == capped || critic.NoBlockers == capped || critic.HasBlockers != capped {
					t.Fatalf("parsed critic paid facts changed: %+v", critic)
				}
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("durable paid usage missing: %+v error=%v", records, err)
			}
			record := records[0]
			saved, err := loadPrivateReview(f.owner.WorkRoot)
			if err != nil {
				t.Fatal(err)
			}
			wantSavedCalls := wantCalls
			wantTokens := usage.Tokens{Input: 18, CachedInput: 1, Output: 5}
			wantReason := ""
			if stage == "After request" {
				wantSavedCalls, wantTokens = 0, usage.Tokens{}
				if deliveredAfter.Load() != 0 || f.sink.pending == nil || f.sink.returned != nil || len(f.sink.failureEvidence().Calls) != 0 {
					t.Fatalf("raw Result fabricated an unsent After checkpoint: delivered=%d sink=%+v", deliveredAfter.Load(), f.sink)
				}
				transport.executor.mu.Lock()
				unavailable := transport.executor.unavailable
				transport.executor.mu.Unlock()
				if !unavailable {
					t.Fatal("missing checkpoint sequence released capacity")
				}
			} else if stage == "After ACK" {
				wantTokens = mainTokens
			} else if stage == "approved decision ACK" {
				wantReason = "approved"
			} else if stage == "capped decision ACK" {
				wantReason = "round_cap"
			}
			if len(f.sink.calls) != wantSavedCalls || !slices.Equal(f.sink.calls, got.result.Calls[:wantSavedCalls]) || record.CodexCalls != 1 || record.ClaudeCalls != wantCalls-1 || record.Tokens != wantTokens || record.UsageComplete != (stage != "After request") {
				t.Fatalf("raw claims changed durable accounting or acknowledged receipts: record=%+v sink=%+v raw=%+v", record, f.sink.calls, got.result.Calls)
			}
			wantLedgerCalls := wantSavedCalls
			if wantCalls == 1 {
				wantLedgerCalls = 0
			}
			if !slices.Equal(saved.Calls, got.result.Calls[:wantLedgerCalls]) || !slices.Equal(f.sink.review.Calls, saved.Calls) || saved.CriticStopReason != wantReason || f.sink.review.CriticStopReason != wantReason || got.result.CriticStopReason != wantReason || len(saved.Refutations) != 0 {
				t.Fatalf("ACK loss rewrote an actually saved prefix/decision: saved=%+v acknowledged=%+v raw=%+v", saved, f.sink.review, got.result)
			}
		})
	}
}

func TestHTTPMissingGitPreparationPreservesDiagnosticBeforeNativeWork(t *testing.T) {
	f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
	store := f.startUsage()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	transport := newHTTPServerTest(t, ctx, f.job, f.sink, nil)
	t.Setenv("PATH", t.TempDir())
	var native atomic.Int32
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		native.Add(1)
		return aicli.Result{}, errors.New("missing Git reached native work")
	}}
	workerErr := RunHTTP(ctx, transport.server.Client(), transport.server.URL, f.job.Identity.ExecutorID, &runner)
	got := transport.result(t)
	finalErr := f.sink.finish(got.result, got.err)
	if !errors.Is(workerErr, exec.ErrNotFound) || errors.Is(workerErr, monitor.ErrTerminalJob) || errors.Is(workerErr, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("worker lost its known missing Git cause: %v", workerErr)
	}
	for _, err := range []error{got.err, finalErr} {
		var private *workerFailureError
		if err == nil || strings.Contains(err.Error(), "executable file not found") || !errors.As(err, &private) || !strings.Contains(private.failure.Message, "executable file not found") || errors.Is(err, exec.ErrNotFound) || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("remote missing Git changed parent policy or exposed its diagnostic: %v", err)
		}
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].CodexCalls != 0 || records[0].ClaudeCalls != 0 || records[0].Tokens != (usage.Tokens{}) || !records[0].UsageComplete || native.Load() != 0 || len(got.result.Calls) != 0 || len(f.sink.calls) != 0 {
		t.Fatalf("missing Git claimed paid work: records=%+v raw=%+v native=%d error=%v", records, got.result, native.Load(), err)
	}
}

func TestHTTPPaidOutputLimitPreservesUsageWithoutTrustingWorkerPolicy(t *testing.T) {
	gitFixtureRemote(t)
	var writes atomic.Int32
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			t.Errorf("paid output-limit failure attempted publication: %s %s", r.Method, r.URL.Path)
			http.Error(w, "publication forbidden", http.StatusForbidden)
		} else if !serveIssueFixture(t, w, r) {
			t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer githubServer.Close()
	remote, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	workerServer := httptest.NewServer(remote)
	defer workerServer.Close()
	defer remote.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 5}
	var native atomic.Int32
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		native.Add(1)
		return aicli.Result{Tokens: tokens}, fmt.Errorf("paid native event limit: %w", aicli.ErrOutputLimit)
	}}
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunHTTP(ctx, workerServer.Client(), workerServer.URL, "local", &runner) }()
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	orchestrator := Orchestrator{Executor: remote, WorkRoot: t.TempDir(), GitHub: testGitHub(githubServer, "fixture-token"), BotLogin: "bot", Usage: store,
		Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}}
	request := usageJob()
	request.Repository.Actions = []string{"comment"}
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	parentErr := orchestrator.Execute(ctx, request)
	var workerErr error
	select {
	case workerErr = <-workerDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker did not receive paid output-limit outcome receipt: parent=%v native=%d writes=%d", parentErr, native.Load(), writes.Load())
	}
	if !errors.Is(workerErr, aicli.ErrOutputLimit) || errors.Is(workerErr, monitor.ErrTerminalJob) || parentErr == nil || strings.Contains(parentErr.Error(), "paid native event limit") || errors.Is(parentErr, aicli.ErrOutputLimit) || errors.Is(parentErr, monitor.ErrTerminalJob) || !errors.Is(parentErr, monitor.ErrExecutionOutcomeUnknown) || native.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("paid worker limit changed parent policy or reached publication: native=%d writes=%d worker=%v parent=%v", native.Load(), writes.Load(), workerErr, parentErr)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].FinishedAt == nil || records[0].CodexCalls != 1 || records[0].Tokens != tokens || records[0].UsageComplete {
		t.Fatalf("paid output-limit failure lost its actual durable lower bound: records=%+v error=%v", records, err)
	}
	path := filepath.Join(orchestrator.WorkRoot, "worker-diagnostics", records[0].ID+".json")
	data, err := os.ReadFile(path)
	var private retainedWorkerDiagnostics
	if err == nil {
		err = json.Unmarshal(data, &private)
	}
	if err != nil || private.Identity.JobID != request.ID || private.Identity.AttemptID != records[0].ID || private.WorkerReportedFailure == nil || !strings.Contains(private.WorkerReportedFailure.Message, "paid native event limit") {
		t.Fatalf("paid worker detail was not retained privately: path=%q record=%+v error=%v", path, private, err)
	}
}
