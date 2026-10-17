package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	writeFixtureFile(t, source, "source.txt", []byte("reviewed"))
	writeFixtureFile(t, source, "added", []byte("new executable"))
	if err := os.Chmod(filepath.Join(source, "added"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, source, "runtime.log", make([]byte, workspace.MaxChangeBytes+1))
	snapshot := mustFreeze(t, context.Background(), t.TempDir(), job, source, "", index)
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
	if err := snapshot.verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, source, "source.txt", []byte("changed after proposal"))
	data, err := workspace.ReadRegular(filepath.Join(snapshot.source, "source.txt"), workspace.MaxBytes)
	if err != nil || string(data) != "reviewed" {
		t.Fatalf("background change reached critic copy=%q,%v", data, err)
	}
	if err := snapshot.verify(context.Background()); err == nil {
		t.Fatal("accepted candidate source changed during review")
	}
}

func TestReviewSnapshotFixFreezesFinalReplacementLayout(t *testing.T) {
	job := portableJob(command.Fix)
	unchanged := job.Source.Files[0]
	job.Source.Files = append(job.Source.Files,
		workspace.File{Path: "Foo/old", Mode: workspace.Regular},
		workspace.File{Path: "to-directory", Mode: workspace.Regular},
		workspace.File{Path: "to-file/child", Mode: workspace.Regular})
	index, source := offlineIndexFixture(t, job.Source.Files)
	for _, name := range []string{"Foo", "to-directory", "to-file"} {
		if err := os.RemoveAll(filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	want := []workspace.File{
		unchanged,
		{Path: "foo/new", Mode: workspace.Regular, Data: []byte("renamed directory")},
		{Path: "to-directory/child", Mode: workspace.Regular, Data: []byte("new child")},
		{Path: "to-file", Mode: workspace.Regular, Data: []byte("new file")},
	}
	for _, file := range want[1:] {
		writeFixtureFile(t, source, file.Path, file.Data)
	}
	snapshot := mustFreeze(t, context.Background(), t.TempDir(), job, source, "", index)
	if len(snapshot.changes) != 6 {
		t.Fatalf("replacement changes=%+v", snapshot.changes)
	}
	if err := workspace.CheckUnchanged(context.Background(), snapshot.source, want); err != nil {
		t.Fatalf("frozen critic tree differs from exported changes: %v", err)
	}
	if err := snapshot.verify(context.Background()); err != nil {
		t.Fatalf("unchanged candidate verification failed: %v", err)
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
			if snapshot.source != source || snapshot.ownedSource {
				t.Fatal("copied an already read-only audit source")
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
			if err := snapshot.verify(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "source":
				writeFixtureFile(t, source, "source.txt", []byte("background source edit"))
			case "report":
				writeFixtureFile(t, output, "REPORT.md", []byte("background report edit"))
			case "new artifact":
				writeFixtureFile(t, output, "findings.json", []byte("[]"))
			case "removed artifact":
				if err := os.Remove(filepath.Join(output, "REPORT.md")); err != nil {
					t.Fatal(err)
				}
			}
			data, err = workspace.ReadRegular(filepath.Join(snapshot.outputDir, "REPORT.md"), maxOutputBytes)
			if err != nil || string(data) != "reviewed report" {
				t.Fatalf("background work changed frozen report=%q,%v", data, err)
			}
			if mutation == "source" {
				if err := snapshot.verify(context.Background()); err != nil {
					t.Fatalf("read-only source checked before final canary: %v", err)
				}
				if err := workspace.CheckUnchanged(context.Background(), source, job.Source.Files); err == nil {
					t.Fatal("final read-only canary accepted a changed source")
				}
			} else if err := snapshot.verify(context.Background()); err == nil || !strings.Contains(err.Error(), "candidate reports changed") {
				t.Fatalf("mutation %s: got %v", mutation, err)
			}
			if err := snapshot.close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("critic copies remain after cleanup=%v,%v", entries, err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("cleanup removed the shared read-only source: %v", err)
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
				writeFixtureFile(t, output, "REPORT.md", []byte(strings.Repeat("x", maxOutputBytes+1)))
			case "artifact limit":
				writeFixtureFile(t, output, "findings.json", make([]byte, maxAuditArtifactBytes+1))
			case "symlink":
				if err := os.Symlink(filepath.Join(source, "source.txt"), filepath.Join(output, "REPORT.md")); err != nil {
					t.Fatal(err)
				}
			}
			wantCause := "limit"
			if invalid == "symlink" {
				wantCause = "symbolic links"
			}
			if snapshot, err := freezeReviewSnapshot(context.Background(), workRoot, job, source, output, nil); err == nil || snapshot != nil || (invalid == "symlink" && !errors.Is(err, syscall.ELOOP)) || (invalid != "symlink" && !strings.Contains(err.Error(), wantCause)) {
				t.Fatalf("accepted unsafe reports: %+v,%v", snapshot, err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed freeze left copies=%v,%v", entries, err)
			}
		})
	}
}

func TestReviewSnapshotCleansFailedFixCandidateMaterialization(t *testing.T) {
	for _, stage := range []string{"source", "addition"} {
		t.Run(stage, func(t *testing.T) {
			job := portableJob(command.Fix)
			name := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 250)+"/", 14), "/")
			if stage == "source" {
				job.Source.Files = append(job.Source.Files, workspace.File{Path: name, Mode: workspace.Regular, Data: []byte("source")})
			}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid source control: %v", err)
			}
			index, source := offlineIndexFixture(t, job.Source.Files)
			if stage == "addition" {
				writeFixtureFile(t, source, name, []byte("added"))
			}
			control, err := freezeReviewSnapshot(context.Background(), t.TempDir(), job, source, "", index)
			if err != nil {
				t.Fatalf("valid candidate failed under short root: %v", err)
			}
			if err := control.close(); err != nil {
				t.Fatal(err)
			}
			workRoot := t.TempDir()
			for i := 0; i < 3; i++ {
				workRoot = filepath.Join(workRoot, strings.Repeat("b", 200))
			}
			if err := os.MkdirAll(workRoot, 0700); err != nil {
				t.Fatal(err)
			}
			failed, err := freezeReviewSnapshot(context.Background(), workRoot, job, source, "", index)
			if failed != nil || !errors.Is(err, syscall.ENAMETOOLONG) {
				t.Fatalf("expected %s filesystem failure: snapshot=%v error=%v", stage, failed, err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed %s left frozen source: entries=%v error=%v", stage, entries, err)
			}
		})
	}
}
