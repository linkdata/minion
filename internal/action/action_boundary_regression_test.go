package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func TestJobActionTextBounds(t *testing.T) {
	text := strings.Repeat("x", workspace.MaxBytes+1)
	for _, tt := range []struct {
		name, title, description, notes string
		valid                           bool
	}{
		{"combined exact", "t", text[:workspace.MaxBytes-2], "n", true},
		{"combined over", "t", text[:workspace.MaxBytes-1], "n", false},
		{"description exact", "", text[:workspace.MaxBytes], "", true},
		{"description over", "", text, "", false},
		{"notes exact", "", "", text[:workspace.MaxBytes], true},
		{"notes over", "", "", text, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Title, job.Description, job.Notes = tt.title, tt.description, tt.notes
			err := job.Validate()
			if (err == nil) != tt.valid || err != nil && !strings.Contains(err.Error(), "action text is invalid or exceeds limits") {
				t.Fatalf("text boundary=%v want valid=%t", err, tt.valid)
			}
		})
	}
}

func TestJobCombinedRoleSkillBound(t *testing.T) {
	for _, optional := range []int{32, 33} {
		job := portableJob(command.Comment)
		job.Main.Skills = nil
		for i := 0; i < 32; i++ {
			job.Main.Skills = append(job.Main.Skills, skills.BuiltinAntiPromptInjection)
		}
		for i := 0; i < optional; i++ {
			job.Main.AlwaysOnSkills = append(job.Main.AlwaysOnSkills, skills.BuiltinAntiPromptInjection)
		}
		err := job.Validate()
		if (err == nil) != (optional == 32) || err != nil && !strings.Contains(err.Error(), "invalid action model or skills") {
			t.Fatalf("32+%d role skills: %v", optional, err)
		}
	}
}

func TestJobCriticBudgetBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name              string
		critic            bool
		rounds, threshold int
		valid             bool
	}{
		{"disabled", false, 0, 0, true}, {"inactive budget", false, 1, 0, false}, {"inactive threshold", false, 0, 1, false},
		{"zero active", true, 0, 0, false}, {"over maximum", true, config.MaxCriticMaxRounds + 1, 0, false},
		{"one", true, 1, 0, true}, {"maximum", true, config.MaxCriticMaxRounds, 0, true},
		{"negative threshold", true, 2, -1, false}, {"threshold exceeds budget", true, 2, 3, false}, {"threshold at budget", true, 2, 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := portableJob(command.Comment)
			if tt.critic {
				job.Critic = &RoleSettings{Provider: "claude"}
			}
			job.CriticMaxRounds, job.CriticNonblockingRounds = tt.rounds, tt.threshold
			if err := job.Validate(); (err == nil) != tt.valid {
				t.Fatalf("budget validation=%v want valid=%t", err, tt.valid)
			}
		})
	}
}

func TestAuditIncompleteOversizedReportKeepsOtherArtifacts(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	root := t.TempDir()
	for _, file := range auditArtifactsFixture(job, sourceIdentity(job), strings.Repeat("x", maxOutputBytes+1)) {
		writeFixtureFile(t, root, file.Path, file.Data)
	}
	files, diagnostics, err := readAuditArtifacts(root, true)
	if err != nil || len(files) != len(auditArtifacts)-1 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_unavailable" || !strings.Contains(diagnostics[0].Message, "REPORT.md") {
		t.Fatalf("partial report collection: files=%d diagnostics=%+v error=%v", len(files), diagnostics, err)
	}
	for _, file := range files {
		if file.Path == "REPORT.md" {
			t.Fatal("oversized raw report retained")
		}
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Artifacts: files, Diagnostics: diagnostics}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("six safe artifacts cannot be retained: %v", err)
	}
}

func TestAuditReviewReportRejectsInvalidUTF8AndSize(t *testing.T) {
	for _, report := range [][]byte{{0xff}, []byte(strings.Repeat("x", maxOutputBytes+1))} {
		root := t.TempDir()
		writeFixtureFile(t, root, "REPORT.md", report)
		if _, _, err := readAuditArtifacts(root, false); err == nil || !strings.Contains(err.Error(), "REPORT.md") {
			t.Fatalf("invalid report accepted: %v", err)
		}
	}
}

func TestExecuteCriticAdmissionUsesControlledWorkRoot(t *testing.T) {
	for _, critic := range []bool{false, true} {
		for _, unsafe := range []bool{false, true} {
			t.Run(fmt.Sprintf("critic_%t/unsafe_%t", critic, unsafe), func(t *testing.T) {
				gitFixtureRemote(t)
				root, other := filepath.Join(t.TempDir(), "controlled"), t.TempDir()
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				calls := 0
				failure := errors.New("provider stopped")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !serveIssueFixture(t, w, r) {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				e := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}, Runner: Runner{WorkRoot: other, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if filepath.Dir(request.dir) != root {
						t.Fatalf("runner ignored orchestrator placement: %s", request.dir)
					}
					return aicli.Result{}, failure
				}}}
				if critic {
					e.Adversarial = "claude"
				}
				if unsafe {
					e.Runner.Codex.ReadOnlyDirs = []string{filepath.Dir(root)}
				}
				request := usageJob()
				request.Command = command.Command{Name: "comment", Action: command.Comment}
				request.Repository.Actions = []string{"comment"}
				err := e.Execute(context.Background(), request)
				if unsafe {
					if !errors.Is(err, workroot.ErrUnsafe) || errors.Is(err, monitor.ErrTerminalJob) || calls != 0 {
						t.Fatalf("admission classification: calls=%d error=%v", calls, err)
					}
					if _, err := os.Stat(filepath.Join(root, "reviews")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused admission wrote private evidence: %v", err)
					}
				} else if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("safe controlled root did not dispatch: calls=%d error=%v", calls, err)
				}
				entries, err := os.ReadDir(other)
				if err != nil || len(entries) != 0 {
					t.Fatalf("unused Runner root changed: %v,%v", entries, err)
				}
			})
		}
	}
}

func TestRefutationInitialCheckpointIsRetryableBeforeModels(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	checkpoints := 0
	runner := Runner{WorkRoot: t.TempDir(), recordReview: func(progress Result) error {
		checkpoints++
		if len(progress.Calls) != 0 || len(progress.Refutations) != 0 || progress.Identity != job.Identity || progress.SourceRef != job.Source.Head {
			t.Fatalf("initial checkpoint is not metadata only: %+v", progress)
		}
		return &os.PathError{Op: "write", Path: "private-review", Err: syscall.ENOSPC}
	}, runModel: forbidModel(t)}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, monitor.ErrTerminalJob) || checkpoints != 1 || len(result.Calls) != 0 || len(result.Refutations) != 0 || result.Answer != "" {
		t.Fatalf("initial checkpoint policy: checkpoints=%d result=%+v error=%v", checkpoints, result, err)
	}
}

func TestPrivateReviewRejectsChangedSavedCheckpoint(t *testing.T) {
	for _, kind := range []string{"corrupt", "identity", "source", "policy", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			result := privateReviewResult(job)
			e := Orchestrator{WorkRoot: t.TempDir()}
			if err := e.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.WorkRoot, "reviews", job.Identity.AttemptID+".json")
			saved := readPrivateReview(t, e.WorkRoot)
			outside := filepath.Join(t.TempDir(), "outside")
			data := []byte("{")
			switch kind {
			case "identity":
				saved.Identity.RequesterID++
			case "source":
				saved.SourceRef = strings.Repeat("b", 40)
			case "policy":
				saved.CriticNonblockingRounds++
			case "symlink":
				writeFixtureFile(t, filepath.Dir(outside), filepath.Base(outside), []byte("outside sentinel"))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "corrupt" && kind != "symlink" {
				var err error
				data, err = json.Marshal(saved)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind != "symlink" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := e.retainReviewEvidence(job, result)
			if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("tampered checkpoint accepted: %v", err)
			}
			if kind == "corrupt" && !strings.Contains(err.Error(), "saved review evidence is invalid") {
				t.Fatalf("corruption lost: %v", err)
			}
			if kind == "identity" || kind == "source" || kind == "policy" {
				if !strings.Contains(err.Error(), "changed or discarded a saved prefix") {
					t.Fatalf("checkpoint binding lost: %v", err)
				}
			}
			if kind == "symlink" {
				if data, err := os.ReadFile(outside); err != nil || string(data) != "outside sentinel" {
					t.Fatalf("outside checkpoint touched: %q,%v", data, err)
				}
			}
		})
	}
}

func TestRetainedAuditRequiresPrivateDirectory(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			auditRoot := filepath.Join(root, "audits")
			if symlink {
				if err := os.Symlink(outside, auditRoot); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(auditRoot, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(auditRoot, 0755); err != nil {
					t.Fatal(err)
				}
			}
			job := portableJob(command.SecurityAudit)
			e := Orchestrator{WorkRoot: root}
			if err := e.retainAudit(&preparedAction{job: job}, completeAuditResult(job), false); err == nil {
				t.Fatal("audit retained through unsafe directory")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside directory changed: %v,%v", entries, err)
			}
		})
	}
}
