//go:build !windows

package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestExecuteModeOfflineChild(t *testing.T) {
	if os.Getenv("MINION_TEST_EXECUTE_CHILD") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-mode=execute"}
	os.Exit(run())
}

func offlineModeWorkerPath(t *testing.T, binary string) string {
	t.Helper()
	t.Setenv("MINION_TEST_EXECUTE_CHILD", "1")
	worker := filepath.Join(t.TempDir(), "test-worker")
	script := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = '-mode=execute' ] || exit 98\nexec " + aicli.ShellQuote(binary) + " '-test.run=^TestExecuteModeOfflineChild$'\n"
	if err := os.WriteFile(worker, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return worker
}

func offlineModeGitProxy(t *testing.T, realGit, rewrite string) {
	t.Helper()
	const denyRemote = " -c protocol.https.allow=never -c protocol.http.allow=never -c protocol.ssh.allow=never -c protocol.git.allow=never"
	testsupport.FakeOnPath(t, "git", "#!/bin/sh\nexec "+aicli.ShellQuote(realGit)+denyRemote+" -c "+aicli.ShellQuote("include.path="+rewrite)+" \"$@\"\n")
}

func offlineModeCodex(t *testing.T, modelCalls string) {
	t.Helper()
	testsupport.FakeOnPath(t, "codex", offlineModeCodexScript(modelCalls))
}

func offlineModeCodexScript(modelCalls string) string {
	return `#!/bin/sh
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
    case " $* " in *' minion_unsupported_capability_probe=true '*) printf '%s\n' 'unknown config key: minion_unsupported_capability_probe' >&2; exit 2 ;; esac
    IFS= read -r request || exit 0
    printf '%s\n' '{"id":1,"result":{}}'
    IFS= read -r request || exit 1
    IFS= read -r request || exit 1
    printf '%s\n' '{"id":2,"result":{"data":[{"model":"main-1","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}'
    exec /bin/cat >/dev/null ;;
  exec)
    case " $* " in
      *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unexpected argument --minion-unsupported-capability-probe' >&2; exit 2 ;;
      *' --help '*)
        case "$2" in
          resume) printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]' ;;
          *) printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]' ;;
        esac
        exit 0 ;;
    esac
    cat >/dev/null
    printf 'model\n' >> ` + aicli.ShellQuote(modelCalls) + `
    printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"No findings."}}' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'
    exit 0 ;;
esac
exit 99
`
}
