package claudecli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

// Session owns one action role's native conversation and scratch directories.
// Its zero value is ready to use. Calls must be sequential; Close ends the role.
// Stored-login transcripts follow the CLI's normal private retention policy.
// Minion never resumes this Session for another action or role.
type Session struct {
	// OnEvent receives redacted, fully emitted events; final follows validation.
	OnEvent func(aicli.StreamEvent)
	// OnInput receives prepared stdin and command metadata before the CLI starts.
	OnInput                      func(aicli.PreparedInput)
	workspace, binary, outputDir string
	writable                     bool
	settings                     config.ClaudeConfig
	runtimeDir, cacheDir         string
	authDir, configDir           string
	nativeHome                   string
	embeddedDir                  string
	skillPaths                   []string
	id                           string
	// A missing snapshot leaves only a cumulative floor until a covering snapshot arrives.
	baseline       usage.Tokens
	usageGap       bool
	started, reset bool
	closed         bool
	initErr        error
}

// NewConversation forgets the native history and accounting, retaining role paths.
// Run clears model-writable scratch before starting the independent conversation.
func (s *Session) NewConversation() {
	s.id, s.started, s.baseline, s.usageGap = "", false, usage.Tokens{}, false
	s.reset = s.runtimeDir != ""
}

// Close removes only action-owned scratch, credentials and materialized skills.
// Failed paths remain available for a later cleanup retry; Run is no longer allowed.
func (s *Session) Close() error {
	s.closed = true
	var err error
	for _, resource := range []struct {
		path  *string
		label string
	}{{&s.embeddedDir, "skill cleanup"}, {&s.authDir, "credential cleanup"}, {&s.runtimeDir, "cleanup"}} {
		if *resource.path == "" {
			continue
		}
		if failure := cleanup.RemoveAll(*resource.path); failure != nil {
			err = errors.Join(err, fmt.Errorf("claude %s: %w", resource.label, failure))
		} else {
			*resource.path = ""
		}
	}
	return aicli.RedactError(err, s.settings.APIKey)
}

func (s *Session) prepare(workspace, binary string, writable bool, outputDir string, settings config.ClaudeConfig) error {
	if s.closed {
		return errors.New("claude session is closed")
	}
	bound := settings
	bound.ReadOnlyDirs = nil // Evidence grants are selected afresh for each phase.
	if len(bound.Skills) == 0 {
		bound.Skills = nil
	}
	if s.workspace != "" {
		if s.workspace != workspace || s.binary != binary || s.outputDir != outputDir || s.writable != writable || !reflect.DeepEqual(s.settings, bound) {
			return errors.New("claude session source, role or settings changed")
		}
		if s.initErr != nil {
			return s.initErr
		}
		selected, home, err := configDirectory(s.authDir)
		if err != nil {
			return err
		}
		if s.authDir == "" && home != s.nativeHome {
			return errors.New("claude session native home changed")
		}
		if selected != s.configDir {
			return errors.New("claude session native configuration changed")
		}
	} else {
		s.workspace, s.binary, s.outputDir, s.writable, s.settings = workspace, binary, outputDir, writable, bound
		s.settings.Skills = slices.Clone(bound.Skills)
		s.initErr = s.initialize()
		if s.initErr != nil {
			return s.initErr
		}
	}
	if s.reset {
		if err := cleanup.RemoveAll(s.runtimeDir); err != nil {
			return fmt.Errorf("claude conversation scratch cleanup: %w", err)
		}
		if err := os.Mkdir(s.runtimeDir, 0700); err != nil {
			return err
		}
		if err := os.Mkdir(s.cacheDir, 0700); err != nil {
			return err
		}
		s.reset = false
	}
	// Do not let a previous model call supply Git config to native startup.
	if err := cleanup.RemoveAll(filepath.Join(s.runtimeDir, ".git")); err != nil {
		return fmt.Errorf("claude runtime Git metadata cleanup: %w", err)
	}
	// Native startup must not follow temp paths planted by the previous model call.
	// Earlier CLI temp outputs are intentionally discarded; native transcripts stay outside tmp.
	tmp := filepath.Join(s.runtimeDir, "tmp")
	if err := cleanup.RemoveAll(tmp); err != nil {
		return fmt.Errorf("claude native temp cleanup: %w", err)
	}
	if err := os.Mkdir(tmp, 0700); err != nil {
		return fmt.Errorf("claude native temp directory: %w", err)
	}
	return nil
}

func (s *Session) initialize() (err error) {
	if s.runtimeDir, err = os.MkdirTemp(filepath.Dir(s.workspace), ".minion-claude-"); err != nil {
		return
	}
	if s.authDir, err = authentication(filepath.Dir(s.workspace), s.settings.APIKey); err != nil {
		return
	}
	if s.configDir, s.nativeHome, err = configDirectory(s.authDir); err != nil {
		return
	}
	s.cacheDir = filepath.Join(s.runtimeDir, "go-build")
	if err = os.Mkdir(s.cacheDir, 0700); err != nil {
		return
	}
	s.skillPaths, s.embeddedDir, err = aicli.MaterializeSkills(s.workspace, s.settings.Skills)
	if err != nil {
		return fmt.Errorf("claude skills: %w", err)
	}
	return nil
}

func conversationID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6], id[8] = id[6]&0x0f|0x40, id[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}
