package action

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"
)

const statusHeartbeatInterval = 15 * time.Second
const statusSendTimeout = 5 * time.Second

// workerStatus keeps observations separate from the required checkpoint stream.
type workerStatus struct {
	mu         sync.Mutex
	sendMu     sync.Mutex
	value      Status
	progressAt time.Time
	changed    chan struct{}
	ack        uint64
	pending    *attemptRecord
	supplied   bool
	incomplete bool
	closed     bool
}

func newWorkerStatus(job Job) *workerStatus {
	status := &workerStatus{
		value: Status{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
			ProgressSequence: 1, WorkerState: "busy", Phase: "prepare"},
		progressAt: time.Now(), changed: make(chan struct{}, 1),
	}
	status.changed <- struct{}{}
	return status
}

func (s *workerStatus) progress(phase string, round int, reviewPhase string) {
	s.mu.Lock()
	s.value.Phase, s.value.Round, s.value.ReviewPhase = phase, round, reviewPhase
	s.value.ProgressSequence++
	s.progressAt = time.Now()
	s.mu.Unlock()
	s.signal()
}

func (s *workerStatus) stopping() {
	s.mu.Lock()
	s.value.WorkerState = "stopping"
	s.mu.Unlock()
	s.signal()
}

func (s *workerStatus) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *workerStatus) snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value.Sequence++
	s.value.ProgressAge = time.Since(s.progressAt)
	return s.value
}

func (s *workerStatus) send(ctx context.Context, transport *httpExecutionClient) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendLocked(ctx, transport)
}

func (s *workerStatus) sendLocked(ctx context.Context, transport *httpExecutionClient) error {
	status := s.snapshot()
	observation := httpObservation{Status: status}
	if s.pending != nil {
		observation.Logs = []attemptRecord{*s.pending}
		observation.Status.LogSequence = s.pending.Sequence
	} else {
		observation.Status.LogSequence = s.ack
	}
	data, err := httpEncode(observation, httpControlBytes)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, statusSendTimeout)
	defer cancel()
	var ack httpAcknowledgement
	if err := transport.post(requestCtx, "status", data, httpControlBytes, &ack,
		httpBinding{Version: status.Version, Identity: status.Identity, SourceRef: status.SourceRef}, status.Sequence); err != nil {
		return err
	}
	if ack.httpBinding != (httpBinding{Version: status.Version, Identity: status.Identity, SourceRef: status.SourceRef}) ||
		ack.Kind != "status" || ack.Sequence != status.Sequence || ack.Failure != nil ||
		ack.LogSequence < s.ack || ack.LogSequence > observation.Status.LogSequence {
		return errors.New("invalid HTTP status acknowledgement")
	}
	if s.pending != nil && ack.LogSequence != s.pending.Sequence {
		return errors.New("HTTP status log acknowledgement is behind the sent record")
	}
	s.ack = ack.LogSequence
	if s.pending != nil {
		s.pending = nil
	}
	return nil
}

func (s *workerStatus) markIncomplete() {
	s.sendMu.Lock()
	s.incomplete = true
	s.sendMu.Unlock()
}

// emit accepts one source event at a time; a failed record blocks later events.
func (s *workerStatus) emit(ctx context.Context, transport *httpExecutionClient, event attemptRecord, secrets ...string) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	if !closed && !s.incomplete {
		s.supplied = true
	}
	s.mu.Unlock()
	if closed || s.incomplete {
		return errors.New("HTTP status log stream is closed or incomplete")
	}
	fail := func(err error) error { s.incomplete = true; return err }
	if event.Kind == attemptRecordStop || event.Kind == attemptRecordCoverage {
		return fail(errors.New("HTTP status log event cannot be a marker"))
	}
	if event.More {
		return fail(errors.New("HTTP status log event must be assembled before transport"))
	}
	if !utf8.ValidString(event.Text) {
		return fail(errors.New("HTTP status log event is not valid UTF-8"))
	}
	if len(event.Text) > maxAttemptEventBytes {
		return fail(errAttemptRecordsFull)
	}
	if err := (attemptRecord{Kind: event.Kind, Call: event.Call, Agent: event.Agent,
		Role: event.Role, Provider: event.Provider, Model: event.Model, ObservedAt: event.ObservedAt,
		Round: event.Round, ReviewPhase: event.ReviewPhase, EventSource: event.EventSource}).validateShape(); err != nil {
		return fail(err)
	}
	raw := event
	event.Identity, event.SourceRef, event.Sequence = s.value.Identity, s.value.SourceRef, 0
	event.Text = redactActionText(event.Text, secrets...)
	event.Call = redactActionText(event.Call, secrets...)
	event.Agent = redactActionText(event.Agent, secrets...)
	event.Role = redactActionText(event.Role, secrets...)
	event.Provider = redactActionText(event.Provider, secrets...)
	event.Model = redactActionText(event.Model, secrets...)
	event.ObservedAt = redactActionText(event.ObservedAt, secrets...)
	event.ReviewPhase = redactActionText(event.ReviewPhase, secrets...)
	event.EventSource = redactActionText(event.EventSource, secrets...)
	if raw.Text != "" && event.Text == "" || raw.Call != "" && event.Call == "" ||
		raw.Agent != "" && event.Agent == "" || raw.Role != "" && event.Role == "" ||
		raw.Provider != "" && event.Provider == "" || raw.Model != "" && event.Model == "" ||
		raw.ObservedAt != "" && event.ObservedAt == "" ||
		raw.ReviewPhase != "" && event.ReviewPhase == "" ||
		raw.EventSource != "" && event.EventSource == "" {
		return fail(errors.New("HTTP status log cannot be safely redacted"))
	}
	if len(event.Text) > maxAttemptEventBytes {
		return fail(errAttemptRecordsFull)
	}
	if err := (attemptRecord{Kind: event.Kind, Call: event.Call, Agent: event.Agent, Role: event.Role,
		Provider: event.Provider, Model: event.Model, ObservedAt: event.ObservedAt,
		Round: event.Round, ReviewPhase: event.ReviewPhase, EventSource: event.EventSource}).validateShape(); err != nil {
		return fail(err)
	}
	for _, value := range []string{event.Text, event.Call, event.Agent, event.Role, event.Provider, event.Model,
		event.ObservedAt, event.ReviewPhase, event.EventSource} {
		data, err := json.Marshal(value)
		if err != nil {
			return fail(err)
		}
		for _, secret := range secrets {
			if secret != "" && containsConfiguredKey(data, secret) {
				return fail(errors.New("HTTP status log retains a configured credential"))
			}
		}
	}
	remaining := event.Text
	for {
		part := remaining
		if len(part) > maxAttemptRecordTextBytes {
			end := maxAttemptRecordTextBytes
			for !utf8.RuneStart(remaining[end]) {
				end--
			}
			part = remaining[:end]
		}
		remaining = remaining[len(part):]
		record := event
		record.Text, record.More, record.Sequence = part, len(remaining) != 0, s.ack+1
		if err := record.validateShape(); err != nil {
			return fail(err)
		}
		s.pending = &record
		if err := s.sendLocked(ctx, transport); err != nil {
			// Retry a lost ACK with fresh Status.Sequence and unchanged record.
			if retryErr := s.sendLocked(ctx, transport); retryErr != nil {
				return fail(retryErr)
			}
		}
		if len(remaining) == 0 {
			return nil
		}
	}
}

// drain closes the live stream before Result and marks coverage only when all
// supplied records reached the parent's durable highwater.
func (s *workerStatus) drain(ctx context.Context, transport *httpExecutionClient) error {
	s.mu.Lock()
	if !s.supplied {
		s.closed = true
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.pending != nil {
		if err := s.sendLocked(ctx, transport); err != nil {
			return err
		}
	}
	if s.incomplete {
		return errors.New("HTTP status log coverage is incomplete")
	}
	marker := attemptRecord{Identity: s.value.Identity, SourceRef: s.value.SourceRef,
		Sequence: s.ack + 1, Kind: attemptRecordCoverage}
	s.pending = &marker
	if err := s.sendLocked(ctx, transport); err != nil {
		return s.sendLocked(ctx, transport)
	}
	return nil
}

// finish lets optional coverage race the required outcome without holding it
// behind a lost Status acknowledgement.
func (s *workerStatus) finish(ctx context.Context, transport *httpExecutionClient, outcome func() error, beforeDrain func()) error {
	drainCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if beforeDrain != nil {
			beforeDrain()
		}
		_ = s.drain(drainCtx, transport)
	}()
	err := outcome()
	cancel()
	<-done
	return err
}

func (s *workerStatus) run(ctx context.Context, transport *httpExecutionClient) {
	ticker := time.NewTicker(statusHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.changed:
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return
		}
		_ = s.send(ctx, transport)
	}
}
