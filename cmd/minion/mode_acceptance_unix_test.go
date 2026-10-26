//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/testsupport"
)

// The parent supplies a GitHub command and repository, then publishes the
// answer returned by a real, separately spawned public execute-mode child.
// Losing the local completion record after publication must not repeat either
// the model call or the GitHub write when the service starts again.
func TestExecuteModeOfflinePublicationAndRestart(t *testing.T) {
	offlineModeGitRemote(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	for _, dir := range []string{home, filepath.Join(base, "codex-home"), filepath.Join(base, "work")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
	t.Setenv("MINION_TEST_EXECUTE_CHILD", "1")
	modelCalls := filepath.Join(base, "model-calls")
	testsupport.FakeOnPath(t, "codex", offlineModeCodexScript(modelCalls))
	now := time.Date(2026, 10, 9, 8, 1, 0, 0, time.UTC)
	requestAt := now.Add(-time.Second)
	request := github.Comment{ID: 42, NodeID: "request-node", Body: "@linkdataminion comment",
		User: github.User{ID: 7, Login: "writer"}, CreatedAt: requestAt, UpdatedAt: requestAt,
		HTMLURL: "https://github.com/o/r/issues/7#issuecomment-42"}
	var mu sync.Mutex
	var published []github.Comment
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer offline-token" {
			t.Errorf("unexpected authorization for %s %s", r.Method, r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/notifications":
			since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
			if err != nil {
				t.Errorf("notification since: %v", err)
				http.Error(w, "invalid since", http.StatusBadRequest)
				return
			}
			if since.After(requestAt) {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_ = json.NewEncoder(w).Encode([]github.Notification{{ID: "notification-7",
				Repository: github.Repository{ID: 1, FullName: "o/r", Owner: github.User{ID: 2, Login: "o"}},
				Subject:    github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: requestAt}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"title":"Issue","body":"Describe the source"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			mu.Lock()
			comments := append([]github.Comment{request}, published...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode publication: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if !strings.Contains(input.Body, "No findings.") || !strings.Contains(input.Body, "<!-- linkdataminion:") {
				t.Errorf("publication lacks model answer or marker: %q", input.Body)
			}
			mu.Lock()
			published = append(published, github.Comment{ID: 99, NodeID: "reply-node", Body: input.Body,
				User: github.User{ID: 8, Login: "linkdataminion"}, CreatedAt: requestAt, UpdatedAt: requestAt})
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"id":99}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var input struct {
				Query     string `json:"query"`
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode GraphQL: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			switch {
			case strings.Contains(input.Query, "addReaction") && input.Variables.ID == request.NodeID:
				_, _ = fmt.Fprint(w, `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`)
			case strings.Contains(input.Query, "... on Comment") && input.Variables.ID == request.NodeID:
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
					"body": request.Body, "author": map[string]any{"login": "writer", "id": 7}}}})
			case strings.Contains(input.Query, "... on Comment") && input.Variables.ID == "reply-node":
				mu.Lock()
				if len(published) != 1 {
					mu.Unlock()
					t.Errorf("reply provenance requested after %d publications", len(published))
					http.Error(w, "missing reply", http.StatusNotFound)
					return
				}
				body := published[0].Body
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
					"body": body, "author": map[string]any{"login": "linkdataminion", "id": 8}}}})
			default:
				t.Errorf("unexpected GraphQL query for %q: %q", input.Variables.ID, input.Query)
				http.Error(w, "unexpected GraphQL request", http.StatusBadRequest)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/collaborators/writer/permission":
			_, _ = fmt.Fprint(w, `{"permission":"write","user":{"login":"writer","id":7}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("offline-token")
	client.HTTPClient, client.BaseURL = server.Client(), server.URL
	commands := map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{WorkRoot: filepath.Join(base, "work"), StatePath: filepath.Join(base, "state.json"),
		BotLogin: "linkdataminion", Main: "codex", Codex: config.CodexConfig{Model: "main-1", ReasoningEffort: "high"},
		PollIntervalSeconds: 60, Commands: commands,
		Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"},
			AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}}
	store, err := state.Open(cfg.StatePath, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	runtimeParent, err := os.MkdirTemp("", "m239-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(runtimeParent); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("TMPDIR", runtimeParent)
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(base, "test-worker")
	script := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = '-mode=execute' ] || exit 98\nexec " + aicli.ShellQuote(binary) + " '-test.run=^TestExecuteModeOfflineChild$'\n"
	if err := os.WriteFile(worker, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runPoll := func(at time.Time) {
		t.Helper()
		var logs bytes.Buffer
		logger := log.New(&logs, "", 0)
		owner := newOrchestrator(cfg, client, store, logger, nil)
		watcher := &monitor.Monitor{Config: cfg, GitHub: client, Handler: owner, Now: func() time.Time { return at }}
		if code := runOrchestrateMode(t.Context(), owner, watcher, store, false, logger, worker, 10*time.Second); code != 0 {
			t.Fatalf("orchestrate poll returned %d: %s", code, logs.String())
		}
		entries, err := os.ReadDir(runtimeParent)
		if err != nil || len(entries) != 0 {
			t.Fatalf("owned runtime directory was not cleaned: entries=%v error=%v", entries, err)
		}
	}
	runPoll(now)
	assertOfflineModeCounts(t, modelCalls, &mu, &published, 1)
	if len(store.Data.Processed) != 1 {
		t.Fatalf("first poll processed = %v", store.Data.Processed)
	}
	// GitHub accepted the reply, but the process stopped before retaining its
	// final local completion checkpoint. Restart with a new child and state lock.
	for key := range store.Data.Processed {
		delete(store.Data.Processed, key)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(cfg.StatePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	runPoll(now.Add(time.Minute))
	assertOfflineModeCounts(t, modelCalls, &mu, &published, 1)
	if len(store.Data.Processed) != 1 {
		t.Fatalf("restart did not recover completion: %v", store.Data.Processed)
	}
}

func TestExecuteModeOfflineChild(t *testing.T) {
	if os.Getenv("MINION_TEST_EXECUTE_CHILD") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-mode=execute"}
	os.Exit(run())
}

func TestExecuteModeRejectsServiceFlagsBeforeStartup(t *testing.T) {
	for _, args := range [][]string{
		{"-mode=execute", "-config=unused.json"},
		{"-mode=execute", "-loop"},
		{"-mode=execute", "-check"},
		{"-mode=execute", "-install-skills"},
		{"-mode=unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			oldFlags, oldArgs := flag.CommandLine, os.Args
			defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
			flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
			os.Args = append([]string{"minion"}, args...)
			if code := runWithGitCheck(func(context.Context, string) error {
				t.Fatal("invalid mode reached Git preflight")
				return nil
			}); code != 2 {
				t.Fatalf("invalid mode returned %d; want usage error 2", code)
			}
		})
	}
}

func assertOfflineModeCounts(t *testing.T, modelCalls string, mu *sync.Mutex, published *[]github.Comment, want int) {
	t.Helper()
	data, err := os.ReadFile(modelCalls)
	if err != nil || strings.Count(string(data), "model\n") != want {
		t.Fatalf("model calls = %q, %v; want %d", data, err, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*published) != want {
		t.Fatalf("published comments = %d; want %d", len(*published), want)
	}
}

func offlineModeGitRemote(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source, remote := filepath.Join(root, "source"), filepath.Join(root, "remote.git")
	scratch := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), realGit, args...)
		cmd.Env = testsupport.GitEnvironment(scratch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", "--template=", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", source, "add", "--all")
	git("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "source")
	git("clone", "--template=", "--bare", source, remote)
	rewrite := filepath.Join(root, "gitconfig")
	git("config", "--file", rewrite, "url."+remote+".insteadOf", "https://github.com/o/r.git")
	const denyRemote = " -c protocol.https.allow=never -c protocol.http.allow=never -c protocol.ssh.allow=never -c protocol.git.allow=never"
	testsupport.FakeOnPath(t, "git", "#!/bin/sh\nexec "+aicli.ShellQuote(realGit)+denyRemote+" -c "+aicli.ShellQuote("include.path="+rewrite)+" \"$@\"\n")
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
