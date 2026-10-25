package gitwork

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("MINION_TEST_GH_HELPER") == "1" {
		if len(os.Args) != 4 || os.Args[1] != "auth" || os.Args[2] != "git-credential" || os.Args[3] != "get" {
			os.Exit(2)
		}
		fmt.Print("username=trusted\npassword=fixture\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsCredentialHelperQuotedNativePath(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "gh's tools")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "gh.exe")
	if err := os.Link(self, helper); err != nil {
		from, openErr := os.Open(self)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer from.Close()
		to, createErr := os.Create(helper)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, copyErr := io.Copy(to, from)
		if err := errors.Join(copyErr, to.Close()); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), git, "-c", "credential.helper=", "-c", credentialHelperConfig(helper), "credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cleanGitEnvironment(), "MINION_TEST_GH_HELPER=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "username=trusted") || !strings.Contains(string(output), "password=fixture") {
		t.Fatalf("native credential helper path was not invoked: %s: %v", output, err)
	}
}
