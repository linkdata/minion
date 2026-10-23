package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workroot"
)

type httpNetworkAction func(context.Context, monitor.Job) error

func (f httpNetworkAction) Execute(ctx context.Context, job monitor.Job) error { return f(ctx, job) }

func TestHTTPWatchAdmissionWorkerClaimsDoNotRefundMonitorAttempts(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	for _, test := range []struct {
		name           string
		unknownClaim   bool
		parentGap      bool
		unsafeClaim    bool
		controlTimeout bool
	}{
		{name: "worker transport claim"},
		{name: "worker transport and unsafe claims", unsafeClaim: true},
		{name: "worker transport and unknown claims", unknownClaim: true},
		{name: "parent-observed outcome sequence gap", parentGap: true},
		{name: "worker watch header control timeout", controlTimeout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
			clock := now
			store := openState(t, now.Add(-time.Minute))
			gh := &actionMonitorGitHub{items: []github.Item{{Key: "request", NodeID: "request-node", Body: "@linkdataminion comment", Author: github.User{ID: 7, Login: "writer"}}},
				notification: github.Notification{Repository: github.Repository{FullName: "o/r"}, Subject: github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: now}}
			var executions, admissions, outcomes, native atomic.Int32
			handler := httpNetworkAction(func(ctx context.Context, request monitor.Job) error {
				executions.Add(1)
				job := withClaudeCritic(testJob(command.Comment))
				job.Identity.JobID = request.ID
				timeout := 10 * time.Second
				if test.controlTimeout {
					job.RuntimeBudget, timeout = 2*time.Minute, 2*time.Minute
				}
				f := newExecutionAdapterFixture(t, job)
				accounting := f.startUsage()
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				transport := newHTTPServerTest(t, ctx, f.job, f.sink, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if test.parentGap && r.URL.Path == "/outcome" {
							var outcome httpOutcome
							if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
								t.Errorf("decode outcome for sequence gap: %v", err)
								return
							}
							outcome.Sequence++
							body, err := httpEncode(outcome, httpOutcomeBytes)
							if err != nil {
								t.Errorf("encode outcome for sequence gap: %v", err)
								return
							}
							_ = r.Body.Close()
							r.Body = io.NopCloser(bytes.NewReader(body))
							r.ContentLength = int64(len(body))
						}
						if test.controlTimeout && r.URL.Path == "/cancel" {
							admissions.Add(1)
							var binding httpBinding
							if err := httpDecode(r.Body, httpControlBytes, &binding); err != nil || binding != httpJobBinding(f.job) {
								t.Errorf("invalid stalled watch: binding=%+v error=%v", binding, err)
								return
							}
							<-r.Context().Done() // Exercise the real admission timer without sending headers.
							return
						}
						next.ServeHTTP(w, r)
					})
				})
				client := *transport.server.Client()
				client.Timeout = 0
				underlying := client.Transport
				client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/outcome" {
						outcomes.Add(1)
					}
					if !test.controlTimeout && r.URL.Path == "/cancel" {
						admissions.Add(1)
						_ = r.Body.Close()
						var cause error = io.ErrUnexpectedEOF
						if test.unknownClaim {
							// A worker can claim uncertainty even without a paid call.
							cause = errors.Join(cause, monitor.ErrExecutionOutcomeUnknown)
						}
						if test.unsafeClaim {
							cause = errors.Join(cause, workroot.ErrUnsafe)
						}
						return nil, cause // http.Client wraps the real failed request in *url.Error.
					}
					return underlying.RoundTrip(r)
				})
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
					native.Add(1)
					return aicli.Result{}, errors.New("watch admission failure reached native work")
				}}
				workerErr := RunHTTP(ctx, &client, transport.server.URL, f.job.Identity.ExecutorID, &runner)
				got := transport.result(t)
				finalErr := f.sink.finish(got.result, got.err)
				var network *url.Error
				if !test.controlTimeout && !errors.As(workerErr, &network) || finalErr == nil || errors.Is(finalErr, monitor.ErrTerminalJob) || errors.Is(finalErr, monitor.ErrExecutionOutcomeUnknown) != test.parentGap || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 || f.sink.pending != nil || f.sink.reviewed {
					t.Fatalf("watch failure lost its known no-work outcome: worker=%v parent=%v finish=%v raw=%+v", workerErr, got.err, finalErr, got.result)
				}
				var restored *url.Error
				if errors.Is(finalErr, monitor.ErrRequestTransport) || errors.Is(finalErr, workroot.ErrUnsafe) || errors.As(finalErr, &restored) {
					t.Fatalf("worker claim changed parent transport or unsafe policy: %v", finalErr)
				}
				if test.controlTimeout && (!errors.Is(workerErr, context.DeadlineExceeded) || !errors.Is(workerErr, monitor.ErrRequestTransport) || errors.Is(finalErr, context.DeadlineExceeded) || ctx.Err() != nil) {
					t.Fatalf("worker watch timeout became a parent deadline: worker=%v parent=%v caller=%v", workerErr, finalErr, ctx.Err())
				}
				records, err := usage.ReadAll(accounting.Dir)
				if err != nil || len(records) != 1 || records[0].CodexCalls != 0 || records[0].ClaudeCalls != 0 || records[0].Tokens != (usage.Tokens{}) || !records[0].UsageComplete {
					t.Fatalf("unstarted Runner claimed usage: records=%+v error=%v", records, err)
				}
				if _, err := os.Stat(filepath.Join(f.owner.WorkRoot, "reviews", f.job.Identity.AttemptID+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unstarted Runner claimed a private review checkpoint: %v", err)
				}
				return finalErr
			})
			m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", Main: "codex", PollIntervalSeconds: 60,
				Commands:     map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{testReviewSkillURL}}},
				Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"}, AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
				GitHub: gh, Handler: handler, Now: func() time.Time { return clock }}
			polls, wantProcessed := 3, 1
			if test.controlTimeout {
				polls, wantProcessed = 1, 0
			}
			if test.parentGap {
				polls = 1
			}
			for poll := 0; poll < polls; poll++ {
				_, err := m.RunOnce(context.Background(), store)
				wantErr := test.controlTimeout || !test.parentGap && poll < polls-1
				wantFailures, processed := 0, wantProcessed
				if wantErr {
					wantFailures, processed = 1, 0
				}
				if (err != nil) != wantErr || len(store.Data.JobFailures) != wantFailures || store.Data.UnconfirmedProcessJob != "" || len(store.Data.Processed) != processed {
					t.Fatalf("worker claim refunded an attempt or changed unknown precedence on poll %d: error=%v state=%+v", poll, err, store.Data)
				}
				for _, failure := range store.Data.JobFailures {
					if failure.Attempts != poll+1 {
						t.Fatalf("worker claim refunded attempt %d: %+v", poll+1, failure)
					}
				}
				clock = clock.Add(10 * time.Minute)
			}
			wantExecutions := int32(polls)
			if executions.Load() != wantExecutions || admissions.Load() != wantExecutions || outcomes.Load() != wantExecutions || native.Load() != 0 {
				t.Fatalf("worker claim changed attempt limit or repeated unknown work: executions=%d admissions=%d outcomes=%d native=%d", executions.Load(), admissions.Load(), outcomes.Load(), native.Load())
			}
		})
	}
}
