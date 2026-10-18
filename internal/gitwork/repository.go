package gitwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/workspace"
)

// Repository owns sanitized, immutable model-visible Git metadata.
type Repository struct {
	dir    string
	digest [32]byte
}

// MaterializeRepository imports only objects/refs into fresh offline metadata; it never checks out source or copies configuration.
func MaterializeRepository(ctx context.Context, source string, snapshot workspace.Snapshot) (*Repository, error) {
	if err := workspace.ValidateSnapshot(snapshot); err != nil {
		return nil, err
	}
	if err := workspace.CheckPrivateRoot(source); err != nil {
		return nil, err
	}

	if len(snapshot.Repository) == 0 {
		return nil, errors.New("missing complete repository bundle")
	}
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(source, ".git")
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	index := &OfflineIndex{dir: dir, source: source, binary: binary}
	run := func(args ...string) ([]byte, error) { return index.run(ctx, maxPathListBytes, nil, args...) }
	if _, err := run("init", "--quiet", "--template=", "--object-format=sha1"); err != nil {
		return nil, err
	}
	if err := disableAttributes(dir); err != nil {
		return nil, err
	}
	// Only fixed offline settings reach model commands; refs need neither reflogs nor a host identity.
	const config = `[core]
repositoryformatversion = 0
bare = false
filemode = true
hooksPath = /dev/null
fsmonitor = false
logAllRefUpdates = false
pager = cat
[log]
mailmap = false
[protocol]
allow = never
[credential]
helper =
[gc]
auto = 0
[maintenance]
auto = false
`
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0600); err != nil {
		return nil, err
	}
	bundle, err := os.CreateTemp(dir, ".minion-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(bundle.Name())
	if _, err := bundle.Write(snapshot.Repository); err != nil {
		_ = bundle.Close()
		return nil, err
	}
	if err := bundle.Close(); err != nil {
		return nil, err
	}
	// Unbundle checks prerequisites before importing into the empty repository.
	refs, err := run("bundle", "unbundle", bundle.Name())
	if err != nil {
		return nil, err
	}
	packs, err := os.ReadDir(filepath.Join(dir, "objects", "pack"))
	if err != nil {
		return nil, err
	}
	for _, pack := range packs {
		if strings.HasSuffix(pack.Name(), ".promisor") {
			return nil, errors.New("incomplete repository: filtered bundles are unsupported")
		}
	}
	var updates strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if !ok || !workspace.ValidSHA(sha) || (name != "HEAD" && !strings.HasPrefix(name, "refs/")) || strings.ContainsAny(name, "\r\n\x00 ") {
			return nil, errors.New("invalid repository reference")
		}
		if name != "HEAD" {
			fmt.Fprintf(&updates, "create %s %s\n", name, sha)
		}
	}
	if _, err := index.run(ctx, 1024, []byte(updates.String()), "update-ref", "--stdin"); err != nil {
		return nil, err
	}
	if _, err := run("pack-refs", "--all"); err != nil {
		return nil, err
	}
	for _, sha := range []string{snapshot.Head, snapshot.Base, snapshot.MergeBase} {
		if sha != "" {
			kind, err := run("cat-file", "-t", sha)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(string(kind)) != "commit" {
				return nil, errors.New("source identity is not a commit")
			}
		}
	}
	if _, err := run("update-ref", "--no-deref", "HEAD", snapshot.Head); err != nil {
		return nil, err
	}
	if _, err := run("fsck", "--connectivity-only", "--no-dangling"); err != nil {
		return nil, fmt.Errorf("incomplete repository: %w", err)
	}
	if snapshot.Base != "" {
		base, err := run("merge-base", snapshot.Base, snapshot.Head)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(base)) != snapshot.MergeBase {
			return nil, errors.New("comparison merge-base differs from repository")
		}
	}
	if _, err := run("read-tree", snapshot.Head); err != nil {
		return nil, err
	}
	entries, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	files := make(map[string]workspace.File, len(snapshot.Files))
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	omittedPaths := make(map[string]bool, len(snapshot.Omitted))
	for _, name := range snapshot.Omitted {
		omittedPaths[name] = true
	}
	var omitted bytes.Buffer
	for _, entry := range bytes.Split(entries, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || fields[2] != "0" {
			return nil, errors.New("invalid repository index entry")
		}
		if fields[0] == "120000" || fields[0] == "160000" {
			if !omittedPaths[string(name)] {
				return nil, errors.New("repository has undeclared omitted source")
			}
			omitted.Write(name)
			omitted.WriteByte(0)
			continue
		}
		file, exists := files[string(name)]
		if !exists || fmt.Sprintf("%06o", file.Mode) != fields[0] || gitBlobID(file.Data) != fields[1] {
			return nil, errors.New("source files differ from pinned repository")
		}
		delete(files, string(name))
	}
	if len(files) != 0 {
		return nil, errors.New("source files absent from pinned repository")
	}
	if omitted.Len() > 0 {
		if _, err := index.run(ctx, 1024, omitted.Bytes(), "update-index", "--skip-worktree", "-z", "--stdin"); err != nil {
			return nil, err
		}
	}
	if _, err := run("update-index", "-q", "--refresh", "--ignore-missing"); err != nil {
		return nil, err
	}
	if err := os.Remove(bundle.Name()); err != nil {
		return nil, err
	}
	repo := &Repository{dir: dir}
	if repo.digest, err = repositoryDigest(ctx, dir); err != nil {
		return nil, err
	}
	return repo, nil
}

// Verify detects changes without executing model-visible Git configuration.
func (r *Repository) Verify(ctx context.Context) error {
	digest, err := repositoryDigest(ctx, r.dir)
	if err != nil {
		return err
	}
	if digest != r.digest {
		return errors.New("Git metadata changed")
	}
	return nil
}

func repositoryDigest(ctx context.Context, dir string) (sum [32]byte, err error) {
	hash := sha256.New()
	count, total := 0, 0
	err = filepath.WalkDir(dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > workspace.MaxEntries {
			return errors.New("repository metadata exceeds entry limit")
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", rel, info.Mode())
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("repository metadata is not regular")
		}
		data, err := workspace.ReadRegular(name, int64(min(workspace.MaxBytes, 2*workspace.MaxBytes-total)))
		if err != nil {
			return err
		}
		total += len(data)
		fmt.Fprintf(hash, "%d\x00", len(data))
		hash.Write(data)
		return nil
	})
	copy(sum[:], hash.Sum(nil))
	return sum, err
}
