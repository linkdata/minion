package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

type sourceEntry struct {
	file    workspace.File
	sha     string
	size    int64
	omitted bool
}

// sourceTree checks API metadata before any partial-clone blob is requested.
func (c *Checkout) sourceTree(ctx context.Context, ref string) ([]sourceEntry, error) {
	if c.lookupTree == nil {
		return nil, errors.New("source tree metadata lookup is missing")
	}
	treeSHA, err := runGit(ctx, c.path, "rev-parse", "--verify", ref+"^{tree}")
	if err != nil {
		return nil, err
	}
	tree, err := c.lookupTree(ctx, treeSHA)
	if err != nil {
		return nil, fmt.Errorf("load source tree metadata: %w", err)
	}
	return c.sourceEntries(ctx, ref, tree, treeSHA)
}

func (c *Checkout) sourceEntries(ctx context.Context, ref string, tree github.GitTree, treeSHA string) ([]sourceEntry, error) {
	if err := checkSourceTree(tree, treeSHA, true); err != nil {
		return nil, err
	}
	if len(tree.Tree) > maxWorktreeEntries {
		return nil, ErrCheckoutTooLarge
	}
	metadata := map[string]github.GitTreeEntry{}
	seen := map[string]bool{}
	for _, entry := range tree.Tree {
		key := strings.ToLower(entry.Path)
		if !workspace.ValidPath(entry.Path) || seen[key] {
			return nil, fmt.Errorf("%w: unsafe or duplicate source path %q", ErrCheckoutTooLarge, entry.Path)
		}
		seen[key] = true
		if entry.Type != "tree" {
			metadata[entry.Path] = entry
		}
	}
	output, _, err := c.boundedGit(ctx, maxPathListBytes, false, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	if len(output) == 0 {
		if len(metadata) != 0 {
			return nil, fmt.Errorf("%w: API tree differs from Git tree", ErrCheckoutTooLarge)
		}
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("%w: invalid Git tree listing", ErrCheckoutTooLarge)
	}
	var entries []sourceEntry
	var validation []workspace.File
	for _, line := range bytes.Split(output[:len(output)-1], []byte{0}) {
		fieldsRaw, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(fieldsRaw))
		if !ok || len(fields) != 3 || !validSHA(fields[2]) {
			return nil, fmt.Errorf("%w: invalid Git source entry", ErrCheckoutTooLarge)
		}
		entry, exists := metadata[string(name)]
		if !exists || entry.Mode != fields[0] || entry.Type != fields[1] {
			return nil, fmt.Errorf("%w: API tree differs from Git tree at %q", ErrCheckoutTooLarge, name)
		}
		delete(metadata, string(name))
		mode, err := strconv.ParseUint(entry.Mode, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid source mode", ErrCheckoutTooLarge)
		}
		source := sourceEntry{file: workspace.File{Path: entry.Path, Mode: uint32(mode)}, sha: fields[2], omitted: entry.Type == "commit" || entry.Mode == "120000"}
		file := source.file
		if source.omitted {
			file.Mode = workspace.Regular
		} else {
			source.size = *entry.Size
		}
		validation = append(validation, file)
		entries = append(entries, source)
	}
	if len(metadata) != 0 {
		return nil, fmt.Errorf("%w: API tree has unmatched entries", ErrCheckoutTooLarge)
	}
	if err := workspace.ValidateSnapshot(workspace.Snapshot{Head: ref, Files: validation}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCheckoutTooLarge, err)
	}
	return entries, nil
}

func (c *Checkout) sourceFiles(ctx context.Context, entries []sourceEntry, selected map[string]bool) (files []workspace.File, omitted []string, err error) {
	for _, entry := range entries {
		if selected != nil && !selected[entry.file.Path] {
			continue
		}
		if entry.omitted {
			omitted = append(omitted, entry.file.Path)
			continue
		}
		entry.file.Data, _, err = c.boundedGit(ctx, int(entry.size), false, "cat-file", "blob", entry.sha)
		if err != nil {
			return nil, nil, err
		}
		if int64(len(entry.file.Data)) != entry.size {
			return nil, nil, fmt.Errorf("%w: Git blob size differs from tree metadata", ErrCheckoutTooLarge)
		}
		files = append(files, entry.file)
	}
	return files, omitted, nil
}

// Snapshot exports pinned source and optional comparison/history data, never Git configuration.
func (c *Checkout) Snapshot(ctx context.Context, history, pull bool) (s workspace.Snapshot, err error) {
	s.HistoryRequested = history
	if s.Head, err = c.Head(ctx); err != nil {
		return s, err
	}
	entries, err := c.sourceTree(ctx, s.Head)
	if err != nil {
		return s, err
	}
	var baseEntries []sourceEntry
	var selected map[string]bool
	if pull {
		if s.Base, err = runGit(ctx, c.path, "rev-parse", "--verify", "refs/remotes/minion/base^{commit}"); err != nil {
			return s, err
		}
		if s.MergeBase, err = runGit(ctx, c.path, "merge-base", s.Base, s.Head); err != nil {
			return s, err
		}
		if !validSHA(s.Base) || !validSHA(s.MergeBase) {
			return s, fmt.Errorf("%w: invalid comparison commits", ErrCheckoutTooLarge)
		}
		if baseEntries, err = c.sourceTree(ctx, s.MergeBase); err != nil {
			return s, err
		}
		paths, _, err := c.boundedGit(ctx, maxPathListBytes, false, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", s.MergeBase, s.Head, "--")
		if err != nil {
			return s, err
		}
		selected = map[string]bool{}
		for _, name := range bytes.Split(paths, []byte{0}) {
			if len(name) != 0 {
				selected[string(name)] = true
				s.ChangedPaths = append(s.ChangedPaths, string(name))
			}
		}
	}
	var total int64
	for _, entry := range entries {
		if !entry.omitted {
			total += entry.size
		}
	}
	for _, entry := range baseEntries {
		if selected[entry.file.Path] && !entry.omitted {
			total += entry.size
		}
	}
	if total > workspace.MaxBytes {
		return s, fmt.Errorf("%w: combined source exceeds byte limit", ErrCheckoutTooLarge)
	}
	if pull {
		if s.Diff, s.DiffIncomplete, err = c.boundedGit(ctx, workspace.MaxContextBytes, true, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", s.MergeBase, s.Head, "--"); err != nil {
			return s, err
		}
		if s.DiffIncomplete {
			s.Diff = nil
		}
	}
	if s.Files, s.Omitted, err = c.sourceFiles(ctx, entries, nil); err != nil {
		return s, err
	}
	if pull {
		var omitted []string
		if s.BaseFiles, omitted, err = c.sourceFiles(ctx, baseEntries, selected); err != nil {
			return s, err
		}
		seen := map[string]bool{}
		for _, name := range s.Omitted {
			seen[name] = true
		}
		for _, name := range omitted {
			if !seen[name] {
				s.Omitted = append(s.Omitted, name)
				seen[name] = true
			}
		}
	}
	if history {
		count, err := runGit(ctx, c.path, "rev-list", "--count", s.Head)
		if err != nil {
			return s, err
		}
		commits, err := strconv.ParseUint(count, 10, 64)
		if err != nil {
			return s, errors.New("invalid history commit count")
		}
		var truncated bool
		s.History, truncated, err = c.boundedGit(ctx, workspace.MaxContextBytes, true, "log", "--max-count=100", "--format=fuller", "--date=iso-strict", "--no-decorate", "--no-show-signature", s.Head, "--")
		if err != nil {
			return s, err
		}
		shallow, err := runGit(ctx, c.path, "rev-parse", "--is-shallow-repository")
		if err != nil {
			return s, err
		}
		s.HistoryComplete = !truncated && commits <= 100 && shallow == "false"
	}
	if err := c.ready(); err != nil {
		return s, err
	}
	if err := workspace.ValidateSnapshot(s); err != nil {
		return s, fmt.Errorf("%w: %w", ErrCheckoutTooLarge, err)
	}
	return s, nil
}

// Import applies data-only changes to the original orchestrator-owned checkout.
func (c *Checkout) Import(ctx context.Context, expectedHead string, changes []workspace.Change) error {
	if err := c.ready(); err != nil {
		return err
	}
	if !validSHA(expectedHead) || !strings.EqualFold(expectedHead, c.expectedHead) {
		return errors.New("result source commit does not match checkout")
	}
	if err := c.assertHead(ctx); err != nil {
		return err
	}
	if err := workspace.ValidateChanges(changes); err != nil {
		return err
	}
	if err := c.disableAttributes(); err != nil {
		return err
	}
	return workspace.Apply(c.path, changes)
}

func (c *Checkout) disableAttributes() error {
	if err := os.MkdirAll(filepath.Join(c.path, ".git", "info"), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.path, ".git", "info", "attributes"), []byte(checkoutAttributes), 0600)
}

type boundedGitWriter struct {
	buffer cappedBuffer
	cancel context.CancelFunc
}

func (w *boundedGitWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if w.buffer.exceeded {
		w.cancel()
	}
	return n, err
}

func (c *Checkout) boundedGit(ctx context.Context, limit int, truncate bool, args ...string) ([]byte, bool, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := boundedGitWriter{buffer: cappedBuffer{limit: limit}, cancel: cancel}
	err := runGitTo(readCtx, c.path, &output, args...)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if output.buffer.exceeded {
		if truncate {
			return output.buffer.Bytes(), true, nil
		}
		return nil, false, fmt.Errorf("%w: Git %s output exceeds %d bytes", ErrCheckoutTooLarge, args[0], limit)
	}
	return output.buffer.Bytes(), false, err
}
