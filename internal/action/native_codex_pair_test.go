package action

import (
	"errors"
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
	message := func(answer string) string {
		return `{"type":"item.completed","item":{"type":"agent_message","text":` + strconv.Quote(answer) + `}}`
	}
	firstMain := message(proposalJSON("first", ""))
	lastMain := message(proposalJSON("done", ""))
	firstCritic := message(`{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Revise the candidate."}`)
	lastCritic := message(approvedCritiqueJSON)
	script := `#!/bin/sh
set -eu
receipts=` + shellQuote(receipts) + `
root=` + shellQuote(root) + `
role=main
for arg do
 [ "$arg" != 'sandbox_mode="read-only"' ] || role=critic
done
count="$receipts/$role.count"
n=1
[ ! -f "$count" ] || n=$(( $(cat "$count") + 1 ))
printf '%s' "$n" > "$count"
mode=fresh
if [ "$1" = exec ] && [ "$2" = resume ]; then mode=resume; fi
requested=
positional=false
for arg do
 if $positional && [ -z "$requested" ] && [ "$arg" != - ]; then requested="$arg"; fi
 [ "$arg" != -- ] || positional=true
done
printf '%s\n' "$@" > "$receipts/$role.$n.args"
printf '%s' "$PWD" > "$receipts/$role.$n.cwd"
cat > "$receipts/$role.$n.prompt"
previous=
for arg do
 if [ "$previous" = --output-schema ]; then cat "$arg" > "$receipts/$role.$n.schema"; fi
 previous="$arg"
done
if [ "$role" = main ]; then
 id=11111111-1111-4111-8111-000000000001
 if [ "$n" -eq 1 ]; then event=` + shellQuote(firstMain) + `; else event=` + shellQuote(lastMain) + `; fi
 input=3
 output=2
 found=false
 for source in "$root"/minion-*; do
  [ -f "$source/source.txt" ] || continue
  printf 'candidate %s' "$n" > "$source/source.txt"
  found=true
 done
else
 id=22222222-2222-4222-8222-000000000002
 if [ "$n" -eq 3 ]; then id=33333333-3333-4333-8333-000000000003; fi
 if [ "$n" -eq 1 ]; then event=` + shellQuote(firstCritic) + `; else event=` + shellQuote(lastCritic) + `; fi
 input=7
 output=5
 found=false
 for source in "$root"/minion-review-*; do
  [ -f "$source/source.txt" ] || continue
  cat "$source/source.txt" > "$receipts/critic.$n.read"
  printf '%s' "$source" > "$receipts/critic.$n.source"
  found=true
 done
fi
[ "$found" = true ]
if [ "$n" -eq 2 ]; then
 [ "$mode" = resume ] && [ "$requested" = "$id" ]
else
 [ "$mode" = fresh ] && [ -z "$requested" ]
fi
printf '%s' "$id" > "$receipts/$role.$n.id"
dir="$CODEX_HOME/sessions/` + day + `"
mkdir -p "$dir"
if [ "$mode" = fresh ]; then cat > "$dir/rollout-$id.jsonl" <<EOF
{"type":"session_meta","payload":{"id":"$id","session_id":"$id","source":"exec"}}
EOF
fi
cat >> "$dir/rollout-$id.jsonl" <<EOF
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-$role-$n"}}
{"type":"token_usage_record","payload":{"thread_id":"$id","session_id":"$id","turn_id":"turn-$role-$n","response_id":"response-$role-$n","usage":{"input_tokens":$input,"cached_input_tokens":0,"output_tokens":$output}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-$role-$n"}}
EOF
printf '{"type":"thread.started","thread_id":"%s"}\n' "$id"
printf '%s\n' "$event"
printf '{"type":"turn.completed","usage":{"input_tokens":%s,"cached_input_tokens":0,"output_tokens":%s}}\n' "$input" "$output"
`
	fakeOnPath(t, "codex", script)
	job := testJob(command.Fix)
	job.Main.Model, job.Main.Effort = "main-model", "high"
	job.Critic = &RoleSettings{Provider: "codex", Model: "critic-model", Effort: "medium"}
	job.CriticMaxRounds = 2
	result, err := (&Runner{WorkRoot: root}).Run(t.Context(), job)
	if err != nil || result.Answer != "done" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "candidate 2" || len(result.Calls) != 5 || result.Validate(job) != nil {
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
	roleRead := func(role string, call int, kind string) string {
		return read(role + "." + strconv.Itoa(call) + "." + kind)
	}
	if read("main.count") != "2" || read("critic.count") != "3" ||
		roleRead(roleMain, 1, "id") != roleRead(roleMain, 2, "id") ||
		roleRead(roleCritic, 1, "id") != roleRead(roleCritic, 2, "id") ||
		roleRead(roleCritic, 1, "id") == roleRead(roleCritic, 3, "id") ||
		roleRead(roleMain, 1, "id") == roleRead(roleCritic, 1, "id") ||
		roleRead(roleMain, 1, "cwd") == roleRead(roleCritic, 1, "cwd") ||
		roleRead(roleMain, 1, "cwd") != roleRead(roleMain, 2, "cwd") ||
		roleRead(roleCritic, 1, "cwd") != roleRead(roleCritic, 2, "cwd") ||
		roleRead(roleCritic, 1, "cwd") != roleRead(roleCritic, 3, "cwd") {
		t.Fatal("Codex pair lost separate native identities or same-role continuation")
	}
	for n, want := range []string{"candidate 1", "candidate 2", "candidate 2"} {
		call := n + 1
		source := roleRead(roleCritic, call, "source")
		if roleRead(roleCritic, call, "read") != want ||
			!strings.Contains(roleRead(roleCritic, call, "prompt"), "Source snapshot: "+strconv.Quote(source)) {
			t.Fatalf("critic call%d did not read its prompted frozen source", call)
		}
	}
	for _, test := range []struct {
		role                        string
		calls                       int
		mode, model, effort, schema string
	}{
		{roleMain, 2, `sandbox_mode="workspace-write"`, "main-model", "high", mainOutputSchema},
		{roleCritic, 3, `sandbox_mode="read-only"`, "critic-model", "medium", criticOutputSchema},
	} {
		for call := 1; call <= test.calls; call++ {
			args := roleRead(test.role, call, "args")
			if !strings.Contains(args, test.mode) || !strings.Contains(args, "--model\n"+test.model+"\n") ||
				!strings.Contains(args, "model_reasoning_effort="+strconv.Quote(test.effort)) ||
				test.role == roleCritic && strings.Contains(args, "sandbox_workspace_write.writable_roots") ||
				roleRead(test.role, call, "schema") != test.schema {
				t.Fatalf("%s call%d arguments or schema mixed: %s", test.role, call, args)
			}
		}
		if _, err := os.Lstat(roleRead(test.role, 1, "cwd")); !os.IsNotExist(err) {
			t.Fatalf("%s runtime survived cleanup: %v", test.role, err)
		}
	}
	for i, role := range []string{roleMain, roleCritic, roleMain, roleCritic, roleCritic} {
		want := usage.Tokens{Input: 3, Output: 2}
		if role == roleCritic {
			want = usage.Tokens{Input: 7, Output: 5}
		}
		call := result.Calls[i]
		if call.Role != role || call.Provider != "codex" || call.Tokens != want || !call.Complete {
			t.Fatalf("Codex pair call%d attribution=%+v", i, call)
		}
	}
	if files, err := os.ReadDir(filepath.Join(codexHome, "sessions", filepath.FromSlash(day))); err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && len(files) != 0 {
		t.Fatalf("Codex rollouts survived cleanup: %v, %v", files, err)
	}
	assertEmptyDir(t, root)
}
