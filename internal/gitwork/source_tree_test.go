package gitwork

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func TestCheckSourceTree(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size, fullSize, oversized := int64(1), int64(workspace.MaxBytes), int64(workspace.MaxBytes+1)
	truncated := true
	entries := func(count int, kind, mode string) github.GitTree {
		items := make([]github.GitTreeEntry, count)
		for i := range items {
			items[i] = github.GitTreeEntry{Path: fmt.Sprintf("entry-%d", i), Type: kind, Mode: mode}
		}
		return completeTree(sha, items)
	}
	for _, tc := range []struct {
		name string
		tree github.GitTree
		want error
	}{
		{"wrong identity", completeTree(strings.Repeat("b", 40), []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644", Size: &size}}), ErrUnsupportedSource},
		{"missing completeness", github.GitTree{SHA: sha}, ErrUnsupportedSource},
		{"missing completeness with empty tree", github.GitTree{SHA: sha, Tree: []github.GitTreeEntry{}}, ErrUnsupportedSource},
		{"missing tree", completeTree(sha, nil), ErrUnsupportedSource},
		{"missing size", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644"}}), ErrUnsupportedSource},
		{"unknown type", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644", Size: &size}, {Path: "y", Type: "unknown"}}), ErrUnsupportedSource},
		{"missing mode", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Size: &size}}), ErrUnsupportedSource},
		{"invalid blob mode", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100664", Size: &size}}), ErrUnsupportedSource},
		{"invalid tree mode", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "tree", Mode: "120000"}}), ErrUnsupportedSource},
		{"invalid gitlink mode", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "commit", Mode: "100644"}}), ErrUnsupportedSource},
		{"symlink", completeTree(sha, []github.GitTreeEntry{{Path: ".agents/skills/escape", Type: "blob", Mode: "120000", Size: &size}}), nil},
		{"regular", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644", Size: &size}}), nil},
		{"executable", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100755", Size: &size}}), nil},
		{"directory", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "tree", Mode: "040000"}}), nil},
		{"gitlink", completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "commit", Mode: "160000"}}), nil},
		// Size/completeness gates precede the invalid or missing first entry.
		{"truncated", github.GitTree{SHA: sha, Truncated: &truncated}, ErrCheckoutTooLarge},
		{"too many entries before invalid path", completeTree(sha, make([]github.GitTreeEntry, workspace.MaxEntries+1)), ErrCheckoutTooLarge},
		{"exact files", entries(workspace.MaxFiles, "commit", "160000"), nil},
		{"exact entries", entries(workspace.MaxEntries, "tree", "040000"), nil},
		{"exact bytes", completeTree(sha, []github.GitTreeEntry{{Path: "full", Type: "blob", Mode: "100644", Size: &fullSize}}), nil},
		{"omitted symlink bytes", completeTree(sha, []github.GitTreeEntry{{Path: "link", Type: "blob", Mode: "120000", Size: &oversized}}), ErrCheckoutTooLarge},
		{"too many gitlinks", entries(workspace.MaxFiles+1, "commit", "160000"), ErrCheckoutTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSourceTree(tc.tree, sha)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrUnsupportedSource) && tc.want != ErrUnsupportedSource || errors.Is(err, ErrCheckoutTooLarge) && tc.want != ErrCheckoutTooLarge {
				t.Fatalf("metadata error=%v, want %v", err, tc.want)
			}
		})
	}
}
