package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
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
		{"descendants", processgroup.ErrDescendants},
		{"unconfirmed", processgroup.ErrUnconfirmed},
		{"scope unconfirmed", processgroup.ErrScopeUnconfirmed},
		{"exhausted", processgroup.ErrExhausted},
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

func TestHTTPFailureExhaustionRoundTrip(t *testing.T) {
	for _, stop := range []string{"natural", "graceful", "forced", "start_failed"} {
		t.Run(stop, func(t *testing.T) {
			event := processgroup.Exhaustion{MaxTasks: 128, MaxMemoryBytes: 3 << 30, PIDEventsMax: 1, Stop: stop, Released: true}
			assertHTTPExhaustionRoundTrip(t, event)
		})
	}
	t.Run("unreleased scope and joined causes", func(t *testing.T) {
		event := processgroup.Exhaustion{MaxTasks: 128, MaxMemoryBytes: 3 << 30, MemoryEventsMax: 2, MemoryOOM: 1, MemoryOOMKill: 1, Stop: "forced", Scope: "/sys/fs/cgroup/minion-command-fixture"}
		assertHTTPExhaustionRoundTrip(t, event, monitor.ErrTerminalJob, context.Canceled, processgroup.ErrUnconfirmed, processgroup.ErrScopeUnconfirmed)
	})
	t.Run("finite facts are not configuration admission", func(t *testing.T) {
		event := processgroup.Exhaustion{MaxTasks: 4097, MaxMemoryBytes: math.MaxInt64, PIDEventsMax: math.MaxUint64, MemoryEventsMax: 1, MemoryOOMKill: math.MaxUint64, Stop: "natural", Released: true}
		assertHTTPExhaustionRoundTrip(t, event)
	})
	t.Run("maximum scope bytes", func(t *testing.T) {
		event := processgroup.Exhaustion{MaxTasks: 1, MaxMemoryBytes: 1, MemorySwapEventsMax: 1, Stop: "forced", Scope: "/" + strings.Repeat("x", httpFailureScopeLimit-1)}
		assertHTTPExhaustionRoundTrip(t, event, processgroup.ErrScopeUnconfirmed)
	})
	t.Run("released event can coexist with separate uncertain scope", func(t *testing.T) {
		event := processgroup.Exhaustion{MaxTasks: 1, MaxMemoryBytes: 1, PIDEventsMax: 1, Stop: "natural", Released: true}
		assertHTTPExhaustionRoundTrip(t, event, processgroup.ErrScopeUnconfirmed)
	})
}

func TestHTTPFailureRejectsMalformedFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*httpFailure)
	}{
		{"oversized diagnostic", func(f *httpFailure) { f.Message = strings.Repeat("x", httpFailureMessageLimit+1) }},
		{"invalid diagnostic UTF8", func(f *httpFailure) { f.Message = "bad\xff" }},
		{"event without exhausted cause", func(f *httpFailure) { f.Exhausted = false }},
		{"zero tasks", func(f *httpFailure) { f.Exhaustion.MaxTasks = 0 }},
		{"negative tasks", func(f *httpFailure) { f.Exhaustion.MaxTasks = -1 }},
		{"zero memory", func(f *httpFailure) { f.Exhaustion.MaxMemoryBytes = 0 }},
		{"negative memory", func(f *httpFailure) { f.Exhaustion.MaxMemoryBytes = -1 }},
		{"unfinished stop", func(f *httpFailure) { f.Exhaustion.Stop = "" }},
		{"unknown stop", func(f *httpFailure) { f.Exhaustion.Stop = "killed" }},
		{"no limit event", func(f *httpFailure) { f.Exhaustion.PIDEventsMax = 0 }},
		{"unattributed oom kill", func(f *httpFailure) { f.Exhaustion.MemoryOOMKill = 1 }},
		{"unattributed group oom kill", func(f *httpFailure) { f.Exhaustion.MemoryOOMGroupKill = 1 }},
		{"released with scope", func(f *httpFailure) { f.Exhaustion.Scope = "/scope" }},
		{"unreleased without scope", func(f *httpFailure) { f.Exhaustion.Released = false; f.ScopeUnconfirmed = true }},
		{"unreleased without cause", func(f *httpFailure) { f.Exhaustion.Released = false; f.Exhaustion.Scope = "/scope" }},
		{"oversized scope", func(f *httpFailure) {
			f.Exhaustion.Released = false
			f.ScopeUnconfirmed = true
			f.Exhaustion.Scope = strings.Repeat("x", httpFailureScopeLimit+1)
		}},
		{"invalid scope UTF8", func(f *httpFailure) {
			f.Exhaustion.Released = false
			f.ScopeUnconfirmed = true
			f.Exhaustion.Scope = "/bad\xff"
		}},
		{"NUL scope", func(f *httpFailure) {
			f.Exhaustion.Released = false
			f.ScopeUnconfirmed = true
			f.Exhaustion.Scope = "/bad\x00scope"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &httpFailure{Message: "diagnostic", Exhausted: true, Exhaustion: &processgroup.Exhaustion{MaxTasks: 128, MaxMemoryBytes: 3 << 30, PIDEventsMax: 1, Stop: "natural", Released: true}}
			tc.mutate(failure)
			if failure.validate() == nil {
				t.Fatal("malformed failure accepted")
			}
			if errors.Is(failure.err(), processgroup.ErrExhausted) {
				t.Fatal("malformed facts reconstructed a trusted exhausted cause")
			}
		})
	}
}

func TestHTTPFailureOmitsUnrepresentableDetailWithoutLosingCauses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
		stop  string
	}{
		{"oversized scope", "/" + strings.Repeat("x", httpFailureScopeLimit), "forced"},
		{"invalid scope UTF8", "/scope\xff", "forced"},
		{"NUL scope", "/scope\x00tail", "forced"},
		{"unfinished event", "/scope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := &processgroup.Exhaustion{MaxTasks: 128, MaxMemoryBytes: 3 << 30, PIDEventsMax: 1, Stop: tc.stop, Scope: tc.scope}
			source := errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded, processgroup.ErrUnconfirmed, processgroup.ErrScopeUnconfirmed, event)
			failure := roundTripHTTPFailure(t, newHTTPFailure(source))
			if failure.Exhaustion != nil || !failure.OutcomeUnknown || !strings.Contains(failure.Message, "details omitted") || event.Scope != tc.scope {
				t.Fatalf("unrepresentable exact evidence was rewritten or hidden: %+v original=%+v", failure, event)
			}
			got := failure.err()
			for _, cause := range []error{monitor.ErrTerminalJob, context.DeadlineExceeded, processgroup.ErrUnconfirmed, processgroup.ErrScopeUnconfirmed, processgroup.ErrExhausted, monitor.ErrExecutionOutcomeUnknown} {
				if !errors.Is(got, cause) {
					t.Fatalf("actual cause or unknown-outcome fallback lost %v: %v", cause, got)
				}
			}
			var restored *processgroup.Exhaustion
			if errors.As(got, &restored) {
				t.Fatalf("invented shortened exhaustion detail: %+v", restored)
			}
		})
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
			if failure.OutcomeUnknown || failure.Terminal || failure.Exhaustion != nil {
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
		Terminal: true, Canceled: true, Deadline: true, Unsafe: true, Descendants: true,
		Unconfirmed: true, ScopeUnconfirmed: true, Exhausted: true, OutcomeUnknown: true, RequestTransport: true,
		Exhaustion: &processgroup.Exhaustion{
			MaxTasks: int(^uint(0) >> 1), MaxMemoryBytes: math.MaxInt64,
			PIDEventsMax: math.MaxUint64, MemoryEventsMax: math.MaxUint64, MemorySwapEventsMax: math.MaxUint64,
			MemoryOOM: math.MaxUint64, MemoryOOMKill: math.MaxUint64, MemoryOOMGroupKill: math.MaxUint64,
			Stop: "start_failed", Scope: strings.Repeat("\x02", httpFailureScopeLimit),
		},
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

func assertHTTPExhaustionRoundTrip(t *testing.T, want processgroup.Exhaustion, joined ...error) {
	t.Helper()
	event := want
	source := errors.Join(append(joined, &event)...)
	failure := newHTTPFailure(source)
	event.MaxTasks++
	if failure.Exhaustion == nil || *failure.Exhaustion != want {
		t.Fatalf("factory did not snapshot returned event: %+v", failure)
	}
	failure = roundTripHTTPFailure(t, failure)
	got := failure.err()
	var restored *processgroup.Exhaustion
	if !errors.Is(got, processgroup.ErrExhausted) || !errors.As(got, &restored) || *restored != want {
		t.Fatalf("typed exhaustion changed: got=%+v want=%+v error=%v", restored, want, got)
	}
	for _, cause := range joined {
		if !errors.Is(got, cause) {
			t.Fatalf("joined exhaustion cause lost %v: %v", cause, got)
		}
	}
	failure.Exhaustion.MaxTasks++
	if *restored != want {
		t.Fatal("reconstructed error aliases mutable wire facts")
	}
}
