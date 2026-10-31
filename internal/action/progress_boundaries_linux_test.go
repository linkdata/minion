//go:build linux

package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestSelectedRoleCLIReportsCompletedChecks(t *testing.T) {
	for _, tc := range []struct {
		name, script, model, wantMessage string
		wantProgress                     int
	}{
		{name: "login failure", script: "#!/bin/sh\ncase \"$1 $2\" in 'login status') exit 7;; esac\nexit 99\n", wantMessage: preflightCodexLoginUnavailable},
		{name: "boundary failure", script: "#!/bin/sh\ncase \"$1 $2\" in 'login status') exit 0;; 'debug prompt-input') exit 7;; esac\nexit 99\n", wantMessage: preflightCodexBoundaryUnavailable, wantProgress: 1},
		{name: "model failure", script: preflightCodexCLI, model: "missing-model", wantMessage: preflightCodexInvocationUnsupported, wantProgress: 3},
		{name: "ready", script: preflightCodexCLI, wantProgress: 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("CODEX_ACCESS_TOKEN", "")
			fakeOnPath(t, "codex", tc.script)
			role := remoteCLIJob().Main
			if tc.model != "" {
				role.Model = tc.model
			}
			progress := 0
			message, err := checkSelectedRoleCLI(t.Context(), NewRemoteRunner(t.TempDir()), role, true, func() { progress++ })
			if progress != tc.wantProgress || message != tc.wantMessage || (err != nil) != (tc.wantMessage != "") {
				t.Fatalf("completed checks=%d, message=%q, error=%v; want %d, %q", progress, message, err, tc.wantProgress, tc.wantMessage)
			}
		})
	}
}

func TestAuditValidationReportsCompletedScripts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		findings, coverage int
		cancelAfterFirst   bool
		wantProgress       int
		wantFailedScript   string
	}{
		{name: "first fails", findings: 7, wantFailedScript: "validate-findings.cjs"},
		{name: "second fails", coverage: 7, wantProgress: 1, wantFailedScript: "validate-coverage-ledger.cjs"},
		{name: "both finish", wantProgress: 2},
		{name: "canceled after first", cancelAfterFirst: true, wantProgress: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *validate-findings.cjs*) exit %d;; *validate-coverage-ledger.cjs*) exit %d;; esac\nexit 99\n", tc.findings, tc.coverage)
			binary := fakeOnPath(t, "node", script)
			job := testJob(command.SecurityAudit)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			progress := 0
			err := validateAuditFiles(ctx, auditArtifactsFixture(job, "/source", "Audit report\n"), t.TempDir(), "/installed/SKILL.md",
				auditTools{binary: binary + "/node", permissionFlag: "--permission"}, job.Identity.Repository, "/source", job.Source.Head, func() {
					progress++
					if tc.cancelAfterFirst {
						cancel()
					}
				})
			if progress != tc.wantProgress {
				t.Fatalf("completed scripts=%d, want %d; error=%v", progress, tc.wantProgress, err)
			}
			if tc.cancelAfterFirst {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled validation error=%v", err)
				}
			} else if tc.wantFailedScript != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantFailedScript) {
					t.Fatalf("failed script error=%v, want %s", err, tc.wantFailedScript)
				}
			} else if err != nil {
				t.Fatalf("successful validation error=%v", err)
			}
		})
	}
}
