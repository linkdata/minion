package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/config"
)

const httpControlTimeout = 30 * time.Second

// Required for a current remote Outcome's read lane; the decoded Result still decides admission.
const httpOutcomeBindingHeader = "X-Minion-Outcome-Binding"

type httpBinding struct {
	Version   int      `json:"version"`
	Identity  Identity `json:"identity"`
	SourceRef string   `json:"source_ref"`
}

func httpJobBinding(job Job) httpBinding {
	return httpBinding{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
}

func httpBindingDigest(binding httpBinding) string {
	data, _ := json.Marshal(binding) // httpBinding contains only JSON scalar fields.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type httpPull struct {
	Version             int    `json:"version"`
	ExecutorID          string `json:"executor_id"`
	AuditHostSupported  bool   `json:"audit_host_supported,omitempty"`
	PreflightGeneration uint64 `json:"preflight_generation,omitempty"`
	PreflightResult     string `json:"preflight_result,omitempty"`
}

type remoteChoicePreflight struct {
	Version             int                      `json:"version"`
	ExecutorID          string                   `json:"executor_id"`
	Main                RoleSettings             `json:"main"`
	Critic              *RoleSettings            `json:"critic,omitempty"`
	MainChoices         []config.RoleModelConfig `json:"main_choices,omitempty"`
	CriticChoices       []config.RoleModelConfig `json:"critic_choices,omitempty"`
	Credentials         WorkerCredentials        `json:"credentials"`
	PreflightGeneration uint64                   `json:"preflight_generation,omitempty"`
	Refresh             bool                     `json:"refresh,omitempty"`
}

// WorkerCredentials are trusted parent input for one assignment, outside Job.
type WorkerCredentials struct {
	CodexAPIKey       string `json:"codex_api_key,omitempty"`
	CodexNativeAPIKey string `json:"codex_native_api_key,omitempty"`
	CodexAccessToken  string `json:"codex_access_token,omitempty"`
	ClaudeAPIKey      string `json:"claude_api_key,omitempty"`
}

func (e *Orchestrator) workerCredentials(main, critic string) WorkerCredentials {
	var credentials WorkerCredentials
	if main == "codex" || critic == "codex" {
		credentials.CodexAPIKey = e.Runner.Codex.APIKey
		if credentials.CodexAPIKey == "" {
			credentials.CodexNativeAPIKey = e.Runner.Codex.NativeAPIKey
			credentials.CodexAccessToken = e.Runner.Codex.NativeAccessToken
		}
	}
	if main == "claude" || critic == "claude" {
		credentials.ClaudeAPIKey = e.Runner.Claude.APIKey
	}
	return credentials
}

const maxWorkerAccessTokenBytes = 4 << 10

// Validate checks credentials before they cross the worker boundary.
func (credentials WorkerCredentials) Validate() error {
	if len(credentials.CodexAPIKey) > config.MaxProviderAPIKeyBytes || len(credentials.CodexNativeAPIKey) > config.MaxProviderAPIKeyBytes ||
		len(credentials.CodexAccessToken) > maxWorkerAccessTokenBytes ||
		len(credentials.ClaudeAPIKey) > config.MaxProviderAPIKeyBytes {
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
	if err := credentials.Validate(); err != nil {
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
	Status Status          `json:"status"`
	Logs   []attemptRecord `json:"logs,omitempty"`
}

type httpOutcome struct {
	Sequence uint64       `json:"sequence"`
	Result   Result       `json:"result"`
	Failure  *httpFailure `json:"failure,omitempty"`
}

type httpAcknowledgement struct {
	httpBinding
	Sequence    uint64       `json:"sequence"`
	LogSequence uint64       `json:"log_sequence,omitempty"`
	Kind        string       `json:"kind"`
	Failure     *httpFailure `json:"failure,omitempty"`
}

type httpCancellation struct {
	httpBinding
	Reason        string        `json:"reason"`
	StopRemaining time.Duration `json:"stop_remaining_ns"`
}
