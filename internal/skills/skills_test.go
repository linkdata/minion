package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testURL = "https://github.com/example/skills/tree/main/skills/review"
	testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	}
}

func symlinkFixture(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestName(t *testing.T) {
	for _, tt := range []struct {
		url, want string
	}{
		{testURL, "review"},
		{testURL + "/", "review"},
		{BuiltinAntiPromptInjection, "anti-prompt-injection"},
		{BuiltinAntiPromptInjection + "/", ""},
		{BuiltinAntiPromptInjection + "?version=1", ""},
		{"https://github.com/example/skills/tree/main/skills/review?x=1", ""},
		{"http://github.com/example/skills/tree/main/skills/review", ""},
		{"https://evil.example/example/skills/tree/main/skills/review", ""},
		{"https://github.com/example/skills/tree/main/skills/../review", ""},
		{"https://github.com/example/skills/tree/main/skills/%2e%2e/review", ""},
		{"https://github.com/example/skills/tree/release/v1/skills/review", ""},
		{"https://github.com/example/skills/tree/main", ""},
	} {
		got, err := Name(tt.url)
		if got != tt.want || (err != nil) != (tt.want == "") {
			t.Errorf("Name(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
		}
	}
}

func TestBuiltinAvailableWithoutInstallation(t *testing.T) {
	setTestHome(t, t.TempDir())
	path, installed, err := InstalledPath(BuiltinAntiPromptInjection)
	if err != nil || !installed || path != BuiltinAntiPromptInjection {
		t.Fatalf("embedded skill = %q, %t, %v", path, installed, err)
	}
	content, ok := BuiltinContent(path)
	if !ok || !strings.Contains(string(content), "Untrusted content boundaries") {
		t.Fatalf("embedded skill content = %q, %t", content, ok)
	}
	content[0] = 'X'
	again, _ := BuiltinContent(path)
	if again[0] == 'X' {
		t.Fatal("embedded skill content can be changed by caller")
	}
}

func TestInstalledPathRequiresMatchingSafeInstallation(t *testing.T) {
	for _, tt := range []struct {
		name, parent string
		lookup       func(string) (string, bool, error)
	}{
		{name: "default", parent: ".agents", lookup: InstalledPath},
		{name: "codex", parent: ".agents", lookup: func(url string) (string, bool, error) { return InstalledPathFor(url, "codex") }},
		{name: "claude", parent: ".claude", lookup: func(url string) (string, bool, error) { return InstalledPathFor(url, "claude") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			setTestHome(t, home)
			canonicalHome, err := filepath.EvalSymlinks(home)
			if err != nil {
				t.Fatal(err)
			}
			if path, installed, err := tt.lookup(testURL); err != nil || installed || path != "" {
				t.Fatalf("missing skill = %q, %t, %v", path, installed, err)
			}
			dir := filepath.Join(home, tt.parent, "skills", "review")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, installed, err := tt.lookup(testURL); err == nil || installed {
				t.Fatalf("skill directory without SKILL.md = %t, %v", installed, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\n---\nReview code.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, installed, err := tt.lookup(testURL); err != nil || installed {
				t.Fatalf("unmarked skill = %t, %v", installed, err)
			}
			metadata := []byte(`{"url":"` + testURL + `","commit":"` + testSHA + `"}`)
			if err := os.WriteFile(filepath.Join(dir, installMetadataName), metadata, 0o600); err != nil {
				t.Fatal(err)
			}
			if path, installed, err := tt.lookup(testURL); err != nil || !installed || path != filepath.Join(canonicalHome, tt.parent, "skills", "review", "SKILL.md") {
				t.Fatalf("installed skill = %q, %t, %v", path, installed, err)
			}
			if _, installed, err := tt.lookup("https://github.com/other/skills/tree/main/skills/review"); !errors.Is(err, ErrSourceMismatch) || installed {
				t.Fatalf("mismatched source = %t, %v", installed, err)
			}
			for _, bad := range []string{`not JSON`, `{"url":"not a skill URL","commit":"` + testSHA + `"}`, `{"url":"https://github.com/other/skills/tree/main/skills/review","commit":"bad"}`} {
				if err := os.WriteFile(filepath.Join(dir, installMetadataName), []byte(bad), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, installed, err := tt.lookup(testURL); err == nil || errors.Is(err, ErrSourceMismatch) || installed {
					t.Fatalf("invalid metadata = %t, %v", installed, err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, installMetadataName), metadata, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Run("symlinked directory", func(t *testing.T) {
				realDir := dir + "-installed"
				if err := os.Rename(dir, realDir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
					if err := os.Rename(realDir, dir); err != nil {
						t.Error(err)
					}
				})
				symlinkFixture(t, realDir, dir)
				if path, installed, err := tt.lookup(testURL); err == nil || installed || path != "" || !strings.Contains(err.Error(), "must not be a symlink") {
					t.Fatalf("symlinked skill directory = %q, %t, %v", path, installed, err)
				}
			})
			t.Run("symlink in skill", func(t *testing.T) {
				symlinkFixture(t, t.TempDir(), filepath.Join(dir, "outside"))
				if _, installed, err := tt.lookup(testURL); err == nil || installed {
					t.Fatalf("symlink in skill = %t, %v", installed, err)
				}
			})
		})
	}
}

func TestInstalledPathForBuiltinAndUnsupportedProvider(t *testing.T) {
	setTestHome(t, t.TempDir())
	for _, provider := range []string{"codex", "claude", "", "other"} {
		path, installed, err := InstalledPathFor(BuiltinAntiPromptInjection, provider)
		if provider == "codex" || provider == "claude" {
			if err != nil || !installed || path != BuiltinAntiPromptInjection {
				t.Fatalf("embedded skill for %s = %q, %t, %v", provider, path, installed, err)
			}
		} else if err == nil || installed || path != "" {
			t.Fatalf("unsupported provider %q = %q, %t, %v", provider, path, installed, err)
		}
	}
}

func TestUserHomeResolvesSymlink(t *testing.T) {
	realHome := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(t.TempDir(), "home")
	symlinkFixture(t, realHome, linkedHome)
	setTestHome(t, linkedHome)
	home, err := userHome()
	if err != nil || home != canonicalHome {
		t.Fatalf("userHome() = %q, %v; want %q", home, err, canonicalHome)
	}
}

type archiveEntry struct {
	name, body string
	kind       byte
	mode       int64
}

func tarball(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var data strings.Builder
	gz := gzip.NewWriter(&data)
	w := tar.NewWriter(gz)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: mode}
		if kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := w.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(data.String())
}

func testInstaller(t *testing.T, home string, archive []byte) (*installer, *int, *int) {
	t.Helper()
	commits, archives := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/skills/commits/main":
			commits++
			if _, err := fmt.Fprintf(w, `{"sha":"%s"}`, testSHA); err != nil {
				t.Errorf("commit response: %v", err)
			}
		case "/repos/example/skills/tarball/" + testSHA:
			archives++
			if _, err := w.Write(archive); err != nil {
				t.Errorf("archive response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &installer{client: server.Client(), apiURL: server.URL, home: home}, &commits, &archives
}

func TestInstallWholeSubtreesForBothAgents(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	old := filepath.Join(home, ".agents", "skills", "review")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "stale.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("Old review skill."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, installMetadataName), []byte(`{"url":"https://github.com/other/skills/tree/main/skills/review","commit":"`+testSHA+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstalledPathFor(testURL, "codex"); !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("old review skill should be a safe source mismatch: %v", err)
	}
	archive := tarball(t, []archiveEntry{
		{name: "prefix/skills/review/SKILL.md", body: "Review code."},
		{name: "prefix/skills/audit/SKILL.md", body: "Audit code."},
		{name: "prefix/skills/audit/scripts/check.sh", body: "#!/bin/sh\nexit 0\n", mode: 0o755},
	})
	i, commits, archives := testInstaller(t, home, archive)
	auditURL := "https://github.com/example/skills/tree/main/skills/audit"
	if err := i.install(t.Context(), []string{testURL, auditURL, testURL}); err != nil {
		t.Fatal(err)
	}
	if *commits != 1 || *archives != 1 {
		t.Fatalf("downloads = %d commits, %d archives; want one each", *commits, *archives)
	}
	for _, root := range []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".claude", "skills")} {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 2 {
			t.Fatalf("unexpected skill directories in %s: %v, %v", root, entries, err)
		}
		for name, want := range map[string]string{"review": "Review code.", "audit": "Audit code."} {
			data, err := os.ReadFile(filepath.Join(root, name, "SKILL.md"))
			if err != nil || string(data) != want {
				t.Fatalf("%s/%s = %q, %v", root, name, data, err)
			}
			metadata, err := os.ReadFile(filepath.Join(root, name, installMetadataName))
			if err != nil || !strings.Contains(string(metadata), testSHA) || !strings.Contains(string(metadata), `"url": "https://github.com/example/skills/tree/main/skills/`+name+`"`) {
				t.Fatalf("install metadata = %q, %v", metadata, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "review", "stale.txt")); !os.IsNotExist(err) {
			t.Fatalf("old content remains: %v", err)
		}
		info, err := os.Stat(filepath.Join(root, "audit", "scripts", "check.sh"))
		if err != nil || runtime.GOOS != "windows" && info.Mode()&0o100 == 0 {
			t.Fatalf("supporting script mode = %v, %v", info, err)
		}
	}
	if _, installed, err := InstalledPath(testURL); err != nil || !installed {
		t.Fatalf("installed skill unavailable: %t, %v", installed, err)
	}
}

func TestInstallForLeavesOtherProviderUntouched(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	codexDir := filepath.Join(home, ".agents", "skills", "review")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldSkill := []byte("Existing Codex skill.\n")
	oldMetadata := []byte(`{"url":"` + testURL + `","commit":"` + testSHA + `"}`)
	if err := os.WriteFile(filepath.Join(codexDir, "SKILL.md"), oldSkill, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, installMetadataName), oldMetadata, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := tarball(t, []archiveEntry{{name: "prefix/skills/review/SKILL.md", body: "New Claude skill."}})
	i, commits, archives := testInstaller(t, home, archive)
	if err := i.install(t.Context(), []string{testURL}, "claude"); err != nil {
		t.Fatal(err)
	}
	if *commits != 1 || *archives != 1 {
		t.Fatalf("downloads = %d commits, %d archives; want one each", *commits, *archives)
	}
	for name, want := range map[string][]byte{"SKILL.md": oldSkill, installMetadataName: oldMetadata} {
		got, err := os.ReadFile(filepath.Join(codexDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Codex %s changed: %q, %v", name, got, err)
		}
	}
	claudePath, installed, err := InstalledPathFor(testURL, "claude")
	if err != nil || !installed {
		t.Fatalf("Claude skill was not installed: %q, %t, %v", claudePath, installed, err)
	}
	got, err := os.ReadFile(claudePath)
	if err != nil || string(got) != "New Claude skill." {
		t.Fatalf("Claude skill content = %q, %v", got, err)
	}
	for _, parent := range []string{".agents", ".claude"} {
		entries, err := os.ReadDir(filepath.Join(home, parent))
		if err != nil || len(entries) != 1 || entries[0].Name() != "skills" {
			t.Fatalf("%s left staging files: %v, %v", parent, entries, err)
		}
	}
}

func TestInstallBuiltinForBothAgentsWithoutNetwork(t *testing.T) {
	home := t.TempDir()
	i := &installer{home: home}
	if err := i.install(t.Context(), []string{BuiltinAntiPromptInjection}); err != nil {
		t.Fatal(err)
	}
	want, _ := BuiltinContent(BuiltinAntiPromptInjection)
	for _, root := range []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".claude", "skills")} {
		dir := filepath.Join(root, "anti-prompt-injection")
		got, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil || string(got) != string(want) {
			t.Fatalf("installed embedded skill = %q, %v", got, err)
		}
		metadata, err := os.ReadFile(filepath.Join(dir, installMetadataName))
		if err != nil {
			t.Fatal(err)
		}
		var recorded struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(metadata, &recorded); err != nil || recorded.URL != BuiltinAntiPromptInjection {
			t.Fatalf("embedded install metadata = %q, %v", metadata, err)
		}
	}
	if err := i.install(t.Context(), []string{BuiltinAntiPromptInjection, "https://github.com/example/skills/tree/main/skills/anti-prompt-injection"}); err == nil {
		t.Fatal("different sources with the same skill name accepted")
	}
}

func TestInstallBundledDefaultsWithoutNetwork(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	urls := []string{
		"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail",
		"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-review",
		"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-audit",
		"https://github.com/JuliusBrussee/caveman/tree/main/skills/caveman",
		"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit",
	}
	// A missing HTTP client makes any attempted download fail this test.
	if err := (&installer{home: home}).install(t.Context(), urls); err != nil {
		t.Fatal(err)
	}
	for _, url := range urls {
		name, err := Name(url)
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range []string{"codex", "claude"} {
			path, installed, err := InstalledPathFor(url, provider)
			if err != nil || !installed {
				t.Fatalf("%s %s installation = %q, %t, %v", provider, name, path, installed, err)
			}
			dir := filepath.Dir(path)
			metadata, err := os.ReadFile(filepath.Join(dir, installMetadataName))
			if err != nil {
				t.Fatal(err)
			}
			var recorded struct {
				URL, Commit string
			}
			if err := json.Unmarshal(metadata, &recorded); err != nil || recorded.URL != url || recorded.Commit != bundledCommits[url] {
				t.Fatalf("%s %s metadata = %q, %v", provider, name, metadata, err)
			}
			for _, file := range append([]string{"SKILL.md", "LICENSE"}, map[string][]string{
				"caveman":        {"LICENSE-MIT", "LICENSING.md", "NOTICE", "README.md"},
				"security-audit": {"HUNTING.md", "report-schema.json", "validate-findings.cjs", "validate-coverage-ledger.cjs"},
			}[name]...) {
				if data, err := os.ReadFile(filepath.Join(dir, file)); err != nil || len(data) == 0 {
					t.Fatalf("%s %s %s = %d bytes, %v", provider, name, file, len(data), err)
				}
			}
			if name == "security-audit" {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 20 { // Runtime skill files, LICENSE, and install metadata.
					t.Fatalf("%s security-audit files = %d, %v", provider, len(entries), err)
				}
			}
		}
	}
	other := "https://github.com/other/ponytail/tree/main/skills/ponytail"
	if _, installed, err := InstalledPathFor(other, "codex"); !errors.Is(err, ErrSourceMismatch) || installed {
		t.Fatalf("other source = %t, %v; want source mismatch", installed, err)
	}
}

func TestInstallRejectsUnsafeArchiveWithoutReplacingSkill(t *testing.T) {
	for _, tt := range []struct {
		name, path string
		kind       byte
	}{
		{"traversal", "prefix/skills/review/../../outside", tar.TypeReg},
		{"symlink", "prefix/skills/review/link", tar.TypeSymlink},
		{"no manifest", "prefix/skills/review/other.md", tar.TypeReg},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			old := filepath.Join(home, ".agents", "skills", "review")
			if err := os.MkdirAll(old, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(old, "old.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			archive := tarball(t, []archiveEntry{{name: tt.path, body: "bad", kind: tt.kind}})
			i, _, _ := testInstaller(t, home, archive)
			if err := i.install(t.Context(), []string{testURL}); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if data, err := os.ReadFile(filepath.Join(old, "old.txt")); err != nil || string(data) != "keep" {
				t.Fatalf("old skill was changed: %q, %v", data, err)
			}
		})
	}
}

func TestInstallRejectsBlockedDestinationBeforeReplacing(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, ".agents", "skills", "review")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "old.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeRoot := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(claudeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRoot, "review"), []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	i, _, _ := testInstaller(t, home, tarball(t, []archiveEntry{{name: "prefix/skills/review/SKILL.md", body: "new"}}))
	if err := i.install(t.Context(), []string{testURL}); err == nil {
		t.Fatal("existing non-directory destination accepted")
	}
	if data, err := os.ReadFile(filepath.Join(old, "old.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("old Codex skill was changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(claudeRoot, "review")); err != nil || string(data) != "block" {
		t.Fatalf("Claude destination changed: %q, %v", data, err)
	}
}

func TestRestoreRetainsBackupOnFailure(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "old.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := &stagedInstall{target: filepath.Join(root, "missing-parent", "skill"), backup: backup, hadOld: true, installed: true}
	if err := restore(item); err == nil {
		t.Fatal("restore to missing parent succeeded")
	}
	if !item.hadOld || item.installed {
		t.Fatalf("failed restore lost backup state: %+v", item)
	}
	if data, err := os.ReadFile(filepath.Join(backup, "old.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("old skill backup was lost: %q, %v", data, err)
	}
}

func TestInstallRejectsCaseCollisions(t *testing.T) {
	home := t.TempDir()
	i, _, _ := testInstaller(t, home, tarball(t, []archiveEntry{
		{name: "prefix/skills/review/SKILL.md", body: "Review code."},
		{name: "prefix/skills/review/skill.md", body: "A second spelling."},
	}))
	if err := i.install(t.Context(), []string{testURL}); err == nil {
		t.Fatal("case-colliding files accepted")
	}
	if err := i.install(t.Context(), []string{testURL, "https://github.com/example/skills/tree/main/skills/Review"}); err == nil {
		t.Fatal("case-colliding skill names accepted")
	}
	i, _, _ = testInstaller(t, home, tarball(t, []archiveEntry{
		{name: "prefix/skills/review/SKILL.md", body: "Review code."},
		{name: "prefix/skills/review/scripts/first.txt", body: "first"},
		{name: "prefix/skills/review/Scripts/second.txt", body: "second"},
	}))
	if err := i.install(t.Context(), []string{testURL}); err == nil {
		t.Fatal("case-colliding subdirectories accepted")
	}
}

func TestInstallIgnoresUnrelatedFilename(t *testing.T) {
	home := t.TempDir()
	i, _, _ := testInstaller(t, home, tarball(t, []archiveEntry{
		{name: "prefix/unrelated\\file.txt", body: "unrelated"},
		{name: "prefix/skills/review/SKILL.md", body: "Review code."},
	}))
	if err := i.install(t.Context(), []string{testURL}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRejectsSymlinkedParentBeforeWriting(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	symlinkFixture(t, outside, filepath.Join(home, ".agents"))
	i, _, _ := testInstaller(t, home, tarball(t, []archiveEntry{{name: "prefix/skills/review/SKILL.md", body: "new"}}))
	if err := i.install(t.Context(), []string{testURL}); err == nil {
		t.Fatal("symlinked skill parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "skills")); !os.IsNotExist(err) {
		t.Fatalf("installer wrote through symlink: %v", err)
	}
}
