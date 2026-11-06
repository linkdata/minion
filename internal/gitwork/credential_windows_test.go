package gitwork

import (
	"bytes"
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
	if os.Getenv("MINION_TEST_GIT_ECHO_TOKEN") == "1" && strings.EqualFold(filepath.Base(os.Args[0]), "git.exe") {
		fmt.Fprintln(os.Stderr, os.Getenv("MINION_GITHUB_TOKEN"))
		os.Exit(9)
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

func TestWindowsConfiguredGitHubCredentialIsolated(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh.exe")
	testsupport.LinkTestExecutable(t, gh)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ghMarker := filepath.Join(root, "gh-invoked")
	t.Setenv("MINION_TEST_REFUSE_GH_MARKER", ghMarker)
	t.Setenv("GH_TOKEN", "ambient-gh")
	t.Setenv("GITHUB_TOKEN", "ambient-github")
	t.Setenv("MINION_GITHUB_TOKEN", "ambient-minion")
	global := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = !f() { echo username=ambient; echo password=ambient; }; f\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, test := range []struct {
		host string
		want string
	}{
		{host: "github.com", want: "username=x-access-token\npassword=configured-token\n"},
		{host: "example.invalid"},
	} {
		t.Run(test.host, func(t *testing.T) {
			var output bytes.Buffer
			err := runGitInputToEnvWithToken(t.Context(), root, strings.NewReader("protocol=https\nhost="+test.host+"\n\n"), &output, nil, "configured-token", "credential", "fill")
			if test.want == "" {
				if err == nil || output.Len() != 0 {
					t.Fatalf("other host received credentials: %q: %v", output.String(), err)
				}
			} else if err != nil || !strings.Contains(output.String(), test.want) || strings.Contains(output.String(), "ambient") {
				t.Fatalf("configured Git credential = %q: %v", output.String(), err)
			}
			if _, err := os.Stat(ghMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("gh was invoked: %v", err)
			}
		})
	}
}

func TestWindowsGitFailureRedactsConfiguredToken(t *testing.T) {
	bin := t.TempDir()
	testsupport.LinkTestExecutable(t, filepath.Join(bin, "git.exe"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MINION_TEST_GIT_ECHO_TOKEN", "1")
	_, err := runGitWithToken(t.Context(), t.TempDir(), "configured-secret", "clone")
	if err == nil || strings.Contains(err.Error(), "configured-secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("Git diagnostic was not redacted: %v", err)
	}
}
