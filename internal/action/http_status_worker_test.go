package action

import (
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
)

func TestWorkerStatusHeartbeatsKeepProgressAgeAndSequenceIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		observed := make(chan Status, 8)
		client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			var payload httpObservation
			if err := httpDecode(r.Body, httpControlBytes, &payload); err != nil {
				return nil, err
			}
			observed <- payload.Status
			if payload.Status.Sequence == 2 {
				return nil, errors.New("lost observation")
			}
			ack, err := httpEncode(httpAcknowledgement{
				httpBinding: httpJobBinding(job), Kind: "status", Sequence: payload.Status.Sequence,
			}, httpControlBytes)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(ack)))}, nil
		})}
		base, _ := url.Parse("http://worker.test")
		status := newWorkerStatus(job)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			status.run(ctx, &httpExecutionClient{client: client, base: base})
			close(done)
		}()
		synctest.Wait()
		first := <-observed
		if first.Sequence != 1 || first.ProgressSequence != 1 || first.ProgressAge != 0 || first.Phase != "prepare" {
			t.Fatalf("initial observation = %+v", first)
		}
		time.Sleep(statusHeartbeatInterval)
		synctest.Wait()
		second := <-observed
		if second.Sequence != 2 || second.ProgressSequence != first.ProgressSequence || second.ProgressAge != statusHeartbeatInterval {
			t.Fatalf("heartbeat advanced progress or missed interval: %+v", second)
		}
		status.progress("main", 1, "")
		synctest.Wait()
		third := <-observed
		if third.Sequence != 3 || third.ProgressSequence != 2 || third.ProgressAge != 0 || third.Phase != "main" || third.Round != 1 {
			t.Fatalf("progress after lost heartbeat = %+v", third)
		}
		time.Sleep(statusHeartbeatInterval)
		synctest.Wait()
		fourth := <-observed
		if fourth.Sequence != 4 || fourth.ProgressSequence != third.ProgressSequence || fourth.ProgressAge != statusHeartbeatInterval {
			t.Fatalf("heartbeat reset progress age: %+v", fourth)
		}
		status.progress("cleanup", 0, "")
		synctest.Wait()
		last := <-observed
		if last.Sequence != 5 || last.ProgressSequence != 3 || last.Phase != "cleanup" {
			t.Fatalf("cleanup progress = %+v", last)
		}
		cancel()
		<-done
	})
}
