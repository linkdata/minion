package action

import (
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
)

type httpNetworkAction func(context.Context, monitor.Job) error

func (f httpNetworkAction) Execute(ctx context.Context, job monitor.Job) error { return f(ctx, job) }

func TestHTTPWatchAdmissionTransportFailurePreservesMonitorPolicy(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	for _, unknown := range []bool{false, true} {
		name := "known no work"
		if unknown {
			name = "transport joined with unknown"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
			clock := now
			store := openState(t, now.Add(-time.Minute))
			gh := &actionMonitorGitHub{items: []github.Item{{Key: "request", NodeID: "request-node", Body: "@linkdataminion comment", Author: github.User{ID: 7, Login: "writer"}}},
				notification: github.Notification{Repository: github.Repository{FullName: "o/r"}, Subject: github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: now}}
			var executions, admissions, native atomic.Int32
			handler := httpNetworkAction(func(ctx context.Context, request monitor.Job) error {
				executions.Add(1)
				job := withClaudeCritic(testJob(command.Comment))
				job.Identity.JobID = request.ID
				f := newExecutionAdapterFixture(t, job)
				accounting := f.startUsage()
				ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				transport := newHTTPServerTest(t, ctx, f.job, f.sink, nil)
				client := *transport.server.Client()
				underlying := client.Transport
				client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/cancel" {
						admissions.Add(1)
						_ = r.Body.Close()
						var cause error = io.ErrUnexpectedEOF
						if unknown {
							// Exercise precedence of an explicitly joined unknown fact, not a paid call.
							cause = errors.Join(cause, monitor.ErrExecutionOutcomeUnknown)
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
				if !errors.As(workerErr, &network) || finalErr == nil || errors.Is(finalErr, monitor.ErrTerminalJob) || errors.Is(finalErr, monitor.ErrExecutionOutcomeUnknown) != unknown || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 || f.sink.pending != nil || f.sink.reviewed {
					t.Fatalf("watch failure lost its known no-work outcome: worker=%v parent=%v finish=%v raw=%+v", workerErr, got.err, finalErr, got.result)
				}
				var restored *url.Error
				if !errors.Is(finalErr, monitor.ErrRequestTransport) || errors.As(finalErr, &restored) {
					t.Fatalf("parent lost finite transport policy or fabricated request metadata: %v", finalErr)
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
			wantProcessed, wantExecutions := 0, int32(4)
			if unknown {
				wantProcessed, wantExecutions = 1, 1
			}
			for poll := 0; poll < 4; poll++ {
				_, err := m.RunOnce(context.Background(), store)
				if (err == nil) != unknown || len(store.Data.JobFailures) != 0 || store.Data.UnconfirmedProcessJob != "" || len(store.Data.Processed) != wantProcessed {
					t.Fatalf("transport refund or unknown precedence changed on poll %d: error=%v state=%+v", poll, err, store.Data)
				}
				clock = clock.Add(10 * time.Minute)
			}
			if executions.Load() != wantExecutions || admissions.Load() != wantExecutions || native.Load() != 0 {
				t.Fatalf("known failure exhausted retries or unknown work repeated: executions=%d admissions=%d native=%d", executions.Load(), admissions.Load(), native.Load())
			}
		})
	}
}
