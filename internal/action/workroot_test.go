package action

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workroot"
)

func TestExecuteChecksWorkRootBeforeGitHub(t *testing.T) {
	e := &Orchestrator{
		CheckWorkRoot: func() error { return workroot.ErrUnsafe },
	}
	job := usageJob()
	job.Command = command.Command{Name: "review", Action: command.Review}
	selectFixtureRequest(t, e, &job, false)
	if err := e.Execute(t.Context(), job); !errors.Is(err, workroot.ErrUnsafe) {
		t.Fatalf("unsafe work root did not stop job before GitHub access: %v", err)
	}
}

func TestExecuteRechecksWorkRootAfterActionError(t *testing.T) {
	checks := 0
	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	e := &Orchestrator{
		GitHub: testGitHub(server, "fixture"),
		Logger: log.New(&logs, "", 0),
		CheckWorkRoot: func() error {
			checks++
			if checks == 2 {
				return workroot.ErrUnsafe
			}
			return nil
		},
	}
	job := usageJob()
	job.Command = command.Command{Name: "unsupported", Action: command.Comment}
	selectFixtureRequest(t, e, &job, false)
	err := e.Execute(t.Context(), job)
	if err == nil || checks != 2 || errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "is not enabled") || !strings.Contains(logs.String(), "work root after action failure:") {
		t.Fatalf("action failure after root filled = %v, checks=%d", err, checks)
	}
}
