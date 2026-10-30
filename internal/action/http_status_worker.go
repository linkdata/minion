package action

import (
	"context"
	"sync"
	"time"
)

const statusHeartbeatInterval = 15 * time.Second
const statusSendTimeout = 5 * time.Second

// workerStatus keeps observations separate from the required checkpoint stream.
type workerStatus struct {
	mu         sync.Mutex
	value      Status
	progressAt time.Time
	changed    chan struct{}
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

func (s *workerStatus) progress(phase string, round int, reviewPhase string, err error) {
	s.mu.Lock()
	s.value.Phase, s.value.Round, s.value.ReviewPhase = phase, round, reviewPhase
	s.value.ProgressSequence++
	s.progressAt = time.Now()
	if err != nil {
		s.value.Error = httpFailureDiagnostic(err.Error())
	}
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

func (s *workerStatus) send(ctx context.Context, transport *httpExecutionClient) {
	status := s.snapshot()
	data, err := httpEncode(httpObservation{Status: status}, httpControlBytes)
	if err != nil {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, statusSendTimeout)
	defer cancel()
	var ack httpAcknowledgement
	_ = transport.post(requestCtx, "status", data, httpControlBytes, &ack,
		httpBinding{Version: status.Version, Identity: status.Identity, SourceRef: status.SourceRef}, status.Sequence)
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
		s.send(ctx, transport)
	}
}
