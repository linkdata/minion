package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workspace"
)

const maxGitCheckBundleBytes = 1 << 20

var gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)\.([0-9]+)(?:\.windows\.[0-9]+)?(?: \([^()[:cntrl:]]+\))?$`)

// Check proves Git 2.45.1 or newer supports the synthetic offline bundle/import/index operations.
// The caller admits workRoot and process ownership before calling; no project data is used.
func Check(ctx context.Context, workRoot string) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	workRoot, err := workspace.CanonicalPath(workRoot)
	if err != nil {
		return fmt.Errorf("git readiness work root: %w", err)
	}
	root, err := os.MkdirTemp(workRoot, ".minion-git-check-")
	if err != nil {
		return err
	}
	defer func() { resultErr = finishGitCheck(root, resultErr) }()
	source, metadata, imported := filepath.Join(root, "source"), filepath.Join(root, "index"), filepath.Join(root, "import")
	for _, dir := range []string{source, metadata, imported} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
	}
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		return fmt.Errorf("git readiness requires git: %w", err)
	}
	probe := &OfflineIndex{dir: metadata, source: source, binary: binary}
	version, err := probe.run(ctx, 1024, nil, "--version")
	if err != nil {
		return err
	}
	if !supportedGitVersion(string(version)) {
		return fmt.Errorf("git readiness requires Git 2.45.1 or later; reported %q", strings.TrimSpace(string(version)))
	}
	files := []workspace.File{{Path: "probe.txt", Mode: workspace.Regular, Data: []byte("git readiness before\n")}}
	if err := workspace.Materialize(source, files); err != nil {
		return err
	}
	index, err := NewOfflineIndex(ctx, metadata, source, files)
	if err != nil {
		return err
	}
	commit := fmt.Sprintf("tree %s\nauthor Minion <minion@localhost> 1 +0000\ncommitter Minion <minion@localhost> 1 +0000\n\nGit readiness probe\n", index.baseline)
	object, err := index.run(ctx, 128, []byte(commit), "hash-object", "-w", "-t", "commit", "--stdin")
	if err != nil {
		return err
	}
	head := strings.TrimSpace(string(object))
	if !workspace.ValidSHA(head) {
		return errors.New("git readiness returned invalid commit identity")
	}
	if _, err := index.run(ctx, 1024, nil, "update-ref", "refs/heads/minion-readiness", head); err != nil {
		return err
	}
	bundle, err := index.run(ctx, maxGitCheckBundleBytes, nil, "bundle", "create", "-", "--all")
	if err != nil {
		return err
	}
	if err := workspace.Materialize(imported, files); err != nil {
		return err
	}
	if _, err := MaterializeRepository(ctx, imported, workspace.Snapshot{Head: head, Base: head, MergeBase: head, Files: files, Repository: bundle}); err != nil {
		return fmt.Errorf("git readiness import: %w", err)
	}
	changed := []byte("git readiness after\n")
	if err := os.WriteFile(filepath.Join(source, files[0].Path), changed, 0600); err != nil {
		return err
	}
	changes, err := index.Changes(ctx)
	if err != nil {
		return err
	}
	if len(changes) != 1 || changes[0].Path != files[0].Path || changes[0].Mode != workspace.Regular || changes[0].Delete || !bytes.Equal(changes[0].Data, changed) {
		return errors.New("git readiness index did not preserve the synthetic change")
	}
	return ctx.Err()
}

func supportedGitVersion(output string) bool {
	match := gitVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if match == nil {
		return false
	}
	var version [3]uint64
	for i := range version {
		number, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return false
		}
		version[i] = number
	}
	return slices.Compare(version[:], []uint64{2, 45, 1}) >= 0
}

func finishGitCheck(root string, runErr error) error {
	if errors.Is(runErr, processgroup.ErrScopeUnconfirmed) {
		return fmt.Errorf("git readiness files retained at %q: %w", root, runErr)
	}
	if err := cleanup.RemoveAll(root); err != nil {
		return errors.Join(runErr, fmt.Errorf("remove git readiness files %q: %w", root, err))
	}
	return runErr
}
