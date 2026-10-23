package action

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workroot"
)

const (
	httpFailureMessageLimit = 8 << 10
	httpFailureScopeLimit   = 4096
)

// httpFailure carries only the policy facts used across one execution transaction.
// Message is a private diagnostic; original parent callback errors remain local.
type httpFailure struct {
	Message           string                   `json:"message"`
	Terminal          bool                     `json:"terminal,omitempty"`
	Canceled          bool                     `json:"canceled,omitempty"`
	Deadline          bool                     `json:"deadline,omitempty"`
	Unsafe            bool                     `json:"unsafe,omitempty"`
	Descendants       bool                     `json:"descendants,omitempty"`
	Unconfirmed       bool                     `json:"unconfirmed,omitempty"`
	ScopeUnconfirmed  bool                     `json:"scope_unconfirmed,omitempty"`
	Exhausted         bool                     `json:"exhausted,omitempty"`
	OutcomeUnknown    bool                     `json:"outcome_unknown,omitempty"`
	RequestTransport  bool                     `json:"request_transport,omitempty"`
	MissingExecutable bool                     `json:"missing_executable,omitempty"`
	NoSpace           bool                     `json:"no_space,omitempty"`
	QuotaExceeded     bool                     `json:"quota_exceeded,omitempty"`
	OutputLimit       bool                     `json:"output_limit,omitempty"`
	Exhaustion        *processgroup.Exhaustion `json:"exhaustion,omitempty"`
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
		Descendants: errors.Is(err, processgroup.ErrDescendants), Unconfirmed: errors.Is(err, processgroup.ErrUnconfirmed),
		ScopeUnconfirmed: errors.Is(err, processgroup.ErrScopeUnconfirmed), Exhausted: errors.Is(err, processgroup.ErrExhausted),
		OutcomeUnknown:    errors.Is(err, monitor.ErrExecutionOutcomeUnknown),
		RequestTransport:  errors.Is(err, monitor.ErrRequestTransport) || errors.As(err, &network),
		MissingExecutable: errors.Is(err, exec.ErrNotFound), NoSpace: errors.Is(err, syscall.ENOSPC),
		QuotaExceeded: errors.Is(err, syscall.EDQUOT), OutputLimit: errors.Is(err, aicli.ErrOutputLimit),
	}
	var exhaustion *processgroup.Exhaustion
	if errors.As(err, &exhaustion) && exhaustion != nil {
		copy := *exhaustion
		failure.Exhaustion = &copy
		if detailErr := failure.validate(); detailErr != nil {
			// Scope is semantic recovery evidence. Omit an unrepresentable detail
			// explicitly, retaining its actual flags, rather than shorten its path.
			failure.Exhaustion = nil
			failure.OutcomeUnknown = true
			failure.Message = httpFailureDiagnostic("Process exhaustion details omitted (" + detailErr.Error() + "): " + err.Error())
		}
	}
	return failure
}

// Independent control uncertainty cannot erase an actual execution or process-safety fact.
func httpWatchUncertainty(err error) bool {
	f := newHTTPFailure(err)
	return f != nil && f.OutcomeUnknown && !f.Terminal && !f.Unsafe &&
		!f.Descendants && !f.Unconfirmed && !f.ScopeUnconfirmed && !f.Exhausted &&
		!f.MissingExecutable && !f.NoSpace && !f.QuotaExceeded && !f.OutputLimit
}

func (f *httpFailure) validate() error {
	if f == nil {
		return nil
	}
	if len(f.Message) > httpFailureMessageLimit || !utf8.ValidString(f.Message) {
		return errors.New("invalid HTTP failure diagnostic")
	}
	if f.Exhaustion == nil {
		return nil
	}
	e := f.Exhaustion
	if !f.Exhausted || e.MaxTasks <= 0 || e.MaxMemoryBytes <= 0 {
		return errors.New("invalid HTTP exhaustion ceilings or cause")
	}
	switch e.Stop {
	case "natural", "graceful", "forced", "start_failed":
	default:
		return errors.New("invalid HTTP exhaustion stop")
	}
	if e.PIDEventsMax == 0 && e.MemoryEventsMax == 0 && e.MemoryOOM == 0 && e.MemorySwapEventsMax == 0 {
		return errors.New("HTTP exhaustion has no limit event")
	}
	if (e.MemoryOOMKill != 0 || e.MemoryOOMGroupKill != 0) && e.MemoryEventsMax == 0 && e.MemoryOOM == 0 {
		return errors.New("HTTP exhaustion has an unattributed OOM kill")
	}
	if len(e.Scope) > httpFailureScopeLimit || !utf8.ValidString(e.Scope) || strings.ContainsRune(e.Scope, 0) {
		return errors.New("invalid HTTP exhaustion scope")
	}
	if e.Released && e.Scope != "" || !e.Released && (e.Scope == "" || !f.ScopeUnconfirmed) {
		return errors.New("contradictory HTTP exhaustion release facts")
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
		{f.Descendants, processgroup.ErrDescendants}, {f.Unconfirmed, processgroup.ErrUnconfirmed},
		{f.ScopeUnconfirmed, processgroup.ErrScopeUnconfirmed}, {f.Exhausted && f.Exhaustion == nil, processgroup.ErrExhausted},
		{f.OutcomeUnknown, monitor.ErrExecutionOutcomeUnknown},
		{f.RequestTransport, monitor.ErrRequestTransport},
		{f.MissingExecutable, exec.ErrNotFound}, {f.NoSpace, syscall.ENOSPC},
		{f.QuotaExceeded, syscall.EDQUOT}, {f.OutputLimit, aicli.ErrOutputLimit},
	} {
		if fact.present {
			causes = append(causes, fact.cause)
		}
	}
	if f.Exhaustion != nil {
		copy := *f.Exhaustion
		causes = append(causes, &copy)
	}
	return errors.Join(causes...)
}

// Worker diagnostics do not establish parent process or retry policy.
func (f *httpFailure) workerErr() error {
	if f == nil {
		return nil
	}
	if err := f.validate(); err != nil {
		return err
	}
	claim := *f
	claim.Message = httpFailureDiagnostic(strconv.Quote(f.Message))
	claim.ScopeUnconfirmed, claim.Unconfirmed = false, false
	claim.Unsafe, claim.RequestTransport = false, false
	if claim.Exhaustion != nil && !claim.Exhaustion.Released {
		claim.Exhaustion = nil // Its unproved release scope is also a worker claim.
	}
	return workerFailureError{message: claim.Message, cause: claim.err()}
}

type workerFailureError struct {
	message string
	cause   error
}

func (e workerFailureError) Error() string { return "worker execution failed: " + e.message }
func (e workerFailureError) Unwrap() error { return e.cause }

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
