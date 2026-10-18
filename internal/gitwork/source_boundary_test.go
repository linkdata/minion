package gitwork

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func TestImportRequiresExactlyReviewedPaths(t *testing.T) {
	for _, scenario := range []string{"no-op", "ignored", "undeclared", "unsorted", "mode-only"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t)
			changes := []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("changed\n")}}
			switch scenario {
			case "no-op":
				changes[0].Data = []byte("fixture\n")
			case "ignored":
				writeFile(t, filepath.Join(fixture.checkout.path, ".git", "info", "exclude"), "ignored.txt\n")
				changes = append(changes, workspace.Change{Path: "ignored.txt", Mode: workspace.Regular, Data: []byte("reviewed\n")})
			case "undeclared":
				writeFile(t, filepath.Join(fixture.checkout.path, "extra.txt"), "not reviewed\n")
			case "unsorted":
				changes = append([]workspace.Change{{Path: "z.txt", Mode: workspace.Regular, Data: []byte("reviewed\n")}}, changes...)
			case "mode-only":
				changes[0].Mode, changes[0].Data = workspace.Executable, []byte("fixture\n")
			}
			err := fixture.checkout.Import(context.Background(), fixture.base, changes)
			switch scenario {
			case "unsorted", "mode-only":
				if err != nil {
					t.Fatalf("valid unsorted import error=%v", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "staged paths do not match reviewed changes") {
					t.Fatalf("%s import error=%v", scenario, err)
				}
			}
		})
	}
}

func TestImportPreservesStagingIOFailure(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.checkout.path, ".git", "index.lock"), "held by another process")
	err := fixture.checkout.Import(context.Background(), fixture.base, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("changed\n")}})
	var exit *exec.ExitError
	if err == nil || errors.Is(err, ErrUnsupportedSource) || !errors.As(err, &exit) || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("locked-index import error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fixture.checkout.Import(ctx, fixture.base, nil); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("cancelled import error=%v", err)
	}
}

func TestImportAttestsStagedBytesAndModes(t *testing.T) {
	for _, mismatch := range []string{"mode", "content"} {
		t.Run(mismatch, func(t *testing.T) {
			fixture := newFixture(t)
			ctx := context.Background()
			const branch = "minion/fix-staged-binding"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			change := workspace.Change{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			if mismatch == "mode" {
				// Production pins fileMode=true. Simulate ineffective executable bits with real Git, without a special filesystem.
				fakeOnPath(t, "git", `#!/bin/sh
for argument do
  shift
  if [ "$argument" = core.fileMode=true ]; then argument=core.fileMode=false; fi
  set -- "$@" "$argument"
done
exec `+shellQuote(git)+` "$@"
`)
				change.Mode = workspace.Executable
			} else {
				fakeOnPath(t, "git", `#!/bin/sh
for argument do
  if [ "$argument" = add ]; then
    `+shellQuote(git)+` "$@" || exit "$?"
    object=$(printf unreviewed | `+shellQuote(git)+` hash-object -w --stdin) || exit "$?"
    exec `+shellQuote(git)+` update-index --cacheinfo 100644 "$object" README.md
  fi
done
exec `+shellQuote(git)+` "$@"
`)
			}
			err = fixture.checkout.Import(ctx, fixture.base, []workspace.Change{change})
			stage := fixture.git(fixture.checkout.path, "ls-files", "--stage", "--", change.Path)
			if mismatch == "mode" && !strings.HasPrefix(stage, "100644 ") || mismatch == "content" && !strings.Contains(stage, gitBlobID([]byte("unreviewed"))) {
				t.Fatalf("fixture did not produce staged %s mismatch: %s", mismatch, stage)
			}
			if err == nil || !strings.Contains(err.Error(), "staged content or mode differs") {
				t.Fatalf("unreviewed %s accepted: %v", mismatch, err)
			}
			if head := fixture.git(fixture.checkout.path, "rev-parse", "HEAD"); head != fixture.base {
				t.Fatalf("rejected import committed %s", head)
			}
			if refs := fixture.git("", "--git-dir="+fixture.remote, "for-each-ref", "refs/heads/"+branch); refs != "" {
				t.Fatalf("rejected import pushed %s", refs)
			}
		})
	}
}

func TestImportAttestsUnsortedBinaryEmptyModeAndDeletionChanges(t *testing.T) {
	fixture := newFixture(t)
	if err := fixture.checkout.Import(context.Background(), fixture.base, nil); err != nil {
		t.Fatalf("empty import refused: %v", err)
	}
	changes := []workspace.Change{
		{Path: "z-empty", Mode: workspace.Regular},
		{Path: "README.md", Delete: true},
		{Path: "binary", Mode: workspace.Executable, Data: []byte{0, 255, '\n'}},
	}
	if err := fixture.checkout.Import(context.Background(), fixture.base, changes); err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Delete {
			if output := fixture.git(fixture.checkout.path, "ls-files", "--", change.Path); output != "" {
				t.Fatalf("deleted file still staged: %s", output)
			}
			continue
		}
		want := strconv.FormatUint(uint64(change.Mode), 8) + " " + gitBlobID(change.Data) + " 0\t" + change.Path
		if got := fixture.git(fixture.checkout.path, "ls-files", "--stage", "--", change.Path); got != want {
			t.Fatalf("staged entry=%q want=%q", got, want)
		}
	}
	if err := fixture.checkout.Import(context.Background(), fixture.base, nil); err == nil {
		t.Fatal("empty declaration accepted existing staged changes")
	}
}

func TestSourceAliasesAreUnsupportedForDefaultAndPullRequestCheckout(t *testing.T) {
	const path = "git~1/hooks/test"
	t.Parallel()
	fixture := newFixture(t)
	alias := filepath.Join(fixture.source, filepath.FromSlash(path))
	writeFile(t, alias, "not Git metadata")
	fixture.git(fixture.source, "-c", "core.protectHFS=false", "-c", "core.protectNTFS=false", "add", "--all", "--", ".")
	fixture.git(fixture.source, "commit", "-m", "source alias")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main", "HEAD:refs/pull/7/head")
	lookup := localTreeMetadata(fixture.source)
	checked := false
	lookupChecked := func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		for _, entry := range tree.Tree {
			if entry.Path == path {
				checked = true
			}
		}
		return tree, err
	}
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", lookupChecked, true)
	if checkout != nil {
		defer func() {
			if err := checkout.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	var exit *exec.ExitError
	if !checked || !errors.Is(err, ErrUnsupportedSource) || errors.As(err, &exit) || !strings.Contains(err.Error(), "unsafe source path") {
		t.Fatalf("default checkout metadataChecked=%v error=%v", checked, err)
	}
	checked = false
	fixture.checkout.lookupTree = lookupChecked
	err = fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head)
	if !checked || !errors.Is(err, ErrUnsupportedSource) || errors.As(err, &exit) || !strings.Contains(err.Error(), "unsafe source path") {
		t.Fatalf("PR checkout metadataChecked=%v error=%v", checked, err)
	}
}

func TestGitFailureClassificationIsSpecificAndPreservesCancellation(t *testing.T) {
	root := t.TempDir()
	shim := "#!/bin/sh\nprintf '%s\\n' \"$MINION_TEST_GIT_DIAGNOSTIC\" >&2\nexit 1\n"
	fakeOnPath(t, "git", shim)
	for _, tc := range []struct {
		operation   string
		diagnostic  string
		unsupported bool
	}{
		{"checkout", "error: invalid path 'git~1/config'", true},
		{"add", "warning: ignored path\nerror: invalid path 'git~1/config'", true},
		{"status", "error: invalid path 'git~1/config'", false},
		{"checkout", "fatal: unable to create index.lock: No space left on device", false},
		{"checkout", "fatal: cannot read 'error: invalid path file'", false},
	} {
		t.Setenv("MINION_TEST_GIT_DIAGNOSTIC", tc.diagnostic)
		_, err := runGit(context.Background(), root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", tc.operation)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || errors.Is(err, ErrUnsupportedSource) != tc.unsupported || !strings.HasPrefix(err.Error(), "git "+tc.operation+":") || !strings.Contains(err.Error(), strconv.Quote(tc.diagnostic)) || strings.ContainsAny(err.Error(), "\r\n") {
			t.Fatalf("operation=%s diagnostic=%q error=%v", tc.operation, tc.diagnostic, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runGit(ctx, root, "checkout"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("cancelled checkout error=%v", err)
	}
	if _, err := runGit(context.Background(), filepath.Join(root, "missing"), "checkout"); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("checkout I/O error=%v", err)
	}
}

func TestSourceTreeResponseOverflowIsTerminalSizeRefusal(t *testing.T) {
	fixture := newFixture(t)
	fixture.checkout.lookupTree = func(context.Context, string) (github.GitTree, error) {
		return github.GitTree{}, github.ErrResponseTooLarge
	}
	_, err := fixture.checkout.Snapshot(context.Background(), false)
	if !errors.Is(err, ErrCheckoutTooLarge) || !errors.Is(err, github.ErrResponseTooLarge) {
		t.Fatalf("source response overflow classification: %v", err)
	}
}
