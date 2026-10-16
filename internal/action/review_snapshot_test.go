package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

func TestReviewSnapshotFixFreezesOnlyExportedCandidate(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = append(job.Source.Files,
		workspace.File{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.log\n")},
		workspace.File{Path: "deleted", Mode: workspace.Regular, Data: []byte("deleted")})
	index, source := offlineIndexFixture(t, job.Source.Files)
	writeIndexFixture(t, source, "source.txt", []byte("reviewed"))
	writeIndexFixture(t, source, "added", []byte("new executable"))
	if err := os.Chmod(filepath.Join(source, "added"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "runtime.log", make([]byte, workspace.MaxChangeBytes+1))
	snapshot, err := freezeReviewSnapshot(context.Background(), t.TempDir(), job, source, "", index)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.close(); err != nil {
			t.Error(err)
		}
	})
	if snapshot.source == source || snapshot.outputDir != "" || len(snapshot.changes) != 3 {
		t.Fatalf("unexpected critic snapshot: %+v", snapshot)
	}
	for _, name := range []string{"runtime.log", "deleted", ".git"} {
		if _, err := os.Lstat(filepath.Join(snapshot.source, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied non-candidate path %q: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(snapshot.source, "added"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("frozen executable=%v,%v", info, err)
	}
	if err := snapshot.verify(context.Background(), job, source, "", index); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "source.txt", []byte("changed after proposal"))
	data, err := workspace.ReadRegular(filepath.Join(snapshot.source, "source.txt"), workspace.MaxBytes)
	if err != nil || string(data) != "reviewed" {
		t.Fatalf("background change reached critic copy=%q,%v", data, err)
	}
	if err := snapshot.verify(context.Background(), job, source, "", index); err == nil {
		t.Fatal("accepted candidate source changed during review")
	}
}

func TestReviewSnapshotFreezesRawAuditArtifactsAndAbsence(t *testing.T) {
	for _, mutation := range []string{"source", "report", "new artifact", "removed artifact"} {
		t.Run(mutation, func(t *testing.T) {
			job := portableJob(command.SecurityAudit)
			source := t.TempDir()
			if err := os.Chmod(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := workspace.Materialize(source, job.Source.Files); err != nil {
				t.Fatal(err)
			}
			output, workRoot := t.TempDir(), t.TempDir()
			rawMetadata := `{"target":"` + source + `","raw":"not normalized"}`
			for name, data := range map[string]string{"REPORT.md": "reviewed report", "run-metadata.json": rawMetadata, "FINDINGS-DETAIL.md": ""} {
				if err := os.WriteFile(filepath.Join(output, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := freezeReviewSnapshot(context.Background(), workRoot, job, source, output, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := snapshot.close(); err != nil {
					t.Error(err)
				}
			})
			data, err := workspace.ReadRegular(filepath.Join(snapshot.outputDir, "run-metadata.json"), maxAuditArtifactBytes)
			if err != nil || string(data) != rawMetadata || len(snapshot.artifacts) != 3 {
				t.Fatalf("frozen raw reports=%q,%+v,%v", data, snapshot.artifacts, err)
			}
			if _, err := os.Lstat(filepath.Join(snapshot.outputDir, "findings.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("absent artifact was created: %v", err)
			}
			if err := snapshot.verify(context.Background(), job, source, output, nil); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "source":
				writeIndexFixture(t, source, "source.txt", []byte("background source edit"))
			case "report":
				writeIndexFixture(t, output, "REPORT.md", []byte("background report edit"))
			case "new artifact":
				writeIndexFixture(t, output, "findings.json", []byte("[]"))
			case "removed artifact":
				if err := os.Remove(filepath.Join(output, "REPORT.md")); err != nil {
					t.Fatal(err)
				}
			}
			data, err = workspace.ReadRegular(filepath.Join(snapshot.outputDir, "REPORT.md"), maxOutputBytes)
			if err != nil || string(data) != "reviewed report" {
				t.Fatalf("background work changed frozen report=%q,%v", data, err)
			}
			if err := snapshot.verify(context.Background(), job, source, output, nil); err == nil {
				t.Fatalf("accepted %s during review", mutation)
			}
			if err := snapshot.close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("critic copies remain after cleanup=%v,%v", entries, err)
			}
		})
	}
}

func TestReviewSnapshotRejectsUnsafeOrOversizedReportsAndCleansUp(t *testing.T) {
	for _, invalid := range []string{"report limit", "artifact limit", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			job := portableJob(command.SecurityAudit)
			source, output, workRoot := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Chmod(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := workspace.Materialize(source, job.Source.Files); err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "report limit":
				writeIndexFixture(t, output, "REPORT.md", []byte(strings.Repeat("x", maxOutputBytes+1)))
			case "artifact limit":
				writeIndexFixture(t, output, "findings.json", make([]byte, maxAuditArtifactBytes+1))
			case "symlink":
				if err := os.Symlink(filepath.Join(source, "source.txt"), filepath.Join(output, "REPORT.md")); err != nil {
					t.Fatal(err)
				}
			}
			if snapshot, err := freezeReviewSnapshot(context.Background(), workRoot, job, source, output, nil); err == nil || snapshot != nil {
				t.Fatalf("accepted unsafe reports: %+v,%v", snapshot, err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed freeze left copies=%v,%v", entries, err)
			}
		})
	}
}
