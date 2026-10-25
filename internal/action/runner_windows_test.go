package action

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
)

func TestPreparationFailureNativeWindowsErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       syscall.Errno
		want       error
		unsafeRoot bool
	}{
		{"access denied", syscall.ERROR_ACCESS_DENIED, nil, true},
		{"handle disk full", 39, syscall.ENOSPC, false},
		{"disk full", 112, syscall.ENOSPC, false},
		{"disk quota exceeded", 1295, syscall.EDQUOT, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &os.PathError{Op: "write", Path: "source", Err: test.code}
			err := preparationFailure("materialize source", cause)
			if !errors.Is(err, test.code) || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, workroot.ErrUnsafe) != test.unsafeRoot {
				t.Fatalf("native preparation classification = %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("native capacity classification = %v, want %v", err, test.want)
			}
			fact := newHTTPFailure(err)
			if fact.Unsafe != test.unsafeRoot || fact.NoSpace != (test.want == syscall.ENOSPC) || fact.QuotaExceeded != (test.want == syscall.EDQUOT) {
				t.Fatalf("native HTTP failure facts = %+v", fact)
			}
		})
	}
}

func TestWindowsFixPromptDisclosesExecutableModeLimit(t *testing.T) {
	prompt, err := actionPrompt(t.Context(), testJob(command.Fix), `C:\source`, `C:\context`, "", "")
	if err != nil || !strings.Contains(prompt, "Do not attempt or claim a new Git-executable file, a mode-only change, or a non-case-only rename") {
		t.Fatalf("Windows fix mode limit missing: prompt=%q error=%v", prompt, err)
	}
}
