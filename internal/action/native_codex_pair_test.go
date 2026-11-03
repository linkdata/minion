package action

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/usage"
)

func TestNativeCodexPairKeepsFixCriticReadOnly(t *testing.T) {
	root, receipts, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	day := time.Now().UTC().Format("2006/01/02")
	mainEvent := `{"type":"item.completed","item":{"type":"agent_message","text":` + strconv.Quote(proposalJSON("done", "")) + `}}`
	criticEvent := `{"type":"item.completed","item":{"type":"agent_message","text":` + strconv.Quote(approvedCritiqueJSON) + `}}`
	script := `#!/bin/sh
set -eu
receipts=` + shellQuote(receipts) + `
root=` + shellQuote(root) + `
role=main
for arg do
 [ "$arg" != 'sandbox_mode="read-only"' ] || role=critic
done
printf '%s\n' "$@" > "$receipts/$role.args"
printf '%s' "$PWD" > "$receipts/$role.cwd"
cat > "$receipts/$role.prompt"
previous=
for arg do
 if [ "$previous" = --output-schema ]; then cat "$arg" > "$receipts/$role.schema"; fi
 previous="$arg"
done
if [ "$role" = main ]; then
 id=11111111-1111-4111-8111-000000000001
 event=` + shellQuote(mainEvent) + `
 found=false
 for source in "$root"/minion-*; do
  [ -f "$source/source.txt" ] || continue
  printf candidate > "$source/source.txt"
  found=true
 done
else
 id=22222222-2222-4222-8222-000000000002
 event=` + shellQuote(criticEvent) + `
 found=false
 for source in "$root"/minion-review-*; do
  [ -f "$source/source.txt" ] || continue
  cat "$source/source.txt" > "$receipts/critic.read"
  found=true
 done
fi
[ "$found" = true ]
printf '%s' "$id" > "$receipts/$role.id"
dir="$CODEX_HOME/sessions/` + day + `"
mkdir -p "$dir"
cat > "$dir/rollout-$id.jsonl" <<EOF
{"type":"session_meta","payload":{"id":"$id","session_id":"$id","source":"exec"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"token_usage_record","payload":{"thread_id":"$id","session_id":"$id","turn_id":"turn-1","response_id":"response-1","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}
EOF
printf '{"type":"thread.started","thread_id":"%s"}\n' "$id"
printf '%s\n' "$event" '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'
`
	fakeOnPath(t, "codex", script)
	job := testJob(command.Fix)
	job.Main.Model, job.Main.Effort = "main-model", "high"
	job.Critic = &RoleSettings{Provider: "codex", Model: "critic-model", Effort: "medium"}
	job.CriticMaxRounds = 1
	result, err := (&Runner{WorkRoot: root}).Run(t.Context(), job)
	if err != nil || result.Answer != "done" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "candidate" || len(result.Calls) != 2 || result.Validate(job) != nil {
		t.Fatalf("Codex pair fix result=%+v error=%v", result, err)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(receipts, path))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	if read("main.id") == read("critic.id") || read("main.cwd") == read("critic.cwd") || read("critic.read") != "candidate" {
		t.Fatal("Codex roles shared native identity/runtime or critic missed frozen source")
	}
	mainArgs, criticArgs := read("main.args"), read("critic.args")
	if !strings.Contains(mainArgs, `sandbox_mode="workspace-write"`) || !strings.Contains(mainArgs, "--model\nmain-model\n") ||
		!strings.Contains(criticArgs, `sandbox_mode="read-only"`) || strings.Contains(criticArgs, "sandbox_workspace_write.writable_roots") ||
		!strings.Contains(criticArgs, "--model\ncritic-model\n") || read("main.schema") != mainOutputSchema || read("critic.schema") != criticOutputSchema {
		t.Fatalf("Codex role arguments or schemas mixed: main=%s critic=%s", mainArgs, criticArgs)
	}
	for i, role := range []string{roleMain, roleCritic} {
		call := result.Calls[i]
		if call.Role != role || call.Provider != "codex" || call.Tokens != (usage.Tokens{Input: 3, Output: 2}) || !call.Complete {
			t.Fatalf("Codex pair call%d attribution=%+v", i, call)
		}
		if _, err := os.Lstat(read(role + ".cwd")); !os.IsNotExist(err) {
			t.Fatalf("%s runtime survived cleanup: %v", role, err)
		}
	}
	if files, err := os.ReadDir(filepath.Join(codexHome, "sessions", filepath.FromSlash(day))); err != nil || len(files) != 0 {
		t.Fatalf("Codex rollouts survived cleanup: %v, %v", files, err)
	}
	assertEmptyDir(t, root)
}
