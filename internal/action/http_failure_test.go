package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
)

func TestHTTPFailurePreservesFiniteCauses(t *testing.T) {
	causes := []struct {
		name string
		err  error
	}{
		{"terminal", monitor.ErrTerminalJob},
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"unsafe root", workroot.ErrUnsafe},
		{"outcome unknown", monitor.ErrExecutionOutcomeUnknown},
		{"request transport", monitor.ErrRequestTransport},
		{"missing executable", exec.ErrNotFound},
		{"no space", syscall.ENOSPC},
		{"quota exceeded", syscall.EDQUOT},
		{"output limit", aicli.ErrOutputLimit},
	}
	for _, cause := range causes {
		t.Run(cause.name, func(t *testing.T) {
			failure := roundTripHTTPFailure(t, newHTTPFailure(fmt.Errorf("native call: %w", cause.err)))
			got := failure.err()
			for _, other := range causes {
				if errors.Is(got, other.err) != (other.err == cause.err) {
					t.Fatalf("cause %q changed after wire round trip: %v", other.name, got)
				}
			}
		})
	}
	t.Run("joined", func(t *testing.T) {
		var joined []error
		for _, cause := range causes {
			joined = append(joined, cause.err)
		}
		got := roundTripHTTPFailure(t, newHTTPFailure(errors.Join(joined...))).err()
		for _, cause := range causes {
			if !errors.Is(got, cause.err) {
				t.Fatalf("joined cause %q lost: %v", cause.name, got)
			}
		}
	})
	t.Run("ordinary preparation error", func(t *testing.T) {
		source := fmt.Errorf("prepare native command: %w", os.ErrNotExist)
		failure := roundTripHTTPFailure(t, newHTTPFailure(source))
		got := failure.err()
		if !strings.Contains(got.Error(), source.Error()) || errors.Is(got, os.ErrNotExist) {
			t.Fatalf("ordinary diagnostic or finite error boundary changed: %v", got)
		}
		for _, cause := range causes {
			if errors.Is(got, cause.err) {
				t.Fatalf("ordinary failure invented %q: %v", cause.name, got)
			}
		}
	})
	var absent *httpFailure
	if newHTTPFailure(nil) != nil || absent.validate() != nil || absent.err() != nil {
		t.Fatal("nil failure became a wire failure")
	}
}

func TestHTTPFailureCarriesOnlyInactivityLabelFromWorker(t *testing.T) {
	failure := roundTripHTTPFailure(t, newHTTPFailure(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity)))
	if !failure.Inactivity || !failure.Terminal || failure.Deadline {
		t.Fatalf("inactivity changed across wire: %+v", failure)
	}
	workerErr := failure.workerErr()
	var claim interface{ ExecutorInactivity() bool }
	if !errors.As(workerErr, &claim) || !claim.ExecutorInactivity() ||
		errors.Is(workerErr, monitor.ErrTerminalJob) || errors.Is(workerErr, context.DeadlineExceeded) {
		t.Fatalf("worker claim gained parent policy authority: %v", workerErr)
	}
}

func TestHTTPFailureCarriesRuntimeCapOnlyAsWorkerClaim(t *testing.T) {
	failure := roundTripHTTPFailure(t, newHTTPFailure(errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap)))
	if !failure.RuntimeCap || !failure.Deadline || !failure.Terminal || failure.Inactivity {
		t.Fatalf("worker runtime cap changed across wire: %+v", failure)
	}
	workerErr := failure.workerErr()
	var claim *workerFailureError
	if !errors.As(workerErr, &claim) || !claim.failure.RuntimeCap ||
		errors.Is(workerErr, monitor.ErrTerminalJob) || errors.Is(workerErr, context.DeadlineExceeded) {
		t.Fatalf("worker cap claim gained parent policy authority: %v", workerErr)
	}
}

func TestHTTPFailureGenericDeadlineIsNotRuntimeCap(t *testing.T) {
	failure := roundTripHTTPFailure(t, newHTTPFailure(context.DeadlineExceeded))
	if !failure.Deadline || failure.RuntimeCap {
		t.Fatalf("generic deadline became worker runtime cap: %+v", failure)
	}
}

func TestHTTPFailurePreservesRequestTransportPolicyWithoutReconstructingURL(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			var source error = fmt.Errorf("watch admission: %w", &url.Error{Op: "Post", URL: "https://executor.invalid/cancel", Err: io.ErrUnexpectedEOF})
			if unknown {
				source = errors.Join(source, monitor.ErrExecutionOutcomeUnknown)
			}
			failure := roundTripHTTPFailure(t, newHTTPFailure(source))
			got := failure.err()
			var network *url.Error
			if !failure.RequestTransport || !errors.Is(got, monitor.ErrRequestTransport) || errors.Is(got, monitor.ErrExecutionOutcomeUnknown) != unknown || errors.As(got, &network) {
				t.Fatalf("finite request policy was lost or an arbitrary URL error was invented: failure=%+v error=%v", failure, got)
			}
		})
	}
}

func TestHTTPFailureRejectsMalformedDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, message, observed string
	}{
		{name: "oversized diagnostic", message: strings.Repeat("x", httpFailureMessageLimit+1)},
		{name: "invalid diagnostic UTF8", message: "bad\xff"},
		{name: "oversized observed diagnostic", message: "limit", observed: strings.Repeat("x", httpFailureMessageLimit+1)},
		{name: "invalid observed diagnostic UTF8", message: "limit", observed: "bad\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &httpFailure{Message: tc.message, ObservedMessage: tc.observed}
			if failure.validate() == nil {
				t.Fatal("malformed failure accepted")
			}
		})
	}
}

func TestHTTPFailureRejectsConflictingWorkerLimits(t *testing.T) {
	failure := &httpFailure{Message: "worker limit", Inactivity: true, RuntimeCap: true}
	if err := failure.validate(); err == nil {
		t.Fatal("conflicting worker limit claims accepted")
	}
}

func TestHTTPFailureBoundsDiagnosticsWithoutChangingOrdinaryFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		message   string
		truncated bool
	}{
		{"empty", "", false},
		{"ASCII boundary", strings.Repeat("x", httpFailureMessageLimit), false},
		{"UTF8 boundary", strings.Repeat("α", httpFailureMessageLimit/2), false},
		{"oversized ASCII", strings.Repeat("x", httpFailureMessageLimit+1), true},
		{"split UTF8 boundary", strings.Repeat("x", httpFailureMessageLimit-1) + "€", true},
		{"invalid UTF8", "diagnostic\xff", false},
		{"UTF8 repair expands", strings.Repeat("x\xff", httpFailureMessageLimit/2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := roundTripHTTPFailure(t, newHTTPFailure(errors.New(tc.message)))
			if len(failure.Message) > httpFailureMessageLimit || !utf8.ValidString(failure.Message) || strings.HasSuffix(failure.Message, " [truncated]") != tc.truncated {
				t.Fatalf("diagnostic bound/UTF8 invalid: bytes=%d truncated=%t message=%q", len(failure.Message), tc.truncated, failure.Message)
			}
			if failure.OutcomeUnknown || failure.Terminal {
				t.Fatalf("ordinary diagnostic shortening changed retry policy: %+v", failure)
			}
			if utf8.ValidString(tc.message) && !tc.truncated && failure.Message != tc.message {
				t.Fatal("representable diagnostic changed")
			}
		})
	}
}

func TestHTTPFailureEscapedMaximumFitsControlBody(t *testing.T) {
	failure := &httpFailure{
		Message:  strings.Repeat("\x01", httpFailureMessageLimit),
		Terminal: true, Canceled: true, Deadline: true, RuntimeCap: true, Unsafe: true, OutcomeUnknown: true,
		RequestTransport: true, MissingExecutable: true, NoSpace: true, QuotaExceeded: true, OutputLimit: true,
	}
	if err := failure.validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(failure)
	if err != nil || len(encoded)+1 > 128<<10 {
		t.Fatalf("finite failure exceeds shared control cap including newline: bytes=%d error=%v", len(encoded)+1, err)
	}
}

func roundTripHTTPFailure(t *testing.T, failure *httpFailure) *httpFailure {
	t.Helper()
	encoded, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	var decoded *httpFailure
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.validate(); err != nil {
		t.Fatalf("encoded failure failed validation: %v", err)
	}
	return decoded
}
