package gitwork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestRestoreCheckoutUsesPinnedSourceAfterDefaultBranchMoves(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.source, "README.md"), "later default branch\n")
	fixture.commit("refs/heads/main", "advance default branch")

	restored, err := RestoreCheckout(t.Context(), t.TempDir(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	if head, err := restored.verifiedHead(t.Context()); err != nil || head != snapshot.Head {
		t.Fatalf("restored head = %q, error = %v; want %q", head, err, snapshot.Head)
	}
	const branch = "minion/fix-recovered"
	if err := restored.NewBranch(t.Context(), branch); err != nil {
		t.Fatal(err)
	}
	if err := restored.Import(t.Context(), snapshot.Head, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
	var recorded string
	if err := restored.commitAndPush(t.Context(), fixture.remote, branch, "example-bot", "Fix from saved result", testCommitDate, func(sha string) error {
		recorded = sha
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if remote := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch); remote != recorded || remote == snapshot.Head {
		t.Fatalf("pushed SHA = %q, recorded SHA = %q, source head = %q", remote, recorded, snapshot.Head)
	}
	if parent := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch+"^"); parent != snapshot.Head {
		t.Fatalf("restored push parent = %q, want %q", parent, snapshot.Head)
	}
}

func TestRestoreCheckoutRejectsChangedPinnedFilesAndCleansUp(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Files[0].Data = []byte("changed")
	root := t.TempDir()
	checkout, err := RestoreCheckout(t.Context(), root, snapshot)
	if err == nil || checkout != nil || !strings.Contains(err.Error(), "source files differ from pinned repository") {
		t.Fatalf("changed source returned checkout=%v error=%v", checkout, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed restore left entries=%v error=%v", entries, err)
	}
}
