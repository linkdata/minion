package codexcli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type ownedRollout struct {
	meta rolloutMeta
	info os.FileInfo
}

var errRolloutLeafMissing = errors.New("rollout file is missing")

// Inventory paths in the usage discovery window, including nonregular entries.
func rolloutPaths(ctx context.Context, home string, started time.Time) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(home, "sessions")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("sessions path is not a directory")
	}
	paths := map[string]bool{}
	entries := 0
	err = walkUsageFiles(ctx, root, started, func(path string, entry fs.DirEntry) error {
		entries++
		if entries > maxUsageWalkEntries {
			return fs.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 && recentSessionDateComponent(root, path, started) {
			return fmt.Errorf("rollout date directory is a symlink at %q", path)
		}
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			paths[path] = true
		}
		return nil
	})
	if errors.Is(err, fs.SkipAll) {
		err = fmt.Errorf("rollout path inventory exceeded %d entries", maxUsageWalkEntries)
	}
	return paths, err
}

func recentSessionDateComponent(root, path string, started time.Time) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	date := filepath.ToSlash(rel)
	oldest := started.UTC().AddDate(0, 0, -1).Format("2006/01/02")
	var layout, cutoff string
	switch strings.Count(date, "/") {
	case 0:
		layout, cutoff = "2006", oldest[:4]
	case 1:
		layout, cutoff = "2006/01", oldest[:7]
	case 2:
		layout, cutoff = "2006/01/02", oldest
	default:
		return false
	}
	_, err = time.Parse(layout, date)
	return err == nil && date >= cutoff
}

func rolloutFileInfo(home, path string) (os.FileInfo, error) {
	root := filepath.Join(home, "sessions")
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || !strings.HasSuffix(path, ".jsonl") {
		return nil, errors.New("rollout path is outside sessions")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("rollout parent is not a directory")
		}
		if dir == root {
			break
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errRolloutLeafMissing
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("rollout is not a regular file")
	}
	return info, nil
}

func sameRolloutMeta(a, b rolloutMeta) bool {
	if a.ID != b.ID || a.SessionID != b.SessionID || a.ParentID != b.ParentID {
		return false
	}
	return a.HistoryStart == nil && b.HistoryStart == nil || a.HistoryStart != nil && b.HistoryStart != nil && *a.HistoryStart == *b.HistoryStart
}

func (s *Session) rolloutScanError(err error) {
	s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: inspect rollouts under %q: %w", filepath.Join(s.home, "sessions"), err))
}

// Usage discovery can be incomplete because an unrelated recent header is malformed.
func cleanupHeaderFiles(ctx context.Context, paths map[string]bool, started time.Time, owned map[string]ownedRollout) (map[string]rolloutMeta, map[string]bool, error) {
	files, ambiguous := map[string]rolloutMeta{}, map[string]bool{}
	budget := int64(maxUsageScanBytes)
	allPaths := maps.Clone(paths)
	for path := range owned {
		allPaths[path] = true
	}
	for _, path := range slices.Sorted(maps.Keys(allPaths)) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if _, known := owned[path]; !known && info.ModTime().Before(started.Add(-time.Minute)) {
			continue
		}
		budget -= min(info.Size(), maxUsageHeaderBytes)
		if budget < 0 {
			return nil, nil, errors.New("rollout header scan exceeded byte limit")
		}
		meta, err := readRolloutHeader(path)
		if err != nil {
			continue
		}
		if _, exists := files[meta.ID]; exists {
			ambiguous[meta.ID] = true
		} else {
			files[meta.ID] = meta
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return files, ambiguous, nil
}

func (s *Session) admitRollouts(ctx context.Context, rootID string, started time.Time, paths map[string]bool) {
	if rootID == "" {
		return
	}
	if err := ctx.Err(); err != nil {
		s.rolloutScanError(err)
		return
	}
	rootInfo, err := os.Lstat(filepath.Join(s.home, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: inspect rollout sessions under %q: %w", s.home, err))
		return
	}
	if !rootInfo.IsDir() {
		s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: rollout sessions is not a directory at %q", filepath.Join(s.home, "sessions")))
		return
	}
	files, ambiguous, complete := usageFiles(ctx, s.home, rootID, started, s.accounting.known)
	if err := ctx.Err(); err != nil {
		s.rolloutScanError(err)
		return
	}
	if !complete {
		var err error
		files, ambiguous, err = cleanupHeaderFiles(ctx, paths, started, s.ownedRollouts)
		if err != nil {
			s.rolloutScanError(err)
			return
		}
	}
	root, exists := files[rootID]
	if !exists {
		return
	}
	_, rootOwned := s.ownedRollouts[root.path]
	if s.excludedRollouts[root.path] && !rootOwned {
		return
	}
	if ambiguous[rootID] || root.duplicate || root.ParentID != "" || root.SessionID != "" && root.SessionID != rootID {
		return
	}
	selected := map[string]rolloutMeta{rootID: root}
	for changed := true; changed; {
		changed = false
		for id, meta := range files {
			if id == rootID || ambiguous[id] || meta.duplicate || meta.SessionID != "" && meta.SessionID != rootID {
				continue
			}
			if _, known := selected[id]; known {
				continue
			}
			_, parent := selected[meta.ParentID]
			if meta.ParentID == "" && meta.SessionID == rootID || meta.ParentID != "" && parent {
				selected[id], changed = meta, true
			}
		}
	}
	if err := ctx.Err(); err != nil {
		s.rolloutScanError(err)
		return
	}
	if s.ownedRollouts == nil {
		s.ownedRollouts = map[string]ownedRollout{}
	}
	for _, meta := range selected {
		if err := ctx.Err(); err != nil {
			s.rolloutScanError(err)
			return
		}
		if _, alreadyOwned := s.ownedRollouts[meta.path]; alreadyOwned {
			continue
		}
		if s.excludedRollouts[meta.path] {
			continue
		}
		info, err := rolloutFileInfo(s.home, meta.path)
		if err != nil {
			continue
		}
		header, err := readRolloutHeader(meta.path)
		if err != nil || !sameRolloutMeta(meta, header) {
			continue
		}
		if err := ctx.Err(); err != nil {
			s.rolloutScanError(err)
			return
		}
		s.ownedRollouts[meta.path] = ownedRollout{meta: meta, info: info}
	}
}

func (s *Session) reportUnownedNewRollouts(ctx context.Context, paths map[string]bool, rootID string, children []string, spawns map[string]bool, conflictingID string) {
	if err := ctx.Err(); err != nil {
		s.rolloutScanError(err)
		return
	}
	newPaths := []string{}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if s.excludedRollouts[path] {
			continue
		}
		if _, owned := s.ownedRollouts[path]; !owned {
			newPaths = append(newPaths, path)
		}
	}
	if len(newPaths) > maxUsageFiles {
		s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: inspect new rollouts under %q: file limit exceeded", filepath.Join(s.home, "sessions")))
		return
	}
	if rootID == "" {
		// Child IDs cannot identify the call's root rollout among concurrent ones.
		for _, path := range newPaths {
			s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: rollout cleanup requires operator recovery at %q: thread identity missing", path))
		}
		return
	}
	linked := map[string]bool{}
	if rootID != "" {
		linked[rootID] = true
	}
	if conflictingID != "" {
		linked[conflictingID] = true
	}
	for _, id := range children {
		linked[id] = true
	}
	for id := range spawns {
		linked[id] = true
	}
	for changed := true; changed; {
		changed = false
		for _, owned := range s.ownedRollouts {
			meta := owned.meta
			if !linked[meta.ID] && (meta.ID == rootID || rootID != "" && meta.SessionID == rootID || meta.ParentID != "" && linked[meta.ParentID]) {
				linked[meta.ID], changed = true, true
			}
		}
	}
	metas := map[string]rolloutMeta{}
	unknown := []string{}
	for _, path := range newPaths {
		if err := ctx.Err(); err != nil {
			s.rolloutScanError(err)
			return
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && info.Mode().IsRegular() {
			if meta, readErr := readRolloutHeader(path); readErr == nil {
				metas[path] = meta
				continue
			}
		}
		unknown = append(unknown, path)
	}
	if err := ctx.Err(); err != nil {
		s.rolloutScanError(err)
		return
	}
	for changed := true; changed; {
		changed = false
		for path, meta := range metas {
			if linked[meta.ID] || rootID != "" && meta.SessionID == rootID || meta.ParentID != "" && linked[meta.ParentID] {
				if !linked[meta.ID] {
					linked[meta.ID], changed = true, true
				}
				s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: rollout cleanup requires operator recovery at %q: ownership unverified", path))
				delete(metas, path)
			}
		}
	}
	for _, path := range unknown {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		for id := range linked {
			if strings.HasSuffix(name, "-"+id) {
				s.rolloutDiscoveryErr = errors.Join(s.rolloutDiscoveryErr, fmt.Errorf("codex exec: rollout cleanup requires operator recovery at %q: ownership unverified", path))
				break
			}
		}
	}
}

func (s *Session) cleanupRollouts() error {
	if len(s.ownedRollouts) == 0 {
		return s.rolloutDiscoveryErr
	}
	var result error = s.rolloutDiscoveryErr
	for _, path := range slices.Sorted(maps.Keys(s.ownedRollouts)) {
		owned := s.ownedRollouts[path]
		info, err := rolloutFileInfo(s.home, path)
		if errors.Is(err, errRolloutLeafMissing) {
			delete(s.ownedRollouts, path)
			continue
		}
		if err != nil || !os.SameFile(info, owned.info) {
			result = errors.Join(result, fmt.Errorf("codex exec: rollout cleanup requires operator recovery at %q: identity changed", path), err)
			continue
		}
		header, err := readRolloutHeader(path)
		if err != nil || !sameRolloutMeta(header, owned.meta) {
			result = errors.Join(result, fmt.Errorf("codex exec: rollout cleanup requires operator recovery at %q: header changed", path), err)
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("codex exec: remove rollout %q: %w", path, err))
			continue
		}
		delete(s.ownedRollouts, path)
	}
	return result
}
