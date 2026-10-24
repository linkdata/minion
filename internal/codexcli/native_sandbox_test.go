package codexcli

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This opt-in test runs Codex's native sandbox without starting an AI turn.
// CI installs the CLI and sets MINION_NATIVE_SANDBOX_TEST=1 on macOS and Windows.
func TestNativeCodexSandboxEnforcement(t *testing.T) {
	if os.Getenv("MINION_NATIVE_SANDBOX_TEST") != "1" {
		t.Skip("native Codex sandbox probe is opt-in")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("native Codex sandbox probe requires macOS or Windows")
	}
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("native Codex sandbox probe requires installed CLI: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(home, "minion-sandbox-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	private, err := os.MkdirTemp(home, "minion-sandbox-private-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	probeHome, err := os.MkdirTemp(home, ".minion-codex-sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probeHome) })
	runtimeDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(workspace, "allowed.txt")
	gitDir := filepath.Join(workspace, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(gitDir, "config")
	privateFile := filepath.Join(private, "private.txt")
	if err := os.WriteFile(allowedFile, []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateFile, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataAlias := ""
	if runtime.GOOS == "darwin" {
		alias := filepath.Join("/System/Volumes/Data", strings.TrimPrefix(privateFile, "/"))
		original, err := os.Stat(privateFile)
		if err != nil {
			t.Fatal(err)
		}
		if candidate, err := os.Stat(alias); err == nil && os.SameFile(original, candidate) {
			if data, err := os.ReadFile(alias); err == nil && string(data) == "private" {
				dataAlias = alias
			}
		}
		if dataAlias == "" {
			t.Log("Data volume alias is unavailable; skipping alias probe")
		}
	}
	if err := os.WriteFile(gitFile, []byte("metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAlias := ""
	if runtime.GOOS == "windows" {
		alias := filepath.Join(workspace, ".GIT", "config")
		original, err := os.Stat(gitFile)
		if err != nil {
			t.Fatal(err)
		}
		if candidate, err := os.Stat(alias); err == nil && os.SameFile(original, candidate) {
			gitAlias = alias
		} else {
			t.Log("case-insensitive .GIT alias is unavailable; skipping alias probe")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("native network probe needs loopback listener: %v", err)
	}
	defer listener.Close()
	baseline, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("loopback baseline unavailable: %v", err)
	}
	_ = baseline.Close()
	rule, err := sandboxFilesystemRule(workspace, runtimeDir, executable, "", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rule, `":minimal"`) {
		t.Fatal("native sandbox probe received opaque minimal host grants")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli,
		"sandbox", "-c", `default_permissions="minion"`, "-c", rule,
		"-c", "permissions.minion.network.enabled=false", "--",
		executable, "-test.run=^TestNativeCodexSandboxProbeChild$",
	)
	cmd.Dir = runtimeDir
	cmd.Env = append(os.Environ(),
		"CODEX_HOME="+probeHome,
		"MINION_NATIVE_SANDBOX_CHILD=1",
		"MINION_NATIVE_SANDBOX_ALLOWED="+allowedFile,
		"MINION_NATIVE_SANDBOX_PRIVATE="+privateFile,
		"MINION_NATIVE_SANDBOX_DATA_ALIAS="+dataAlias,
		"MINION_NATIVE_SANDBOX_GIT="+gitFile,
		"MINION_NATIVE_SANDBOX_GIT_ALIAS="+gitAlias,
		"MINION_NATIVE_SANDBOX_NETWORK="+listener.Addr().String(),
	)
	cmd.WaitDelay = 2 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("native Codex sandbox probe timed out: %v; output: %s", ctx.Err(), output)
		}
		t.Fatalf("native Codex sandbox did not enforce Minion profile (%v): %s", err, output)
	}
	if string(dataMustRead(t, privateFile)) != "private" {
		t.Fatal("native sandbox changed private file")
	}
	if string(dataMustRead(t, gitFile)) != "metadata" {
		t.Fatal("native sandbox changed Git metadata")
	}
}

func TestNativeCodexSandboxProbeChild(t *testing.T) {
	if os.Getenv("MINION_NATIVE_SANDBOX_CHILD") != "1" {
		return
	}
	allowed := os.Getenv("MINION_NATIVE_SANDBOX_ALLOWED")
	private := os.Getenv("MINION_NATIVE_SANDBOX_PRIVATE")
	gitFile := os.Getenv("MINION_NATIVE_SANDBOX_GIT")
	addr := os.Getenv("MINION_NATIVE_SANDBOX_NETWORK")
	if allowed == "" || private == "" || gitFile == "" || addr == "" {
		t.Fatal("sandbox probe paths or address missing")
	}
	if data, err := os.ReadFile(allowed); err != nil || string(data) != "allowed" {
		t.Fatalf("permitted source read failed: %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(allowed), "written.txt"), []byte("written"), 0o600); err != nil {
		t.Fatalf("permitted source write failed: %v", err)
	}
	if data, err := os.ReadFile(gitFile); err != nil || string(data) != "metadata" {
		t.Fatalf("Git metadata read failed: %q, %v", data, err)
	}
	if err := os.WriteFile(gitFile, []byte("changed"), 0o600); err == nil {
		t.Fatal("sandboxed command overwrote Git metadata")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(gitFile), "new"), []byte("changed"), 0o600); err == nil {
		t.Fatal("sandboxed command created Git metadata")
	}
	if alias := os.Getenv("MINION_NATIVE_SANDBOX_GIT_ALIAS"); alias != "" {
		if err := os.WriteFile(alias, []byte("changed"), 0o600); err == nil {
			t.Fatal("sandboxed command overwrote Git metadata through .GIT alias")
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(alias), "new"), []byte("changed"), 0o600); err == nil {
			t.Fatal("sandboxed command created Git metadata through .GIT alias")
		}
	}
	if data, err := os.ReadFile(private); err == nil {
		t.Fatalf("private host file was readable: %q", data)
	}
	if alias := os.Getenv("MINION_NATIVE_SANDBOX_DATA_ALIAS"); alias != "" {
		if data, err := os.ReadFile(alias); err == nil {
			t.Fatalf("private host file was readable through Data volume alias: %q", data)
		}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(private), "breakout.txt"), []byte("breakout"), 0o600); err == nil {
		t.Fatal("private host path was writable")
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("sandboxed command reached a local network listener")
	}
}

func dataMustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
