package action

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type observedResultReceiver struct {
	*executionCheckpoints
	entered chan struct{}
}

func (r observedResultReceiver) ReceiveCompletedResult(Result) error {
	close(r.entered)
	return errors.New("result receiver ran before the required ACK completed")
}

func TestSelectedOutcomeWaitsForRequiredACKBeforeSaving(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ackEntered := make(chan struct{})
		ackRelease := make(chan struct{})
		var releaseACK sync.Once
		unlockACK := func() { releaseACK.Do(func() { close(ackRelease) }) }
		defer unlockACK()
		f, _, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/after" {
					w = httpServerHeldWrite{ResponseWriter: httpServerFailWrite{w}, entered: ackEntered, release: ackRelease}
				}
				next.ServeHTTP(w, r)
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) })
		receiverEntered := make(chan struct{})
		f.tx.sink = observedResultReceiver{executionCheckpoints: f.tx.sink.(*executionCheckpoints), entered: receiverEntered}
		f.ack(t, "/before", f.call(1), http.StatusOK, 1)
		paid := f.call(2)
		paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 7, Output: 2}, true
		afterDone := make(chan httpServerTestExchange, 1)
		go func() { afterDone <- f.request("/after", paid) }()
		select {
		case <-ackEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("required ACK did not begin")
		}

		outcome := f.outcome(3, nil)
		outcome.Result.Answer = "Completed answer"
		outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
		outcomeDone := make(chan httpServerTestExchange, 1)
		go func() { outcomeDone <- f.request("/outcome", outcome) }()
		synctest.Wait()
		f.tx.mu.Lock()
		sequence := f.tx.sequence
		f.tx.mu.Unlock()
		if sequence != 3 {
			t.Fatalf("complete outcome never entered while required ACK was held: sequence=%d", sequence)
		}
		select {
		case <-outcomeDone:
			t.Fatal("outcome finished while required ACK was held")
		default:
		}
		select {
		case <-receiverEntered:
			t.Fatal("complete result reached the receiver before required ACK finished")
		default:
		}

		unlockACK()
		select {
		case <-afterDone:
		case <-time.After(5 * time.Second):
			t.Fatal("failed required ACK did not finish")
		}
		f.tx.requiredACKs.Wait()
		f.tx.mu.Lock()
		failed := f.tx.requiredControlErr != nil
		f.tx.mu.Unlock()
		if !failed {
			t.Fatal("required ACK failure was not recorded")
		}
		select {
		case reply := <-outcomeDone:
			if reply.err != nil || reply.status != http.StatusInternalServerError || reply.ack.Failure == nil {
				t.Fatalf("failed required ACK produced wrong outcome response: %+v", reply)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("outcome remained blocked after failed required ACK")
		}
		select {
		case <-receiverEntered:
			t.Fatal("failed required ACK reached the complete result receiver")
		default:
		}
		if got := f.result(t); !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrAcceptanceState) {
			t.Fatalf("failed required ACK was not an unknown outcome: %+v", got)
		}
	})
}
