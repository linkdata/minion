package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

type sourceEntry struct {
	file    workspace.File
	sha     string
	size    int64
	omitted bool
}

const maxBatchHeaderBytes = 128

// sourceTree checks API metadata before any partial-clone blob is requested.
func (c *Checkout) sourceTree(ctx context.Context, ref string) ([]sourceEntry, error) {
	if entries, ok := c.sourceTrees[ref]; ok {
		return entries, nil
	}
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
	entries, err := c.sourceEntries(ctx, ref, tree, treeSHA)
	if err != nil {
		return nil, err
	}
	if c.sourceTrees == nil {
		c.sourceTrees = make(map[string][]sourceEntry)
	}
	c.sourceTrees[ref] = entries
	return entries, nil
}

func (c *Checkout) sourceEntries(ctx context.Context, ref string, tree github.GitTree, treeSHA string) ([]sourceEntry, error) {
	if err := checkSourceTree(tree, treeSHA); err != nil {
		return nil, err
	}
	if len(tree.Tree) > workspace.MaxEntries {
		return nil, ErrCheckoutTooLarge
	}
	metadata := map[string]github.GitTreeEntry{}
	seen := map[string]bool{}
	for _, entry := range tree.Tree {
		if seen[entry.Path] {
			return nil, fmt.Errorf("%w: unsafe or duplicate source path %q", ErrUnsupportedSource, entry.Path)
		}
		seen[entry.Path] = true
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
			return nil, fmt.Errorf("%w: API tree differs from Git tree", ErrUnsupportedSource)
		}
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("%w: invalid Git tree listing", ErrUnsupportedSource)
	}
	var entries []sourceEntry
	var validation []workspace.File
	for _, line := range bytes.Split(output[:len(output)-1], []byte{0}) {
		fieldsRaw, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(fieldsRaw))
		if !ok || len(fields) != 3 || !workspace.ValidSHA(fields[2]) {
			return nil, fmt.Errorf("%w: invalid Git source entry", ErrUnsupportedSource)
		}
		entry, exists := metadata[string(name)]
		if !exists || entry.Mode != fields[0] || entry.Type != fields[1] {
			return nil, fmt.Errorf("%w: API tree differs from Git tree at %q", ErrUnsupportedSource, name)
		}
		delete(metadata, string(name))
		mode, err := strconv.ParseUint(entry.Mode, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid source mode", ErrUnsupportedSource)
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
		return nil, fmt.Errorf("%w: API tree has unmatched entries", ErrUnsupportedSource)
	}
	if err := workspace.ValidateSnapshot(workspace.Snapshot{Head: ref, Files: validation}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedSource, err)
	}
	return entries, nil
}

func (c *Checkout) sourceFiles(ctx context.Context, entries []sourceEntry) (files []workspace.File, omitted []string, err error) {
	var requested []sourceEntry
	var input strings.Builder
	var size int64
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.omitted {
			if !seen[entry.file.Path] {
				omitted = append(omitted, entry.file.Path)
				seen[entry.file.Path] = true
			}
			continue
		}
		requested = append(requested, entry)
		size += entry.size
		input.WriteString(entry.sha + "\n")
	}
	if len(omitted) > workspace.MaxFiles {
		return nil, nil, ErrCheckoutTooLarge
	}
	if len(requested) == 0 {
		return nil, omitted, nil
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Content bounds were checked against API metadata before requesting blobs.
	// Leave room for each bounded cat-file header and its content terminator.
	output := bounded.Buffer{Limit: int(size) + (maxBatchHeaderBytes+2)*len(requested), Cancel: cancel}
	err = runGitInputTo(readCtx, c.path, strings.NewReader(input.String()), &output, "cat-file", "--batch")
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if output.Exceeded {
		return nil, nil, fmt.Errorf("%w: Git blobs exceed API-declared sizes", ErrUnsupportedSource)
	}
	if err != nil {
		return nil, nil, err
	}
	files, err = readBatchBlobs(output.Bytes(), requested)
	return files, omitted, err
}

func readBatchBlobs(data []byte, entries []sourceEntry) ([]workspace.File, error) {
	files := make([]workspace.File, 0, len(entries))
	for _, entry := range entries {
		header, rest, ok := bytes.Cut(data, []byte{'\n'})
		if !ok || len(header) > maxBatchHeaderBytes {
			return nil, fmt.Errorf("%w: invalid Git batch blob header", ErrUnsupportedSource)
		}
		fields := bytes.Fields(header)
		if len(fields) != 3 || string(fields[0]) != entry.sha || string(fields[1]) != "blob" {
			return nil, fmt.Errorf("%w: invalid Git batch blob header", ErrUnsupportedSource)
		}
		size, err := strconv.ParseInt(string(fields[2]), 10, 64)
		if err != nil || size < 0 || size != entry.size || size >= int64(len(rest)) || rest[size] != '\n' {
			return nil, fmt.Errorf("%w: Git blob size differs from tree metadata", ErrUnsupportedSource)
		}
		entry.file.Data = rest[:size:size]
		files = append(files, entry.file)
		data = rest[size+1:]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("%w: unexpected Git batch blob output", ErrUnsupportedSource)
	}
	return files, nil
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
		if !workspace.ValidSHA(s.Base) || !workspace.ValidSHA(s.MergeBase) {
			return s, fmt.Errorf("%w: invalid comparison commits", ErrUnsupportedSource)
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
		if len(s.ChangedPaths) > workspace.MaxFiles {
			return s, ErrCheckoutTooLarge
		}
	}
	var total int64
	headFiles := 0
	allEntries := append([]sourceEntry(nil), entries...)
	for _, entry := range entries {
		if !entry.omitted {
			total += entry.size
			headFiles++
		}
	}
	for _, entry := range baseEntries {
		if selected[entry.file.Path] {
			allEntries = append(allEntries, entry)
			if !entry.omitted {
				total += entry.size
			}
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
	files, omitted, err := c.sourceFiles(ctx, allEntries)
	if err != nil {
		return s, err
	}
	s.Files, s.BaseFiles, s.Omitted = files[:headFiles], files[headFiles:], omitted
	if history {
		count, err := runGit(ctx, c.path, "rev-list", "--count", "--max-count=101", s.Head)
		if err != nil {
			return s, err
		}
		commits, err := strconv.ParseUint(count, 10, 64)
		if err != nil {
			return s, errors.New("invalid history commit count")
		}
		var truncated bool
		s.History, truncated, err = c.boundedGit(ctx, workspace.MaxContextBytes, true, "log", "--no-mailmap", "--max-count=100", "--format=fuller", "--date=iso-strict", "--no-decorate", "--no-show-signature", s.Head, "--")
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
		return s, fmt.Errorf("%w: %w", ErrUnsupportedSource, err)
	}
	return s, nil
}

// Import applies data-only changes to the original orchestrator-owned checkout.
// The caller must validate the complete action result, including omitted-source overlap.
func (c *Checkout) Import(ctx context.Context, expectedHead string, changes []workspace.Change) error {
	if err := c.ready(); err != nil {
		return err
	}
	if !workspace.ValidSHA(expectedHead) || !strings.EqualFold(expectedHead, c.expectedHead) {
		return errors.New("result source commit does not match checkout")
	}
	if err := c.assertHead(ctx); err != nil {
		return err
	}
	if err := workspace.Apply(c.path, changes); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "add", "--all", "--", "."); err != nil {
		return fmt.Errorf("stage imported changes: %w", err)
	}
	staged, err := c.stagedPaths(ctx, "")
	if err != nil {
		return err
	}
	if len(changes) != 0 && len(staged) == 0 {
		return ErrNoChanges
	}
	declared := make([]string, len(changes))
	for i, change := range changes {
		declared[i] = change.Path
	}
	slices.Sort(declared)
	slices.Sort(staged)
	if !slices.Equal(staged, declared) {
		return errors.New("staged paths do not match reviewed changes")
	}
	return nil
}

func (c *Checkout) disableAttributes() error {
	if err := os.MkdirAll(filepath.Join(c.path, ".git", "info"), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.path, ".git", "info", "attributes"), []byte(workspace.GitAttributes), 0600)
}

func (c *Checkout) boundedGit(ctx context.Context, limit int, truncate bool, args ...string) ([]byte, bool, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := bounded.Buffer{Limit: limit, Cancel: cancel}
	err := runGitTo(readCtx, c.path, &output, args...)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if output.Exceeded {
		if truncate {
			return output.Bytes(), true, nil
		}
		return nil, false, fmt.Errorf("%w: Git %s output exceeds %d bytes", ErrCheckoutTooLarge, args[0], limit)
	}
	return output.Bytes(), false, err
}
