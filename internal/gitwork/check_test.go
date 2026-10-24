package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestSupportedGitVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		want   bool
	}{
		{"git version 2.45.1\n", true},
		{"git version 2.45.2", true},
		{"git version 2.46.0", true},
		{"git version 3.0.0", true},
		{"git version 2.45.1.windows.1", true},
		{"git version 2.45.1 (Vendor Git-1)", true},
		{"git version 2.45.1 (Vendor Git-1)\n", true},
		{"git version 2.45.1 (Apple Git-154)", true},
		{"git version 2.45.1 (Vendor) extra)", false},
		{"git version 2.45.1 (Vendor (nested))", false},
		{"git version 2.45.1 (Vendor\x00 Git-1)", false},
		{"git version 2.45.1 (Vendor\tGit-1)", false},
		{"git version 2.45.1 (Vendor\x1bGit-1)", false},
		{"git version 2.45.1 (Vendor\x7fGit-1)", false},
		{"git version 2.46.0.rc0", false},
		{"git version 2.45.1.rc0", false},
		{"git version 2.45.1-rc1", false},
		{"git version 2.45.1-RC1", false},
		{"git version 2.45.1-beta1", false},
		{"git version 2.45.1-dev", false},
		{"git version 2.45.1.windows.", false},
		{"git version 2.45.1.windows.1-dev", false},
		{"git version 2.45.0", false},
		{"git version 2.44.99", false},
		{"git version 1.99.99", false},
		{"", false},
		{"2.45.1", false},
		{"git version 2.45", false},
		{"git version 2.x.1", false},
		{"git version 2.-45.1", false},
		{"git version 2.45.+1", false},
		{"git version 2.45.1garbage", false},
		{"git version 2.45.1\nforged version", false},
		{"git version 18446744073709551616.45.1", false},
		{"git version 2.18446744073709551616.1", false},
		{"git version 2.45.18446744073709551616", false},
	} {
		if got := supportedGitVersion(test.output); got != test.want {
			t.Errorf("supportedGitVersion(%q)=%t; want %t", test.output, got, test.want)
		}
	}
}

func TestCheckGitUsesSyntheticPrivateOfflinePaths(t *testing.T) {
	requireGitProcessGroup(t)
	realGit, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "work-root")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, alias)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GIT_DIR", "GIT_WORK_TREE", "GIT_SSH_COMMAND", "GIT_CONFIG_SYSTEM", "GIT_AUTHOR_NAME"} {
		t.Setenv(key, "must-not-reach-offline-git")
	}
	receipt := filepath.Join(t.TempDir(), "commands")
	script := `#!/bin/sh
set -eu
metadata= source= operation=
for argument do
  case "$argument" in
    --git-dir=*) metadata=${argument#--git-dir=} ;;
    --work-tree=*) source=${argument#--work-tree=} ;;
    --version|init|add|write-tree|hash-object|update-ref|bundle|pack-refs|cat-file|fsck|merge-base|read-tree|ls-files|update-index|check-ignore|diff) operation=$argument ;;
  esac
done
case "$metadata" in ` + shellQuote(canonical+"/.minion-git-check-") + `*) ;; *) exit 81 ;; esac
[ "$HOME" = "$metadata" ] && [ "$TMPDIR" = "$metadata" ] || exit 82
if [ "$metadata" = "$source/.git" ]; then
  [ "$PWD" = "$source" ] || exit 83
else
  [ "$PWD" = "$metadata" ] || exit 84
fi
[ -z "${GH_TOKEN-}${GITHUB_TOKEN-}${GIT_DIR-}${GIT_WORK_TREE-}${GIT_SSH_COMMAND-}${GIT_CONFIG_SYSTEM-}${GIT_AUTHOR_NAME-}" ] || exit 85
[ "$GIT_CONFIG_NOSYSTEM" = 1 ] && [ "$GIT_CONFIG_GLOBAL" = /dev/null ] && [ "$GIT_TERMINAL_PROMPT" = 0 ] || exit 86
case " $* " in *' credential.helper= '*) ;; *) exit 87 ;; esac
case " $* " in *' protocol.allow=never '*) ;; *) exit 88 ;; esac
printf '%s\n' "$operation" >> ` + shellQuote(receipt) + `
exec ` + shellQuote(realGit) + ` "$@"
`
	testsupport.FakeOnPath(t, "git", script)
	if err := Check(t.Context(), relative); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"--version", "write-tree", "hash-object", "bundle", "fsck", "merge-base", "read-tree", "diff"} {
		if !slices.Contains(strings.Fields(string(data)), operation) {
			t.Errorf("readiness did not exercise %s: %s", operation, data)
		}
	}
	assertGitCheckClean(t, root)
}

func TestCheckGitFailuresRetainCauseAndCleanOwnedFiles(t *testing.T) {
	for _, test := range []struct {
		name, match, action, want string
		exit                      int
		limit                     bool
	}{
		{name: "old version", match: " --version ", action: "printf 'git version 2.45.0\\n'; exit 0", want: "requires Git 2.45.1 or later"},
		{name: "malformed version", match: " --version ", action: "printf 'unknown version\\n'; exit 0", want: `reported "unknown version"`},
		{name: "version output bound", match: " --version ", action: "exec head -c 1025 /dev/zero", want: "offline git --version output exceeds action limit", limit: true},
		{name: "index capability", match: " init ", action: "printf 'capability failed\\nforged journal\\n' >&2; exit 7", want: `"capability failed\nforged journal"`, exit: 7},
		{name: "commit identity", match: " hash-object ", action: "printf 'not-a-commit\\n'; exit 0", want: "invalid commit identity"},
		{name: "bundle output bound", match: " bundle create ", action: fmt.Sprintf("exec head -c %d /dev/zero", maxGitCheckBundleBytes+1), want: "offline git bundle output exceeds action limit", limit: true},
		{name: "import capability", match: " --ref-format=files ", action: "printf 'import unavailable' >&2; exit 8", want: "git readiness import:", exit: 8},
		{name: "merge-base capability", match: " merge-base ", action: "printf 'merge-base unavailable' >&2; exit 10", want: "git readiness import: offline git merge-base:", exit: 10},
		{name: "missing index change", match: " --raw ", action: "exit 0", want: "index did not preserve the synthetic change"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireGitProcessGroup(t)
			realGit, err := aicli.AbsoluteExecutable("git")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			script := "#!/bin/sh\ncase \" $* \" in\n*" + shellQuote(test.match) + "*) " + test.action + " ;;\n" +
				"*' --version '*) printf 'git version 2.45.1\\n'; exit 0 ;;\nesac\nexec " + shellQuote(realGit) + " \"$@\"\n"
			testsupport.FakeOnPath(t, "git", script)
			err = Check(t.Context(), root)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "\n") || errors.Is(err, ErrCheckoutTooLarge) != test.limit {
				t.Fatalf("check lost failure classification: %v", err)
			}
			if test.exit != 0 {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != test.exit {
					t.Fatalf("check lost exit cause: %v", err)
				}
			}
			assertGitCheckClean(t, root)
		})
	}
	t.Run("missing git", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		if err := Check(t.Context(), root); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("missing git cause: %v", err)
		}
		assertGitCheckClean(t, root)
	})
	t.Run("bounded diagnostics", func(t *testing.T) {
		requireGitProcessGroup(t)
		root := t.TempDir()
		testsupport.FakeOnPath(t, "git", fmt.Sprintf("#!/bin/sh\nhead -c %d /dev/zero | tr '\\000' x >&2\nexit 9\n", bounded.DiagnosticBytes+1024))
		err := Check(t.Context(), root)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 9 || len(err.Error()) > bounded.DiagnosticBytes+200 {
			t.Fatalf("unbounded or lost diagnostic: %v", err)
		}
		assertGitCheckClean(t, root)
	})
}

func TestCheckGitHonorsCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		root := t.TempDir()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := Check(ctx, root); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled check: %v", err)
		}
		assertGitCheckClean(t, root)
	})
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			requireGitProcessGroup(t)
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "started")
			testsupport.FakeOnPath(t, "git", "#!/bin/sh\nprintf x > "+shellQuote(marker)+"\nexec sleep 30\n")
			budget := 5 * time.Second
			if deadline {
				budget = time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			done := make(chan struct{})
			if deadline {
				close(done)
			} else {
				go func() {
					defer close(done)
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						if _, err := os.Stat(marker); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
			}
			err := Check(ctx, root)
			cancel()
			<-done
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || errors.Is(err, processgroup.ErrUnconfirmed) || errors.Is(err, processgroup.ErrScopeUnconfirmed) {
				t.Fatalf("cancellation lost cause or release: %v", err)
			}
			if !deadline {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("check never dispatched before cancellation: %v", err)
				}
			}
			assertGitCheckClean(t, root)
		})
	}
}

func TestFinishGitCheckPreservesUncertainScopeAndCleanupErrors(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("scope uncertain=%t", uncertain), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "owned")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			event := &processgroup.Exhaustion{MaxTasks: 1, Released: !uncertain}
			runErr := errors.Join(processgroup.ErrUnconfirmed, event)
			if uncertain {
				runErr = errors.Join(runErr, processgroup.ErrScopeUnconfirmed)
			}
			err := finishGitCheck(root, runErr)
			var retained *processgroup.Exhaustion
			if !errors.Is(err, runErr) || !errors.As(err, &retained) || retained != event || errors.Is(err, processgroup.ErrScopeUnconfirmed) != uncertain {
				t.Fatalf("finalizer lost typed cause: %v", err)
			}
			_, statErr := os.Stat(root)
			if uncertain && (statErr != nil || !strings.Contains(err.Error(), root)) || !uncertain && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("wrong owned-root retention: exists error=%v result=%v", statErr, err)
			}
		})
	}
	t.Run("cleanup error joins command error", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "invalid\x00name")
		runErr := errors.New("command failed")
		if err := finishGitCheck(root, runErr); !errors.Is(err, runErr) || !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("cleanup error hid a cause: %v", err)
		}
	})
}

func assertGitCheckClean(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("readiness left owned files: %v, %v", entries, err)
	}
}
