package gitwork

import (
	"bytes"
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
	"github.com/linkdata/minion/internal/testsupport"
)

func TestMain(m *testing.M) {
	if strings.EqualFold(filepath.Base(os.Args[0]), "minion-git-environment-probe.exe") {
		for _, entry := range os.Environ() {
			fmt.Println(entry)
		}
		os.Exit(0)
	}
	if marker := os.Getenv("MINION_TEST_REFUSE_GH_MARKER"); marker != "" && strings.EqualFold(filepath.Base(os.Args[0]), "gh.exe") {
		if err := os.WriteFile(marker, []byte("invoked"), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(1)
	}
	if marker := os.Getenv("MINION_TEST_CREDENTIAL_PROBE_PATH"); marker != "" && strings.EqualFold(filepath.Base(os.Args[0]), "credential-probe.exe") {
		if err := os.WriteFile(marker, []byte("invoked"), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Print("username=probe\npassword=probe\n")
		os.Exit(0)
	}
	if os.Getenv("MINION_TEST_GH_HELPER") == "1" {
		if len(os.Args) != 4 || os.Args[1] != "auth" || os.Args[2] != "git-credential" || os.Args[3] != "get" {
			os.Exit(2)
		}
		fmt.Print("username=trusted\npassword=fixture\n")
		os.Exit(0)
	}
	if realGit := os.Getenv("MINION_TEST_GIT_PROXY"); realGit != "" {
		for i, arg := range os.Args[1:] {
			if arg == "cat-file" && i+2 < len(os.Args) && os.Args[i+2] == "--batch" {
				arg = "cat-file --batch"
			}
			if arg != "clone" && arg != "ls-tree" && arg != "checkout" && arg != "update-ref" && arg != "bundle" && arg != "cat-file --batch" {
				continue
			}
			log, err := os.OpenFile(os.Getenv("MINION_TEST_GIT_COMMANDS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			_, writeErr := fmt.Fprintln(log, arg)
			if err := errors.Join(writeErr, log.Close()); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			break
		}
		cmd := exec.Command(realGit, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				os.Exit(exit.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsCredentialHelperQuotedNativePath(t *testing.T) {
	git, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "gh's tools")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "gh.exe")
	testsupport.LinkTestExecutable(t, helper)
	cmd := exec.CommandContext(t.Context(), git, "-c", "credential.helper=", "-c", credentialHelperConfig(helper), "credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cleanGitEnvironment(), "MINION_TEST_GH_HELPER=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "username=trusted") || !strings.Contains(string(output), "password=fixture") {
		t.Fatalf("native credential helper path was not invoked: %s: %v", output, err)
	}
}

func TestWindowsInstalledGitRefusesAmbientCredentials(t *testing.T) {
	git, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := t.TempDir()
	refusingGH := filepath.Join(bin, "gh.exe")
	testsupport.LinkTestExecutable(t, refusingGH)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ghMarker := filepath.Join(root, "gh-invoked")
	t.Setenv("MINION_TEST_REFUSE_GH_MARKER", ghMarker)
	if got := resolveCredentialHelperGH(); got != refusingGH {
		t.Fatalf("fixture gh helper = %q, want %q", got, refusingGH)
	}
	helper := filepath.Join(root, "credential-probe.exe")
	testsupport.LinkTestExecutable(t, helper)
	global := filepath.Join(root, "global.gitconfig")
	helperValue := strings.TrimPrefix(credentialHelperConfig(helper), "credential.helper=")
	config := exec.Command(git, "config", "--file", global, "credential.helper", helperValue)
	config.Env = cleanGitEnvironment()
	if output, err := config.CombinedOutput(); err != nil {
		t.Fatalf("configure ambient helper: %s: %v", output, err)
	}
	marker := filepath.Join(root, "credential-invoked")
	t.Setenv("MINION_TEST_CREDENTIAL_PROBE_PATH", marker)
	control := gitCommand(t.Context(), git, root, append(cleanGitEnvironment(), "GIT_CONFIG_GLOBAL="+global), "credential", "fill")
	control.Stdin = strings.NewReader("protocol=https\nhost=example.invalid\n\n")
	if output, err := control.CombinedOutput(); err != nil || !strings.Contains(string(output), "username=probe") {
		t.Fatalf("ambient helper control did not run: %s: %v", output, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("ambient helper control left no marker: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_ASKPASS", helper)
	for _, extraEnv := range [][]string{nil, {"GIT_CONFIG_GLOBAL=" + global}} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		var output bytes.Buffer
		err := runGitInputToEnv(ctx, root, strings.NewReader("protocol=https\nhost=example.invalid\n\n"), &output, extraEnv, "credential", "fill")
		contextErr := ctx.Err()
		cancel()
		if err == nil || contextErr != nil || output.Len() != 0 {
			t.Fatalf("Git used ambient credentials or waited for input (extraEnv=%v): %s: %v (context: %v)", extraEnv, output.String(), err, contextErr)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ambient credential program ran (extraEnv=%v): %v", extraEnv, err)
		}
		if err := os.Remove(ghMarker); err != nil {
			t.Fatalf("trusted gh helper did not run (extraEnv=%v): %v", extraEnv, err)
		}
	}
}
