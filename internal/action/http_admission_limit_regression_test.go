package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestWatchAdmissionObservationSurvivesLaterTimer(t *testing.T) {
	for _, test := range []struct {
		name      string
		openError error
	}{
		{name: "headers"},
		{name: "transport error", openError: errors.New("watch transport failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				action, cancelAction := context.WithCancelCause(t.Context())
				defer cancelAction(nil)
				watch, stopWatch := context.WithCancelCause(t.Context())
				defer stopWatch(nil)
				admission := &watchAdmission{}
				headerErr := errors.New("watch admission timed out")
				deadline := time.Now().Add(time.Second)
				timer := time.AfterFunc(time.Second, func() { admission.expire(stopWatch, headerErr) })
				client := &httpExecutionClient{
					client: &http.Client{Transport: httpClientTestTransport(func(*http.Request) (*http.Response, error) {
						if test.openError != nil {
							return nil, test.openError
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
							Body: io.NopCloser(strings.NewReader("{}"))}, nil
					})},
					base: &url.URL{Scheme: "http", Host: "worker.test"},
				}
				response, err := client.openObserved(watch, "cancel", []byte("{}"), httpBinding{}, 0, func() {
					admission.observe(action, watch)
					time.Sleep(2 * time.Second) // Hold the caller after open completed, across the header deadline.
				})
				timer.Stop()
				observedAt, cause, timedOut := admission.finish(deadline, stopWatch, headerErr)
				if response != nil {
					defer response.Body.Close()
				}
				if !observedAt.Before(deadline) || cause != nil || timedOut || context.Cause(watch) != nil ||
					(response != nil) == (test.openError != nil) || (test.openError != nil && !errors.Is(err, test.openError)) ||
					(test.openError == nil && err != nil) {
					t.Fatalf("early watch result replaced: response=%v error=%v observed=%v deadline=%v cause=%v timedOut=%t watch=%v",
						response != nil, err, observedAt, deadline, cause, timedOut, context.Cause(watch))
				}
				if test.openError != nil {
					idleTimer := time.NewTimer(time.Hour)
					defer idleTimer.Stop()
					idle := &inactivityTimer{last: deadline.Add(-time.Second), timeout: time.Second, timer: idleTimer, cancel: cancelAction}
					selected := admissionFailure(err, cause, idle, observedAt, deadline.Add(time.Minute), true)
					if !errors.Is(selected, test.openError) || errors.Is(selected, errExecutorInactivity) {
						t.Fatalf("later inactivity replaced observed transport error: %v", selected)
					}
				}
			})
		})
	}
}

func TestWatchAdmissionDeadlineWinsDelayedTimerCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		action, cancelAction := context.WithCancelCause(t.Context())
		defer cancelAction(nil)
		watch, stopWatch := context.WithCancelCause(t.Context())
		defer stopWatch(nil)
		admission := &watchAdmission{}
		headerErr := errors.New("watch admission timed out")
		deadline := time.Now().Add(time.Second)
		releaseTimer := make(chan struct{})
		timerEntered := make(chan struct{})
		timer := time.AfterFunc(time.Second, func() {
			close(timerEntered)
			<-releaseTimer
			admission.expire(stopWatch, headerErr)
		})
		client := &httpExecutionClient{
			client: &http.Client{Transport: httpClientTestTransport(func(*http.Request) (*http.Response, error) {
				time.Sleep(2 * time.Second) // Ignore cancellation until after the absolute header deadline.
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})},
			base: &url.URL{Scheme: "http", Host: "worker.test"},
		}
		response, err := client.openObserved(watch, "cancel", []byte("{}"), httpBinding{}, 0, func() {
			admission.observe(action, watch)
		})
		if response != nil {
			defer response.Body.Close()
		}
		<-timerEntered
		timer.Stop()
		observedAt, cause, timedOut := admission.finish(deadline, stopWatch, headerErr)
		close(releaseTimer)
		synctest.Wait()
		if err != nil || response == nil || observedAt.Before(deadline) || cause != nil || !timedOut || context.Cause(watch) != headerErr {
			t.Fatalf("late watch result accepted: response=%v error=%v observed=%v deadline=%v cause=%v timedOut=%t watch=%v",
				response != nil, err, observedAt, deadline, cause, timedOut, context.Cause(watch))
		}
	})
}

func TestWatchAdmissionKeepsCallerCancellationBeforeLateOpenReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		caller, cancelCaller := context.WithCancel(t.Context())
		defer cancelCaller()
		action, cancelAction := context.WithCancelCause(caller)
		defer cancelAction(nil)
		watch, stopWatch := context.WithCancelCause(caller)
		defer stopWatch(nil)
		admission := &watchAdmission{}
		headerErr := errors.New("watch admission timed out")
		deadline := time.Now().Add(time.Second)
		cancelTimer := time.AfterFunc(time.Second/2, cancelCaller)
		defer cancelTimer.Stop()
		headerTimer := time.AfterFunc(time.Second, func() { admission.expire(stopWatch, headerErr) })
		client := &httpExecutionClient{
			client: &http.Client{Transport: httpClientTestTransport(func(*http.Request) (*http.Response, error) {
				time.Sleep(2 * time.Second) // Transport returns after both caller stop and header deadline.
				return nil, context.Canceled
			})},
			base: &url.URL{Scheme: "http", Host: "worker.test"},
		}
		response, err := client.openObserved(watch, "cancel", []byte("{}"), httpBinding{}, 0, func() {
			admission.observe(action, watch)
		})
		headerTimer.Stop()
		observedAt, cause, timedOut := admission.finish(deadline, stopWatch, headerErr)
		if response != nil || !errors.Is(err, context.Canceled) || !errors.Is(cause, context.Canceled) ||
			!observedAt.After(deadline) || timedOut || context.Cause(watch) != context.Canceled {
			t.Fatalf("caller stop replaced by late header deadline: response=%v error=%v observed=%v deadline=%v cause=%v timedOut=%t watch=%v",
				response != nil, err, observedAt, deadline, cause, timedOut, context.Cause(watch))
		}
	})
}

func TestSelectedWatchAdmissionControlTimeoutSurvivesLaterInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = 2 * httpControlTimeout
		job.InactivityTimeout = httpControlTimeout + time.Second
		binding := httpJobBinding(job)
		var received httpOutcome
		requests := 0
		response := func(value any, limit int) (*http.Response, error) {
			data, err := httpEncode(value, limit)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			requests++
			switch r.URL.Path {
			case "/next":
				return response(httpAssignment{Job: job, StopGrace: httpControlTimeout}, httpJobBytes)
			case "/cancel":
				time.Sleep(httpControlTimeout + 2*time.Second) // Virtual delay: control < idle < processing < hard.
				return nil, context.DeadlineExceeded
			case "/outcome":
				if err := httpDecode(r.Body, httpOutcomeBytes, &received); err != nil {
					t.Fatal(err)
				}
				return response(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: received.Sequence}, httpControlBytes)
			default:
				t.Fatalf("worker advanced before watch admission: %s", r.URL.Path)
				return nil, errors.New("unexpected request")
			}
		})}
		err := RunHTTP(t.Context(), client, "http://worker.test", job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
		if !errors.Is(err, monitor.ErrRequestTransport) || errors.Is(err, errExecutorInactivity) || requests != 3 ||
			received.Failure == nil || !received.Failure.RequestTransport || !received.Failure.Deadline ||
			received.Failure.Inactivity || received.Failure.RuntimeCap || len(received.Result.Calls) != 0 {
			t.Fatalf("selected watch control timeout changed after idle callback: error=%v requests=%d failure=%+v", err, requests, received.Failure)
		}
	})
}

func TestSelectedWorkerLimitDiagnosticKeepsIndependentError(t *testing.T) {
	for _, test := range []struct {
		name     string
		detail   string
		degraded bool
	}{
		{name: "model failure", detail: "independent model failure"},
		{name: "cleanup failure in degraded Outcome", detail: "independent cleanup failure", degraded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			runErr := errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap, errors.New(test.detail))
			got, degraded, err := captureWorkerLimitOutcome(t, errExecutorRuntimeCap, runErr,
				now.Add(-time.Second), now.Add(-2*time.Second), test.degraded)
			if degraded != test.degraded || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != test.degraded {
				t.Fatalf("degraded=%t error=%v, want degraded=%t", degraded, err, test.degraded)
			}
			if got.Failure == nil || !got.Failure.Inactivity || !got.Failure.Terminal ||
				!strings.Contains(got.Failure.ObservedMessage, test.detail) ||
				strings.Contains(got.Failure.Message, "runtime cap") || len(got.Failure.Message) > httpFailureMessageLimit ||
				len(got.Failure.ObservedMessage) > httpFailureMessageLimit {
				t.Fatalf("selected limit lost independent diagnostic: %+v", got.Failure)
			}
		})
	}
}
