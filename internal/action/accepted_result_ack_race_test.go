package action

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type heldRequiredACKWriter struct {
	http.ResponseWriter
	entered chan struct{}
	release <-chan struct{}
}

func (w heldRequiredACKWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w heldRequiredACKWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, io.ErrClosedPipe
}

func waitForHTTPSequence(t *testing.T, tx *httpTransaction, want uint64) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		tx.mu.Lock()
		got := tx.sequence
		tx.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("HTTP sequence = %d, want %d", got, want)
		}
	}
}

func TestSelectedOutcomeWaitsForRequiredACKBeforeSaving(t *testing.T) {
	ackEntered := make(chan struct{})
	ackRelease := make(chan struct{})
	var releaseACK sync.Once
	unlockACK := func() { releaseACK.Do(func() { close(ackRelease) }) }
	t.Cleanup(unlockACK)
	f, store, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/after" {
				w = heldRequiredACKWriter{ResponseWriter: w, entered: ackEntered, release: ackRelease}
			}
			next.ServeHTTP(w, r)
		})
	})
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

	storeHeld := make(chan struct{})
	storeRelease := make(chan struct{})
	storeDone := make(chan error, 1)
	var releaseStore sync.Once
	unlockStore := func() { releaseStore.Do(func() { close(storeRelease) }) }
	t.Cleanup(unlockStore)
	go func() {
		storeDone <- store.WithLock(func() error {
			close(storeHeld)
			<-storeRelease
			return nil
		})
	}()
	select {
	case <-storeHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("state gate was not held")
	}
	outcome := f.outcome(3, nil)
	outcome.Result.Answer = "Completed answer"
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	outcomeDone := make(chan httpServerTestExchange, 1)
	go func() { outcomeDone <- f.request("/outcome", outcome) }()
	waitForHTTPSequence(t, f.tx, 3)
	// The prior ACK is still blocked while the complete outcome has entered.
	// Holding state makes any old receiver wait until after the ACK failure.
	unlockACK()
	select {
	case <-afterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("failed required ACK did not finish")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		f.tx.mu.Lock()
		failed := f.tx.requiredControlErr != nil
		f.tx.mu.Unlock()
		if failed {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("required ACK failure was not recorded")
		}
	}
	unlockStore()
	if err := <-storeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-outcomeDone:
		if reply.err == nil && reply.status == http.StatusOK && reply.ack.Failure == nil {
			t.Fatalf("outcome was acknowledged despite failed required ACK: %+v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("outcome remained blocked after failed required ACK")
	}
	f.tx.mu.Lock()
	stopCancel := f.tx.stopCancel
	f.tx.mu.Unlock()
	if stopCancel != nil {
		stopCancel() // End the test's post-stop collection grace.
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("failed required ACK was not an unknown outcome: %+v", got)
	}
	if _, err := store.ReadAcceptedResult(AcceptedResultMaxBytes); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed required ACK left a publishable result: %v", err)
	}
}
