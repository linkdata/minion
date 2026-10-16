package action

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git object identity, not a security signature.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

// fixIndex keeps Git metadata outside every model grant. It has no remotes,
// repository configuration, credential environment, hooks or checkout filters.
type fixIndex struct {
	dir, source, binary string
}

func newFixIndex(ctx context.Context, dir, source string) (*fixIndex, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("offline fix index requires git: %w", err)
	}
	if !filepath.IsAbs(binary) {
		return nil, errors.New("offline git binary must have an absolute path")
	}
	index := &fixIndex{dir: dir, source: source, binary: binary}
	if _, err := index.run(ctx, nil, 1024, "init", "--quiet", "--template="); err != nil {
		return nil, err
	}
	// info/attributes overrides untrusted .gitattributes even for forced baseline files.
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "info", "attributes"), []byte("* -text -ident -filter -working-tree-encoding !diff\n"), 0600); err != nil {
		return nil, err
	}
	if _, err := index.run(ctx, nil, 1024, "add", "--all", "--force", "--", "."); err != nil {
		return nil, err
	}
	tree, err := index.run(ctx, nil, 128, "write-tree")
	if err != nil {
		return nil, err
	}
	treeID := strings.TrimSpace(string(tree))
	if !gitObjectID(treeID) {
		return nil, errors.New("offline git returned invalid tree identity")
	}
	commit, err := index.run(ctx, nil, 128, "commit-tree", treeID, "-m", "Source baseline")
	if err != nil {
		return nil, err
	}
	commitID := strings.TrimSpace(string(commit))
	if !gitObjectID(commitID) {
		return nil, errors.New("offline git returned invalid baseline identity")
	}
	if _, err := index.run(ctx, nil, 1024, "update-ref", "HEAD", commitID); err != nil {
		return nil, err
	}
	return index, nil
}

func (index *fixIndex) changes(ctx context.Context) ([]workspace.Change, error) {
	if _, err := index.run(ctx, nil, 1024, "add", "--all", "--", ".", ":(exclude,icase,glob)**/.git", ":(exclude,icase,glob)**/.git/**"); err != nil {
		return nil, err
	}
	raw, err := index.run(ctx, nil, (workspace.MaxChanges+1)*(4096+128), "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	selected, objects, err := parseIndexChanges(raw, workspace.MaxChanges)
	if err != nil {
		return nil, err
	}
	changes, err := workspace.ReadChanges(ctx, index.source, selected)
	if err != nil {
		return nil, err
	}
	total := 0
	for i, change := range changes {
		total += len(change.Data)
		if total > workspace.MaxChangeBytes {
			return nil, errors.New("fix result exceeds changed byte limit")
		}
		if !change.Delete && gitBlobID(change.Data) != objects[i] {
			return nil, errors.New("fix file changed after indexing")
		}
	}
	if _, err := index.run(ctx, nil, workspace.MaxContextBytes, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short", "HEAD", "--"); err != nil {
		return nil, err
	}
	return changes, nil
}

func parseIndexChanges(raw []byte, limit int) ([]workspace.Change, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, nil, errors.New("invalid offline git diff")
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	if len(parts)%2 != 0 || len(parts)/2 > limit {
		return nil, nil, errors.New("fix result exceeds changed file limit")
	}
	changes, objects := make([]workspace.Change, 0, len(parts)/2), make([]string, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		fields := strings.Fields(string(parts[i]))
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || len(fields[1]) != 6 || !gitObjectID(fields[2]) || !gitObjectID(fields[3]) {
			return nil, nil, errors.New("invalid offline git change record")
		}
		mode, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			return nil, nil, errors.New("invalid offline git file mode")
		}
		change := workspace.Change{Path: string(parts[i+1]), Mode: uint32(mode)}
		switch fields[4] {
		case "D":
			change.Delete, change.Mode = true, 0
		case "A", "M", "T":
			if change.Mode != workspace.Regular && change.Mode != workspace.Executable {
				return nil, nil, errors.New("fix changed a non-regular file")
			}
		default:
			return nil, nil, errors.New("unsupported offline git change")
		}
		changes, objects = append(changes, change), append(objects, fields[3])
	}
	if err := workspace.ValidateChanges(changes); err != nil {
		return nil, nil, err
	}
	return changes, objects, nil
}

func gitObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func gitBlobID(data []byte) string {
	// #nosec G401 -- this reproduces the private index's SHA-1 object identifier.
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func (index *fixIndex) run(ctx context.Context, stdin io.Reader, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gitArgs := []string{"--git-dir=" + index.dir, "--work-tree=" + index.source,
		"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null", "-c", "core.excludesFile=/dev/null", "-c", "core.fileMode=true",
		"-c", "core.protectNTFS=true", "-c", "core.protectHFS=true", "-c", "commit.gpgsign=false",
		"-c", "protocol.allow=never", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	cmd := exec.CommandContext(ctx, index.binary, append(gitArgs, args...)...)
	cmd.Dir, cmd.Env, cmd.Stdin = index.dir, index.environment(), stdin
	cmd.WaitDelay = 2 * time.Second
	output, diagnostic := indexBuffer{limit: limit, cancel: cancel}, indexBuffer{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	err := cmd.Run()
	if output.exceeded {
		return nil, errors.New("offline git output exceeds action limit")
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			err = errors.Join(err, contextErr)
		}
		return nil, fmt.Errorf("offline git %s: %w: %s", args[0], err, strings.TrimSpace(string(diagnostic.data)))
	}
	return output.data, nil
}

func (index *fixIndex) environment() []string {
	var paths []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			paths = append(paths, dir)
		}
	}
	if len(paths) == 0 {
		paths = []string{"/usr/bin", "/bin"}
	}
	return []string{"PATH=" + strings.Join(paths, string(os.PathListSeparator)), "HOME=" + index.dir, "TMPDIR=" + index.dir, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_AUTHOR_NAME=source", "GIT_AUTHOR_EMAIL=source@localhost", "GIT_COMMITTER_NAME=source", "GIT_COMMITTER_EMAIL=source@localhost",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
}

type indexBuffer struct {
	data     []byte
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *indexBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if len(data) > b.limit-len(b.data) {
		data, b.exceeded = data[:b.limit-len(b.data)], true
		if b.cancel != nil {
			b.cancel()
		}
	}
	b.data = append(b.data, data...)
	return n, nil
}
