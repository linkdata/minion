package codexcli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func TestCollectUsageFreshHelperEvidenceWithRootFallback(t *testing.T) {
	for _, tc := range []struct {
		name, child string
		expected    []string
		want        usage.Tokens
		complete    bool
	}{
		{name: "unreferenced completed control", child: "complete", want: usage.Tokens{Input: 30, Output: 3}, complete: true},
		{name: "unreferenced unfinished child", child: "unfinished", want: usage.Tokens{Input: 30, Output: 3}},
		{name: "missing referenced child", expected: []string{"child"}, want: usage.Tokens{Input: 10, Output: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			if tc.child != "" {
				events := []string{usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2)}
				if tc.child == "complete" {
					events = append(events, usageEvent("task_complete", "child-turn"))
				}
				writeUsageRollout(t, home, "child", "root", events...)
			}
			fallback := usage.Tokens{Input: 10, Output: 1}
			got, complete, err := (&usageSession{}).collectUsage(context.Background(), home, "root", time.Now(), &fallback, tc.expected, nil)
			wantUnverified := !tc.complete
			if got != tc.want || complete != tc.complete || errors.Is(err, errUnverifiedChildren) != wantUnverified || (err != nil) != wantUnverified {
				t.Fatalf("fresh helper evidence: tokens=%+v complete=%t error=%v", got, complete, err)
			}
		})
	}
}
