package action

import (
	"context"
	"errors"
	"testing"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
)

func TestExecuteChecksWorkRootBeforeGitHub(t *testing.T) {
	e := &Executor{CheckWorkRoot: func() error { return workroot.ErrUnsafe }}
	if err := e.Execute(context.Background(), monitor.Job{}); !errors.Is(err, workroot.ErrUnsafe) {
		t.Fatalf("unsafe work root did not stop job before GitHub access: %v", err)
	}
}
