package codexcli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

func TestInstructionProbePreservesOutputLimit(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nhead -c 4194305 /dev/zero\nexec sleep 30\n")
	if err := ValidateInstructionBoundary(t.Context()); !errors.Is(err, aicli.ErrOutputLimit) {
		t.Fatalf("probe limit cause changed: %v", err)
	}
}

func TestInstructionProbeRejectsRelativeGitExecutable(t *testing.T) {
	fakeCodex := installFakeCodexUnchecked(t, "#!/bin/sh\nexit 0\n")
	root := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Mkdir("relbin", 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "git-ran")
	if err := os.WriteFile(filepath.Join("relbin", "git"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODEBUG", "execerrdot=0")
	t.Setenv("PATH", "relbin"+string(os.PathListSeparator)+filepath.Dir(fakeCodex))
	if err := ValidateInstructionBoundary(t.Context()); err == nil || !strings.Contains(err.Error(), "git binary must have an absolute path") {
		t.Fatalf("relative Git executable accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative Git executable ran: %v", err)
	}
}

func TestValidateInstructionBoundaryPassesIsolationSettings(t *testing.T) {
	fakeCodex := installFakeCodex(t, `#!/bin/sh
[ "${CODEX_HOME+x}" = x ] && [ "$CODEX_HOME" = "$HOME" ] || exit 4
pwd > "$0.cwd"
printf '%s\n' "$@" >> "$0.args"
printf 'END\n' >> "$0.args"
if test -f "$PWD/.minion-codex-root"; then
  test -f "$PWD/AGENTS.md" || exit 3
  test -f "$PWD/../../../AGENTS.md" || exit 3
  test -f "$PWD/../../../.agents/skills/untrusted-canary/SKILL.md" || exit 3
  test -d "$PWD/../../../.git" || exit 3
  printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78"}]}]'
else
  test -f "$PWD/AGENTS.md" || exit 3
  test -f "$PWD/.agents/skills/untrusted-canary/SKILL.md" || exit 3
  test -d "$PWD/.git" || exit 3
  printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78 MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"}]}]'
fi
`)
	if err := ValidateInstructionBoundary(t.Context()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(fakeCodex + ".args")
	if err != nil {
		t.Fatal(err)
	}
	wantControl := []string{
		"debug", "prompt-input",
		"-c", "skills.include_instructions=true",
		probePrompt,
	}
	wantProduction := []string{
		"debug", "prompt-input",
		"-c", "project_doc_max_bytes=0",
		"-c", `project_root_markers=[".minion-codex-root"]`,
		"-c", "skills.include_instructions=false",
		probePrompt,
	}
	wantDiscovery := []string{
		"debug", "prompt-input",
		"-c", "project_doc_max_bytes=0",
		"-c", `project_root_markers=[".minion-codex-root"]`,
		probePrompt,
	}
	want := strings.Join(wantControl, "\n") + "\nEND\n" + strings.Join(wantProduction, "\n") + "\nEND\n" + strings.Join(wantDiscovery, "\n") + "\nEND\n"
	if string(args) != want {
		t.Fatalf("Codex debug args = %q, want %q", args, want)
	}
	cwd, err := os.ReadFile(fakeCodex + ".cwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(cwd)), "/repo/var/work/runtime") {
		t.Fatalf("Codex debug cwd = %q, want probe runtime under repository", cwd)
	}
}

func TestValidateInstructionBoundaryRejectsUntrustedContent(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
		role string
	}{
		{role: "user", name: "agents", text: probePrompt + " " + probeAgent, want: "loaded untrusted instructions"},
		{role: "user", name: "runtime agents", text: probePrompt + " " + probeRuntimeAgent, want: "loaded untrusted instructions"},
		{role: "user", name: "skill", text: probePrompt + " " + probeSkill, want: "loaded untrusted instructions"},
		{role: "user", name: "missing prompt", text: "unrelated", want: "omitted the trusted"},
		{role: "system", name: "trusted prompt only in system role", text: probePrompt, want: "omitted the trusted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := json.Marshal([]map[string]any{{
				"role": test.role, "content": []map[string]string{{"text": test.text}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			installFakeCodex(t, "#!/bin/sh\nif test -f \"$PWD/.minion-codex-root\"; then\ncat <<'EOF'\n"+string(output)+"\nEOF\nelse\nprintf '%s\\n' '[{\"role\":\"user\",\"content\":[{\"text\":\"MINION_TRUSTED_PROMPT_78 MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78\"}]}]'\nfi\n")
			if err := ValidateInstructionBoundary(t.Context()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateInstructionBoundary() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInstalledCodexInstructionBoundary(t *testing.T) {
	if os.Getenv("MINION_LIVE_CODEX_BOUNDARY") != "1" {
		t.Skip("set MINION_LIVE_CODEX_BOUNDARY=1 for installed-CLI verification")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("installed Codex CLI is unavailable")
	}
	if err := ValidateInstructionBoundary(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionBoundaryRejectsVacuousOrInvalidControl(t *testing.T) {
	for _, test := range []struct{ name, output, want string }{
		{"missing canaries", `[{"role":"user","content":[{"text":"` + probePrompt + `"}]}]`, "cannot observe repository instructions"},
		{"invalid JSON", probePrompt + " " + probeAgent + " " + probeSkill, "decode Codex instruction probe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+test.output+"\nEVENTS\n")
			if err := ValidateInstructionBoundary(t.Context()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("vacuous control accepted: %v", err)
			}
		})
	}
}
