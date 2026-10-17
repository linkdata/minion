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
	for _, scenario := range []string{"no-op", "ignored", "undeclared", "unsorted"} {
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
			}
			err := fixture.checkout.Import(context.Background(), fixture.base, changes)
			switch scenario {
			case "unsorted":
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

func TestSourceAliasesAreUnsupportedForDefaultAndPullRequestCheckout(t *testing.T) {
	for _, path := range []string{"git~1/hooks/test"} {
		t.Run(path, func(t *testing.T) {
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
		})
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
