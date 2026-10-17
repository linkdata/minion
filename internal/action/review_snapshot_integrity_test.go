package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

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
				return aicli.Result{Answer: `{"approved":true,"has_blockers":false,"feedback":""}`}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantCalls := 2
			if phase == reviewVerification {
				wantCalls = 4
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "frozen candidate source changed") || calls != wantCalls || fullReviews != 1 || result.Answer != "" || len(result.Changes) != 0 {
				t.Fatalf("mutated critic copy reached a subsequent call or successful export: calls=%d full=%d result=%+v error=%v", calls, fullReviews, result, err)
			}
		})
	}
}

func TestReviewSnapshotChecksExactFrozenFiles(t *testing.T) {
	for _, reports := range []bool{false, true} {
		for _, mutation := range []string{"bytes", "permissions", "special permissions", "new file", "removed file"} {
			t.Run(fmt.Sprintf("reports=%t/%s", reports, mutation), func(t *testing.T) {
				job := portableJob(command.Fix)
				index, source := offlineIndexFixture(t, job.Source.Files)
				output, name := "", "source.txt"
				if reports {
					job.Action = command.SecurityAudit
					output, name, index = t.TempDir(), "REPORT.md", nil
					writeIndexFixture(t, output, name, []byte("reviewed report"))
				}
				snapshot, err := freezeReviewSnapshot(context.Background(), t.TempDir(), job, source, output, index)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := snapshot.close(); err != nil {
						t.Error(err)
					}
				})
				if err := snapshot.verify(context.Background()); err != nil {
					t.Fatalf("unchanged frozen copy rejected: %v", err)
				}
				root := snapshot.source
				if reports {
					root = snapshot.outputDir
				}
				filename := filepath.Join(root, name)
				switch mutation {
				case "bytes":
					writeIndexFixture(t, root, name, []byte("tampered copy"))
				case "permissions":
					// Preserve the executable bit: even non-executable permission changes break the copy's identity.
					if err := os.Chmod(filename, 0600); err != nil {
						t.Fatal(err)
					}
				case "special permissions":
					if err := os.Chmod(filename, os.ModeSetuid|0644); err != nil {
						t.Fatal(err)
					}
				case "new file":
					// An unknown report name is not visited by the known-artifact collector.
					writeIndexFixture(t, root, "unexpected.txt", []byte("new copy content"))
				case "removed file":
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
				}
				if err := snapshot.verify(context.Background()); err == nil || !strings.Contains(err.Error(), "frozen candidate") {
					t.Fatalf("frozen %s mutation accepted: %v", mutation, err)
				}
				if reports {
					data, err := os.ReadFile(filepath.Join(output, name))
					if err != nil || string(data) != "reviewed report" {
						t.Fatalf("test changed the live reports instead of the frozen copy: %q,%v", data, err)
					}
				} else if changes, err := reviewChanges(context.Background(), job, index); err != nil || len(changes) != 0 {
					t.Fatalf("test changed live source instead of frozen copy: %+v,%v", changes, err)
				}
			})
		}
	}
}
