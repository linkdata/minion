//go:build windows

package action

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

// The linked or copied test binary redirects production Git URLs to local bare fixtures.
func TestMain(m *testing.M) {
	if realGit := os.Getenv("MINION_WINDOWS_GIT_PROXY"); realGit != "" {
		args := append([]string{"-c", "protocol.https.allow=never", "-c", "protocol.http.allow=never", "-c", "protocol.ssh.allow=never", "-c", "include.path=" + filepath.ToSlash(os.Getenv("MINION_WINDOWS_GIT_CONFIG"))}, os.Args[1:]...)
		cmd := exec.Command(realGit, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				os.Exit(exit.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsFixPublicationCommitsPushesAndReplies(t *testing.T) {
	realGit, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	baseRemote, forkRemote := filepath.Join(root, "base.git"), filepath.Join(root, "fork.git")
	scratch := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Env = testsupport.GitEnvironment(scratch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--template=", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", source, "add", "source.txt")
	git("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "base")
	base := git("-C", source, "rev-parse", "HEAD")
	git("clone", "--template=", "--bare", source, baseRemote)
	git("clone", "--template=", "--bare", baseRemote, forkRemote)
	configFile := filepath.Join(root, "rewrite.gitconfig")
	git("config", "--file", configFile, "url."+filepath.ToSlash(baseRemote)+".insteadOf", "https://github.com/o/r.git")
	git("config", "--file", configFile, "url."+filepath.ToSlash(forkRemote)+".insteadOf", "https://github.com/linkdataminion/r.git")

	proxyDir := t.TempDir()
	testsupport.LinkTestExecutable(t, filepath.Join(proxyDir, "git.exe"))
	t.Setenv("PATH", proxyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MINION_WINDOWS_GIT_PROXY", realGit)
	t.Setenv("MINION_WINDOWS_GIT_CONFIG", configFile)

	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, privateRepositoryJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
			_, _ = fmt.Fprint(w, privateForkRepositoryJSON)
		case serveSafeForkChecks(w, r, "linkdataminion/r", true):
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Issue","bodyText":"Details","lastEditedAt":null}}}`)
		case captureComment(t, w, r, &comment):
		default:
			t.Errorf("unexpected GitHub fixture request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	workRoot := t.TempDir()
	checkout, err := gitwork.Clone(t.Context(), workRoot, "o/r", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	const branch = "minion/fix-7-aaaaaaaaaaaa"
	if err := checkout.NewBranch(t.Context(), branch); err != nil {
		t.Fatal(err)
	}
	request := monitor.Job{
		ID: strings.Repeat("a", 64), Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2},
		SubjectType: "Issue", Number: 7,
		Source:     github.Item{Author: github.User{ID: 7, Login: "writer"}, UpdatedAt: time.Now()},
		Command:    command.Command{Name: "fix", Action: command.Fix},
		AcceptedAt: time.Now().UTC(),
	}
	orchestrator := Orchestrator{GitHub: testGitHub(server, "token"), BotLogin: "linkdataminion", Store: store, WorkRoot: workRoot}
	repo, fork, err := orchestrator.fixFork(t.Context(), request.Repository)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedAction{job: Job{Source: workspace.Snapshot{Head: base}}, request: request, issueNodeID: "issue-node", checkout: checkout, repository: repo, fork: fork, branch: branch}
	result := Result{Changes: []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}}
	if err := orchestrator.publishFix(t.Context(), prepared, result, publication{answer: "Fixed."}); err != nil {
		t.Fatal(err)
	}
	wantSHA := git("--git-dir="+forkRemote, "rev-parse", "refs/heads/"+branch)
	if pending := store.Data.PendingFixes[request.ID]; pending.SHA != wantSHA || wantSHA == base {
		t.Fatalf("published head=%q checkpoint=%+v base=%q", wantSHA, pending, base)
	}
	if got := git("--git-dir="+forkRemote, "show", "refs/heads/"+branch+":source.txt"); got != "after" {
		t.Fatalf("published content=%q", got)
	}
	if !strings.Contains(comment, branch) || !strings.Contains(comment, wantSHA) || !strings.HasSuffix(comment, request.Marker()) {
		t.Fatalf("published reply=%q", comment)
	}
}
