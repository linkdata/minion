package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/config"
)

// Session owns one action role's stable scratch paths and native conversation.
// Its zero value is ready to use. Calls must be serialized, followed by Close.
// Native transcripts retain the CLI's normal private persistence policy.
type Session struct {
	bound, used, reset, skillsReady bool
	workspace, binary, home, output string
	homeEnv, overrideEnv            string
	writable                        bool
	settings                        config.CodexConfig
	runtimeDir, embeddedDir         string
	skillPaths                      []string
	threadID                        string
	accounting                      usageSession
}

// Run executes one task in a new conversation and removes its scratch paths.
// Use Session.Run to continue an action conversation across invocations.
func Run(ctx context.Context, dir, prompt string, writable bool, outputDir string, settings config.CodexConfig) (result aicli.Result, runError error) {
	var session Session
	defer func() {
		if err := session.Close(); err != nil {
			result.Answer = ""
			runError = errors.Join(runError, err)
		}
	}()
	return session.Run(ctx, dir, prompt, writable, outputDir, settings)
}

// NewConversation makes the next invocation fresh, at the same stable paths.
// Scratch removal is deferred to Run so filesystem failures can be returned.
func (s *Session) NewConversation() {
	s.threadID, s.used = "", false
	s.accounting = usageSession{}
	s.reset = s.runtimeDir != ""
}

// Close removes owned scratch and embedded skills, but never native transcripts.
func (s *Session) Close() error {
	var err error
	for _, owned := range []struct {
		path  *string
		label string
	}{{&s.runtimeDir, "temporary directory"}, {&s.embeddedDir, "embedded skill root"}} {
		if *owned.path == "" {
			continue
		}
		if cleanupErr := cleanup.RemoveAll(*owned.path); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("codex exec: remove %s: %w", owned.label, cleanupErr))
		} else {
			*owned.path = ""
		}
	}
	err = aicli.RedactError(err, s.settings.APIKey)
	if err == nil {
		*s = Session{}
	}
	return err
}

func (s *Session) prepare(workspace, binary, home, output string, writable bool, settings config.CodexConfig) error {
	if s.bound {
		if workspace != s.workspace || binary != s.binary || home != s.home || output != s.output || writable != s.writable ||
			os.Getenv("HOME") != s.homeEnv || os.Getenv("CODEX_HOME") != s.overrideEnv ||
			settings.Model != s.settings.Model || settings.ReasoningEffort != s.settings.ReasoningEffort || settings.APIKey != s.settings.APIKey ||
			settings.OutputSchema != s.settings.OutputSchema || !slices.Equal(settings.Skills, s.settings.Skills) {
			return errors.New("codex exec: session source, output or host settings changed")
		}
		if s.used && s.threadID == "" {
			return errors.New("codex exec: previous invocation did not identify a resumable thread")
		}
	} else {
		s.bound = true
		s.workspace, s.binary, s.home, s.output, s.writable = workspace, binary, home, output, writable
		s.homeEnv, s.overrideEnv = os.Getenv("HOME"), os.Getenv("CODEX_HOME")
		s.settings = settings
		s.settings.Skills = slices.Clone(settings.Skills)
		s.settings.ReadOnlyDirs = nil
	}
	if s.runtimeDir == "" {
		var err error
		s.runtimeDir, err = os.MkdirTemp(filepath.Dir(workspace), ".minion-codex-")
		if err != nil {
			return fmt.Errorf("codex exec: create temporary directory: %w", err)
		}
	}
	if s.reset {
		if err := cleanup.RemoveAll(s.runtimeDir); err != nil {
			return fmt.Errorf("codex exec: reset conversation scratch: %w", err)
		}
		if err := os.Mkdir(s.runtimeDir, 0700); err != nil {
			return fmt.Errorf("codex exec: recreate conversation scratch: %w", err)
		}
		s.reset = false
	}
	if err := writeRuntimeFile(filepath.Join(s.runtimeDir, instructionRoot), nil); err != nil {
		return fmt.Errorf("codex exec: mark isolated project root: %w", err)
	}
	cacheDir := filepath.Join(s.runtimeDir, "go-build")
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("codex exec: create build cache: %w", err)
		}
		info, statErr := os.Lstat(cacheDir)
		if statErr != nil {
			return fmt.Errorf("codex exec: inspect build cache: %w", statErr)
		}
		if !info.IsDir() {
			return errors.New("codex exec: build cache is not a directory")
		}
	}
	if !s.skillsReady {
		if s.embeddedDir != "" {
			if err := cleanup.RemoveAll(s.embeddedDir); err != nil {
				return fmt.Errorf("codex exec: remove incomplete embedded skill root: %w", err)
			}
			s.embeddedDir = ""
		}
		var err error
		s.skillPaths, s.embeddedDir, err = aicli.MaterializeSkills(workspace, settings.Skills)
		if err != nil {
			return fmt.Errorf("codex exec: skills: %w", err)
		}
		s.skillsReady = true
	}
	return nil
}

// A previous model may have replaced a runtime file with a symlink. Unlink it
// and create exclusively rather than following model-controlled file targets.
func writeRuntimeFile(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}
