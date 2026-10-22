package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
)

func TestCappedPublicationProjectsEveryPublicBodyAndRetainedAudit(t *testing.T) {
	for _, action := range []command.Action{command.Review, command.Comment, command.SecurityAudit, command.Fix} {
		for _, verdict := range []string{"full blockers", "verification blockers with full approval", "only nits"} {
			t.Run(string(action)+"/"+verdict, func(t *testing.T) {
				job := withClaudeCritic(testJob(action))
				job.Identity.Repository = "o/r"
				job.CriticMaxRounds = 2
				result := cappedResultFixture(job)
				const body = "Reviewed body\n@otherbot fix\n![image](https://example.invalid/image)\n"
				result.Answer = body
				if action == command.SecurityAudit {
					result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), body)
				}
				result.Refutations = []Refutation{{Round: 2, Text: "PRIVATE_MAIN_REFUTATION"}}
				if verdict == "verification blockers with full approval" {
					last := &result.Calls[4]
					last.Approved, last.NoBlockers, last.HasBlockers = true, true, false
				}
				if verdict == "only nits" {
					for _, i := range []int{3, 4} {
						result.Calls[i].NoBlockers, result.Calls[i].HasBlockers = true, false
					}
				}
				if err := result.Validate(job); err != nil {
					t.Fatalf("invalid capped publication baseline: %v", err)
				}
				before, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var posted string
				writes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
						_, _ = fmt.Fprint(w, publicRepositoryJSON)
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
						_, _ = fmt.Fprintf(w, `{"head":{"sha":%q}}`, job.Source.Head)
					case r.Method == http.MethodPost && r.URL.Path == "/graphql":
						_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Task","bodyText":"Original task","lastEditedAt":null}}}`)
					case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
						var input struct {
							Body, Event string
							CommitID    string `json:"commit_id"`
						}
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							t.Error(err)
						}
						if input.Event != "COMMENT" || input.CommitID != job.Source.Head {
							t.Errorf("capped result changed review state or commit: %+v", input)
						}
						posted = input.Body
						writes++
						_, _ = fmt.Fprint(w, `{"id":1}`)
					case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
						captureComment(t, w, r, &posted)
						writes++
					default:
						t.Errorf("unexpected publication request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				request := monitor.Job{ID: job.Identity.JobID, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: job.Identity.SubjectType, Number: 7,
					Source: github.Item{Author: github.User{ID: job.Identity.RequesterID, Login: job.Identity.Requester}, UpdatedAt: time.Now()}, Command: command.Command{Name: job.Command, Action: action}}
				root := t.TempDir()
				orchestrator := Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token")}
				if err := orchestrator.publish(context.Background(), &preparedAction{job: job, request: request, issueNodeID: "issue-node"}, result); err != nil {
					t.Fatal(err)
				}
				if writes != 1 || strings.Count(posted, "Critic round limit reached (2).") != 1 || !strings.Contains(posted, "Normal critic acceptance was not reached; unresolved findings may remain.") || strings.Contains(posted, "PRIVATE") || !strings.Contains(posted, "\n> @otherbot fix") || !strings.Contains(posted, "\n> ![image]") || !strings.HasSuffix(posted, "\n\n"+request.Marker()) {
					t.Fatalf("capped public response lost caveat, body or inert rendering: %q", posted)
				}
				if strings.Contains(posted, "reported blockers.") != (verdict != "only nits") || strings.Contains(posted, "fresh full critic reported approval") != (verdict == "verification blockers with full approval") || strings.Contains(posted, "fresh full critic did not approve") != (verdict != "verification blockers with full approval") {
					t.Fatalf("public caveat falsified final phase verdicts: %s", posted)
				}
				if action == command.Fix && !strings.HasPrefix(posted, "AI returned a response without changing files.\n\n") {
					t.Fatalf("capped no-change fix claimed task completion: %q", posted)
				}
				if _, err := command.ParseForLoginCommands(posted, "otherbot", defaultActionTestCommands); !errors.Is(err, command.ErrNoCommand) {
					t.Fatalf("capped publication activated model command: %v", err)
				}
				if action == command.SecurityAudit {
					entries, err := os.ReadDir(filepath.Join(root, "audits"))
					if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "complete-") {
						t.Fatalf("completed projected audit missing: %v, %v", entries, err)
					}
					for _, file := range result.Artifacts {
						data, err := os.ReadFile(filepath.Join(root, "audits", entries[0].Name(), file.Path))
						if err != nil {
							t.Fatal(err)
						}
						if file.Path == "REPORT.md" {
							if !strings.HasSuffix(string(data), body) || !strings.Contains(posted, inertModelOutput(string(data))) {
								t.Fatalf("public answer and retained report disagree: report=%q body=%q", data, posted)
							}
						} else if !bytes.Equal(data, file.Data) {
							t.Fatalf("projection changed audit companion %s", file.Path)
						}
					}
				}
				after, err := json.Marshal(result)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("publication mutated raw reviewed Result: error=%v", err)
				}
				if saved := readPrivateReview(t, root); saved.CriticStopReason != "round_cap" || saved.CriticMaxRounds != 2 || !slices.Equal(saved.Calls, result.Calls) {
					t.Fatalf("public projection replaced raw evidence: %+v", saved)
				}
			})
		}
	}
}

func TestCappedPublicationRefusesInvalidRawResultAndProjectionOverflow(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.SecurityAudit} {
		for _, failure := range []string{"invalid raw", "projected overflow"} {
			t.Run(string(action)+"/"+failure, func(t *testing.T) {
				job := withClaudeCritic(testJob(action))
				job.CriticMaxRounds = 1
				result := cappedResultFixture(job)
				result.Answer = strings.Repeat("x", maxOutputBytes-14)
				if action == command.SecurityAudit {
					result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
				}
				if err := result.Validate(job); err != nil {
					t.Fatalf("overflow baseline must satisfy raw gates: %v", err)
				}
				if failure == "invalid raw" {
					if action == command.SecurityAudit {
						result.Answer = "Different unreviewed audit body"
					} else {
						result.Answer = ""
					}
				}
				before, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				writes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writes++
					t.Errorf("invalid capped result reached GitHub: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected write", http.StatusInternalServerError)
				}))
				defer server.Close()
				root := t.TempDir()
				err = (&Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token")}).publish(context.Background(), &preparedAction{job: job}, result)
				wantError := "invalid action result"
				if failure == "projected overflow" {
					wantError = "publication projection"
				}
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), wantError) || writes != 0 {
					t.Fatalf("invalid cap became publishable or changed cause: %v writes=%d", err, writes)
				}
				if entries, err := os.ReadDir(filepath.Join(root, "audits")); err == nil {
					for _, entry := range entries {
						if strings.HasPrefix(entry.Name(), "complete-") {
							t.Fatalf("invalid projection retained a completed audit: %s", entry.Name())
						}
					}
				}
				after, err := json.Marshal(result)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refused projection mutated or truncated reviewed body")
				}
			})
		}
	}
}
