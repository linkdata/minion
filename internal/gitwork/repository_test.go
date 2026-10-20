package gitwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

func repositorySource(t *testing.T, files []workspace.File) string {
	t.Helper()
	source := t.TempDir()
	if err := os.Chmod(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, files); err != nil {
		t.Fatal(err)
	}
	return source
}

func repositoryFixture(t *testing.T, snapshot workspace.Snapshot) (string, *Repository) {
	t.Helper()
	source := repositorySource(t, snapshot.Files)
	repo, err := MaterializeRepository(context.Background(), source, snapshot)
	if err != nil || repo == nil {
		t.Fatalf("materialize valid repository: repository=%v error=%v", repo, err)
	}
	if err := repo.Verify(context.Background()); err != nil {
		t.Fatalf("new repository failed verification: %v", err)
	}
	return source, repo
}

func assertModelRepositoryGit(t *testing.T, source, want string, args ...string) {
	t.Helper()
	out, err := testsupport.ModelGit(t, source, args...)
	if err != nil || string(out) != want {
		t.Fatalf("model git %v=%q error=%v; want %q", args, out, err, want)
	}
}

func TestRepositoryCarriesCompleteHistoryOffline(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "deleted"), "historical contents\n")
	fixture.git(fixture.source, "add", "deleted")
	fixture.git(fixture.source, "commit", "-m", "old contents")
	old := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "tag", "old-tag")
	fixture.git(fixture.source, "branch", "other")
	fixture.git(fixture.source, "rm", "deleted")
	fixture.git(fixture.source, "commit", "-m", "delete old contents")
	// Full history must extend beyond the removed 100-commit excerpt boundary.
	for i := 0; i < 101; i++ {
		fixture.git(fixture.source, "commit", "--allow-empty", "-m", "complete history")
	}
	tagBlob := fixture.gitInput(fixture.source, "tag-only historical contents\n", "hash-object", "-w", "--stdin")
	tagTree := fixture.gitInput(fixture.source, "100644 blob "+tagBlob+"\ttag-only\n", "mktree")
	tagOnly := fixture.gitInput(fixture.source, "tag-only root\n", "commit-tree", tagTree)
	fixture.git(fixture.source, "tag", "--annotate", "orphan-tag", "--message", "tag-only history", tagOnly)
	fixture.git(fixture.source, "push", fixture.remote, "main", "other", "--tags")
	checkout := fixture.keepCheckout(cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", false))
	// Preparation and all later inspection must work after the remote disappears.
	if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := repositoryFixture(t, snapshot)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"show", old + ":deleted"}, "historical contents\n"},
		{[]string{"rev-list", "--count", "HEAD"}, "104\n"},
		{[]string{"rev-list", "--count", "--all"}, "105\n"},
		{[]string{"rev-parse", "refs/tags/orphan-tag^{commit}"}, tagOnly + "\n"},
		{[]string{"show", "refs/tags/orphan-tag:tag-only"}, "tag-only historical contents\n"},
		{[]string{"rev-parse", "refs/remotes/origin/other"}, old + "\n"},
		{[]string{"rev-parse", "refs/tags/old-tag"}, old + "\n"},
		{[]string{"rev-parse", "--is-shallow-repository"}, "false\n"},
		{[]string{"remote"}, ""},
	} {
		assertModelRepositoryGit(t, source, tt.want, tt.args...)
	}
	config, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil || strings.Contains(string(config), fixture.remote) || strings.Contains(string(config), "promisor") {
		t.Fatalf("source config leaked: %s %v", config, err)
	}
	if _, err := os.Stat(filepath.Join(source, ".git", "hooks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hooks copied: %v", err)
	}
	t.Run("wrong merge base", func(t *testing.T) {
		wrong := snapshot
		wrong.Base, wrong.MergeBase = snapshot.Head, old
		repo, err := MaterializeRepository(context.Background(), repositorySource(t, wrong.Files), wrong)
		if repo != nil || err == nil || !strings.Contains(err.Error(), "merge-base differs") {
			t.Fatalf("mismatched comparison returned repository=%v error=%v", repo, err)
		}
	})
}

func TestRepositoryRejectsInvalidOrMismatchedInput(t *testing.T) {
	fixture := newFixture(t)
	original, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A valid control keeps setup failures from making every rejection pass.
	repositoryFixture(t, original)
	tree := fixture.git(fixture.checkout.path, "rev-parse", "HEAD^{tree}")
	for _, scenario := range []struct {
		name string
		want string
		exit bool
	}{
		{"missing", "complete repository bundle", false},
		{"malformed", "does not look like a v2 or v3 bundle file", true},
		{"truncated", "index-pack died", true},
		{"oversized", "snapshot context exceeds limits", false},
		{"invalid ref", "invalid repository reference", false},
		{"missing head", "cat-file", true},
		{"head not commit", "source identity is not a commit", false},
		{"base not commit", "source identity is not a commit", false},
		{"merge base not commit", "source identity is not a commit", false},
		{"wrong contents", "source files differ from pinned repository", false},
		{"wrong mode", "source files differ from pinned repository", false},
		{"missing file", "source files differ from pinned repository", false},
		{"extra file", "source files absent from pinned repository", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot := original
			snapshot.Files = append([]workspace.File(nil), original.Files...)
			switch scenario.name {
			case "missing":
				snapshot.Repository = nil
			case "malformed":
				snapshot.Repository = []byte("not a bundle")
			case "truncated":
				snapshot.Repository = snapshot.Repository[:len(snapshot.Repository)-1]
			case "oversized":
				snapshot.Repository = make([]byte, workspace.MaxBytes+1)
			case "invalid ref":
				_, pack, ok := bytes.Cut(original.Repository, []byte("\n\n"))
				if !ok {
					t.Fatal("valid bundle is missing its pack separator")
				}
				snapshot.Repository = append([]byte("# v2 git bundle\n"+original.Head+" foo\n\n"), pack...)
			case "missing head":
				snapshot.Head = strings.Repeat("a", 40)
			case "head not commit":
				snapshot.Head = tree
			case "base not commit":
				snapshot.Base, snapshot.MergeBase = tree, original.Head
			case "merge base not commit":
				snapshot.Base, snapshot.MergeBase = original.Head, tree
			case "wrong contents":
				snapshot.Files[0].Data = []byte("forged")
			case "wrong mode":
				snapshot.Files[0].Mode = workspace.Executable
			case "missing file":
				snapshot.Files = nil
			case "extra file":
				snapshot.Files = append(snapshot.Files, workspace.File{Path: "extra", Mode: workspace.Regular})
			}
			repo, err := MaterializeRepository(context.Background(), repositorySource(t, snapshot.Files), snapshot)
			if repo != nil || err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("invalid repository=%v error=%v; want %q", repo, err, scenario.want)
			}
			var exit *exec.ExitError
			if scenario.exit && !errors.As(err, &exit) {
				t.Fatalf("Git rejection lost its native exit cause: %v", err)
			}
		})
	}
}

func TestRepositoryRejectsIncompleteBundle(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repositoryFixture(t, snapshot)
	// List one object explicitly: this pack omits its tree and blob without any promisor configuration.
	pack, _, err := fixture.checkout.boundedGit(context.Background(), strings.NewReader(snapshot.Head+"\n"), workspace.MaxBytes, false, "pack-objects", "--stdout")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Repository = append([]byte("# v2 git bundle\n"+snapshot.Head+" refs/heads/main\n\n"), pack...)
	repo, err := MaterializeRepository(context.Background(), repositorySource(t, snapshot.Files), snapshot)
	var exit *exec.ExitError
	if repo != nil || err == nil || !strings.Contains(err.Error(), "incomplete repository") || !errors.As(err, &exit) {
		t.Fatalf("incomplete bundle returned repository=%v error=%v", repo, err)
	}
}

func TestRepositoryRejectsIncrementalBundle(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "commit", "--allow-empty", "-m", "new commit")
	bundle, _, err := (&Checkout{path: fixture.source}).boundedGit(context.Background(), nil, workspace.MaxBytes, false, "bundle", "create", "-", fixture.base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repositoryFixture(t, snapshot)
	snapshot.Repository = bundle
	repo, err := MaterializeRepository(context.Background(), repositorySource(t, snapshot.Files), snapshot)
	var exit *exec.ExitError
	if repo != nil || err == nil || !strings.Contains(err.Error(), "prerequisite") || !errors.As(err, &exit) {
		t.Fatalf("incremental bundle returned repository=%v error=%v", repo, err)
	}
}

func TestRepositoryRejectsUnrelatedComparison(t *testing.T) {
	fixture := newFixture(t)
	tree := fixture.git(fixture.checkout.path, "rev-parse", "HEAD^{tree}")
	unrelated := fixture.gitInput(fixture.checkout.path, "unrelated root\n", "commit-tree", tree)
	fixture.git(fixture.checkout.path, "update-ref", "refs/heads/unrelated", unrelated)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repositoryFixture(t, snapshot)
	snapshot.Base, snapshot.MergeBase = unrelated, fixture.base
	repo, err := MaterializeRepository(context.Background(), repositorySource(t, snapshot.Files), snapshot)
	var exit *exec.ExitError
	if repo != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(err.Error(), "merge-base") {
		t.Fatalf("unrelated comparison returned repository=%v error=%v", repo, err)
	}
}

func TestRepositoryOmitsLinksWithoutReportingDeletions(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Symlink("../outside", filepath.Join(fixture.source, "link")); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", "link")
	fixture.git(fixture.source, "update-index", "--add", "--cacheinfo", "160000,"+fixture.base+",sub")
	fixture.git(fixture.source, "commit", "-m", "omitted symlink and gitlink")
	fixture.git(fixture.source, "push", fixture.remote, "main")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source, repo := repositoryFixture(t, snapshot)
	assertModelRepositoryGit(t, source, "", "status", "--porcelain")
	assertModelRepositoryGit(t, source, "S link\nS sub\n", "ls-files", "-v", "--", "link", "sub")
	for _, name := range []string{"link", "sub"} {
		if _, err := os.Lstat(filepath.Join(source, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("omitted path %q materialized: %v", name, err)
		}
	}
	assertModelRepositoryGit(t, source, "../outside", "show", "HEAD:link")
	assertModelRepositoryGit(t, source, "160000 commit "+fixture.base+"\tsub\n", "ls-tree", "HEAD", "sub")
	if err := repo.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "sub"} {
		t.Run("undeclared "+name, func(t *testing.T) {
			invalid := snapshot
			invalid.Omitted = nil
			for _, other := range snapshot.Omitted {
				if other != name {
					invalid.Omitted = append(invalid.Omitted, other)
				}
			}
			repo, err := MaterializeRepository(context.Background(), repositorySource(t, invalid.Files), invalid)
			if repo != nil || err == nil || !strings.Contains(err.Error(), "undeclared omitted source") {
				t.Fatalf("undeclared %s returned repository=%v error=%v", name, repo, err)
			}
		})
	}
}

func TestRepositoryAcceptsLegacyHistoricalDate(t *testing.T) {
	fixture := newFixture(t)
	tree := fixture.git(fixture.source, "rev-parse", "HEAD^{tree}")
	raw := "tree " + tree + "\nparent " + fixture.base + "\nauthor Test <test@example.invalid> 01234567890 +0000\ncommitter Test <test@example.invalid> 01234567890 +0000\n\nlegacy imported date\n"
	legacy := fixture.gitInput(fixture.source, raw, "hash-object", "--literally", "-t", "commit", "-w", "--stdin")
	head := fixture.gitInput(fixture.source, "current valid date\n", "commit-tree", tree, "-p", legacy)
	fixture.git(fixture.source, "update-ref", "refs/heads/main", head)
	if _, err := runGit(context.Background(), fixture.source, "fsck", "--full", "--no-dangling"); err == nil || !strings.Contains(err.Error(), "zeroPaddedDate") {
		t.Fatalf("legacy-date control did not trigger strict fsck: %v", err)
	}
	fixture.git(fixture.source, "push", fixture.remote, "main")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := repositoryFixture(t, snapshot)
	assertModelRepositoryGit(t, source, "3\n", "rev-list", "--count", "HEAD")
	assertModelRepositoryGit(t, source, raw, "cat-file", "commit", legacy)
}

func TestRepositoryPinsFilesRefFormat(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	proxy := filepath.Join(fakeOnPath(t, "git", "#!/bin/sh\nGIT_DEFAULT_REF_FORMAT=reftable exec "+shellQuote(realGit)+" \"$@\"\n"), "git")
	control := filepath.Join(t.TempDir(), "control")
	cmd := exec.Command(proxy, "init", "--quiet", "--template=", control)
	cmd.Env = testsupport.GitEnvironment(t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize reftable control: %q, %v", output, err)
	}
	cmd = exec.Command(realGit, "-C", control, "rev-parse", "--show-ref-format")
	cmd.Env = testsupport.GitEnvironment(t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != "reftable\n" {
		t.Fatalf("control did not select reftable: %q, %v", output, err)
	}
	source, _ := repositoryFixture(t, snapshot)
	assertModelRepositoryGit(t, source, "files\n", "rev-parse", "--show-ref-format")
}

func TestRepositoryPacksRefsWithoutReflogs(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	header, pack, ok := bytes.Cut(snapshot.Repository, []byte("\n\n"))
	if !ok {
		t.Fatal("valid bundle is missing its pack separator")
	}
	var refs strings.Builder
	refs.Write(header)
	refs.WriteByte('\n')
	const count = 512
	for i := 0; i < count; i++ {
		fmt.Fprintf(&refs, "%s refs/heads/many/branch-%04d\n%s refs/tags/many/tag-%04d\n", snapshot.Head, i, snapshot.Head, i)
	}
	snapshot.Repository = append([]byte(refs.String()+"\n"), pack...)
	source, repo := repositoryFixture(t, snapshot)
	out, err := testsupport.ModelGit(t, source, "for-each-ref", "--format=%(refname)", "refs/heads/many", "refs/tags/many")
	if err != nil || len(strings.Fields(string(out))) != 2*count {
		t.Fatalf("packed refs lost: count=%d error=%v", len(strings.Fields(string(out))), err)
	}
	if data, err := os.ReadFile(filepath.Join(source, ".git", "packed-refs")); err != nil || !bytes.Contains(data, []byte("refs/heads/many/branch-0511")) || !bytes.Contains(data, []byte("refs/tags/many/tag-0511")) {
		t.Fatalf("refs were not packed: %v", err)
	}
	refsDir := filepath.Join(source, ".git", "refs")
	if err := filepath.WalkDir(refsDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return fmt.Errorf("loose ref remains at %s", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(source, ".git", "logs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialization wrote reflogs: %v", err)
	}
	if err := repo.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPinsModelConfigAndDisablesLogMailmap(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, ".mailmap"), "Forged <forged@example.invalid> Test <test@example.invalid>\n")
	fixture.commit("refs/heads/main", "untrusted author mapping")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := repositoryFixture(t, snapshot)
	assertModelRepositoryGit(t, source, snapshot.Head+"\n", "rev-parse", "refs/remotes/origin/HEAD")
	for _, setting := range []struct{ key, want string }{
		{"core.hooksPath", "/dev/null\n"}, {"core.fsmonitor", "false\n"}, {"core.fileMode", "true\n"},
		{"core.excludesFile", "/dev/null\n"}, {"core.attributesFile", "/dev/null\n"},
		{"core.logAllRefUpdates", "false\n"}, {"core.pager", "cat\n"}, {"log.mailmap", "false\n"},
		{"protocol.allow", "never\n"}, {"credential.helper", "\n"}, {"gc.auto", "0\n"}, {"maintenance.auto", "false\n"},
	} {
		assertModelRepositoryGit(t, source, setting.want, "config", "--get", setting.key)
	}
	control, err := testsupport.ModelGit(t, source, "-c", "log.mailmap=true", "log", "-1", "--format=fuller")
	if err != nil || !bytes.Contains(control, []byte("Forged <forged@example.invalid>")) {
		t.Fatalf("mailmap control did not rewrite authors: %q error=%v", control, err)
	}
	out, err := testsupport.ModelGit(t, source, "log", "-1", "--format=fuller")
	if err != nil || bytes.Contains(out, []byte("Forged")) || !bytes.Contains(out, []byte("Test <test@example.invalid>")) {
		t.Fatalf("model log applied untrusted mailmap: %q error=%v", out, err)
	}
}

func TestRepositoryIgnoresHostGitDefaults(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, ".gitattributes"), "README.md export-subst\n")
	fixture.commit("refs/heads/main", "repository attribute control")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := repositoryFixture(t, snapshot)
	writeFile(t, filepath.Join(source, "debug.log"), "candidate output\n")
	home := t.TempDir()
	ignore := filepath.Join(home, ".config", "git", "ignore")
	attributes := filepath.Join(home, ".config", "git", "attributes")
	writeFile(t, ignore, "debug.log\n")
	writeFile(t, attributes, "* export-ignore\n")
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	// This regression deliberately supplies a nonempty synthetic HOME instead of the shared helper's empty HOME.
	env := append([]string{"PATH=" + safePath(os.Getenv("PATH")), "HOME=" + home, "TMPDIR=" + home, "LC_ALL=C"}, aicli.ModelGitEnvironment()...)
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--porcelain"}, "?? debug.log\n"},
		{[]string{"check-attr", "export-ignore", "export-subst", "--", "README.md"}, "README.md: export-ignore: unspecified\nREADME.md: export-subst: set\n"},
		{[]string{"-c", "core.excludesFile=" + ignore, "status", "--porcelain"}, ""},
		{[]string{"-c", "core.attributesFile=" + attributes, "check-attr", "export-ignore", "--", "README.md"}, "README.md: export-ignore: set\n"},
	} {
		cmd := exec.CommandContext(context.Background(), binary, check.args...)
		cmd.Dir, cmd.Env = source, env
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != check.want {
			t.Fatalf("model Git with host defaults %v=%q error=%v; want %q", check.args, out, err, check.want)
		}
	}
}

func TestRepositoryRefreshesOriginalIndexAndPreservesCandidateChanges(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "deleted"), "original\n")
	fixture.commit("refs/heads/main", "candidate baseline")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []bool{false, true} {
		t.Run(fmt.Sprint("candidate=", candidate), func(t *testing.T) {
			files := snapshot.Files
			if candidate {
				files = []workspace.File{{Path: "README.md", Mode: workspace.Regular, Data: []byte("candidate\n")}, {Path: "added", Mode: workspace.Regular, Data: []byte("new\n")}}
			}
			source := repositorySource(t, files)
			repo, err := MaterializeRepository(context.Background(), source, snapshot)
			if err != nil || repo == nil {
				t.Fatalf("materialize candidate=%t: repository=%v error=%v", candidate, repo, err)
			}
			if err := repo.Verify(context.Background()); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(source, ".git")
			t.Cleanup(func() {
				if err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() {
						return os.Chmod(name, 0700)
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
			if err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				mode := fs.FileMode(0400)
				if entry.IsDir() {
					mode = 0500
				}
				return os.Chmod(name, mode)
			}); err != nil {
				t.Fatal(err)
			}
			want := ""
			if candidate {
				want = " M README.md\n D deleted\n?? added\n"
			}
			assertModelRepositoryGit(t, source, want, "status", "--porcelain")
			for _, args := range [][]string{{"diff-index", "--quiet", "HEAD"}, {"diff-files", "--quiet"}, {"diff", "--quiet"}} {
				out, err := testsupport.ModelGit(t, source, args...)
				var exit *exec.ExitError
				if (!candidate && err != nil) || (candidate && (!errors.As(err, &exit) || exit.ExitCode() != 1)) || len(out) != 0 {
					t.Fatalf("candidate=%t git %v=%q error=%v", candidate, args, out, err)
				}
			}
		})
	}
}

func TestRepositoryVerifyRejectsMetadataMutations(t *testing.T) {
	fixture := newFixture(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"HEAD contents", "Git metadata changed"}, {"config contents", "Git metadata changed"},
		{"added file", "Git metadata changed"}, {"chmod", "Git metadata changed"},
		{"root symlink", "repository metadata is not regular"}, {"root file", "Git metadata changed"},
		{"added symlink", "repository metadata is not regular"}, {"hard link", "exactly one hard link"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			source, repo := repositoryFixture(t, snapshot)
			dir := filepath.Join(source, ".git")
			var err error
			switch scenario.name {
			case "HEAD contents":
				err = os.WriteFile(filepath.Join(dir, "HEAD"), []byte(strings.Repeat("a", 40)+"\n"), 0600)
			case "config contents":
				err = os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\nbare = true\n"), 0600)
			case "added file":
				err = os.WriteFile(filepath.Join(dir, "added"), []byte("untrusted"), 0600)
			case "chmod":
				err = os.Chmod(filepath.Join(dir, "config"), 0400)
			case "root symlink", "root file":
				saved := filepath.Join(t.TempDir(), "saved.git")
				if err = os.Rename(dir, saved); err == nil {
					if scenario.name == "root symlink" {
						err = os.Symlink(saved, dir)
					} else {
						err = os.WriteFile(dir, []byte("replacement metadata"), 0600)
					}
				}
			case "added symlink":
				err = os.Symlink("config", filepath.Join(dir, "added"))
			case "hard link":
				err = os.Link(filepath.Join(dir, "config"), filepath.Join(t.TempDir(), "extra-link"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("metadata mutation %q error=%v; want %q", scenario.name, err, scenario.want)
			}
		})
	}
}

func TestRepositoryVerifyBoundsMetadata(t *testing.T) {
	dir := t.TempDir()
	digest, err := repositoryDigest(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{dir: dir, digest: digest}
	name := filepath.Join(dir, "oversized")
	writeSparse(t, name, workspace.MaxBytes+1)
	if err := repo.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "file exceeds byte limit") {
		t.Fatalf("oversized metadata error=%v", err)
	}
}

func TestRepositoryDigestEntryLimitCountsRoot(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < workspace.MaxEntries; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repositoryDigest(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "repository metadata exceeds entry limit") {
		t.Fatalf("wide metadata directory error=%v", err)
	}
	if err := os.Remove(filepath.Join(dir, "0")); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryDigest(context.Background(), dir); err != nil {
		t.Fatalf("exact metadata entry limit, including root: %v", err)
	}
}

func TestRepositoryDigestUsesReverseLexicalDepthFirstOrder(t *testing.T) {
	for _, names := range [][]string{{"z/second", "a/first"}, {"a/first", "z/second"}} {
		dir := t.TempDir()
		for _, name := range names {
			writeFile(t, filepath.Join(dir, name), name)
		}
		// The oracle uses a fixed traversal, independent of ReadDir and production sorting.
		hash := sha256.New()
		for _, name := range []string{".", "z", "z/second", "a", "a/first"} {
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(hash, "%s\x00%d\x00", name, info.Mode())
			if info.Mode().IsRegular() {
				fmt.Fprintf(hash, "%d\x00%s", len(name), name)
			}
		}
		digest, err := repositoryDigest(context.Background(), dir)
		if err != nil || !bytes.Equal(digest[:], hash.Sum(nil)) {
			t.Fatalf("metadata digest=%x error=%v; want %x", digest, err, hash.Sum(nil))
		}
	}
}

func TestRepositoryDigestBoundsTotalMetadata(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int64{"a": workspace.MaxBytes, "b": workspace.MaxBytes, "c": 1} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repositoryDigest(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "file exceeds byte limit") {
		t.Fatalf("metadata total byte limit error=%v", err)
	}
	if err := os.Remove(filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryDigest(context.Background(), dir); err != nil {
		t.Fatalf("exact metadata total byte limit: %v", err)
	}
}

func TestMaterializeRepositoryRequiresPrivateRoot(t *testing.T) {
	snapshot := workspace.Snapshot{Head: strings.Repeat("a", 40), Repository: []byte("bundle")}
	for _, kind := range []string{"public directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := root
			if kind == "public directory" {
				if err := os.Chmod(source, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				source = filepath.Join(t.TempDir(), "source")
				if err := os.Symlink(root, source); err != nil {
					t.Fatal(err)
				}
			}
			repository, err := MaterializeRepository(context.Background(), source, snapshot)
			if repository != nil || err == nil || !strings.Contains(err.Error(), "workspace root must be a private directory") {
				t.Fatalf("private-root check returned repository=%v error=%v", repository, err)
			}
			if _, err := os.Lstat(filepath.Join(root, ".git")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected root received Git metadata: %v", err)
			}
		})
	}
}
