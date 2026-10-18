package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestReviewSnapshotFixFreezesOnlyExportedCandidate(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = append(job.Source.Files,
		workspace.File{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.log\n")},
		workspace.File{Path: "deleted", Mode: workspace.Regular, Data: []byte("deleted")})
	attachRepository(t, &job)
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
	for _, name := range []string{"runtime.log", "deleted"} {
		if _, err := os.Lstat(filepath.Join(snapshot.source, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied non-candidate path %q: %v", name, err)
		}
	}
	assertPortableSource(t, snapshot.source)
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
	if err := snapshot.verify(context.Background()); err != nil {
		t.Fatalf("live source edit invalidated the frozen candidate: %v", err)
	}
	for _, change := range snapshot.changes {
		if change.Path == "source.txt" && string(change.Data) != "reviewed" {
			t.Fatalf("exported live source bytes: %q", change.Data)
		}
	}
}

func TestReviewSnapshotFixFreezesFinalReplacementLayout(t *testing.T) {
	job := portableJob(command.Fix)
	unchanged := job.Source.Files[0]
	job.Source.Files = append(job.Source.Files,
		workspace.File{Path: "Foo/old", Mode: workspace.Regular},
		workspace.File{Path: "to-directory", Mode: workspace.Regular},
		workspace.File{Path: "to-file/child", Mode: workspace.Regular})
	attachRepository(t, &job)
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
	if err := workspace.CheckRepositoryChangesUnchanged(context.Background(), snapshot.source, want, nil); err != nil {
		t.Fatalf("frozen critic tree differs from exported changes: %v", err)
	}
	if err := snapshot.verify(context.Background()); err != nil {
		t.Fatalf("unchanged candidate verification failed: %v", err)
	}
}

func TestReviewSnapshotFreezesRawAuditArtifactsAndAbsence(t *testing.T) {
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
	snapshot := mustFreeze(t, context.Background(), workRoot, job, source, output, nil)
	if snapshot.source != source || snapshot.ownedSource {
		t.Fatal("copied an already read-only audit source")
	}
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
	writeFixtureFile(t, output, "REPORT.md", []byte("background report edit"))
	data, err = workspace.ReadRegular(filepath.Join(snapshot.outputDir, "REPORT.md"), maxOutputBytes)
	if err != nil || string(data) != "reviewed report" {
		t.Fatalf("background work changed frozen report=%q,%v", data, err)
	}
	if err := snapshot.verify(context.Background()); err != nil {
		t.Fatalf("live report edit invalidated frozen publication bytes: %v", err)
	}
	for _, artifact := range snapshot.artifacts {
		if artifact.Path == "REPORT.md" && string(artifact.Data) != "reviewed report" {
			t.Fatalf("exported live report bytes: %q", artifact.Data)
		}
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
}

func TestReviewSnapshotCleansFailedReportFreeze(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	source, output, workRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, job.Source.Files); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, output, "findings.json", make([]byte, maxAuditArtifactBytes+1))
	snapshot, err := freezeReviewSnapshot(context.Background(), workRoot, job, source, output, nil)
	if err == nil || snapshot == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized reports accepted: %+v,%v", snapshot, err)
	}
	if err := snapshot.close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("failed freeze left copies: %v,%v", entries, err)
	}
}

func TestReviewSnapshotCleansFailedFixCandidateMaterialization(t *testing.T) {
	for _, stage := range []string{"source", "addition"} {
		t.Run(stage, func(t *testing.T) {
			job := portableJob(command.Fix)
			name := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 250)+"/", 14), "/")
			if stage == "source" {
				job.Source.Files = append(job.Source.Files, workspace.File{Path: name, Mode: workspace.Regular, Data: []byte("source")})
				attachRepository(t, &job)
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
			if failed == nil || !failed.ownedSource || !errors.Is(err, syscall.ENAMETOOLONG) {
				t.Fatalf("expected %s filesystem failure: snapshot=%v error=%v", stage, failed, err)
			}
			if err := failed.close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(workRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed %s left frozen source: entries=%v error=%v", stage, entries, err)
			}
		})
	}
}

func TestReviewSnapshotRejectsCriticCopyMutation(t *testing.T) {
	for _, phase := range []string{reviewFull, reviewVerification} {
		t.Run(phase, func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 2
			calls, fullReviews := 0, 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == roleMain {
					if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte(fmt.Sprintf("candidate %d", request.round)), 0644); err != nil {
						t.Fatal(err)
					}
					return aicli.Result{Answer: proposalJSON("Completed fix.", "")}, nil
				}
				if request.reviewPhase == reviewFull {
					fullReviews++
				}
				if request.reviewPhase == phase {
					if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte("tampered by critic"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if request.reviewPhase == reviewFull {
					return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Revise the candidate."}`}, nil
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantCalls := 2
			if phase == reviewVerification {
				wantCalls = 4
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "frozen candidate source changed") || calls != wantCalls || fullReviews != 1 || result.Answer != "" || len(result.Changes) != 0 {
				t.Fatalf("mutated critic copy reached a subsequent call or successful export: calls=%d full=%d result=%+v error=%v", calls, fullReviews, result, err)
			}
			if len(result.Calls) != wantCalls {
				t.Fatalf("lost calls after integrity failure: %+v", result.Calls)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("invalid partial evidence after integrity failure: %v", err)
			}
			last, wantApproval := result.Calls[len(result.Calls)-1], phase == reviewVerification
			if last.Approved != wantApproval || last.NoBlockers != wantApproval || last.HasBlockers != (phase == reviewFull) {
				t.Fatalf("lost native critic verdict after integrity failure: %+v", last)
			}
		})
	}
}

func TestReviewSnapshotCancellationKeepsVerdictWithoutIntegrityFailure(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", interrupted), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := withClaudeCritic(portableJob(command.Fix))
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleMain {
					writeFixtureFile(t, request.dir, "source.txt", []byte("candidate"))
					return aicli.Result{Answer: proposalJSON("Completed fix.", "")}, nil
				}
				if interrupted {
					cancel()
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			result, err := runner.Run(ctx, job)
			if interrupted {
				if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), "frozen") || result.Answer != "" || len(result.Changes) != 0 {
					t.Fatalf("cancellation reported as an integrity failure: result=%+v error=%v", result, err)
				}
			} else if err != nil || result.Answer != "Completed fix." || len(result.Changes) != 1 {
				t.Fatalf("valid candidate refused: result=%+v error=%v", result, err)
			}
			if len(result.Calls) != 2 || !result.Calls[1].Approved {
				t.Fatalf("lost actual critic verdict: %+v", result.Calls)
			}
		})
	}
}

func TestReviewSnapshotChecksExactFrozenFiles(t *testing.T) {
	for _, reports := range []bool{false, true} {
		for _, mutation := range []string{"special permissions", "new file", "directory"} {
			t.Run(fmt.Sprintf("reports=%t/%s", reports, mutation), func(t *testing.T) {
				job := portableJob(command.Fix)
				index, source := offlineIndexFixture(t, job.Source.Files)
				output, name := "", "source.txt"
				if reports {
					job.Action = command.SecurityAudit
					output, name, index = t.TempDir(), "REPORT.md", nil
					writeFixtureFile(t, output, name, []byte("reviewed report"))
				}
				snapshot := mustFreeze(t, context.Background(), t.TempDir(), job, source, output, index)
				if err := snapshot.verify(context.Background()); err != nil {
					t.Fatalf("unchanged frozen copy rejected: %v", err)
				}
				root, want := snapshot.source, "frozen candidate source"
				if reports {
					root, want = snapshot.outputDir, "frozen candidate reports"
				}
				filename := filepath.Join(root, name)
				switch mutation {
				case "special permissions":
					if err := os.Chmod(filename, os.ModeSetuid|0644); err != nil {
						t.Fatal(err)
					}
				case "new file":
					// An unknown report name is not visited by the known-artifact collector.
					writeFixtureFile(t, root, "unexpected.txt", []byte("new copy content"))
				case "directory":
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filename, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := snapshot.verify(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("frozen %s mutation accepted: %v", mutation, err)
				}
				if reports {
					data, err := os.ReadFile(filepath.Join(output, name))
					if err != nil || string(data) != "reviewed report" {
						t.Fatalf("test changed the live reports instead of the frozen copy: %q,%v", data, err)
					}
				} else {
					changes, err := index.Changes(context.Background())
					if err != nil || len(changes) != 0 {
						t.Fatalf("test changed live source instead of frozen copy: %+v,%v", changes, err)
					}
					if err := job.validateChanges(changes); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestRunnerRejectsGitMetadataMutation(t *testing.T) {
	for _, role := range []string{roleMain, roleCritic} {
		t.Run(role, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Fix))
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				assertPortableSource(t, request.dir)
				if request.role == role {
					writeFixtureFile(t, request.dir, ".git/HEAD", []byte("forged metadata"))
				}
				if request.role == roleMain {
					writeFixtureFile(t, request.dir, "source.txt", []byte("candidate"))
					return aicli.Result{Answer: proposalJSON("candidate", "")}, nil
				}
				return aicli.Result{Answer: `{"approved":true,"has_blockers":false,"feedback":""}`}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || !strings.Contains(err.Error(), "repository changed") || calls < 1 {
				t.Fatalf("metadata mutation accepted: calls=%d result=%+v error=%v", calls, result, err)
			}
		})
	}
}
