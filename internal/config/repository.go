package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/workspace"
)

// AddRepository preserves existing settings and atomically appends a repository.
//
// The caller must hold the state lock to serialize minion's config writes.
func AddRepository(path string, repo Repository) (added Repository, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, err
	}
	target, err := workspace.CanonicalPath(abs)
	if err != nil {
		return Repository{}, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return Repository{}, err
	}
	cfg, err := decode(bytes.NewReader(data))
	if err != nil {
		return Repository{}, err
	}
	for _, existing := range cfg.Repositories {
		if strings.EqualFold(existing.Name, repo.Name) {
			if err := cfg.Validate(filepath.Dir(abs)); err != nil {
				return Repository{}, err
			}
			existing, _ = cfg.Repository(repo.Name)
			return existing, nil
		}
	}
	repo.Name = strings.ToLower(repo.Name)
	cfg.Repositories = append(cfg.Repositories, repo)
	// Encode before validation resolves paths and fills defaults.
	updated, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Repository{}, err
	}
	if err := cfg.Validate(filepath.Dir(abs)); err != nil {
		return Repository{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return Repository{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".config-*")
	if err != nil {
		return Repository{}, err
	}
	defer func() {
		if removeErr := os.Remove(f.Name()); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	err = f.Chmod(info.Mode().Perm())
	if err == nil {
		_, err = f.Write(append(updated, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return Repository{}, err
	}
	if !bytes.Equal(data, current) {
		return Repository{}, fmt.Errorf("config changed while adding %s; retry", repo.Name)
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return Repository{}, err
	}
	if dir, openErr := os.Open(filepath.Dir(target)); openErr == nil {
		dir.Sync()  //nolint:errcheck // -- Directory sync is best effort after replacement; some filesystems do not support it.
		dir.Close() //nolint:errcheck // -- The read-only directory has no pending writes; its sync is best effort.
	}
	added = cfg.Repositories[len(cfg.Repositories)-1]
	return
}
