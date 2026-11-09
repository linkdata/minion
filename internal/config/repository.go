package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/linkdata/minion/internal/workspace"
)

var updateMu sync.Mutex

// Update validates and atomically saves a change to the JSON configuration.
// It serializes Minion's config writers; callers still need the state lock when
// coordinating a write with monitor state.
func Update(path string, mutate func(*Config) error) (candidate Config, err error) {
	return update(path, nil, mutate)
}

// UpdateWithAdminRuntime validates a saved edit with active listener overrides.
func UpdateWithAdminRuntime(path string, runtime AdminConfig, mutate func(*Config) error) (Config, error) {
	return update(path, &runtime, mutate)
}

func update(path string, runtime *AdminConfig, mutate func(*Config) error) (candidate Config, err error) {
	if mutate == nil {
		return Config{}, errors.New("config mutation is required")
	}
	updateMu.Lock()
	defer updateMu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	target, err := workspace.CanonicalPath(abs)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return Config{}, err
	}
	candidate, err = decode(bytes.NewReader(data))
	if err != nil {
		return Config{}, err
	}
	applyAdminRuntime(&candidate, runtime)
	before, err := json.Marshal(candidate)
	if err != nil {
		return Config{}, err
	}
	if err := mutate(&candidate); err != nil {
		return Config{}, err
	}
	after, err := json.Marshal(candidate)
	if err != nil {
		return Config{}, err
	}
	updated, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return Config{}, err
	}
	// Validate only after encoding, which must not persist resolved paths or defaults.
	if err := candidate.Validate(filepath.Dir(abs)); err != nil {
		return Config{}, err
	}
	if bytes.Equal(before, after) {
		return candidate, nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return Config{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".config-*")
	if err != nil {
		return Config{}, err
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
		return Config{}, err
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return Config{}, err
	}
	if !bytes.Equal(data, current) {
		return Config{}, fmt.Errorf("config changed while updating; retry")
	}
	if err := replaceConfigFile(f.Name(), target); err != nil {
		return Config{}, err
	}
	if dir, openErr := os.Open(filepath.Dir(target)); openErr == nil {
		dir.Sync()  //nolint:errcheck // -- Directory sync is best effort after replacement; some filesystems do not support it.
		dir.Close() //nolint:errcheck // -- The read-only directory has no pending writes; its sync is best effort.
	}
	return candidate, nil
}

// AddRepository preserves existing settings and atomically appends a repository.
// Update serializes config writes; the monitor saves invitation cursors separately.
func AddRepository(path string, repo Repository) (Repository, error) {
	return addRepository(path, repo, nil)
}

// AddRepositoryWithAdminRuntime validates an invitation with active listener settings.
func AddRepositoryWithAdminRuntime(path string, repo Repository, runtime *AdminConfig) (Repository, error) {
	return addRepository(path, repo, runtime)
}

func addRepository(path string, repo Repository, runtime *AdminConfig) (Repository, error) {
	cfg, err := update(path, runtime, func(cfg *Config) error {
		for _, existing := range cfg.Repositories {
			if strings.EqualFold(existing.Name, repo.Name) {
				return nil
			}
		}
		repo.Name = strings.ToLower(repo.Name)
		cfg.Repositories = append(cfg.Repositories, repo)
		return nil
	})
	if err != nil {
		return Repository{}, err
	}
	added, _ := cfg.Repository(repo.Name)
	return added, nil
}
