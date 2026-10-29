package action

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestPublishRecoveredFixRejectsUnchangedReviewedFile(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments" {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		if serveFixFlowFixture(t, w, r) {
			return
		}
		writes.Add(1)
		t.Errorf("unchanged fix caused GitHub write: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotID: 8, BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrTerminalJob) ||
		!strings.Contains(err.Error(), "staged paths do not match reviewed changes") || writes.Load() != 0 {
		t.Fatalf("unchanged fix was not terminal: writes=%d error=%v", writes.Load(), err)
	}
}
