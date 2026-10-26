package action

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

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

// WorkerCredentials are trusted parent input for one assignment, outside Job.
type WorkerCredentials struct {
	CodexAPIKey       string `json:"codex_api_key,omitempty"`
	CodexNativeAPIKey string `json:"codex_native_api_key,omitempty"`
	CodexAccessToken  string `json:"codex_access_token,omitempty"`
	ClaudeAPIKey      string `json:"claude_api_key,omitempty"`
}

const maxWorkerCredentialBytes = 8 << 10

func (credentials WorkerCredentials) validate() error {
	// Bound each field before summing so the aggregate cannot overflow int.
	if len(credentials.CodexAPIKey) > maxWorkerCredentialBytes || len(credentials.CodexNativeAPIKey) > maxWorkerCredentialBytes ||
		len(credentials.CodexAccessToken) > maxWorkerCredentialBytes ||
		len(credentials.ClaudeAPIKey) > maxWorkerCredentialBytes ||
		len(credentials.CodexAPIKey)+len(credentials.CodexNativeAPIKey)+len(credentials.CodexAccessToken)+len(credentials.ClaudeAPIKey) > maxWorkerCredentialBytes {
		return errors.New("worker credentials exceed limit")
	}
	for _, value := range []string{credentials.CodexAPIKey, credentials.CodexNativeAPIKey, credentials.CodexAccessToken, credentials.ClaudeAPIKey} {
		if !utf8.ValidString(value) || strings.ContainsAny(value, " \t\r\n\x00") {
			return errors.New("worker credentials are invalid")
		}
	}
	if credentials.CodexAPIKey != "" && (credentials.CodexNativeAPIKey != "" || credentials.CodexAccessToken != "") {
		return errors.New("worker credentials mix configured and native Codex authentication")
	}
	return nil
}

func (credentials WorkerCredentials) validateJob(job Job) error {
	if err := credentials.validate(); err != nil {
		return err
	}
	selected := func(provider string) bool {
		return job.Main.Provider == provider || job.Critic != nil && job.Critic.Provider == provider
	}
	if !selected("codex") && (credentials.CodexAPIKey != "" || credentials.CodexNativeAPIKey != "" || credentials.CodexAccessToken != "") ||
		!selected("claude") && credentials.ClaudeAPIKey != "" {
		return errors.New("worker credentials include an unselected provider")
	}
	return nil
}

type httpAssignment struct {
	Job         Job               `json:"job"`
	Credentials WorkerCredentials `json:"credentials"`
	HeldFor     time.Duration     `json:"held_for_ns"`
	StopGrace   time.Duration     `json:"stop_grace_ns"`
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
	Status Status `json:"status"`
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
	Reason        string        `json:"reason"`
	StopRemaining time.Duration `json:"stop_remaining_ns"`
}
