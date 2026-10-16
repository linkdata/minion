package action

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
)

func TestRunnerRequiresBackgroundResultsBeforeFinalResponse(t *testing.T) {
	for _, critic := range []bool{false, true} {
		name := "without critic"
		if critic {
			name = "with critic"
		}
		t.Run(name, func(t *testing.T) {
			job := portableJob(command.Comment)
			if critic {
				job.Critic = &RoleSettings{Provider: "claude"}
			}
			var roles []string
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				roles = append(roles, request.role)
				wait := "Await all delegated work and background checks before your final response"
				if request.role == "critic" {
					wait = "Await every delegated review and background check before your final verdict"
				}
				for _, want := range []string{wait, "inspect their results and handle failures instead of ending with a waiting or progress message"} {
					if !strings.Contains(request.prompt, want) {
						t.Fatalf("%s prompt omits background completion requirement %q", request.role, want)
					}
				}
				if request.role == "critic" {
					return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
				}
				answer := "completed response"
				if critic {
					answer = proposalJSON(answer, "")
				}
				return aicli.Result{Answer: answer}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantRoles := []string{"main"}
			if critic {
				wantRoles = append(wantRoles, "critic")
			}
			if err != nil || result.Answer != "completed response" || !slices.Equal(roles, wantRoles) {
				t.Fatalf("background instruction run: answer=%q roles=%v error=%v", result.Answer, roles, err)
			}
		})
	}
}
