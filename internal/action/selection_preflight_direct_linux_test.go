//go:build linux

package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSelectionPreflightDirectChild(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{name: "ready"},
		{name: "selected CLI", want: "codex instruction boundary"},
		{name: "selected critic", want: "claude settings"},
		{name: "selected skill", want: "codex skill"},
		{name: "Git", want: "Git 2.45.1 or later"},
		{name: "audit tooling", want: "audit validator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := os.MkdirTemp(os.TempDir(), "minion-preflight-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Errorf("remove direct-child fixture: %v", err)
				}
			})
			for _, dir := range []string{"runtime", "work", "codex-home"} {
				if err := os.Mkdir(filepath.Join(base, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", base)
			t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
				t.Setenv(key, "")
			}
			writeActionTestSkills(t, base, ".agents", testReviewSkillURL, testAuditSkillURL)
			codexDir := fakeOnPath(t, "codex", preflightCodexCLI)
			switch tc.name {
			case "selected CLI":
				fakeOnPath(t, "codex", "#!/bin/sh\ncase \"$1 $2\" in 'login status') exit 0;; 'debug prompt-input') printf 'unsupported prompt-input\\n' >&2; exit 2;; esac\nexit 99\n")
			case "selected critic":
				t.Setenv("PATH", codexDir+string(os.PathListSeparator)+gitOnlyPath(t))
			case "selected skill":
				if err := os.Remove(filepath.Join(base, ".agents", "skills", "ponytail-review", "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "Git":
				git, err := aicli.AbsoluteExecutable("git")
				if err != nil {
					t.Fatal(err)
				}
				fakeOnPath(t, "git", "#!/bin/sh\ncase \" $* \" in *' --version '*) printf 'git version 2.45.0\\n'; exit 0;; esac\nexec "+shellQuote(git)+" \"$@\"\n")
			case "audit tooling":
				if err := os.Remove(filepath.Join(base, ".agents", "skills", "security-audit", "validate-findings.cjs")); err != nil {
					t.Fatal(err)
				}
			}
			worker, err := startLocalWorker(filepath.Join(base, "runtime"), "local", os.Args[0], []string{"-test.run=^TestSelectionPreflightDirectChildHelper$"}, func(cmd *exec.Cmd, socket string) {
				cmd.Env = append(os.Environ(), "MINION_PREFLIGHT_CHILD=1", "MINION_PREFLIGHT_CASE="+tc.name,
					"MINION_PREFLIGHT_SOCKET="+socket, "MINION_PREFLIGHT_BASE="+base)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := worker.Close(); err != nil && tc.want == "" && !t.Failed() {
					t.Errorf("close direct child: %v", err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			err = worker.awaitHeldPull(ctx)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "worker startup diagnostic") || ctx.Err() != nil || len(err.Error()) > 9<<10 {
					t.Fatalf("failed preflight diagnostic or held pull: error=%v", err)
				}
				for _, marker := range []string{"pull", "model"} {
					if _, statErr := os.Stat(filepath.Join(base, marker)); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("failed preflight reached %s: %v", marker, statErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("valid child did not hold a pull: %v", err)
			}
			if _, err := os.Stat(filepath.Join(base, "pull")); err != nil {
				t.Fatalf("child pulled before preflight marker: %v", err)
			}
			job := testJob(command.Comment)
			job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "high", []string{testReviewSkillURL}
			result, err := worker.Execute(ctx, job, runnerExecutionCheckpoints{})
			if err != nil || result.Answer != "Worker answer" || len(result.Calls) != 1 || result.Validate(job) != nil {
				t.Fatalf("preflighted child did not execute once: result=%+v error=%v", result, err)
			}
			select {
			case <-worker.exited:
			case <-ctx.Done():
				t.Fatalf("child did not exit after one assignment: %v", ctx.Err())
			}
			if worker.waitErr != nil {
				t.Fatalf("child exited after assignment: %v", worker.waitErr)
			}
			if data, err := os.ReadFile(filepath.Join(base, "model")); err != nil || string(data) != "1" {
				t.Fatalf("model call marker = %q, %v", data, err)
			}
		})
	}
}

func TestSelectionPreflightDirectChildHelper(t *testing.T) {
	if os.Getenv("MINION_PREFLIGHT_CHILD") != "1" {
		return
	}
	base := os.Getenv("MINION_PREFLIGHT_BASE")
	selection := WorkerSelection{Commands: map[string]config.CommandMapping{
		"comment": {Action: "comment", Skills: []string{testReviewSkillURL}},
	}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	if os.Getenv("MINION_PREFLIGHT_CASE") == "audit tooling" {
		selection.Commands["security-audit"] = config.CommandMapping{Action: "security-audit", Skills: []string{testAuditSkillURL}}
	}
	if os.Getenv("MINION_PREFLIGHT_CASE") == "selected critic" {
		selection.Critic = &RoleSettings{Provider: "claude", Model: "critic-1", Effort: "medium"}
	}
	runner, err := NewSelectedRunner(Runner{WorkRoot: filepath.Join(base, "work"), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		if err := workspace.WriteAtomic(filepath.Join(base, "model"), []byte("1")); err != nil {
			return aicli.Result{}, err
		}
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	}}, selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(t.Context()); err != nil {
		fmt.Fprintln(os.Stderr, "preflight:", err)
		t.Fatalf("preflight: %v", err)
	}
	if err := workspace.WriteAtomic(filepath.Join(base, "pull"), []byte("ready")); err != nil {
		t.Fatal(err)
	}
	client, err := NewUnixExecutionClient(os.Getenv("MINION_PREFLIGHT_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if err := RunHTTP(t.Context(), client, UnixExecutionBaseURL, "local", runner); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightSelectionRedactsNativeCredentialFromCLIError(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("CODEX_HOME", t.TempDir())
	const secret = "native-preflight-credential"
	t.Setenv("CODEX_API_KEY", secret)
	script := strings.Replace(preflightCodexCLI, "  app-server)\n", "  app-server)\n    printf '%s\\n' \"$CODEX_API_KEY\" >&2\n    exit 7\n", 1)
	fakeOnPath(t, "codex", script)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("native credential escaped preflight error redaction: %v", err)
	}
}

const preflightCodexCLI = `#!/bin/sh
case "$1 $2" in
  'login status') exit 0 ;;
  'debug prompt-input')
    case " $* " in
      *' skills.include_instructions=true '*) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78 MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"}]}]' ;;
      *) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78"}]}]' ;;
    esac
    exit 0 ;;
esac
case "$1" in
  app-server)
    case " $* " in
      *' minion_unsupported_capability_probe=true '*) printf '%s\n' 'unknown config key: minion_unsupported_capability_probe' >&2; exit 2 ;;
    esac
    IFS= read -r request || exit 0
    printf '%s\n' '{"id":1,"result":{}}'
    IFS= read -r request || exit 1
    IFS= read -r request || exit 1
    printf '%s\n' '{"id":2,"result":{"data":[{"model":"main-1","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}'
    exec /bin/cat >/dev/null ;;
  exec)
    case " $* " in
      *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unexpected argument --minion-unsupported-capability-probe' >&2; exit 2 ;;
    esac
    case "$2" in
      resume) printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]' ;;
      *) printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]' ;;
    esac
    exit 0 ;;
esac
exit 99
`
