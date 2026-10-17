package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
)

func TestExecuteChecksWorkRootBeforeGitHub(t *testing.T) {
	e := &Orchestrator{
		CheckWorkRoot: func() error { return workroot.ErrUnsafe },
	}
	if err := e.Execute(context.Background(), monitor.Job{}); !errors.Is(err, workroot.ErrUnsafe) {
		t.Fatalf("unsafe work root did not stop job before GitHub access: %v", err)
	}
}

func TestExecuteRechecksWorkRootAfterActionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			_, _ = fmt.Fprint(w, `{"number":1}`)
		case "/repos/o/r/issues/1/comments":
			_, _ = fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected GitHub request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testGitHub(server, "test-token")
	checks := 0
	var logs bytes.Buffer
	e := &Orchestrator{
		GitHub: client,
		Logger: log.New(&logs, "", 0),
		CheckWorkRoot: func() error {
			checks++
			if checks == 2 {
				return workroot.ErrUnsafe
			}
			return nil
		},
	}
	job := monitor.Job{Repository: config.Repository{Name: "o/r"}, SubjectType: "Issue", Number: 1, Command: command.Command{Action: "unsupported"}}
	err := e.Execute(context.Background(), job)
	if checks != 2 || errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "is not enabled") || !strings.Contains(logs.String(), "work root after action failure:") {
		t.Fatalf("action failure after root filled = %v, checks=%d", err, checks)
	}
}
