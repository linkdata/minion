package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
)

func TestLiveReviewAction(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" || os.Getenv("MINION_LIVE_REVIEW") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 and MINION_LIVE_REVIEW=1 to submit a persistent live review")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fixture := liveActionSetup(t, ctx)
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("GH_TOKEN", fixture.botToken)
	pull := liveReviewAuditPull(t, ctx, fixture.owner, fixture.cfg)
	job := liveReviewAuditJob(t, fixture.cfg, fixture.repo, fixture.ownerUser, command.Review, "PullRequest", pull.Number, pull.HTMLURL)
	var calls int
	orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: fixture.bot,
		BotLogin: fixture.cfg.BotLogin,
		Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}},
		Runner: Runner{
			runModel: func(ctx context.Context, modelRequest modelRun) (aicli.Result, error) {
				dir, prompt, writable, outputDir := modelRequest.dir, modelRequest.prompt, modelRequest.writable, modelRequest.outputDir
				calls++
				if writable || outputDir != "" || !strings.Contains(prompt, pull.Head.SHA) {
					return aicli.Result{}, fmt.Errorf("live review did not use the expected read-only pull request")
				}
				assertModelRepository(t, dir, pull.Head.SHA)
				return aicli.Result{Answer: "Live review fixture " + job.ID[:12] + "."}, nil
			},
		},
	}
	if err := orchestrator.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("review model calls = %d, want 1", calls)
	}
	review := liveReviewAuditSubmittedReview(t, ctx, fixture.ownerToken, fixture.cfg, job, pull.Head.SHA)
	if err := orchestrator.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || liveReviewAuditSubmittedReview(t, ctx, fixture.ownerToken, fixture.cfg, job, pull.Head.SHA).ID != review.ID {
		t.Fatal("repeat review execution published another review or reran the model")
	}
	t.Logf("submitted review remains: %s", review.HTMLURL)
}

func TestLiveSecurityAuditAction(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 to post temporary live audit replies")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("audit validators require node")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fixture := liveActionSetup(t, ctx)
	installActionTestSkills(t, testAuditSkillURL)
	t.Setenv("GH_TOKEN", fixture.botToken)
	if fixture.cfg.IssueNumber <= 0 {
		t.Fatal("live audit requires an issue_number fixture")
	}
	issue, err := fixture.owner.IssueDetails(ctx, liveFixRepository, fixture.cfg.IssueNumber)
	if err != nil || !strings.EqualFold(issue.User.Login, fixture.cfg.OwnerLogin) {
		t.Fatalf("issue #%d is not owner-authored: %v", fixture.cfg.IssueNumber, err)
	}
	base, err := fixture.owner.Branch(ctx, liveFixRepository, fixture.repo.DefaultBranch)
	if err != nil || len(base.Commit.SHA) != 40 {
		t.Fatalf("default branch HEAD is unavailable: %v", err)
	}
	pull := liveReviewAuditPull(t, ctx, fixture.owner, fixture.cfg)
	for _, test := range []struct {
		name, subject, url, head string
		number                   int
	}{
		{"issue", "Issue", issue.HTMLURL, base.Commit.SHA, issue.Number},
		{"pull request", "PullRequest", pull.HTMLURL, pull.Head.SHA, pull.Number},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := liveReviewAuditJob(t, fixture.cfg, fixture.repo, fixture.ownerUser, command.SecurityAudit, test.subject, test.number, test.url)
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				liveActionReplyCleanup(t, cleanupCtx, fixture.owner, fixture.ownerToken, fixture.cfg, job)
			})
			report := "Minion live audit fixture " + job.ID[:12] + ".\n"
			var calls int
			orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: fixture.bot,
				BotLogin: fixture.cfg.BotLogin,
				Commands: map[string]config.CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{testAuditSkillURL}}},
				Runner: Runner{
					runModel: func(ctx context.Context, modelRequest modelRun) (aicli.Result, error) {
						dir, prompt, writable, outputDir := modelRequest.dir, modelRequest.prompt, modelRequest.writable, modelRequest.outputDir
						calls++
						if writable || outputDir == "" || strings.HasPrefix(outputDir, dir+string(os.PathSeparator)) || !strings.Contains(prompt, test.head) {
							return aicli.Result{}, fmt.Errorf("live audit did not use a separate report directory and expected commit")
						}
						assertModelRepository(t, dir, test.head)
						metadata := fmt.Sprintf(`{"run_id":%q,"repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.ID, liveFixRepository, dir, test.head, auditTestOwners)
						for name, content := range map[string]string{
							"run-metadata.json": metadata, "architecture.md": "Test fixture architecture.\n",
							"coverage-ledger.json": auditCoveredLedgerFixture, "findings.json": "[]",
							"REPORT.md": report, "FINDINGS-DETAIL.md": "No findings.\n", "NEEDS-VALIDATION.md": "No validation leads.\n",
						} {
							if err := os.WriteFile(filepath.Join(outputDir, name), []byte(content), 0o600); err != nil {
								return aicli.Result{}, err
							}
						}
						return aicli.Result{Answer: "Untrusted final answer"}, nil
					},
				},
			}
			if err := orchestrator.Execute(ctx, job); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("audit model calls = %d, want 1", calls)
			}
			commentID, reply, err := liveActionReply(ctx, fixture.owner, fixture.cfg, job)
			if err != nil || commentID == 0 || !strings.Contains(reply.Body, test.head) || !strings.Contains(reply.Body, strings.TrimSpace(report)) || strings.Contains(reply.Body, "Untrusted final answer") {
				t.Fatalf("live audit acknowledgement does not contain the validated report at %s: %v", test.head, err)
			}
			if err := orchestrator.Execute(ctx, job); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("repeat audit reran the model: %d calls", calls)
			}
			if repeatID, _, err := liveActionReply(ctx, fixture.owner, fixture.cfg, job); err != nil || repeatID != commentID {
				t.Fatalf("repeat audit published another reply: id = %d, want %d: %v", repeatID, commentID, err)
			}
		})
	}
}

func liveReviewAuditJob(t *testing.T, cfg liveFixConfig, repo github.Repository, actor github.User, action command.Action, subject string, number int, url string) monitor.Job {
	t.Helper()
	var randomID [32]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		t.Fatal(err)
	}
	return monitor.Job{
		Repository:  config.Repository{Name: liveFixRepository, ID: repo.ID, OwnerID: repo.Owner.ID, Actions: []string{string(action)}},
		SubjectType: subject, Number: number, ID: hex.EncodeToString(randomID[:]),
		Source:  github.Item{Body: "@" + cfg.BotLogin + " " + string(action), Author: actor, HTMLURL: url, UpdatedAt: time.Now()},
		Command: command.Command{Name: string(action), Action: action},
	}
}

func liveReviewAuditPull(t *testing.T, ctx context.Context, owner *github.Client, cfg liveFixConfig) github.PullRequest {
	t.Helper()
	if cfg.PullRequestNumber <= 0 {
		t.Fatal("live action requires a pull_request_number fixture")
	}
	pull, err := owner.PullRequestDetails(ctx, liveFixRepository, cfg.PullRequestNumber)
	if err != nil || !strings.EqualFold(pull.User.Login, cfg.OwnerLogin) || !pull.Draft || pull.State != "open" || len(pull.Head.SHA) != 40 || pull.Base.Ref == "" {
		t.Fatalf("pull request #%d is not an open owner-authored draft: %v", cfg.PullRequestNumber, err)
	}
	return pull
}

func liveReviewAuditSubmittedReview(t *testing.T, ctx context.Context, ownerToken string, cfg liveFixConfig, job monitor.Job, head string) github.Review {
	t.Helper()
	owner := github.New(ownerToken)
	items, err := owner.ThreadItems(ctx, liveFixRepository, job.SubjectType, job.Number, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	prefix := liveFixRepository + "#review:"
	var id int64
	for _, item := range items {
		if !strings.HasSuffix(strings.TrimSpace(item.Body), "\n\n"+job.Marker()) {
			continue
		}
		value, ok := strings.CutPrefix(item.Key, prefix)
		if !ok {
			t.Fatalf("live review has unexpected item key %q", item.Key)
		}
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || parsed <= 0 || id != 0 || !strings.EqualFold(item.Author.Login, cfg.BotLogin) {
			t.Fatal("live review has duplicate or unexpected provenance")
		}
		id = parsed
	}
	if id == 0 {
		t.Fatal("live review was not published")
	}
	var review struct {
		github.Review
		CommitID string `json:"commit_id"`
	}
	path := fmt.Sprintf("/pulls/%d/reviews/%d", job.Number, id)
	status, err := liveFixAPI(ctx, ownerToken, http.MethodGet, path, &review)
	if err != nil || status != http.StatusOK {
		t.Fatalf("read live review %d: HTTP %d: %v", id, status, err)
	}
	if review.ID != id || !strings.HasSuffix(strings.TrimSpace(review.Body), "\n\n"+job.Marker()) ||
		!strings.EqualFold(review.User.Login, cfg.BotLogin) || review.State != "COMMENTED" || review.CommitID != head || review.NodeID == "" {
		t.Fatal("live review has unexpected body, state, commit, or author")
	}
	current, err := owner.CurrentItem(ctx, github.Item{NodeID: review.NodeID})
	if err != nil || current.Edited || current.Body != review.Body || !strings.EqualFold(current.Author.Login, cfg.BotLogin) {
		t.Fatalf("live review body or provenance changed: %v", err)
	}
	return review.Review
}
