package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestRepositoryDigestAndVerifyReportCompletedReads(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root.txt", filepath.Join("nested", "leaf.txt")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	progress := 0
	report := func() { progress++ }
	digest, err := repositoryDigestWithProgress(t.Context(), dir, workspace.MaxEntries, report)
	if err != nil || progress != 2 {
		t.Fatalf("initial digest progress=%d error=%v; want 2 verified files", progress, err)
	}
	repo := &Repository{dir: dir, digest: digest, progress: report}
	for i := 1; i <= 2; i++ {
		if err := repo.Verify(t.Context()); err != nil || progress != 2*(i+1) {
			t.Fatalf("verification %d progress=%d error=%v", i, progress, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	progress = 0
	if _, err := repositoryDigestWithProgress(ctx, dir, workspace.MaxEntries, func() {
		progress++
		if progress == 2 {
			cancel()
		}
	}); !errors.Is(err, context.Canceled) || progress != 2 {
		t.Fatalf("final-read cancellation progress=%d error=%v", progress, err)
	}
}
