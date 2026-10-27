package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

func offlineIndexFixture(t *testing.T, files []workspace.File) (*OfflineIndex, string) {
	t.Helper()
	root := t.TempDir()
	source, err := os.MkdirTemp(root, "source-")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.MkdirTemp(root, "index-")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, files); err != nil {
		t.Fatal(err)
	}
	index, err := NewOfflineIndex(t.Context(), metadata, source, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(source, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source received Git metadata: %v", err)
	}
	return index, source
}

func writeIndexFixture(t *testing.T, source, name string, data []byte) {
	t.Helper()
	writeFile(t, filepath.Join(source, name), string(data))
}

func TestFixIndexCaseOnlyRename(t *testing.T) {
	for _, ignoreCase := range []string{"false", "true"} {
		for _, directory := range []bool{false, true} {
			name := "README.md"
			if directory {
				name = "Docs/README.md"
			}
			t.Run(ignoreCase+"/"+name, func(t *testing.T) {
				if ignoreCase == "false" && !caseSensitiveTempDir(t) {
					t.Skip("core.ignoreCase=false requires a case-sensitive filesystem")
				}
				index, source := offlineIndexFixture(t, []workspace.File{{Path: name, Mode: workspace.Regular, Data: []byte("fixture\n")}})
				if _, err := index.run(t.Context(), 128, nil, "config", "core.ignoreCase", ignoreCase); err != nil {
					t.Fatal(err)
				}
				old, renamed := "README.md", "readme.md"
				if directory {
					old, renamed = "Docs", "docs"
				}
				intermediate := filepath.Join(source, "case-rename.tmp")
				if err := os.Rename(filepath.Join(source, old), intermediate); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(intermediate, filepath.Join(source, renamed)); err != nil {
					t.Fatal(err)
				}
				changes, err := index.Changes(t.Context())
				if err != nil || len(changes) != 2 {
					t.Fatalf("case-only rename changes=%+v error=%v", changes, err)
				}
				if changes[0].Path != name || !changes[0].Delete || changes[1].Path != strings.Replace(name, old, renamed, 1) || changes[1].Delete || string(changes[1].Data) != "fixture\n" {
					t.Fatalf("case-only rename was not staged as deletion and addition: %+v", changes)
				}
			})
		}
	}
}

func TestFixIndexCaseOnlyRenameUsesCurrentIgnoreRules(t *testing.T) {
	for _, ignoreCase := range []string{"false", "true"} {
		t.Run(ignoreCase, func(t *testing.T) {
			if ignoreCase == "false" && !caseSensitiveTempDir(t) {
				t.Skip("core.ignoreCase=false requires a case-sensitive filesystem")
			}
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("readme.md\n")},
				{Path: "README.md", Mode: workspace.Executable, Data: []byte("fixture\n")},
			})
			if _, err := index.run(t.Context(), 128, nil, "config", "core.ignoreCase", ignoreCase); err != nil {
				t.Fatal(err)
			}
			intermediate := filepath.Join(source, "case-rename.tmp")
			if err := os.Rename(filepath.Join(source, "README.md"), intermediate); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(intermediate, filepath.Join(source, "readme.md")); err != nil {
				t.Fatal(err)
			}
			changes, err := index.Changes(t.Context())
			if changes != nil || err == nil || !strings.Contains(err.Error(), "case-renamed fix path") {
				t.Fatalf("newly ignored case rename was not refused: %+v, %v", changes, err)
			}
		})
	}
}

func TestFixIndexCaseOnlyRenameBoundsNewPath(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "README.md", Mode: workspace.Regular, Data: []byte("fixture\n")}})
	if _, err := index.run(t.Context(), 128, nil, "config", "core.ignoreCase", "true"); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(source, "case-rename.tmp")
	if err := os.Rename(filepath.Join(source, "README.md"), intermediate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(intermediate, filepath.Join(source, "readme.md")); err != nil {
		t.Fatal(err)
	}
	writeSparse(t, filepath.Join(source, "readme.md"), int64(workspace.MaxChangeBytes)+1)
	if changes, err := index.Changes(t.Context()); changes != nil || err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized renamed path reached Git hashing: %+v, %v", changes, err)
	}
}

func TestFixIndexCaseSpellingScanSkipsIgnoredSiblings(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("build/\n")},
		{Path: "build/Docs/README.md", Mode: workspace.Regular, Data: []byte("fixture\n")},
	})
	if _, err := index.run(t.Context(), 128, nil, "config", "core.ignoreCase", "true"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		writeIndexFixture(t, source, fmt.Sprintf("build/%03d.tmp", i), []byte("ignored"))
	}
	intermediate := filepath.Join(source, "build", "case-rename.tmp")
	if err := os.Rename(filepath.Join(source, "build", "Docs"), intermediate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(intermediate, filepath.Join(source, "build", "docs")); err != nil {
		t.Fatal(err)
	}
	changes, err := index.Changes(t.Context())
	if changes != nil || err == nil || !strings.Contains(err.Error(), "case-renamed fix path") {
		t.Fatalf("ignored siblings hid a case rename: %+v, %v", changes, err)
	}
}

func TestFixIndexPinsSHA1WhenGitDefaultsToSHA256(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeOnPath(t, "git", "#!/bin/sh\nGIT_DEFAULT_HASH=sha256 exec "+shellQuote(realGit)+" \"$@\"\n")
	proxy := filepath.Join(bin, "git")
	control := filepath.Join(t.TempDir(), "control")
	cmd := exec.Command(proxy, "init", "--quiet", "--template=", control)
	cmd.Env = testsupport.GitEnvironment(t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize SHA256 control: %s,%v", output, err)
	}
	cmd = exec.Command(realGit, "-C", control, "rev-parse", "--show-object-format")
	cmd.Env = testsupport.GitEnvironment(t.TempDir())
	if output, err := cmd.Output(); err != nil || strings.TrimSpace(string(output)) != "sha256" {
		t.Fatalf("control did not select SHA256: %q,%v", output, err)
	}
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before")}})
	if output, err := index.run(t.Context(), 128, nil, "rev-parse", "--show-object-format"); err != nil || strings.TrimSpace(string(output)) != "sha1" {
		t.Fatalf("offline index changed its object format: %q,%v", output, err)
	}
	writeIndexFixture(t, source, "source.txt", []byte("after"))
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "after" {
		t.Fatalf("pinned index failed byte binding: %+v,%v", changes, err)
	}
}

func TestFixIndexBaselineIsTreeWithoutCommit(t *testing.T) {
	index, _ := offlineIndexFixture(t, []workspace.File{{Path: "source", Mode: workspace.Regular, Data: []byte("baseline")}})
	typeName, err := index.run(t.Context(), 128, nil, "cat-file", "-t", index.baseline)
	if err != nil || strings.TrimSpace(string(typeName)) != "tree" {
		t.Fatalf("baseline type=%q error=%v", typeName, err)
	}
	objects, err := index.run(t.Context(), 1024, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	if err != nil || strings.Contains(string(objects), "commit") {
		t.Fatalf("offline objects=%q error=%v", objects, err)
	}
}

func TestFixIndexRejectsBaselineMetadataAliasWithoutHidingLaunchErrors(t *testing.T) {
	for _, name := range []string{"GIT~1/file", ".git./file"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("Windows normalizes metadata alias fixture paths")
			}
			source, metadata := t.TempDir(), t.TempDir()
			writeIndexFixture(t, source, name, []byte("source"))
			_, err := NewOfflineIndex(t.Context(), metadata, source, nil)
			var exit *exec.ExitError
			if !errors.Is(err, ErrUnsupportedSource) || !errors.As(err, &exit) {
				t.Fatalf("baseline alias error=%v", err)
			}
		})
	}
	index, _ := offlineIndexFixture(t, nil)
	index.dir = filepath.Join(t.TempDir(), "missing")
	if _, err := index.run(t.Context(), 1024, nil, "add", "--all", "--", "."); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("offline Git launch chdir error=%v", err)
	}
}

func TestFixIndexQuotesGitDiagnostic(t *testing.T) {
	index, _ := offlineIndexFixture(t, nil)
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", "#!/bin/sh\nprintf 'first line\\nforged journal entry\\n' >&2\nexit 1\n"), "git-proxy")
	_, err := index.run(t.Context(), 1024, nil, "status")
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), `"first line\nforged journal entry"`) {
		t.Fatalf("unquoted offline Git diagnostic: %v", err)
	}
}

func TestFixIndexBoundsControlFilesBeforeGitReadsThem(t *testing.T) {
	for _, control := range []string{".gitignore", ".gitattributes"} {
		for _, size := range []int64{workspace.MaxChangeBytes, workspace.MaxChangeBytes + 1, workspace.MaxBytes, workspace.MaxBytes + 1} {
			t.Run(fmt.Sprintf("%s/%d", control, size), func(t *testing.T) {
				index, source := offlineIndexFixture(t, nil)
				name := filepath.Join(source, control)
				writeSparse(t, name, size)
				marker := filepath.Join(t.TempDir(), "called")
				const command = "git-control-probe"
				// Git reads an exact-limit control, but not an oversized sparse file.
				index.binary = filepath.Join(fakeOnPath(t, command, "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 91\n"), command)
				_, err := index.Changes(t.Context())
				if size > workspace.MaxChangeBytes {
					want := fmt.Sprintf("fix control file %q exceeds changed byte limit", control)
					if size > workspace.MaxBytes {
						want = fmt.Sprintf("fix control file %q must be regular and bounded", control)
					}
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("oversized control reached later collection: %v", err)
					}
					if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("oversized control reached Git: %v", err)
					}
					return
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 91 {
					t.Fatalf("in-bound control was refused before Git: %v", err)
				}
				if data, err := os.ReadFile(marker); err != nil || string(data) != "called" {
					t.Fatalf("in-bound control did not reach Git: %q, %v", data, err)
				}
			})
		}
	}
}

func TestFixIndexBoundsAggregateControlBytesBeforeGit(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	for _, control := range []string{".gitignore", ".gitattributes"} {
		name := filepath.Join(source, control)
		writeSparse(t, name, workspace.MaxChangeBytes/2+1)
	}
	marker := filepath.Join(t.TempDir(), "called")
	index.binary = filepath.Join(fakeOnPath(t, "git-control-probe", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 91\n"), "git-control-probe")
	if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), `fix control file ".gitattributes" exceeds changed byte limit`) {
		t.Fatalf("aggregate control bytes were accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized aggregate reached Git: %v", err)
	}
}

func TestFixIndexChargesSameSizeTrackedControlRewrite(t *testing.T) {
	baseline := bytes.Repeat([]byte{' '}, workspace.MaxChangeBytes+1)
	for _, control := range []string{".gitignore", ".gitattributes"} {
		t.Run(control, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: control, Mode: workspace.Regular, Data: baseline}})
			marker := filepath.Join(t.TempDir(), "called")
			index.binary = filepath.Join(fakeOnPath(t, "git-control-probe", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 91\n"), "git-control-probe")
			_, err := index.Changes(t.Context())
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Fatalf("unchanged tracked control did not reach Git: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("unchanged tracked control was refused before Git: %v", err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, control), bytes.Repeat([]byte{'a'}, len(baseline)), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("fix control file %q exceeds changed byte limit", control)) {
				t.Fatalf("same-size control rewrite was accepted: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("same-size control rewrite reached Git: %v", err)
			}
		})
	}
}

func TestFixIndexRefusesUnterminatedIgnoreOutput(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	if err := os.Mkdir(filepath.Join(source, "candidate"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor arg do\nif [ \"$arg\" = check-ignore ]; then\nprintf ':(top)candidate'\nexit\nfi\ndone\nexec " + shellQuote(index.binary) + " \"$@\"\n"
	index.binary = filepath.Join(fakeOnPath(t, "git-ignore", script), "git-ignore")
	if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid offline git ignore output") {
		t.Fatalf("unterminated ignore output accepted: %v", err)
	}
}

func TestFixIndexBoundsTrackedChangesWithoutRefusingUnchangedLargeSource(t *testing.T) {
	data := make([]byte, workspace.MaxChangeBytes+1)
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "large", Mode: workspace.Regular, Data: data}})
	if changes, err := index.Changes(t.Context()); err != nil || len(changes) != 0 {
		t.Fatalf("unchanged source over result limit was refused: %+v,%v", changes, err)
	}
	data[len(data)-1] = 1
	writeIndexFixture(t, source, "large", data)
	if changes, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized same-size tracked edit was exported: %+v,%v", changes, err)
	}
}

func TestFixIndexPreservesGitignoreAndTrackedIgnoredFiles(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(".venv/\ntarget/\n__pycache__/\n*.log\ntracked.log\n")},
		{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source")},
		{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("before")},
	})
	writeIndexFixture(t, source, "tracked.log", []byte("after"))
	writeIndexFixture(t, source, "runtime.log", make([]byte, workspace.MaxChangeBytes+1))
	writeIndexFixture(t, source, "target/cache/.git/config", []byte("untrusted nested metadata"))
	for i := 0; i < workspace.MaxChanges+10; i++ {
		writeIndexFixture(t, source, fmt.Sprintf("__pycache__/%d.pyc", i), []byte("cache"))
	}
	if err := os.MkdirAll(filepath.Join(source, ".venv", "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside/python", filepath.Join(source, ".venv", "bin", "python")); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Logf("ignored symlink fixture is unavailable on Windows: %v", err)
	}
	changes, err := index.Changes(t.Context())
	want := []workspace.Change{{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	writeIndexFixture(t, source, "tracked.log", []byte("before"))
	changes, err = index.Changes(t.Context())
	if err != nil || len(changes) != 0 {
		t.Fatalf("ignored-only output changed result: %+v, %v", changes, err)
	}
}

func TestFixIndexRebuildsEachCandidateFromBaseline(t *testing.T) {
	t.Run("restored tracked ignored file", func(t *testing.T) {
		index, source := offlineIndexFixture(t, []workspace.File{
			{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.log\n")},
			{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("original")},
		})
		if err := os.Remove(filepath.Join(source, "tracked.log")); err != nil {
			t.Fatal(err)
		}
		changes, err := index.Changes(t.Context())
		if err != nil || len(changes) != 1 || !changes[0].Delete {
			t.Fatalf("first candidate=%+v, %v", changes, err)
		}
		writeIndexFixture(t, source, "tracked.log", []byte("original"))
		if changes, err := index.Changes(t.Context()); err != nil || len(changes) != 0 {
			t.Fatalf("restored candidate retained staged deletion: %+v, %v", changes, err)
		}
	})
	t.Run("new output ignored in later candidate", func(t *testing.T) {
		index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular}})
		writeIndexFixture(t, source, "runtime.log", []byte("generated output"))
		if changes, err := index.Changes(t.Context()); err != nil || len(changes) != 1 || changes[0].Path != "runtime.log" {
			t.Fatalf("first candidate=%+v, %v", changes, err)
		}
		writeIndexFixture(t, source, ".gitignore", []byte("*.log\n"))
		changes, err := index.Changes(t.Context())
		if err != nil || len(changes) != 1 || changes[0].Path != ".gitignore" {
			t.Fatalf("later ignore retained staged output: %+v, %v", changes, err)
		}
	})
}

func TestFixIndexDisablesAttributesAndOperatorConfiguration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"escape\"]\n clean = touch "+marker+"\n required = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.escape.clean")
	t.Setenv("GIT_CONFIG_VALUE_0", "touch "+marker)
	t.Setenv("GH_TOKEN", "must-not-be-inherited")
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitattributes", Mode: workspace.Regular, Data: []byte("* filter=escape diff=escape working-tree-encoding=UTF-16\n")},
		{Path: "source.txt", Mode: workspace.Regular, Data: []byte("unencoded text")},
	})
	writeIndexFixture(t, source, "source.txt", []byte("changed unencoded text"))
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "changed unencoded text" {
		t.Fatalf("attributes changed result: %+v, %v", changes, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository filter ran: %v", err)
	}
	index.binary = filepath.Join(fakeOnPath(t, "git-environment", "#!/bin/sh\nexec env\n"), "git-environment")
	environment, err := index.run(t.Context(), 8<<10, nil, "environment-probe")
	if err != nil {
		t.Fatal(err)
	}
	systemDisabled, systemAttributesDisabled := false, false
	for _, entry := range strings.Split(string(environment), "\n") {
		systemDisabled = systemDisabled || entry == "GIT_CONFIG_NOSYSTEM=1"
		systemAttributesDisabled = systemAttributesDisabled || entry == "GIT_ATTR_NOSYSTEM=1"
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.Contains(entry, global) || strings.Contains(entry, marker) {
			t.Fatalf("unsafe offline Git environment: %q", entry)
		}
	}
	if !systemDisabled || !systemAttributesDisabled {
		t.Fatal("offline Git did not disable system configuration and attributes")
	}
	for _, pinned := range []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit"} {
		if !strings.Contains(string(environment), pinned+"\n") {
			t.Fatalf("offline Git setting missing: %s", pinned)
		}
	}
}

func TestFixIndexRejectsNonIgnoredUnsafeOutput(t *testing.T) {
	for _, tt := range []struct{ kind, cause string }{
		{"symlink", `fix output "link"`},
		{"unsafe path", "not a portable regular-file path"},
		{"nested repository", `untracked nested repository output "nested/" must be removed or ignored`},
		{"aggregate oversized", "byte limit"},
		{"too many", "changed file limit"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && tt.kind == "unsafe path" {
				t.Skip("backslash is a path separator on native Windows")
			}
			index, source := offlineIndexFixture(t, nil)
			switch tt.kind {
			case "unsafe path":
				writeIndexFixture(t, source, "new\\output", []byte("data"))
			case "symlink":
				symlinkOrSkip(t, "/outside/secret", filepath.Join(source, "link"))
			case "nested repository":
				writeIndexFixture(t, source, "nested/file", []byte("nested source"))
				nested := filepath.Join(source, "nested")
				if _, err := runGit(t.Context(), nested, "init", "--quiet", "--template=", "--object-format=sha1"); err != nil {
					t.Fatal(err)
				}
			case "aggregate oversized":
				writeIndexFixture(t, source, "first", make([]byte, workspace.MaxChangeBytes/2+1))
				writeIndexFixture(t, source, "second", make([]byte, workspace.MaxChangeBytes/2+1))
			case "too many":
				for i := 0; i <= workspace.MaxChanges; i++ {
					writeIndexFixture(t, source, fmt.Sprintf("file-%d", i), nil)
				}
			}
			var marker string
			if runtime.GOOS != "windows" {
				marker = filepath.Join(t.TempDir(), "add-ran")
				script := fmt.Sprintf("#!/bin/sh\nfor argument in \"$@\"; do\n if [ \"$argument\" = add ]; then touch %s; fi\ndone\nexec %s \"$@\"\n", shellQuote(marker), shellQuote(index.binary))
				index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
			}
			changes, err := index.Changes(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("%s output: changes=%+v, err=%v; want %q", tt.kind, changes, err, tt.cause)
			}
			if tt.kind == "too many" && !errors.Is(err, ErrTooManyFiles) {
				t.Fatalf("untracked change count lost native classification: %v", err)
			}
			if marker != "" {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Git hashed rejected output before precheck: %v", err)
				}
			}
		})
	}
}

func TestFixIndexRejectsIgnoredUntrackedIgnoreRules(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprint("tracked=", tracked), func(t *testing.T) {
			files := []workspace.File{{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(".gitignore\n*.gen\n")}}
			if tracked {
				files = append(files, workspace.File{Path: "sub/.gitignore", Mode: workspace.Regular})
			}
			index, source := offlineIndexFixture(t, files)
			writeIndexFixture(t, source, "sub/.gitignore", []byte("!keep.gen\n"))
			writeIndexFixture(t, source, "sub/keep.gen", []byte("included by local ignore rule"))
			changes, err := index.Changes(t.Context())
			if !tracked {
				if err == nil || !strings.Contains(err.Error(), `ignored untracked fix control file "sub/.gitignore"`) {
					t.Fatalf("ignored untracked ignore rule exported changes: %+v,%v", changes, err)
				}
			} else if err != nil || len(changes) != 2 || changes[0].Path != "sub/.gitignore" || changes[1].Path != "sub/keep.gen" {
				t.Fatalf("tracked ignored control file refused: %+v,%v", changes, err)
			}
		})
	}
}

func TestFixIndexRejectsSelfIgnoringToolCachesUntilRemoved(t *testing.T) {
	for _, cache := range []string{".pytest_cache", ".ruff_cache", ".mypy_cache", ".venv"} {
		t.Run(cache, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before")}})
			writeIndexFixture(t, source, "source.txt", []byte("after"))
			writeIndexFixture(t, source, cache+"/.gitignore", []byte("# Created by a test tool\n*\n"))
			writeIndexFixture(t, source, cache+"/state", []byte("generated cache"))
			if changes, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("ignored untracked fix control file %q", cache+"/.gitignore")) {
				t.Fatalf("self-ignoring cache was accepted: changes=%+v error=%v", changes, err)
			}
			if err := os.RemoveAll(filepath.Join(source, cache)); err != nil {
				t.Fatal(err)
			}
			changes, err := index.Changes(t.Context())
			want := []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after")}}
			if err != nil || !reflect.DeepEqual(changes, want) {
				t.Fatalf("removing the tool cache lost the intended fix: changes=%+v error=%v", changes, err)
			}
		})
	}
	t.Run("supplied parent ignore rule", func(t *testing.T) {
		index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(".pytest_cache/\n")}})
		writeIndexFixture(t, source, ".pytest_cache/.gitignore", []byte("*\n"))
		writeIndexFixture(t, source, ".pytest_cache/state", []byte("generated cache"))
		writeIndexFixture(t, source, "fix.txt", []byte("reviewed"))
		changes, err := index.Changes(t.Context())
		want := []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("reviewed")}}
		if err != nil || !reflect.DeepEqual(changes, want) {
			t.Fatalf("supplied ignore rule did not exclude the cache directory: changes=%+v error=%v", changes, err)
		}
	})
}

func TestFixIndexBoundsActualControlScanBeforeStaging(t *testing.T) {
	const scanLimit = 129 // Cross the 128-entry ReadDir batch boundary.
	index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.cache\n")}})
	marker := filepath.Join(t.TempDir(), "add-ran")
	script := "#!/bin/sh\nfor argument in \"$@\"; do\n if [ \"$argument\" = add ]; then touch " + shellQuote(marker) + "; fi\ndone\nexec " + shellQuote(index.binary) + " \"$@\"\n"
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
	// The tracked ignore file and intended change bring this to exactly scanLimit.
	for i := 0; i < scanLimit-2; i++ {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("entry-%06d.cache", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexFixture(t, source, "fix.txt", []byte("reviewed"))
	changes, err := index.changesWithControlLimit(t.Context(), scanLimit)
	want := []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("reviewed")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("exact control-scan bound refused the intended fix: changes=%+v error=%v", changes, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("exact-bound control did not stage output: %v", err)
	}
	if err := os.Rename(filepath.Join(source, "fix.txt"), filepath.Join(source, "next.txt")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "overflow.cache", nil)
	if changes, err := index.changesWithControlLimit(t.Context(), scanLimit); err == nil || !strings.Contains(err.Error(), "fix directory scan exceeds entry limit") {
		t.Fatalf("oversized control scan was accepted: changes=%+v error=%v", changes, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git staged output before refusing the oversized scan: %v", err)
	}
}

func TestFixIndexPreservesModeDeletionAndBinary(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "mode", Mode: workspace.Regular, Data: []byte("same")}, {Path: "deleted", Mode: workspace.Regular, Data: []byte("deleted")}})
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(source, "mode"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "binary", []byte{0, 1, 2})
	changes, err := index.Changes(t.Context())
	want := []workspace.Change{{Path: "binary", Mode: workspace.Regular, Data: []byte{0, 1, 2}}, {Path: "deleted", Delete: true}}
	if runtime.GOOS != "windows" {
		want = append(want, workspace.Change{Path: "mode", Mode: workspace.Executable, Data: []byte("same")})
	}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
}

func TestFixIndexBoundsBinaryAndDeletionDiffs(t *testing.T) {
	for _, kind := range []string{"binary addition", "large deletion"} {
		t.Run(kind, func(t *testing.T) {
			var files []workspace.File
			if kind == "large deletion" {
				files = []workspace.File{{Path: "deleted", Mode: workspace.Regular, Data: []byte(strings.Repeat("x", 2<<20))}}
			}
			index, source := offlineIndexFixture(t, files)
			switch kind {
			case "binary addition":
				data := make([]byte, 1000<<10)
				_, _ = rand.New(rand.NewSource(1)).Read(data) // Deterministic incompressible binary patch.
				writeIndexFixture(t, source, "binary", data)
			case "large deletion":
				if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := index.Changes(t.Context()); !errors.Is(err, ErrDiffTooLarge) {
				t.Fatalf("%s lost its size classification: %v", kind, err)
			}
		})
	}
}

func TestFixIndexHonorsPreCanceledRequest(t *testing.T) {
	index, _ := offlineIndexFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := index.Changes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index = %v", err)
	}
}

func TestFixIndexRejectsBytesChangedAfterStaging(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("original")}})
	writeIndexFixture(t, source, "source.txt", []byte("staged edit"))
	script := fmt.Sprintf(`#!/bin/sh
for argument in "$@"; do
  if [ "$argument" = --raw ]; then
    %s "$@"
    result=$?
    printf 'later edit' > %s
    exit "$result"
  fi
done
exec %s "$@"
`, shellQuote(index.binary), shellQuote(filepath.Join(source, "source.txt")), shellQuote(index.binary))
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
	if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), "changed after indexing") {
		t.Fatalf("accepted unstaged bytes: %v", err)
	}
}

func TestParseIndexChangesRejectsMalformedRecords(t *testing.T) {
	old, current := strings.Repeat("1", 40), strings.Repeat("2", 40)
	record := func(mode, status string) []byte {
		return []byte(":100644 " + mode + " " + old + " " + current + " " + status + "\x00changed\x00")
	}
	for _, test := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"missing terminator", []byte(":100644"), "invalid Git index diff"},
		{"odd field count", []byte("record\x00"), "invalid Git index diff"},
		{"invalid record", []byte("wrong\x00changed\x00"), "invalid Git index change record"},
		{"invalid identity", []byte(":100644 100644 short " + current + " M\x00changed\x00"), "invalid Git index change record"},
		{"invalid mode", record("100888", "M"), "invalid Git index file mode"},
		{"unknown", record("100644", "X"), `unsupported Git index change at "changed"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changes, objects, err := parseIndexChanges(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) || changes != nil || objects != nil {
				t.Fatalf("malformed record returned %+v, %+v, %v; want %q", changes, objects, err, test.want)
			}
		})
	}
}

func TestFixIndexRejectsTrackedGrowthBeforeHashing(t *testing.T) {
	for _, kind := range []string{"single file growth", "aggregate growth"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: "first", Mode: workspace.Regular, Data: []byte("one")},
				{Path: "second", Mode: workspace.Regular, Data: []byte("two")},
			})
			var data []byte
			if kind == "single file growth" {
				data = make([]byte, workspace.MaxChangeBytes+1)
				writeIndexFixture(t, source, "first", data)
			} else {
				data = make([]byte, workspace.MaxChangeBytes/2+1)
				writeIndexFixture(t, source, "first", data)
				writeIndexFixture(t, source, "second", data)
			}
			if changes, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), "tracked fix path") || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("tracked growth returned %+v, %v", changes, err)
			}
			_, err := index.run(t.Context(), 1024, nil, "cat-file", "-e", gitBlobID(data))
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("Git hashed oversized tracked data before refusal, or object lookup failed: %v", err)
			}
		})
	}
}

func TestFixIndexExcludesModelCreatedGitMetadata(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "tracked/source", Mode: workspace.Regular, Data: []byte("before")}})
	writeIndexFixture(t, source, ".GIT/config", []byte("untrusted metadata"))
	if _, err := runGit(t.Context(), filepath.Join(source, "tracked"), "init", "--quiet", "--template="); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "tracked/.Git/minion-marker", []byte("untrusted nested metadata"))
	writeIndexFixture(t, source, "tracked/source", []byte("after"))
	changes, err := index.Changes(t.Context())
	want := []workspace.Change{{Path: "tracked/source", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("model metadata affected selected contents: %+v, %v", changes, err)
	}
}

func TestFixIndexCountsModeOnlyFileContentsAgainstResultLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows Chmod does not set the executable bit for Git")
	}
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "large.sh", Mode: workspace.Regular, Data: make([]byte, workspace.MaxChangeBytes+1)}})
	if err := os.Chmod(filepath.Join(source, "large.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), `changed path "large.sh"`) || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("mode-only update did not obey full-content result limit: %v", err)
	}
}

func TestFixIndexPreservesFileDirectoryTransitions(t *testing.T) {
	for _, kind := range []string{"file to directory", "directory to file"} {
		t.Run(kind, func(t *testing.T) {
			before, after := "parent", "parent/child"
			if kind == "directory to file" {
				before, after = after, before
			}
			index, source := offlineIndexFixture(t, []workspace.File{{Path: before, Mode: workspace.Regular, Data: []byte("before")}})
			if err := os.RemoveAll(filepath.Join(source, "parent")); err != nil {
				t.Fatal(err)
			}
			writeIndexFixture(t, source, after, []byte("after"))
			changes, err := index.Changes(t.Context())
			if err != nil || len(changes) != 2 {
				t.Fatalf("%s returned %+v, %v", kind, changes, err)
			}
			foundDelete, foundAddition := false, false
			for _, change := range changes {
				if change.Path == before && change.Delete {
					foundDelete = true
				}
				if change.Path == after && !change.Delete && string(change.Data) == "after" {
					foundAddition = true
				}
			}
			if !foundDelete || !foundAddition {
				t.Fatalf("%s lost a selected transition: %+v", kind, changes)
			}
		})
	}
}

func TestFixIndexIgnoresUnusedRulesUnderIgnoredTrackedDirectory(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			dir := "vendor"
			if nested {
				dir += "/package"
			}
			tracked := dir + "/lib.go"
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("vendor/\n")},
				{Path: tracked, Mode: workspace.Regular, Data: []byte("before")},
			})
			writeIndexFixture(t, source, tracked, []byte("after"))
			writeIndexFixture(t, source, dir+"/.gitignore", []byte("!*\n!keep.tmp\n"))
			writeIndexFixture(t, source, dir+"/keep.tmp", []byte("still excluded by ancestor"))
			changes, err := index.Changes(t.Context())
			want := []workspace.Change{{Path: tracked, Mode: workspace.Regular, Data: []byte("after")}}
			if err != nil || !reflect.DeepEqual(changes, want) {
				t.Fatalf("unused ignored rules blocked tracked edit or included outputs: %+v,%v", changes, err)
			}
			t.Run("symlink control", func(t *testing.T) {
				if err := os.Remove(filepath.Join(source, dir, ".gitignore")); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, tracked, filepath.Join(source, dir, ".gitignore"))
				if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), "must be regular") {
					t.Fatalf("ignored tracked-directory control shape was not checked: %v", err)
				}
			})
		})
	}
}

func TestFixIndexPathListOverflowIsTooManyFiles(t *testing.T) {
	for _, test := range []struct{ arg, operation string }{{"ls-files", "ls-files"}, {"--raw", "diff"}} {
		t.Run(test.arg, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: "source", Mode: workspace.Regular, Data: []byte("before")}})
			writeIndexFixture(t, source, "source", []byte("after"))
			script := fmt.Sprintf("#!/bin/sh\nfor arg do\nif [ \"$arg\" = %s ]; then\nhead -c %d /dev/zero\nexit\nfi\ndone\nexec %s \"$@\"\n", shellQuote(test.arg), maxChangedPathListBytes+1, shellQuote(index.binary))
			index.binary = filepath.Join(fakeOnPath(t, "git-path-list", script), "git-path-list")
			if _, err := index.Changes(t.Context()); !errors.Is(err, ErrTooManyFiles) || errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, ErrDiffTooLarge) || !strings.Contains(err.Error(), "offline git "+test.operation+" output exceeds action limit") {
				t.Fatalf("result path overflow lost operation or classification: %v", err)
			}
		})
	}
}

func TestFixIndexPatchOverflowClassificationPreservesOtherErrors(t *testing.T) {
	for _, kind := range []string{"exact", "overflow", "failure", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: "source", Mode: workspace.Regular, Data: []byte("before")}})
			writeIndexFixture(t, source, "source", []byte("after"))
			marker := filepath.Join(t.TempDir(), "patch-started")
			behavior := fmt.Sprintf("head -c %d /dev/zero", MaxPatchBytes)
			switch kind {
			case "overflow":
				behavior = fmt.Sprintf("head -c %d /dev/zero", MaxPatchBytes+1)
			case "failure":
				behavior = "printf 'fixture failure' >&2; exit 7"
			case "canceled":
				behavior = "exec sleep 60"
			}
			script := "#!/bin/sh\nfor arg do\nif [ \"$arg\" = --binary ]; then\nprintf x > " + shellQuote(marker) + "\n" + behavior + "\nexit\nfi\ndone\nexec " + shellQuote(index.binary) + " \"$@\"\n"
			index.binary = filepath.Join(fakeOnPath(t, "git-patch", script), "git-patch")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if kind != "canceled" {
					return
				}
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := os.Stat(marker); err == nil {
						cancel()
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			changes, err := index.Changes(ctx)
			cancel()
			<-done
			if _, markerErr := os.Stat(marker); markerErr != nil {
				t.Fatalf("Changes did not reach patch-size command: %v; error=%v", markerErr, err)
			}
			switch kind {
			case "exact":
				want := []workspace.Change{{Path: "source", Mode: workspace.Regular, Data: []byte("after")}}
				if err != nil || !reflect.DeepEqual(changes, want) {
					t.Fatalf("exact patch limit rejected valid changes: %+v,%v", changes, err)
				}
			case "overflow":
				if !errors.Is(err, ErrDiffTooLarge) {
					t.Fatalf("patch overflow lost classification: %v", err)
				}
			case "failure":
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 || errors.Is(err, ErrDiffTooLarge) || !strings.Contains(err.Error(), "fixture failure") {
					t.Fatalf("ordinary patch failure lost cause: %v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDiffTooLarge) {
					t.Fatalf("canceled patch lost cause: %v", err)
				}
			}
		})
	}
	t.Run("generic output", func(t *testing.T) {
		index, _ := offlineIndexFixture(t, nil)
		index.binary = filepath.Join(fakeOnPath(t, "git-output", "#!/bin/sh\nprintf 'too much output'\n"), "git-output")
		_, err := index.run(t.Context(), 1, nil, "diff", "--raw")
		if !errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, ErrDiffTooLarge) || errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "offline git diff output exceeds action limit:") {
			t.Fatalf("generic overflow lost its operation or classification: %v", err)
		}
	})
}

func TestFixIndexClassifiesTrackedChangeCount(t *testing.T) {
	files := make([]workspace.File, workspace.MaxChanges+1)
	for i := range files {
		files[i] = workspace.File{Path: fmt.Sprintf("tracked-%03d", i), Mode: workspace.Regular, Data: []byte("before")}
	}
	index, source := offlineIndexFixture(t, files)
	for _, file := range files {
		writeIndexFixture(t, source, file.Path, []byte("after"))
	}
	if changes, err := index.Changes(t.Context()); !errors.Is(err, ErrTooManyFiles) || changes != nil {
		t.Fatalf("tracked change count lost native classification: %+v, %v", changes, err)
	}
}

func TestFixIndexReportsTrackedGrowthInPathOrder(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: "z-last", Mode: workspace.Regular},
		{Path: "a-first", Mode: workspace.Regular},
	})
	for _, name := range []string{"z-last", "a-first"} {
		file, err := os.OpenFile(filepath.Join(source, name), os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(file.Truncate(workspace.MaxChangeBytes+1), file.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := index.Changes(t.Context()); err == nil || !strings.Contains(err.Error(), `tracked fix path "a-first" exceeds changed byte limit`) {
		t.Fatalf("growth diagnostic did not select first path: %v", err)
	}
}

var fakeOnPath = testsupport.FakeOnPath

func TestOfflineIndexRefusesOverlappingDirectories(t *testing.T) {
	root := t.TempDir()
	for _, pair := range [][2]string{{root, root}, {root, filepath.Join(root, "source")}, {filepath.Join(root, "index"), root}} {
		if _, err := NewOfflineIndex(t.Context(), pair[0], pair[1], nil); err == nil || !strings.Contains(err.Error(), "directories overlap") {
			t.Fatalf("metadata=%q source=%q error=%v", pair[0], pair[1], err)
		}
	}
}

func TestFixIndexBatchesIgnoreQueriesAcrossDirectories(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	for i := 0; i < 260; i++ {
		if err := os.MkdirAll(filepath.Join(source, fmt.Sprint(i), "child"), 0700); err != nil {
			t.Fatal(err)
		}
		writeIndexFixture(t, source, fmt.Sprintf("%d/.gitignore", i), nil)
		writeIndexFixture(t, source, fmt.Sprintf("%d/child/.gitignore", i), nil)
	}
	marker := filepath.Join(t.TempDir(), "queries")
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = check-ignore ]; then printf 'query\\n' >> " + shellQuote(marker) + "; fi\ndone\nexec " + shellQuote(index.binary) + " \"$@\"\n"
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
	if err := index.checkControlFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || bytes.Count(data, []byte("query\n")) != 11 {
		t.Fatalf("ignore queries=%q error=%v; want eleven bounded directory/control batches", data, err)
	}
}

func TestFixIndexSkipsUntrackedContentsOfIgnoredTrackedDirectory(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("build/\n")},
		{Path: "build/tracked/source", Mode: workspace.Regular, Data: []byte("before")},
	})
	if _, err := index.run(t.Context(), 128, nil, "config", "core.ignoreCase", "true"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f, err := os.Create(filepath.Join(source, "build", fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.checkControlFilesWithLimit(t.Context(), 2); err != nil {
		t.Fatalf("ignored contents counted against control scan limit: %v", err)
	}
	writeIndexFixture(t, source, "build/tracked/source", []byte("after"))
	changes, err := index.Changes(t.Context())
	want := []workspace.Change{{Path: "build/tracked/source", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("ignored outputs changed tracked selection: %+v,%v", changes, err)
	}
}

func TestFixIndexBoundsCaseSpellingScanInsideIgnoredTrackedDirectory(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("build/\n")},
		{Path: "build/tracked/source", Mode: workspace.Regular, Data: []byte("before")},
	})
	if err := os.Remove(filepath.Join(source, "build", "tracked", "source")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		writeIndexFixture(t, source, fmt.Sprintf("build/tracked/cache-%d", i), nil)
	}
	if err := index.checkControlFilesWithLimit(t.Context(), 2); err != nil {
		t.Fatalf("ignored cache counted against control scan limit: %v", err)
	}
	if stale, err := index.caseRenamedPaths(t.Context(), 5); stale != nil || err == nil || !strings.Contains(err.Error(), "case-spelling scan exceeds entry limit") {
		t.Fatalf("unbounded case-spelling scan: stale=%q error=%v", stale, err)
	}
	if stale, err := index.caseRenamedPaths(t.Context(), 6); err != nil || len(stale) != 0 {
		t.Fatalf("exact case-spelling scan limit: stale=%q error=%v", stale, err)
	}
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != 1 || changes[0].Path != "build/tracked/source" || !changes[0].Delete {
		t.Fatalf("tracked deletion with bounded ignored cache: changes=%+v error=%v", changes, err)
	}
}

func TestFixIndexExportsNewIgnoreRulesAndExactChangeLimit(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	writeIndexFixture(t, source, "sub/.gitignore", []byte("*.cache\n"))
	writeIndexFixture(t, source, "sub/generated.cache", []byte("ignored"))
	for i := 1; i < workspace.MaxChanges; i++ {
		writeIndexFixture(t, source, fmt.Sprintf("sub/%03d", i), []byte("source"))
	}
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != workspace.MaxChanges {
		t.Fatalf("exact change limit: count=%d error=%v", len(changes), err)
	}
	found := false
	for _, change := range changes {
		if change.Path == "sub/.gitignore" {
			found = true
		}
		if change.Path == "sub/generated.cache" {
			t.Fatal("new ignore rule was not applied")
		}
	}
	if !found {
		t.Fatal("new ignore rules were not exported")
	}
}
