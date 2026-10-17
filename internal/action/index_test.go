package action

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/workspace"
)

func offlineIndexFixture(t *testing.T, files []workspace.File) (*fixIndex, string) {
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
	index, err := newFixIndex(context.Background(), metadata, source, files)
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
	path := filepath.Join(source, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
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
	if output, err := exec.Command(proxy, "init", "--quiet", "--template=", control).CombinedOutput(); err != nil {
		t.Fatalf("initialize SHA256 control: %s,%v", output, err)
	}
	if output, err := exec.Command(realGit, "-C", control, "rev-parse", "--show-object-format").Output(); err != nil || strings.TrimSpace(string(output)) != "sha256" {
		t.Fatalf("control did not select SHA256: %q,%v", output, err)
	}
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before")}})
	if output, err := index.run(context.Background(), 128, nil, "rev-parse", "--show-object-format"); err != nil || strings.TrimSpace(string(output)) != "sha1" {
		t.Fatalf("offline index changed its object format: %q,%v", output, err)
	}
	writeIndexFixture(t, source, "source.txt", []byte("after"))
	changes, err := index.changes(context.Background())
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "after" {
		t.Fatalf("pinned index failed byte binding: %+v,%v", changes, err)
	}
}

func TestFixIndexBaselineIsTreeWithoutCommit(t *testing.T) {
	index, _ := offlineIndexFixture(t, []workspace.File{{Path: "source", Mode: workspace.Regular, Data: []byte("baseline")}})
	typeName, err := index.run(context.Background(), 128, nil, "cat-file", "-t", index.baseline)
	if err != nil || strings.TrimSpace(string(typeName)) != "tree" {
		t.Fatalf("baseline type=%q error=%v", typeName, err)
	}
	objects, err := index.run(context.Background(), 1024, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	if err != nil || strings.Contains(string(objects), "commit") {
		t.Fatalf("offline objects=%q error=%v", objects, err)
	}
}

func TestFixIndexRejectsBaselineMetadataAliasWithoutHidingIOFailures(t *testing.T) {
	for _, name := range []string{"GIT~1/file", ".git./file"} {
		t.Run(name, func(t *testing.T) {
			source, metadata := t.TempDir(), t.TempDir()
			writeIndexFixture(t, source, name, []byte("source"))
			_, err := newFixIndex(context.Background(), metadata, source, nil)
			var exit *exec.ExitError
			if !errors.Is(err, gitwork.ErrUnsupportedSource) || !errors.As(err, &exit) {
				t.Fatalf("baseline alias error=%v", err)
			}
		})
	}
	index, _ := offlineIndexFixture(t, nil)
	index.dir = filepath.Join(t.TempDir(), "missing")
	if _, err := index.run(context.Background(), 1024, nil, "add", "--all", "--", "."); !errors.Is(err, os.ErrNotExist) || errors.Is(err, gitwork.ErrUnsupportedSource) {
		t.Fatalf("offline Git I/O error=%v", err)
	}
}

func TestFixIndexQuotesGitDiagnostic(t *testing.T) {
	index, _ := offlineIndexFixture(t, nil)
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", "#!/bin/sh\nprintf 'first line\\nforged journal entry\\n' >&2\nexit 1\n"), "git-proxy")
	_, err := index.run(context.Background(), 1024, nil, "status")
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), `"first line\nforged journal entry"`) {
		t.Fatalf("unquoted offline Git diagnostic: %v", err)
	}
}

func TestFixIndexBoundsTrackedChangesWithoutRefusingUnchangedLargeSource(t *testing.T) {
	data := make([]byte, workspace.MaxChangeBytes+1)
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "large", Mode: workspace.Regular, Data: data}})
	if changes, err := index.changes(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("unchanged source over result limit was refused: %+v,%v", changes, err)
	}
	data[len(data)-1] = 1
	writeIndexFixture(t, source, "large", data)
	if changes, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "byte limit") {
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
		t.Fatal(err)
	}
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	writeIndexFixture(t, source, "tracked.log", []byte("before"))
	changes, err = index.changes(context.Background())
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
		changes, err := index.changes(context.Background())
		if err != nil || len(changes) != 1 || !changes[0].Delete {
			t.Fatalf("first candidate=%+v, %v", changes, err)
		}
		writeIndexFixture(t, source, "tracked.log", []byte("original"))
		if changes, err := index.changes(context.Background()); err != nil || len(changes) != 0 {
			t.Fatalf("restored candidate retained staged deletion: %+v, %v", changes, err)
		}
	})
	t.Run("new output ignored in later candidate", func(t *testing.T) {
		index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular}})
		writeIndexFixture(t, source, "runtime.log", []byte("generated output"))
		if changes, err := index.changes(context.Background()); err != nil || len(changes) != 1 || changes[0].Path != "runtime.log" {
			t.Fatalf("first candidate=%+v, %v", changes, err)
		}
		writeIndexFixture(t, source, ".gitignore", []byte("*.log\n"))
		changes, err := index.changes(context.Background())
		if err != nil || len(changes) != 1 || changes[0].Path != ".gitignore" {
			t.Fatalf("later ignore retained staged output: %+v, %v", changes, err)
		}
	})
}

func TestFixIndexRejectsRelativeExecutableWhenErrDotDisabled(t *testing.T) {
	root := fakeOnPath(t, "git", "#!/bin/sh\nexit 99\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", relative)
	t.Setenv("GODEBUG", "execerrdot=0")
	if binary, err := exec.LookPath("git"); err != nil || filepath.IsAbs(binary) {
		t.Fatalf("probe did not produce a relative executable: %q, %v", binary, err)
	}
	if _, err := newFixIndex(context.Background(), filepath.Join(root, "index"), filepath.Join(root, "source"), nil); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("accepted relative Git executable: %v", err)
	}
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
	changes, err := index.changes(context.Background())
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "changed unencoded text" {
		t.Fatalf("attributes changed result: %+v, %v", changes, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository filter ran: %v", err)
	}
	index.binary = filepath.Join(fakeOnPath(t, "git-environment", "#!/bin/sh\nexec env\n"), "git-environment")
	environment, err := index.run(context.Background(), 8<<10, nil, "environment-probe")
	if err != nil {
		t.Fatal(err)
	}
	systemDisabled, systemAttributesDisabled := false, false
	for _, entry := range strings.Split(string(environment), "\n") {
		systemDisabled = systemDisabled || entry == "GIT_CONFIG_NOSYSTEM=1"
		systemAttributesDisabled = systemAttributesDisabled || entry == "GIT_ATTR_NOSYSTEM=1"
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.Contains(entry, global) || strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			t.Fatalf("unsafe offline Git environment: %q", entry)
		}
	}
	if !systemDisabled || !systemAttributesDisabled {
		t.Fatal("offline Git did not disable system configuration and attributes")
	}
}

func TestFixIndexRejectsNonIgnoredUnsafeOutput(t *testing.T) {
	for _, tt := range []struct{ kind, cause string }{
		{"symlink", `fix output "link"`},
		{"unsafe path", "not a portable regular-file path"},
		{"nested repository", `untracked nested repository output "nested/" must be removed or ignored`},
		{"oversized", "byte limit"},
		{"aggregate oversized", "byte limit"},
		{"too many", "changed file limit"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, nil)
			switch tt.kind {
			case "unsafe path":
				writeIndexFixture(t, source, "new\\output", []byte("data"))
			case "symlink":
				if err := os.Symlink("/outside/secret", filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "nested repository":
				writeIndexFixture(t, source, "nested/file", []byte("nested source"))
				nested := filepath.Join(source, "nested")
				metadata := filepath.Join(nested, ".git")
				if err := os.Mkdir(metadata, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := newFixIndex(context.Background(), metadata, nested, nil); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeIndexFixture(t, source, "huge", make([]byte, workspace.MaxChangeBytes+1))
			case "aggregate oversized":
				writeIndexFixture(t, source, "first", make([]byte, workspace.MaxChangeBytes/2+1))
				writeIndexFixture(t, source, "second", make([]byte, workspace.MaxChangeBytes/2+1))
			case "too many":
				for i := 0; i <= workspace.MaxChanges; i++ {
					writeIndexFixture(t, source, fmt.Sprintf("file-%d", i), nil)
				}
			}
			marker := filepath.Join(t.TempDir(), "add-ran")
			script := fmt.Sprintf("#!/bin/sh\nfor argument in \"$@\"; do\n if [ \"$argument\" = add ]; then touch %s; fi\ndone\nexec %s \"$@\"\n", shellQuote(marker), shellQuote(index.binary))
			index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
			if changes, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), tt.cause) {
				t.Fatalf("%s output: changes=%+v, err=%v; want %q", tt.kind, changes, err, tt.cause)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Git hashed rejected output before precheck: %v", err)
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
			changes, err := index.changes(context.Background())
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
			if changes, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("ignored untracked fix control file %q", cache+"/.gitignore")) {
				t.Fatalf("self-ignoring cache was accepted: changes=%+v error=%v", changes, err)
			}
			if err := os.RemoveAll(filepath.Join(source, cache)); err != nil {
				t.Fatal(err)
			}
			changes, err := index.changes(context.Background())
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
		changes, err := index.changes(context.Background())
		want := []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("reviewed")}}
		if err != nil || !reflect.DeepEqual(changes, want) {
			t.Fatalf("supplied ignore rule did not exclude the cache directory: changes=%+v error=%v", changes, err)
		}
	})
}

func TestFixIndexBoundsActualControlScanBeforeStaging(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.cache\n")}})
	// The tracked ignore file and intended change bring this to exactly 100000.
	for i := 0; i < workspace.MaxEntries-2; i++ {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("entry-%06d.cache", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexFixture(t, source, "fix.txt", []byte("reviewed"))
	marker := filepath.Join(t.TempDir(), "add-ran")
	script := "#!/bin/sh\nfor argument in \"$@\"; do\n if [ \"$argument\" = add ]; then touch " + shellQuote(marker) + "; fi\ndone\nexec " + shellQuote(index.binary) + " \"$@\"\n"
	index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("reviewed")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("exact control-scan bound refused the intended fix: changes=%+v error=%v", changes, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("exact-bound control did not stage output: %v", err)
	}
	if _, err := index.run(context.Background(), 1024, nil, "read-tree", index.baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(source, "fix.txt"), filepath.Join(source, "next.txt")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "overflow.cache", nil)
	if changes, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "fix directory scan exceeds entry limit") {
		t.Fatalf("oversized control scan was accepted: changes=%+v error=%v", changes, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git staged output before refusing the oversized scan: %v", err)
	}
	if staged, err := index.run(context.Background(), 1024, nil, "ls-files", "--cached", "--", "next.txt"); err != nil || len(staged) != 0 {
		t.Fatalf("refused scan left new output staged: %q,%v", staged, err)
	}
}

func TestFixIndexPreservesModeDeletionAndBinary(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "mode", Mode: workspace.Regular, Data: []byte("same")}, {Path: "deleted", Mode: workspace.Regular, Data: []byte("deleted")}})
	if err := os.Chmod(filepath.Join(source, "mode"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "binary", []byte{0, 1, 2})
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "binary", Mode: workspace.Regular, Data: []byte{0, 1, 2}}, {Path: "deleted", Delete: true}, {Path: "mode", Mode: workspace.Executable, Data: []byte("same")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
}

func TestFixIndexBoundsBinaryDiffAndCancellation(t *testing.T) {
	for _, kind := range []string{"binary addition", "large deletion", "text replacement"} {
		t.Run(kind, func(t *testing.T) {
			var files []workspace.File
			if kind == "large deletion" {
				files = []workspace.File{{Path: "deleted", Mode: workspace.Regular, Data: []byte(strings.Repeat("x", 2<<20))}}
			} else if kind == "text replacement" {
				files = []workspace.File{{Path: "replaced", Mode: workspace.Regular, Data: []byte(strings.Repeat("before\n", 100000))}}
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
			case "text replacement":
				writeIndexFixture(t, source, "replaced", []byte(strings.Repeat("after!\n", 100000)))
			}
			if _, err := index.changes(context.Background()); !errors.Is(err, gitwork.ErrDiffTooLarge) {
				t.Fatalf("oversized binary patch lost its size classification: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := index.changes(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled index = %v", err)
			}
		})
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
	if _, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "changed after indexing") {
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
		{"missing terminator", []byte(":100644"), "invalid offline git diff"},
		{"odd field count", []byte("record\x00"), "invalid offline git diff"},
		{"invalid record", []byte("wrong\x00changed\x00"), "invalid offline git change record"},
		{"invalid identity", []byte(":100644 100644 short " + current + " M\x00changed\x00"), "invalid offline git change record"},
		{"invalid mode", record("100888", "M"), "invalid offline git file mode"},
		{"unmerged", record("100644", "U"), `unsupported offline git change at "changed"`},
		{"unknown", record("100644", "X"), `unsupported offline git change at "changed"`},
		{"copy", record("100644", "C"), `unsupported offline git change at "changed"`},
		{"type change", record("120000", "T"), `unsupported offline git change at "changed"`},
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
	for _, kind := range []string{"sparse huge file", "aggregate growth"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: "first", Mode: workspace.Regular, Data: []byte("one")},
				{Path: "second", Mode: workspace.Regular, Data: []byte("two")},
			})
			if kind == "sparse huge file" {
				if err := os.Truncate(filepath.Join(source, "first"), 1<<40); err != nil {
					t.Fatal(err)
				}
			} else {
				writeIndexFixture(t, source, "first", make([]byte, workspace.MaxChangeBytes/2+1))
				writeIndexFixture(t, source, "second", make([]byte, workspace.MaxChangeBytes/2+1))
			}
			marker := filepath.Join(t.TempDir(), "add-ran")
			script := fmt.Sprintf("#!/bin/sh\nfor argument in \"$@\"; do\n if [ \"$argument\" = add ]; then touch %s; fi\ndone\nexec %s \"$@\"\n", shellQuote(marker), shellQuote(index.binary))
			index.binary = filepath.Join(fakeOnPath(t, "git-proxy", script), "git-proxy")
			if changes, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "tracked fix path") || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("tracked growth returned %+v, %v", changes, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Git hashed oversized tracked data before refusal: %v", err)
			}
		})
	}
}

func TestFixIndexExcludesModelCreatedGitMetadata(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "tracked/source", Mode: workspace.Regular, Data: []byte("before")}})
	writeIndexFixture(t, source, ".GIT/config", []byte("untrusted metadata"))
	writeIndexFixture(t, source, "tracked/.Git/config", []byte("untrusted nested metadata"))
	writeIndexFixture(t, source, "tracked/source", []byte("after"))
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "tracked/source", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("model metadata affected selected contents: %+v, %v", changes, err)
	}
}

func TestFixIndexCountsModeOnlyFileContentsAgainstResultLimit(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "large.sh", Mode: workspace.Regular, Data: make([]byte, workspace.MaxChangeBytes+1)}})
	if err := os.Chmod(filepath.Join(source, "large.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), `changed path "large.sh"`) || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("mode-only update did not obey full-content result limit: %v", err)
	}
}

func TestFixIndexOutputLimitIdentifiesGitOperation(t *testing.T) {
	index, _ := offlineIndexFixture(t, nil)
	_, err := index.run(context.Background(), 1, nil, "rev-parse", "--git-dir")
	if err == nil || !strings.Contains(err.Error(), "offline git rev-parse output exceeds action limit") {
		t.Fatalf("output refusal lacks operation: %v", err)
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
			changes, err := index.changes(context.Background())
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
			changes, err := index.changes(context.Background())
			want := []workspace.Change{{Path: tracked, Mode: workspace.Regular, Data: []byte("after")}}
			if err != nil || !reflect.DeepEqual(changes, want) {
				t.Fatalf("unused ignored rules blocked tracked edit or included outputs: %+v,%v", changes, err)
			}
			if err := os.Remove(filepath.Join(source, dir, ".gitignore")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tracked, filepath.Join(source, dir, ".gitignore")); err != nil {
				t.Fatal(err)
			}
			if _, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "must be regular") {
				t.Fatalf("ignored tracked-directory control shape was not checked: %v", err)
			}
		})
	}
}

func TestFixIndexPatchOverflowClassificationPreservesOtherErrors(t *testing.T) {
	index, _ := offlineIndexFixture(t, nil)
	index.binary = filepath.Join(fakeOnPath(t, "git-output", "#!/bin/sh\nprintf 'too much output'\n"), "git-output")
	if data, err := index.run(context.Background(), len("too much output"), nil, "diff", "--binary"); err != nil || string(data) != "too much output" {
		t.Fatalf("patch output exactly at its limit was rejected: %q,%v", data, err)
	}
	if _, err := index.run(context.Background(), 1, nil, "diff", "--binary"); !errors.Is(err, gitwork.ErrDiffTooLarge) {
		t.Fatalf("binary patch overflow lost classification: %v", err)
	}
	if _, err := index.run(context.Background(), 1, nil, "diff", "--raw"); err == nil || errors.Is(err, gitwork.ErrDiffTooLarge) || !strings.Contains(err.Error(), "output exceeds action limit") {
		t.Fatalf("raw diff overflow was classified as binary patch overflow: %v", err)
	}
	index.binary = filepath.Join(fakeOnPath(t, "git-failure", "#!/bin/sh\nprintf 'fixture failure' >&2\nexit 7\n"), "git-failure")
	_, err := index.run(context.Background(), 128, nil, "diff", "--binary")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || errors.Is(err, gitwork.ErrDiffTooLarge) || !strings.Contains(err.Error(), "fixture failure") {
		t.Fatalf("ordinary binary diff command failure lost its cause: %v", err)
	}
}
