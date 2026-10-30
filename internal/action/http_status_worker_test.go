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
	"unicode/utf8"

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

func TestWorkerStatusRetriesLostLogAckAndDrainsCoverage(t *testing.T) {
	job := testJob(command.Comment)
	var received []httpObservation
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		var observation httpObservation
		if err := httpDecode(r.Body, httpControlBytes, &observation); err != nil {
			return nil, err
		}
		received = append(received, observation)
		if len(received) == 1 {
			return nil, errors.New("ack lost after durable append")
		}
		ack, err := httpEncode(httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "status",
			Sequence: observation.Status.Sequence, LogSequence: observation.Logs[0].Sequence}, httpControlBytes)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(ack)))}, nil
	})}
	base, _ := url.Parse("http://worker.test")
	transport := &httpExecutionClient{client: client, base: base}
	status := newWorkerStatus(job)
	event := attemptRecord{Kind: attemptRecordDiscussion, Text: "before secret after", Call: "call-1",
		Agent: "agent secret", Role: "main", EventSource: "claude"}
	if err := status.emit(t.Context(), transport, event, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := status.drain(t.Context(), transport); err != nil {
		t.Fatal(err)
	}
	if len(received) != 3 {
		t.Fatalf("Status requests = %d, want event, replay and coverage", len(received))
	}
	first, replay, coverage := received[0], received[1], received[2]
	if first.Status.Sequence != 1 || replay.Status.Sequence != 2 || first.Status.LogSequence != 1 ||
		replay.Status.LogSequence != 1 || len(first.Logs) != 1 || len(replay.Logs) != 1 ||
		first.Logs[0] != replay.Logs[0] {
		t.Fatalf("lost ACK changed log replay: first=%+v replay=%+v", first, replay)
	}
	got := first.Logs[0]
	if got.Sequence != 1 || got.Identity != job.Identity || got.SourceRef != job.Source.Head ||
		got.Kind != event.Kind || got.Text != "before [redacted] after" || got.Call != event.Call ||
		got.Agent != "agent [redacted]" || got.Role != event.Role || got.EventSource != event.EventSource {
		t.Fatalf("event metadata or pre-transit redaction changed: %+v", got)
	}
	if coverage.Status.Sequence != 3 || coverage.Status.LogSequence != 2 || len(coverage.Logs) != 1 ||
		coverage.Logs[0].Sequence != 2 || coverage.Logs[0].Kind != attemptRecordCoverage || coverage.Logs[0].Text != "" {
		t.Fatalf("terminal coverage marker = %+v", coverage)
	}
}

func TestWorkerStatusCoverageRequiresSuppliedAndDeliveredEvents(t *testing.T) {
	job := testJob(command.Comment)
	var received []httpObservation
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		var observation httpObservation
		if err := httpDecode(r.Body, httpControlBytes, &observation); err != nil {
			return nil, err
		}
		received = append(received, observation)
		return nil, errors.New("offline")
	})}
	base, _ := url.Parse("http://worker.test")
	transport := &httpExecutionClient{client: client, base: base}
	status := newWorkerStatus(job)
	if err := status.drain(t.Context(), transport); err != nil || len(received) != 0 {
		t.Fatalf("zero-event drain sent coverage: error=%v requests=%d", err, len(received))
	}
	status = newWorkerStatus(job)
	if err := status.emit(t.Context(), transport, attemptRecord{Kind: attemptRecordDiscussion, Text: "first"}); err == nil {
		t.Fatal("failed event was reported delivered")
	}
	if err := status.emit(t.Context(), transport, attemptRecord{Kind: attemptRecordDiscussion, Text: "second"}); err == nil {
		t.Fatal("accepted a second event behind an unacknowledged record")
	}
	if err := status.drain(t.Context(), transport); err == nil {
		t.Fatal("failed record yielded terminal coverage")
	}
	if len(received) != 3 {
		t.Fatalf("requests = %d, want two attempts and one drain", len(received))
	}
	for _, observation := range received {
		if len(observation.Logs) != 1 || observation.Logs[0].Sequence != 1 || observation.Logs[0].Text != "first" {
			t.Fatalf("more than one unacknowledged record or coverage sent: %+v", observation)
		}
	}
}

func TestWorkerStatusSplitsRedactedUTF8BeforeTransit(t *testing.T) {
	job := testJob(command.Comment)
	var records []attemptRecord
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		var observation httpObservation
		if err := httpDecode(r.Body, httpControlBytes, &observation); err != nil {
			return nil, err
		}
		if len(observation.Logs) != 1 {
			return nil, errors.New("missing log")
		}
		records = append(records, observation.Logs[0])
		ack, err := httpEncode(httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "status",
			Sequence: observation.Status.Sequence, LogSequence: observation.Logs[0].Sequence}, httpControlBytes)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(ack)))}, nil
	})}
	base, _ := url.Parse("http://worker.test")
	status := newWorkerStatus(job)
	text := strings.Repeat("x", 9*maxAttemptRecordTextBytes-2) + "🔐secret"
	if len(text) <= httpControlBytes {
		t.Fatal("test event does not exceed one Status body")
	}
	if err := status.emit(t.Context(), &httpExecutionClient{client: client, base: base},
		attemptRecord{Kind: attemptRecordDiscussion, Text: text, More: false}, "secret"); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for i, record := range records {
		if record.Sequence != uint64(i+1) || record.More != (i < len(records)-1) ||
			len(record.Text) > maxAttemptRecordTextBytes || !utf8.ValidString(record.Text) {
			t.Fatalf("invalid fragment %d: %+v", i, record)
		}
		joined.WriteString(record.Text)
	}
	if len(records) < 2 || joined.String() != redactActionText(text, "secret") {
		t.Fatalf("split event did not round-trip: fragments=%d", len(records))
	}
}

func TestWorkerStatusOpenContinuationDoesNotClaimCoverage(t *testing.T) {
	job := testJob(command.Comment)
	var records []attemptRecord
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		var observation httpObservation
		if err := httpDecode(r.Body, httpControlBytes, &observation); err != nil {
			return nil, err
		}
		records = append(records, observation.Logs...)
		ack, err := httpEncode(httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "status",
			Sequence: observation.Status.Sequence, LogSequence: observation.Logs[0].Sequence}, httpControlBytes)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(ack)))}, nil
	})}
	base, _ := url.Parse("http://worker.test")
	transport := &httpExecutionClient{client: client, base: base}
	status := newWorkerStatus(job)
	if err := status.emit(t.Context(), transport, attemptRecord{Kind: attemptRecordDiscussion, Text: "partial", More: true}); err != nil {
		t.Fatal(err)
	}
	if err := status.drain(t.Context(), transport); err == nil {
		t.Fatal("open continuation produced coverage")
	}
	if len(records) != 1 || records[0].Kind != attemptRecordDiscussion {
		t.Fatalf("open continuation records = %+v", records)
	}
}
