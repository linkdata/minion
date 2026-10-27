package action

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestPublishRecoveredFixRejectsUnchangedReviewedFile(t *testing.T) {
	base, _, _ := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before\n")})
	root := t.TempDir()
	checkout, err := gitwork.Clone(t.Context(), root, "o/r", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Head != base {
		t.Fatalf("source head changed: got=%s want=%s", snapshot.Head, base)
	}
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	job.Source = snapshot
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments" {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		if serveFixFlowFixture(t, w, r) {
			return
		}
		writes++
		t.Errorf("unchanged fix caused GitHub write: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrTerminalJob) ||
		!strings.Contains(err.Error(), "staged paths do not match reviewed changes") || writes != 0 {
		t.Fatalf("unchanged fix was not terminal: writes=%d error=%v", writes, err)
	}
}
