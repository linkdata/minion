package gitwork

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/testsupport"
)

func TestMain(m *testing.M) {
	if os.Getenv("MINION_TEST_GH_HELPER") == "1" {
		if len(os.Args) != 4 || os.Args[1] != "auth" || os.Args[2] != "git-credential" || os.Args[3] != "get" {
			os.Exit(2)
		}
		fmt.Print("username=trusted\npassword=fixture\n")
		os.Exit(0)
	}
	if realGit := os.Getenv("MINION_TEST_GIT_PROXY"); realGit != "" {
		for _, arg := range os.Args[1:] {
			if arg != "clone" && arg != "ls-tree" && arg != "checkout" && arg != "update-ref" {
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
	git, err := exec.LookPath("git")
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
