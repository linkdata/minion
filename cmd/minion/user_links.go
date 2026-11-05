package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

const maxUserLinksBytes = 8 << 20
const preferenceWriteInterval = time.Second

var errPreferenceRateLimited = errors.New("preference update rate limited")

type preferenceRateLimitError struct{ retryAfterSeconds int }

func (e preferenceRateLimitError) Error() string { return errPreferenceRateLimited.Error() }
func (e preferenceRateLimitError) Unwrap() error { return errPreferenceRateLimited }

var errUserLinkConflict = errors.New("account link conflicts with an existing identity")

// userIdentity uses the provider's immutable subject.
type userIdentity struct {
	Provider, Host, Subject string
}

func (id userIdentity) key() string {
	return id.Provider + ":" + id.Host + ":" + url.QueryEscape(id.Subject)
}

func validUserKey(key string) bool {
	provider, rest, ok := strings.Cut(key, ":")
	if !ok || provider != "github" && provider != "gitlab" {
		return false
	}
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 {
		return false
	}
	host, rawSubject := rest[:i], rest[i+1:]
	subject, err := url.QueryUnescape(rawSubject)
	if err != nil || subject == "" || len(subject) > 255 || !utf8.ValidString(subject) ||
		strings.IndexFunc(subject, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 ||
		(userIdentity{Provider: provider, Host: host, Subject: subject}).key() != key {
		return false
	}
	if provider == "github" {
		id, err := strconv.ParseInt(subject, 10, 64)
		return host == "github.com" && err == nil && id > 0 && strconv.FormatInt(id, 10) == subject
	}
	u, err := url.Parse("https://" + host)
	return err == nil && u.Host == host && u.Path == "" && u.User == nil && u.Hostname() != "" &&
		strings.ToLower(host) == host
}

type userLinks struct {
	mu                   sync.Mutex
	path                 string
	links                map[string]string // alias to an unmapped canonical identity
	preferences          map[string]config.UserModelPreferences
	bad                  error
	lastPreferenceWrite  time.Time
	lastPreferenceWriter string
}

type userLinksRecord struct {
	Version     int                                    `json:"version"`
	Links       map[string]string                      `json:"links"`
	Preferences map[string]config.UserModelPreferences `json:"preferences,omitempty"`
}

func openUserLinks(path string) (*userLinks, error) {
	links := &userLinks{path: path, links: make(map[string]string), preferences: make(map[string]config.UserModelPreferences)}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return links, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	// shortcut: Windows FileMode omits ACLs; keep the state directory private there.
	if err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		err = errors.New("user links file must be owner-only")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUserLinksBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if len(data) > maxUserLinksBytes {
		return nil, errors.New("user links file exceeds limit")
	}
	var saved userLinksRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, fmt.Errorf("decode user links: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || saved.Version != 1 || saved.Links == nil {
		return nil, errors.New("invalid user links format")
	}
	seenRoots := make(map[string]bool)
	for alias, root := range saved.Links {
		if !validUserKey(alias) || !validUserKey(root) || alias == root {
			return nil, errors.New("invalid user links identity")
		}
		if _, chained := saved.Links[root]; chained {
			return nil, errors.New("user links contain a chain")
		}
		provider, _, _ := strings.Cut(alias, ":")
		if seenRoots[root] || strings.HasPrefix(root, provider+":") {
			return nil, errors.New("user links contain duplicate providers")
		}
		seenRoots[root] = true
	}
	for root, choice := range saved.Preferences {
		if !validUserKey(root) || saved.Links[root] != "" || !validStoredPreferences(choice) {
			return nil, errors.New("invalid user preferences")
		}
	}
	links.links = saved.Links
	links.preferences = saved.Preferences
	if links.preferences == nil {
		links.preferences = make(map[string]config.UserModelPreferences)
	}
	return links, nil
}

func validStoredPreferences(value config.UserModelPreferences) bool {
	return (value.Main == (config.RoleModelConfig{}) || value.Main.ValidChoice()) &&
		(value.Critic == (config.RoleModelConfig{}) || value.Critic.ValidChoice())
}

func (s *userLinks) account(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil || !validUserKey(key) {
		return ""
	}
	return s.accountLocked(key)
}

func (s *userLinks) storageError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bad
}

func (s *userLinks) accountLocked(key string) string {
	if root := s.links[key]; root != "" {
		return root
	}
	return key
}

func (s *userLinks) hasAlias(account string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, root := range s.links {
		if root == account {
			return true
		}
	}
	return false
}

func (s *userLinks) preferencesFor(key string) (config.UserModelPreferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil {
		return config.UserModelPreferences{}, s.bad
	}
	if !validUserKey(key) {
		return config.UserModelPreferences{}, errors.New("invalid user identity")
	}
	return s.preferences[s.accountLocked(key)], nil
}

func (s *userLinks) preferencesForGitHubID(id int64) (config.UserModelPreferences, error) {
	if id <= 0 {
		return config.UserModelPreferences{}, errors.New("invalid GitHub requester ID")
	}
	return s.preferencesFor((userIdentity{Provider: "github", Host: "github.com", Subject: strconv.FormatInt(id, 10)}).key())
}

func (s *userLinks) setPreferences(key string, value config.UserModelPreferences) error {
	if !validUserKey(key) || !validStoredPreferences(value) {
		return errors.New("invalid user preferences")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil {
		return s.bad
	}
	root := s.accountLocked(key)
	if s.preferences[root] == value {
		return nil
	}
	now := time.Now()
	ready := s.lastPreferenceWrite.Add(preferenceWriteInterval)
	// shortcut: coordinated accounts can fill every slot; add a fair queue if that becomes a real workload.
	if s.lastPreferenceWriter == root {
		ready = ready.Add(preferenceWriteInterval)
	}
	if now.Before(ready) {
		return preferenceRateLimitError{retryAfterSeconds: int((ready.Sub(now) + time.Second - 1) / time.Second)}
	}
	next := maps.Clone(s.preferences)
	if next == nil {
		next = make(map[string]config.UserModelPreferences)
	}
	if value == (config.UserModelPreferences{}) {
		delete(next, root)
	} else {
		next[root] = value
	}
	err := s.saveLocked(s.links, next)
	s.lastPreferenceWrite = time.Now()
	s.lastPreferenceWriter = root
	return err
}

// link attaches a proved identity to the current account.
func (s *userLinks) link(current, proved string) error {
	if !validUserKey(current) || !validUserKey(proved) {
		return errors.New("invalid user identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil {
		return s.bad
	}
	first, second := current, proved
	if root := s.links[first]; root != "" {
		first = root
	}
	if root := s.links[second]; root != "" {
		second = root
	}
	if first == second {
		return nil
	}
	if second != proved {
		return errUserLinkConflict
	}
	provider, _, _ := strings.Cut(proved, ":")
	if strings.HasPrefix(first, provider+":") {
		return errUserLinkConflict
	}
	for _, root := range s.links {
		if root == second || root == first {
			return errUserLinkConflict
		}
	}
	next := maps.Clone(s.links)
	next[second] = first
	preferences := maps.Clone(s.preferences)
	if preferences[first] == (config.UserModelPreferences{}) && preferences[second] != (config.UserModelPreferences{}) {
		preferences[first] = preferences[second]
	}
	delete(preferences, second)
	err := s.saveLocked(next, preferences)
	if s.links[second] == first && s.lastPreferenceWriter == second {
		s.lastPreferenceWriter = first
	}
	return err
}

// saveLocked updates links and preferences together, including ambiguous writes.
func (s *userLinks) saveLocked(nextLinks map[string]string, nextPreferences map[string]config.UserModelPreferences) error {
	data, err := json.Marshal(userLinksRecord{Version: 1, Links: nextLinks, Preferences: nextPreferences})
	if err != nil || len(data) > maxUserLinksBytes {
		return errors.New("user links exceed limit")
	}
	if err := workspace.WriteAtomic(s.path, data); err != nil {
		return s.recoverAfterWriteFailure(err, nextLinks, nextPreferences)
	}
	s.links, s.preferences = nextLinks, nextPreferences
	return nil
}

// recoverAfterWriteFailure reconciles pre-rename and post-rename failures.
// The caller holds s.mu.
func (s *userLinks) recoverAfterWriteFailure(writeErr error, nextLinks map[string]string, nextPreferences map[string]config.UserModelPreferences) error {
	if err := workspace.SyncDirectories(filepath.Dir(s.path)); err != nil {
		s.bad = fmt.Errorf("save user links: %w", errors.Join(writeErr, err))
		return s.bad
	}
	saved, err := openUserLinks(s.path)
	if err != nil {
		s.bad = fmt.Errorf("save user links: %w", errors.Join(writeErr, err))
		return s.bad
	}
	oldState := maps.Equal(saved.links, s.links) && maps.Equal(saved.preferences, s.preferences)
	newState := maps.Equal(saved.links, nextLinks) && maps.Equal(saved.preferences, nextPreferences)
	if !oldState && !newState {
		s.bad = fmt.Errorf("save user links: visible links differ from both old and replacement records: %w", writeErr)
		return s.bad
	}
	s.links, s.preferences = saved.links, saved.preferences
	return fmt.Errorf("save user links: %w", writeErr)
}
