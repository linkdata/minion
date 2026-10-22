package action

import "time"

const httpControlTimeout = 30 * time.Second

type httpBinding struct {
	Version   int      `json:"version"`
	Identity  Identity `json:"identity"`
	SourceRef string   `json:"source_ref"`
}

func httpJobBinding(job Job) httpBinding {
	return httpBinding{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
}

type httpPull struct {
	Version    int    `json:"version"`
	ExecutorID string `json:"executor_id"`
}

type httpAssignment struct {
	Job     Job   `json:"job"`
	Expires int64 `json:"expires_unix_ns"`
}

type httpCall struct {
	Sequence   uint64         `json:"sequence"`
	Checkpoint CallCheckpoint `json:"checkpoint"`
}

type httpReview struct {
	Sequence   uint64           `json:"sequence"`
	Checkpoint ReviewCheckpoint `json:"checkpoint"`
}

type httpObservation struct {
	Sequence uint64 `json:"sequence"`
	Status   Status `json:"status"`
}

type httpOutcome struct {
	Sequence uint64       `json:"sequence"`
	Result   Result       `json:"result"`
	Failure  *httpFailure `json:"failure,omitempty"`
}

type httpAcknowledgement struct {
	httpBinding
	Sequence uint64       `json:"sequence"`
	Kind     string       `json:"kind"`
	Failure  *httpFailure `json:"failure,omitempty"`
}

type httpCancellation struct {
	httpBinding
	Reason string `json:"reason"`
}
