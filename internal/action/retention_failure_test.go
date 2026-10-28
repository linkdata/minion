package action

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRetentionFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		retry bool
	}{
		{"disk full", &os.PathError{Op: "write", Path: "record", Err: syscall.ENOSPC}, true},
		{"quota exceeded", syscall.EDQUOT, true},
		{"permission denied", os.ErrPermission, true},
		{"read only", syscall.EROFS, true},
		{"device I/O", syscall.EIO, true},
		{"process descriptors exhausted", &os.PathError{Op: "open", Path: "record", Err: syscall.EMFILE}, true},
		{"system descriptors exhausted", syscall.ENFILE, true},
		{"missing retained file", os.ErrNotExist, false},
		{"invalid record", errors.New("record changed"), false},
		{"disk full and mismatch", errors.Join(fmt.Errorf("rename: %w", syscall.ENOSPC), errors.New("record changed")), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := retainedRecordFailure("retain record", test.cause)
			if errors.Is(err, monitor.ErrPostModelPublication) != test.retry ||
				errors.Is(err, monitor.ErrTerminalJob) == test.retry || !errors.Is(err, test.cause) {
				t.Fatalf("retention classification: %v", err)
			}
		})
	}
	if runtime.GOOS == "windows" {
		for _, code := range []syscall.Errno{4, 8, 14, 19, 21, 23, 29, 30, 31, 1117, 1450, 1455} {
			if err := retainedRecordFailure("retain record", &os.PathError{Op: "write", Path: "record", Err: code}); !errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("Win32 resource or device I/O classification for %d: %v", code, err)
			}
		}
		for _, code := range []syscall.Errno{39, 112, 1295} {
			if err := retainedRecordFailure("retain record", &os.PathError{Op: "write", Path: "record", Err: code}); !errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("Win32 disk-full classification for %d: %v", code, err)
			}
		}
		for _, code := range []syscall.Errno{32, 33} {
			if err := retainedRecordFailure("retain record", &os.LinkError{Op: "rename", Old: "temporary", New: "record", Err: code}); !errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("Win32 sharing/lock classification for %d: %v", code, err)
			}
		}
	}
}

func TestRetentionFailureNeedsSavedResultForPublicationRetry(t *testing.T) {
	job := testJob(command.Comment)
	cause := retainedRecordFailure("save private record", syscall.EMFILE)
	e := &Orchestrator{}
	if err := e.retentionForSavedResult(job, cause); !errors.Is(err, monitor.ErrTerminalJob) ||
		!errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("in-process retention failure was not terminal: %v", err)
	}
	store, _, _ := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	e.Store = store
	if err := e.retentionForSavedResult(job, cause); errors.Is(err, monitor.ErrTerminalJob) ||
		!errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("saved-result retention failure was not retryable: %v", err)
	}
	other := job
	other.Identity.AttemptID = "another-attempt"
	if err := e.retentionForSavedResult(other, cause); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("foreign saved result enabled retry: %v", err)
	}
	if err := store.ClearAcceptedResult(); err != nil {
		t.Fatal(err)
	}
	if err := e.retentionForSavedResult(job, cause); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("missing saved result enabled retry: %v", err)
	}
}

func TestImportDiskFullWithGitExitIsClassifiedRetryable(t *testing.T) {
	for _, errno := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		gitExit := &exec.ExitError{}
		failure := errors.Join(errno, gitExit)
		if retryableHostIO(failure) || !retryableImportIO(failure) {
			t.Fatalf("Git exit masked retryable disk errno %v", errno)
		}
		if retryableImportIO(errors.Join(failure, gitwork.ErrUnsupportedSource)) {
			t.Fatalf("Git source rejection was treated as host I/O for %v", errno)
		}
	}
	if retryableImportIO(&exec.ExitError{}) {
		t.Fatal("ordinary Git failure became retryable host I/O")
	}
}

func TestPrivateRetentionRetriesHostPermissionFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission fixture requires a non-root Unix account")
	}
	for _, test := range []struct {
		name string
		run  func(*Orchestrator) error
	}{
		{"worker-diagnostics", func(e *Orchestrator) error {
			job := testJob(command.Comment)
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
				Diagnostics: []Diagnostic{{Code: "private", Message: "worker message"}}}
			return e.retainWorkerDiagnostics(job, result, nil)
		}},
		{"reviews", func(e *Orchestrator) error {
			job := withClaudeCritic(testJob(command.Comment))
			return e.retainReviewEvidence(job, privateReviewResult(job))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, test.name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			e := &Orchestrator{WorkRoot: root}
			if err := test.run(e); !errors.Is(err, monitor.ErrPostModelPublication) ||
				errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("host permission failure was terminal: %v", err)
			}
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := test.run(e); err != nil {
				t.Fatalf("retention did not recover: %v", err)
			}
		})
	}
}

func TestSavedPrivateRetentionRetriesWithoutModel(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission fixture requires a non-root Unix account")
	}
	for _, name := range []string{"worker-diagnostics", "reviews"} {
		t.Run(name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.Identity.Repository = "o/r"
			result := privateReviewResult(job)
			result.Diagnostics = []Diagnostic{{Code: "private", Message: "worker message"}}
			store, _, request := savedRecoveryFixture(t, job, result)
			var posts atomic.Int32
			server := auditRecoveryServer(t, &posts)
			root := t.TempDir()
			dir := filepath.Join(root, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
				Runner: Runner{runModel: forbidModel(t)}}
			if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) ||
				errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, os.ErrPermission) || posts.Load() != 0 {
				t.Fatalf("retention I/O did not keep saved result retryable: posts=%d error=%v", posts.Load(), err)
			}
			if present, err := store.HasAcceptedResult(); err != nil || !present {
				t.Fatalf("saved result lost before retry: present=%t error=%v", present, err)
			}
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := e.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 {
				t.Fatalf("restored retention did not publish: posts=%d error=%v", posts.Load(), err)
			}
		})
	}
}
