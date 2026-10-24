package action

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestUnixEndpointRejectsOccupiedSocketWithoutRemovingIt(t *testing.T) {
	for _, kind := range []string{"regular file", "live socket", "stale socket", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := unixTestDirectory(t)
			path := filepath.Join(dir, "execution.sock")
			var live *net.UnixListener
			switch kind {
			case "regular file":
				if err := os.WriteFile(path, []byte("caller data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "live socket", "stale socket":
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				if kind == "stale socket" {
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					live = listener
					t.Cleanup(func() { _ = listener.Close() })
				}
			case "symlink":
				if err := os.Symlink("caller-target", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			core := unixTestCore(t)
			endpoint, err := ListenUnixExecution(dir, core)
			if endpoint != nil {
				_ = endpoint.Close()
			}
			if err == nil || endpoint != nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("occupied socket was accepted or lost its reference: endpoint=%v error=%v", endpoint, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("constructor removed or replaced caller entry: %v", err)
			}
			if live != nil {
				if err := live.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				peer, err := net.DialTimeout("unix", path, 5*time.Second)
				if err != nil {
					t.Fatalf("constructor disabled the occupied listener: %v", err)
				}
				defer peer.Close()
				accepted, err := live.Accept()
				if err != nil {
					t.Fatalf("occupied listener stopped accepting: %v", err)
				}
				_ = accepted.Close()
			}
			if kind == "regular file" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "caller data" {
					t.Fatalf("constructor changed caller data: %q %v", data, err)
				}
			}
			if kind != "stale socket" && live == nil {
				client, err := NewUnixExecutionClient(path)
				if client != nil {
					client.CloseIdleConnections()
				}
				if err == nil || client != nil {
					t.Fatalf("Unix client accepted a non-socket entry: client=%v error=%v", client, err)
				}
			}
			core.mu.Lock()
			closed := core.closed
			core.mu.Unlock()
			if closed {
				t.Fatal("failed constructor took ownership of the caller's executor")
			}
		})
	}
}

func TestUnixEndpointRejectsUnsafeDirectoryAndLongPath(t *testing.T) {
	for _, kind := range []string{"public directory", "regular file", "symlink directory", "symlink ancestor", "relative", "missing", "overlong socket"} {
		t.Run(kind, func(t *testing.T) {
			base := unixTestDirectory(t)
			dir := filepath.Join(base, "runtime")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public directory":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "regular file":
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, []byte("caller"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink directory", "symlink ancestor":
				link := filepath.Join(base, "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
				if kind == "symlink ancestor" {
					if err := os.Mkdir(filepath.Join(link, "child"), 0700); err != nil {
						t.Fatal(err)
					}
					dir = filepath.Join(link, "child")
				}
			case "relative":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				dir, err = filepath.Rel(cwd, dir)
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				dir = filepath.Join(base, "missing")
			case "overlong socket":
				dir = filepath.Join(base, strings.Repeat("x", 110))
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.Lstat(dir)
			endpoint, err := ListenUnixExecution(dir, unixTestCore(t))
			if endpoint != nil {
				_ = endpoint.Close()
			}
			if err == nil || endpoint != nil {
				t.Fatalf("unsafe pathname accepted: %q endpoint=%v error=%v", dir, endpoint, err)
			}
			if kind == "overlong socket" && (!strings.Contains(err.Error(), fmt.Sprintf("%q", dir)) || !strings.Contains(err.Error(), fmt.Sprintf("%q", filepath.Join(dir, "execution.sock")))) {
				t.Fatalf("failed bind lost its concrete runtime/socket references: %v", err)
			}
			if beforeErr == nil {
				after, err := os.Lstat(dir)
				if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("failed admission changed caller placement: %v", err)
				}
			} else if kind == "missing" {
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed admission created missing caller directory: %v", err)
				}
			}
			if kind != "regular file" {
				if _, err := os.Lstat(filepath.Join(dir, "execution.sock")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed admission left a socket: %v", err)
				}
			}
		})
	}
	dir := unixTestDirectory(t)
	if endpoint, err := ListenUnixExecution(dir, nil); endpoint != nil || err == nil {
		t.Fatalf("nil executor accepted: endpoint=%v error=%v", endpoint, err)
	}
}

func TestUnixEndpointClosePreservesReplacement(t *testing.T) {
	for _, kind := range []string{"socket", "runtime directory", "runtime permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := unixTestDirectory(t)
			endpoint, err := ListenUnixExecution(dir, unixTestCore(t))
			if err != nil {
				t.Fatal(err)
			}
			unixTestCleanupEndpoint(t, endpoint)
			path := endpoint.SocketPath()
			original, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "socket" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				replacement.SetUnlinkOnClose(false)
				t.Cleanup(func() { _ = replacement.Close() })
			} else if kind == "runtime permissions" {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			} else {
				moved := dir + "-moved"
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(moved) })
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				// Preserve the socket inode while replacing its directory, isolating the directory identity check.
				if err := os.Rename(filepath.Join(moved, "execution.sock"), path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "socket" && (before.Mode()&os.ModeSocket == 0 || os.SameFile(original, before)) {
				t.Fatal("replacement fixture did not bind a distinct socket")
			}
			closeErr := endpoint.Close()
			if closeErr == nil || !strings.Contains(closeErr.Error(), path) {
				t.Fatalf("replacement did not retain a concrete cleanup error: %v", closeErr)
			}
			if kind == "socket" && !strings.Contains(closeErr.Error(), "socket identity changed") {
				t.Fatalf("replacement did not reach the socket identity check: %v", closeErr)
			}
			if kind == "runtime permissions" && !strings.Contains(closeErr.Error(), "0700 directory") {
				t.Fatalf("unsafe runtime did not reach directory revalidation: %v", closeErr)
			}
			if again := endpoint.Close(); again != closeErr {
				t.Fatalf("Close did not cache replacement error: first=%v next=%v", closeErr, again)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("Close deleted or changed replacement: %v", err)
			}
			if kind == "runtime directory" {
				if !os.SameFile(original, after) {
					t.Fatal("runtime replacement fixture lost the original socket identity")
				}
				if info, err := os.Stat(dir); err != nil || !info.IsDir() {
					t.Fatalf("Close removed replacement runtime directory: %v", err)
				}
			}
		})
	}
}

func TestUnixEndpointConcurrentCloseRetainsBorrowedDirectory(t *testing.T) {
	dir := unixTestDirectory(t)
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenUnixExecution(dir, unixTestCore(t))
	if err != nil {
		t.Fatal(err)
	}
	unixTestCleanupEndpoint(t, endpoint)
	const callers = 8
	start, done := make(chan struct{}), make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { <-start; done <- endpoint.Close() }()
	}
	close(start)
	for i := 0; i < callers; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close did not join serving")
		}
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned socket was not removed: %v", err)
	}
	after, err := os.Stat(dir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("endpoint removed the borrowed directory: %v", err)
	}
}

func TestUnixEndpointServesActualHTTPTransaction(t *testing.T) {
	job := testJob(command.Comment)
	core := unixTestCore(t)
	endpoint, err := ListenUnixExecution(unixTestDirectory(t), core)
	if err != nil {
		t.Fatal(err)
	}
	unixTestCleanupEndpoint(t, endpoint)
	client, err := NewUnixExecutionClient(endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || client.Timeout != 0 || transport.ResponseHeaderTimeout != 0 || endpoint.server.WriteTimeout != 0 {
		t.Fatal("Unix transport added proxy fallback or a whole-request/held-response deadline")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := core.Execute(ctx, job, runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	unixTestPull(t, client, job)
	call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, Complete: true}
	fact := CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
	unixTestAck(t, client, "/before", httpCall{Sequence: 1, Checkpoint: fact}, 1)
	fact.Call = call
	unixTestAck(t, client, "/after", httpCall{Sequence: 2, Checkpoint: fact}, 2)
	want := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Unix result", Calls: []CallUsage{call}}
	unixTestAck(t, client, "/outcome", httpOutcome{Sequence: 3, Result: want}, 3)
	select {
	case got := <-done:
		if got.err != nil || !reflect.DeepEqual(got.result, want) || got.result.Validate(job) != nil {
			t.Fatalf("Unix HTTP changed the bound result: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unix HTTP transaction did not finish")
	}
}

func TestUnixEndpointBoundsConnectionsWhileServingWatchAndControl(t *testing.T) {
	job := testJob(command.Comment)
	core := unixTestCore(t)
	endpoint, err := ListenUnixExecution(unixTestDirectory(t), core)
	if err != nil {
		t.Fatal(err)
	}
	unixTestCleanupEndpoint(t, endpoint)
	if cap(endpoint.listener.slots) != 16 {
		t.Fatalf("Unix connection limit = %d, want 16", cap(endpoint.listener.slots))
	}
	client, err := NewUnixExecutionClient(endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := core.Execute(ctx, job, runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	unixTestPull(t, client, job)
	tx := unixTestReady(t, core)
	body, err := json.Marshal(httpJobBinding(job))
	if err != nil {
		t.Fatal(err)
	}
	watchRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, UnixExecutionBaseURL+"/cancel", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	watchRequest.Header.Set("Content-Type", "application/json")
	watch, err := client.Do(watchRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Body.Close()
	if watch.StatusCode != http.StatusOK || watch.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("watch was not admitted: status=%d headers=%v", watch.StatusCode, watch.Header)
	}
	var cancellation httpCancellation
	watchDone := make(chan error, 1)
	go func() { watchDone <- httpDecode(watch.Body, httpControlBytes, &cancellation) }()
	call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, Complete: true}
	fact := CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
	unixTestAck(t, client, "/before", httpCall{Sequence: 1, Checkpoint: fact}, 1)
	fact.Call = call
	unixTestAck(t, client, "/after", httpCall{Sequence: 2, Checkpoint: fact}, 2)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "upload"}
	unixTestAck(t, client, "/status", httpObservation{Status: status}, 1)
	client.CloseIdleConnections()
	waitSlots := func(want int) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for len(endpoint.listener.slots) != want {
			select {
			case <-tick.C:
			case <-timer.C:
				t.Fatalf("admitted Unix connections = %d, want %d", len(endpoint.listener.slots), want)
			}
		}
	}
	waitSlots(1) // Only the admitted watch remains after idle control connections close.
	var stalled []net.Conn
	t.Cleanup(func() {
		// Close our incomplete headers before endpoint shutdown can wait for them.
		for _, peer := range stalled {
			_ = peer.Close()
		}
	})
	for i := 1; i < cap(endpoint.listener.slots); i++ {
		peer, err := net.DialTimeout("unix", endpoint.SocketPath(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		stalled = append(stalled, peer)
		if err := peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(peer, "POST /next HTTP/1.1\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	waitSlots(16) // Slots count only connections actually accepted by the listener.
	overflow, err := net.DialTimeout("unix", endpoint.SocketPath(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Close()
	if err := overflow.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if n, err := overflow.Read(data[:]); n != 0 || err == nil {
		t.Fatalf("overflow connection reached HTTP serving: bytes=%q error=%v", data[:n], err)
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("overflow connection stayed open at the cap: %v", err)
	}
	if len(endpoint.listener.slots) != 16 {
		t.Fatalf("overflow changed the admitted connection count: %d", len(endpoint.listener.slots))
	}
	if err := stalled[0].Close(); err != nil {
		t.Fatal(err)
	}
	waitSlots(15)
	status.Sequence = 2
	unixTestAck(t, client, "/status", httpObservation{Status: status}, 2)
	select {
	case err := <-watchDone:
		t.Fatalf("admitted watch ended before action cancellation: reply=%+v error=%v", cancellation, err)
	default:
	}
	client.CloseIdleConnections()
	waitSlots(15)
	for _, peer := range stalled {
		_ = peer.Close()
	}
	waitSlots(1)
	closed := make(chan error, 1)
	go func() { closed <- endpoint.Close() }()
	select {
	case <-tx.budget.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the watched action")
	}
	select {
	case err := <-watchDone:
		if err != nil || cancellation.httpBinding != httpJobBinding(job) || cancellation.Reason != "canceled" {
			t.Fatalf("connection pressure lost the admitted watch: reply=%+v error=%v", cancellation, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted watch did not report action cancellation")
	}
	_ = watch.Body.Close()
	client.CloseIdleConnections()
	want := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{call}}
	unixTestAck(t, client, "/outcome", httpOutcome{Sequence: 3, Result: want, Failure: newHTTPFailure(context.Canceled)}, 3)
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, want) {
			t.Fatalf("connection pressure changed the known outcome: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watched outcome did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join accepted connections")
	}
	waitSlots(0)
}

func TestUnixEndpointRejectsIncompleteBodiesWithoutBlockingClose(t *testing.T) {
	for _, test := range []struct {
		name, request, kind string
		status              int
		active              bool
	}{
		{"unknown route", "POST /unknown HTTP/1.1\r\nHost: minion\r\nContent-Length: 1\r\n\r\n", "error", http.StatusNotFound, false},
		{"wrong content type", "POST /next HTTP/1.1\r\nHost: minion\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n", "next", http.StatusBadRequest, false},
		{"bounded read cleared its deadline", fmt.Sprintf("POST /next HTTP/1.1\r\nHost: minion\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", httpControlBytes+2, strings.Repeat(" ", httpControlBytes+1)), "next", http.StatusRequestEntityTooLarge, false},
		{"active status wrong content type", "POST /status HTTP/1.1\r\nHost: minion\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n", "status", http.StatusBadRequest, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := unixTestDirectory(t)
			directory, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			core := unixTestCore(t)
			endpoint, err := ListenUnixExecution(dir, core)
			if err != nil {
				t.Fatal(err)
			}
			unixTestCleanupEndpoint(t, endpoint)
			job := testJob(command.Comment)
			var client *http.Client
			var tx *httpTransaction
			var done chan httpServerTestResult
			if test.active {
				client, err = NewUnixExecutionClient(endpoint.SocketPath())
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseIdleConnections()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done = make(chan httpServerTestResult, 1)
				go func() {
					result, err := core.Execute(ctx, job, runnerExecutionCheckpoints{})
					done <- httpServerTestResult{result, err}
				}()
				unixTestPull(t, client, job)
				tx = unixTestReady(t, core)
			}
			peer, err := net.DialTimeout("unix", endpoint.SocketPath(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			// Close our peer before endpoint cleanup even when a BEFORE rejection is stuck draining it.
			t.Cleanup(func() { _ = peer.Close() })
			if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				_ = peer.Close()
				t.Fatal(err)
			}
			if _, err := peer.Write([]byte(test.request)); err != nil {
				_ = peer.Close()
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(peer), &http.Request{Method: http.MethodPost})
			if err != nil {
				_ = peer.Close()
				t.Fatalf("incomplete request withheld its finite rejection: %v", err)
			}
			var ack httpAcknowledgement
			decodeErr := httpDecode(response.Body, httpControlBytes, &ack)
			_ = response.Body.Close()
			if decodeErr != nil || response.StatusCode != test.status || !response.Close || ack.Kind != test.kind || ack.Sequence != 0 || ack.Failure == nil || ack.Failure.validate() != nil {
				_ = peer.Close()
				t.Fatalf("incomplete request rejection: status=%d close=%t ack=%+v error=%v", response.StatusCode, response.Close, ack, decodeErr)
			}
			// The peer still has not completed its advertised body or sent EOF.
			closed := make(chan error, 1)
			go func() { closed <- endpoint.Close() }()
			if test.active {
				select {
				case <-tx.budget.Done():
				case <-time.After(5 * time.Second):
					_ = peer.Close()
					t.Fatal("Close did not stop the delivered status fixture")
				}
				result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}
				unixTestAck(t, client, "/outcome", httpOutcome{Sequence: 1, Result: result, Failure: newHTTPFailure(context.Canceled)}, 1)
				select {
				case got := <-done:
					if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || len(got.result.Calls) != 0 {
						_ = peer.Close()
						t.Fatalf("status rejection changed the known no-work outcome: %+v error=%v", got.result, got.err)
					}
				case <-time.After(5 * time.Second):
					_ = peer.Close()
					t.Fatal("status fixture outcome did not finish")
				}
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				_ = peer.Close()
				t.Fatal("Close waited for an incomplete rejected request body")
			}
			if _, err := os.Lstat(endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected request retained the owned socket: %v", err)
			}
			current, err := os.Stat(dir)
			if err != nil || !os.SameFile(directory, current) {
				t.Fatalf("rejected request removed the borrowed runtime directory: %v", err)
			}
		})
	}
}

func TestUnixEndpointUnexpectedListenerFailureWakesExecute(t *testing.T) {
	core := unixTestCore(t)
	endpoint, err := ListenUnixExecution(unixTestDirectory(t), core)
	if err != nil {
		t.Fatal(err)
	}
	unixTestCleanupEndpoint(t, endpoint)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := core.Execute(ctx, testJob(command.Comment), runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	_ = unixTestReady(t, core)
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || len(got.result.Calls) != 0 {
			t.Fatalf("unexpected Serve exit left a pending assignment: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unexpected listener failure did not wake pending Execute")
	}
	closeErr := endpoint.Close()
	if !errors.Is(closeErr, net.ErrClosed) || endpoint.Close() != closeErr {
		t.Fatalf("unexpected Serve error was lost or not cached: %v", closeErr)
	}
	if _, err := os.Lstat(endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Serve left the still-owned socket: %v", err)
	}
}

func TestUnixEndpointCloseKeepsPaidSaveAndReceiptGraceAvailable(t *testing.T) {
	f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
	store := f.startUsage()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	core := unixTestCore(t)
	endpoint, err := ListenUnixExecution(unixTestDirectory(t), core)
	if err != nil {
		t.Fatal(err)
	}
	unixTestCleanupEndpoint(t, endpoint)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	client, err := NewUnixExecutionClient(endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan httpServerTestResult, 1)
	sink := runnerExecutionCheckpoints{before: f.sink.BeforeCall, review: f.sink.Review, after: func(fact CallCheckpoint) error {
		if err := f.sink.AfterCall(fact); err != nil {
			return err
		}
		close(entered)
		<-release
		return nil
	}}
	go func() { result, err := core.Execute(ctx, f.job, sink); done <- httpServerTestResult{result, err} }()
	unixTestPull(t, client, f.job)
	tx := unixTestReady(t, core)
	unixTestAck(t, client, "/review", httpReview{Sequence: 1, Checkpoint: f.review(nil, nil, "")}, 1)
	call := CallUsage{Provider: f.job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 31, Output: 2}, Complete: true}
	unixTestAck(t, client, "/before", httpCall{Sequence: 2, Checkpoint: f.call(callMetadata(call))}, 2)
	after := make(chan httpServerTestExchange, 1)
	go func() {
		var ack httpAcknowledgement
		status, err := unixTestJSONContext(ctx, client, "/after", httpCall{Sequence: 3, Checkpoint: f.call(call)}, &ack, httpControlBytes)
		after <- httpServerTestExchange{status: status, ack: ack, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("paid durable save was not entered")
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].CodexCalls != 1 || !records[0].UsageComplete || records[0].Tokens != call.Tokens {
		t.Fatalf("entered paid callback did not retain exact accounting: records=%+v error=%v", records, err)
	}
	closed := make(chan error, 1)
	go func() { closed <- endpoint.Close() }()
	select {
	case <-tx.budget.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint Close did not stop the core action")
	}
	select {
	case err := <-closed:
		t.Fatalf("endpoint Close abandoned the entered paid callback: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	probe, err := NewUnixExecutionClient(endpoint.SocketPath())
	if err != nil {
		t.Fatalf("Close removed the endpoint before callback drain: %v", err)
	}
	defer probe.CloseIdleConnections()
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "main", Round: 1}
	unixTestAck(t, probe, "/status", httpObservation{Status: status}, 1)
	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-after:
		if got.err != nil || got.status != http.StatusOK || got.ack.Sequence != 3 || got.ack.Failure != nil {
			t.Fatalf("Close lost paid receipt acknowledgement: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paid receipt did not drain after gate release")
	}
	calls := []CallUsage{call}
	unixTestAck(t, client, "/review", httpReview{Sequence: 4, Checkpoint: f.review(calls, nil, "")}, 4)
	want := f.result(calls, nil, "")
	unixTestAck(t, client, "/outcome", httpOutcome{Sequence: 5, Result: want, Failure: newHTTPFailure(context.Canceled)}, 5)
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, want) || !errors.Is(f.sink.finish(got.result, got.err), context.Canceled) {
			t.Fatalf("Close lost known paid outcome: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paid outcome did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint did not join paid handler drain")
	}
	afterClose, err := usage.ReadAll(store.Dir)
	if err != nil || !reflect.DeepEqual(afterClose, records) {
		t.Fatalf("Close changed durable paid accounting: before=%+v after=%+v error=%v", records, afterClose, err)
	}
	saved := readPrivateReview(t, f.owner.WorkRoot)
	if !slices.Equal(saved.Calls, calls) || saved.CriticStopReason != "" || len(saved.Refutations) != 0 {
		t.Fatalf("Close changed the durable private receipt prefix: %+v", saved)
	}
}

func unixTestDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pathname Unix sockets require Unix")
	}
	parent, err := workspace.CanonicalPath(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(parent, "mn-u-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func unixTestCore(t *testing.T) *HTTPExecutor {
	t.Helper()
	core, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	return core
}

func unixTestCleanupEndpoint(t *testing.T, endpoint *UnixEndpoint) {
	t.Helper()
	t.Cleanup(func() {
		if tx := endpoint.executor.transaction(); tx != nil {
			// Match existing HTTP fixture failure cleanup; normal proofs use the real control grace.
			tx.mutation.Lock()
			tx.mu.Lock()
			tx.fenced = true
			select {
			case <-tx.done:
			default:
				close(tx.done)
			}
			tx.mu.Unlock()
			tx.mutation.Unlock()
		}
		_ = endpoint.Close()
	})
}

func unixTestJSON(t *testing.T, client *http.Client, path string, value, result any, limit int) (int, error) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return unixTestJSONContext(ctx, client, path, value, result, limit)
}

func unixTestJSONContext(ctx context.Context, client *http.Client, path string, value, result any, limit int) (int, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, UnixExecutionBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, httpDecode(response.Body, limit, result)
}

func unixTestAck(t *testing.T, client *http.Client, path string, value any, sequence uint64) {
	t.Helper()
	var ack httpAcknowledgement
	status, err := unixTestJSON(t, client, path, value, &ack, httpControlBytes)
	if err != nil || status != http.StatusOK || ack.Kind != strings.TrimPrefix(path, "/") || ack.Sequence != sequence || ack.Failure != nil {
		t.Fatalf("Unix %s acknowledgement: status=%d ack=%+v error=%v", path, status, ack, err)
	}
}

func unixTestPull(t *testing.T, client *http.Client, job Job) {
	t.Helper()
	var assignment httpAssignment
	status, err := unixTestJSON(t, client, "/next", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, &assignment, httpJobBytes)
	if err != nil || status != http.StatusOK || !reflect.DeepEqual(assignment.Job, job) || assignment.Expires <= time.Now().UnixNano() {
		t.Fatalf("Unix assignment: status=%d job=%+v error=%v", status, assignment, err)
	}
}

func unixTestReady(t *testing.T, core *HTTPExecutor) *httpTransaction {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		core.mu.Lock()
		tx, changed := core.active, core.changed
		if tx != nil {
			tx.mu.Lock()
			ready := tx.ready
			tx.mu.Unlock()
			if ready {
				core.mu.Unlock()
				return tx
			}
		}
		core.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("Unix assignment did not become ready")
		}
	}
}
