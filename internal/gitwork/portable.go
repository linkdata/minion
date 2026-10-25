package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/workspace"
)

type sourceEntry struct {
	file    workspace.File
	sha     string
	size    int64
	omitted bool
}

const maxBatchHeaderBytes = 128

// sourceTree validates bounded local metadata before materialization.
func (c *Checkout) sourceTree(ctx context.Context, ref string) ([]sourceEntry, error) {
	if entries, ok := c.sourceTrees[ref]; ok {
		return entries, nil
	}
	if !workspace.ValidSHA(ref) {
		return nil, fmt.Errorf("%w: invalid source identity", ErrUnsupportedSource)
	}
	output, truncated, err := c.boundedGit(ctx, nil, maxSourceTreeListBytes, true, "ls-tree", "-r", "-t", "-l", "-z", ref)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("%w: source tree listing exceeds %d bytes", ErrCheckoutTooLarge, maxSourceTreeListBytes)
	}
	entries, err := parseSourceEntries(ref, output)
	if err != nil {
		return nil, err
	}
	if c.sourceTrees == nil {
		c.sourceTrees = make(map[string][]sourceEntry)
	}
	c.sourceTrees[ref] = entries
	return entries, nil
}

func parseSourceEntries(ref string, output []byte) ([]sourceEntry, error) {
	lines, err := splitNUL(output)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Git tree listing: %w", ErrUnsupportedSource, err)
	}
	if len(lines) > workspace.MaxEntries {
		return nil, fmt.Errorf("%w: more than %d metadata entries", ErrCheckoutTooLarge, workspace.MaxEntries)
	}
	var entries []sourceEntry
	var validation []workspace.File
	seen := make(map[string]bool, len(lines))
	var total int64
	for _, line := range lines {
		fieldsRaw, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(fieldsRaw))
		if !ok || len(fields) != 4 || !workspace.ValidSHA(fields[2]) {
			return nil, fmt.Errorf("%w: invalid Git source entry", ErrUnsupportedSource)
		}
		path := string(name)
		if !workspace.ValidCheckoutPath(path) {
			return nil, fmt.Errorf("%w: unsafe source path %q", ErrUnsupportedSource, path)
		}
		if seen[path] {
			return nil, fmt.Errorf("%w: duplicate source path %q", ErrUnsupportedSource, path)
		}
		seen[path] = true
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid source mode", ErrUnsupportedSource)
		}
		switch fields[1] {
		case "tree":
			if fields[0] != "040000" || fields[3] != "-" {
				return nil, fmt.Errorf("%w: unsupported tree entry at %q", ErrUnsupportedSource, path)
			}
			continue
		case "commit":
			if fields[0] != "160000" || fields[3] != "-" {
				return nil, fmt.Errorf("%w: unsupported gitlink entry at %q", ErrUnsupportedSource, path)
			}
		case "blob":
			if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
				return nil, fmt.Errorf("%w: unsupported blob mode %q at %q", ErrUnsupportedSource, fields[0], path)
			}
		default:
			return nil, fmt.Errorf("%w: unknown Git tree entry type %q", ErrUnsupportedSource, fields[1])
		}
		if len(entries) >= workspace.MaxFiles {
			return nil, fmt.Errorf("%w: more than %d paths", ErrCheckoutTooLarge, workspace.MaxFiles)
		}
		source := sourceEntry{file: workspace.File{Path: path, Mode: uint32(mode)}, sha: fields[2], omitted: fields[1] == "commit" || fields[0] == "120000"}
		file := source.file
		if source.omitted {
			file.Mode = workspace.Regular
		}
		// A gitlink has no blob; all blob sizes, including symlinks, count.
		if fields[1] == "blob" {
			source.size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil || source.size < 0 {
				return nil, fmt.Errorf("%w: blob size is unavailable", ErrUnsupportedSource)
			}
			if source.size > workspace.MaxBytes-total {
				return nil, fmt.Errorf("%w: more than %d bytes", ErrCheckoutTooLarge, workspace.MaxBytes)
			}
			total += source.size
		}
		validation = append(validation, file)
		entries = append(entries, source)
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
		return nil, nil, fmt.Errorf("%w: more than %d omitted paths", ErrCheckoutTooLarge, workspace.MaxFiles)
	}
	if len(requested) == 0 {
		return nil, omitted, nil
	}
	output, truncated, err := c.boundedGit(ctx, strings.NewReader(input.String()), int(size)+(maxBatchHeaderBytes+2)*len(requested), true, "cat-file", "--batch")
	if err != nil {
		return nil, nil, err
	}
	if truncated {
		return nil, nil, fmt.Errorf("%w: Git blobs exceed tree-declared sizes", ErrUnsupportedSource)
	}
	files, err = readBatchBlobs(output, requested)
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

// Snapshot exports pinned source, comparison data and complete Git history without configuration.
func (c *Checkout) Snapshot(ctx context.Context) (s workspace.Snapshot, err error) {
	if s.Head, err = c.verifiedHead(ctx); err != nil {
		return s, err
	}
	entries, err := c.sourceTree(ctx, s.Head)
	if err != nil {
		return s, err
	}
	var baseEntries []sourceEntry
	var selected map[string]bool
	if s.Base = c.comparisonBase; s.Base != "" {
		if s.MergeBase, err = runGit(ctx, c.path, "merge-base", s.Base, s.Head); err != nil {
			var exit *exec.ExitError
			if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
				return s, fmt.Errorf("%w: pull request histories have no common ancestor: %w", ErrUnsupportedSource, err)
			}
			return s, err
		}
		if baseEntries, err = c.sourceTree(ctx, s.MergeBase); err != nil {
			return s, err
		}
		paths, _, err := c.boundedGit(ctx, nil, maxPathListBytes, false, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", s.MergeBase, s.Head, "--")
		if err != nil {
			return s, err
		}
		names, err := splitNUL(paths)
		if err != nil {
			return s, fmt.Errorf("%w: invalid Git comparison paths: %w", ErrUnsupportedSource, err)
		}
		selected = map[string]bool{}
		for _, name := range names {
			selected[string(name)] = true
			s.ChangedPaths = append(s.ChangedPaths, string(name))
		}
		if len(s.ChangedPaths) > workspace.MaxFiles {
			return s, fmt.Errorf("%w: more than %d changed paths", ErrCheckoutTooLarge, workspace.MaxFiles)
		}
	}
	headFiles := 0
	var total int64
	allEntries := slices.Clone(entries)
	for _, entry := range entries {
		if entry.omitted {
			s.HeadOmitted = append(s.HeadOmitted, entry.file.Path)
		} else {
			headFiles++
			total += entry.size
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
	// Bound regular-file transfer before reading contents or rendering a diff. Omitted
	// symlink blobs are bounded separately by each complete tree's metadata check.
	// sourceFiles separately sizes its bounded batch response.
	if total > workspace.MaxBytes {
		return s, fmt.Errorf("%w: combined source exceeds byte limit", ErrCheckoutTooLarge)
	}
	if s.Repository, _, err = c.boundedGit(ctx, nil, workspace.MaxBytes, false, "bundle", "create", "-", "--all"); err != nil {
		return s, fmt.Errorf("export complete repository: %w", err)
	}
	files, omitted, err := c.sourceFiles(ctx, allEntries)
	if err != nil {
		return s, err
	}
	s.Files, s.BaseFiles, s.Omitted = files[:headFiles:headFiles], files[headFiles:], omitted
	if s.Base != "" {
		if s.Diff, s.DiffIncomplete, err = c.boundedGit(ctx, nil, workspace.MaxContextBytes, true, "-c", "core.quotePath=false", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", s.MergeBase, s.Head, "--"); err != nil {
			return s, err
		}
	}
	if err := c.ready(); err != nil {
		return s, err
	}
	return s, nil
}

// Import applies data-only changes to the original orchestrator-owned checkout.
// The caller must validate the complete action result, including omitted-source overlap.
func (c *Checkout) Import(ctx context.Context, expectedHead string, changes []workspace.Change) error {
	if _, err := c.verifiedHead(ctx); err != nil {
		return err
	}
	if expectedHead != c.expectedHead {
		return errors.New("result source commit does not match checkout")
	}
	if err := workspace.Apply(c.path, changes); err != nil {
		return err
	}
	// Git add can keep the old index spelling for case-only renames with core.ignoreCase.
	var deleted strings.Builder
	for _, change := range changes {
		if change.Delete {
			deleted.WriteString(change.Path)
			deleted.WriteByte(0)
		}
	}
	if deleted.Len() != 0 {
		if err := runGitInputTo(ctx, c.path, strings.NewReader(deleted.String()), io.Discard, "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return fmt.Errorf("stage imported deletions: %w", err)
		}
	}
	if _, err := runGit(ctx, c.path, "add", "--all", "--", "."); err != nil {
		return fmt.Errorf("stage imported changes: %w", err)
	}
	if runtime.GOOS == "windows" {
		indexedPaths, err := c.indexedImportedPaths(ctx, changes)
		if err != nil {
			return err
		}
		for _, mode := range []struct {
			fileMode uint32
			flag     string
		}{{workspace.Executable, "+x"}, {workspace.Regular, "-x"}} {
			var paths strings.Builder
			for _, change := range changes {
				if !change.Delete && change.Mode == mode.fileMode && indexedPaths[change.Path] {
					paths.WriteString(change.Path)
					paths.WriteByte(0)
				}
			}
			if paths.Len() != 0 {
				if err := runGitInputTo(ctx, c.path, strings.NewReader(paths.String()), io.Discard, "update-index", "--chmod="+mode.flag, "-z", "--stdin"); err != nil {
					return fmt.Errorf("restore imported file modes: %w", err)
				}
			}
		}
	}
	raw, truncated, err := c.boundedGit(ctx, nil, maxChangedPathListBytes, true,
		"diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return err
	}
	if truncated {
		return ErrTooManyFiles
	}
	staged, objects, err := parseIndexChanges(raw)
	if err != nil {
		return err
	}
	if len(staged) != len(changes) {
		return errors.New("staged paths do not match reviewed changes")
	}
	declared := make(map[string]workspace.Change, len(changes))
	for _, change := range changes {
		declared[change.Path] = change
	}
	for i, entry := range staged {
		change, exists := declared[entry.Path]
		if !exists {
			return errors.New("staged paths do not match reviewed changes")
		}
		delete(declared, entry.Path)
		if entry.Delete != change.Delete || entry.Mode != change.Mode || (!change.Delete && objects[i] != gitBlobID(change.Data)) {
			return fmt.Errorf("staged content or mode differs from reviewed change %q", entry.Path)
		}
	}
	return nil
}

func (c *Checkout) indexedImportedPaths(ctx context.Context, changes []workspace.Change) (map[string]bool, error) {
	indexed := make(map[string]bool, len(changes))
	for _, change := range changes {
		if !change.Delete {
			indexed[change.Path] = false
		}
	}
	if len(indexed) == 0 {
		return indexed, nil
	}
	raw, truncated, err := c.boundedGit(ctx, nil, maxPathListBytes, true, "ls-files", "--cached", "-z")
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrTooManyFiles
	}
	paths, err := splitNUL(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Git index paths: %w", err)
	}
	for _, path := range paths {
		name := string(path)
		if _, wanted := indexed[name]; wanted {
			indexed[name] = true
		}
	}
	return indexed, nil
}

func (c *Checkout) boundedGit(ctx context.Context, stdin io.Reader, limit int, truncate bool, args ...string) ([]byte, bool, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := bounded.Buffer{Limit: limit, Cancel: cancel}
	err := runGitInputTo(readCtx, c.path, stdin, &output, args...)
	if contextErr := ctx.Err(); contextErr != nil {
		if !errors.Is(err, contextErr) {
			err = errors.Join(err, contextErr)
		}
		return nil, false, err
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil, false, err
	}
	if output.Exceeded {
		if truncate {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("%w: Git %s output exceeds %d bytes", ErrCheckoutTooLarge, args[0], limit)
	}
	return output.Bytes(), false, err
}
