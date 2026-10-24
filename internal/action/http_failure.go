package action

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
)

const httpFailureMessageLimit = 8 << 10

// httpFailure carries finite wire details. Parent ACKs keep typed causes;
// worker outcome fields are diagnostic claims at the receiver.
type httpFailure struct {
	Message           string `json:"message"`
	Terminal          bool   `json:"terminal,omitempty"`
	Canceled          bool   `json:"canceled,omitempty"`
	Deadline          bool   `json:"deadline,omitempty"`
	Unsafe            bool   `json:"unsafe,omitempty"`
	OutcomeUnknown    bool   `json:"outcome_unknown,omitempty"`
	RequestTransport  bool   `json:"request_transport,omitempty"`
	MissingExecutable bool   `json:"missing_executable,omitempty"`
	NoSpace           bool   `json:"no_space,omitempty"`
	QuotaExceeded     bool   `json:"quota_exceeded,omitempty"`
	OutputLimit       bool   `json:"output_limit,omitempty"`
}

func newHTTPFailure(err error) *httpFailure {
	if err == nil {
		return nil
	}
	var network *url.Error
	failure := &httpFailure{
		Message:  httpFailureDiagnostic(err.Error()),
		Terminal: errors.Is(err, monitor.ErrTerminalJob), Canceled: errors.Is(err, context.Canceled),
		Deadline: errors.Is(err, context.DeadlineExceeded), Unsafe: errors.Is(err, workroot.ErrUnsafe),
		OutcomeUnknown:    errors.Is(err, monitor.ErrExecutionOutcomeUnknown),
		RequestTransport:  errors.Is(err, monitor.ErrRequestTransport) || errors.As(err, &network),
		MissingExecutable: errors.Is(err, exec.ErrNotFound), NoSpace: errors.Is(err, syscall.ENOSPC),
		QuotaExceeded: errors.Is(err, syscall.EDQUOT), OutputLimit: errors.Is(err, aicli.ErrOutputLimit),
	}
	return failure
}

// Independent control uncertainty cannot erase an actual execution failure.
func httpWatchUncertainty(err error) bool {
	f := newHTTPFailure(err)
	return f != nil && f.OutcomeUnknown && !f.Terminal && !f.Unsafe &&
		!f.MissingExecutable && !f.NoSpace && !f.QuotaExceeded && !f.OutputLimit
}

func (f *httpFailure) validate() error {
	if f == nil {
		return nil
	}
	if len(f.Message) > httpFailureMessageLimit || !utf8.ValidString(f.Message) {
		return errors.New("invalid HTTP failure diagnostic")
	}
	return nil
}

func (f *httpFailure) err() error {
	if f == nil {
		return nil
	}
	if err := f.validate(); err != nil {
		return err
	}
	causes := []error{errors.New(f.Message)}
	for _, fact := range []struct {
		present bool
		cause   error
	}{
		{f.Terminal, monitor.ErrTerminalJob}, {f.Canceled, context.Canceled},
		{f.Deadline, context.DeadlineExceeded}, {f.Unsafe, workroot.ErrUnsafe},
		{f.OutcomeUnknown, monitor.ErrExecutionOutcomeUnknown},
		{f.RequestTransport, monitor.ErrRequestTransport},
		{f.MissingExecutable, exec.ErrNotFound}, {f.NoSpace, syscall.ENOSPC},
		{f.QuotaExceeded, syscall.EDQUOT}, {f.OutputLimit, aicli.ErrOutputLimit},
	} {
		if fact.present {
			causes = append(causes, fact.cause)
		}
	}
	return errors.Join(causes...)
}

// workerFailureError keeps the bounded worker report available for private retention.
// Its public error text cannot carry worker-controlled content into parent logs.
type workerFailureError struct{ failure httpFailure }

func (*workerFailureError) Error() string {
	return "worker execution failed"
}

// Worker diagnostics do not establish parent process, retry or capacity policy.
func (f *httpFailure) workerErr() error {
	if f == nil {
		return nil
	}
	if err := f.validate(); err != nil {
		return err
	}
	return &workerFailureError{failure: *f}
}

func httpFailureDiagnostic(message string) string {
	truncated := len(message) > httpFailureMessageLimit
	if truncated {
		message = message[:httpFailureMessageLimit]
	}
	message = strings.ToValidUTF8(message, "\uFFFD")
	if truncated || len(message) > httpFailureMessageLimit {
		const suffix = " [truncated]"
		limit := min(len(message), httpFailureMessageLimit-len(suffix))
		message = message[:limit]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += suffix
	}
	return message
}
