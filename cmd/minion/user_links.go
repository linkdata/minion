package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/linkdata/minion/internal/workspace"
)

const maxUserLinksBytes = 8 << 20

// userIdentity uses the provider's immutable ID; Label is display-only.
type userIdentity struct {
	Provider string
	Host     string
	ID       int64
	Label    string
}

func (id userIdentity) key() string {
	return id.Provider + ":" + id.Host + ":" + strconv.FormatInt(id.ID, 10)
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
	host, rawID := rest[:i], rest[i+1:]
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || (userIdentity{Provider: provider, Host: host, ID: id}).key() != key {
		return false
	}
	if provider == "github" {
		return host == "github.com"
	}
	u, err := url.Parse("https://" + host)
	return err == nil && u.Host == host && u.Path == "" && u.User == nil && u.Hostname() != "" &&
		strings.ToLower(host) == host
}

type userLinks struct {
	mu    sync.Mutex
	path  string
	links map[string]string // alias to an unmapped canonical identity
	bad   error
}

func openUserLinks(path string) (*userLinks, error) {
	links := &userLinks{path: path, links: make(map[string]string)}
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
	var saved struct {
		Version int               `json:"version"`
		Links   map[string]string `json:"links"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, fmt.Errorf("decode user links: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || saved.Version != 1 {
		return nil, errors.New("invalid user links version or trailing data")
	}
	for alias, root := range saved.Links {
		if !validUserKey(alias) || !validUserKey(root) || alias == root {
			return nil, errors.New("invalid user links identity")
		}
		if _, chained := saved.Links[root]; chained {
			return nil, errors.New("user links contain a chain")
		}
	}
	links.links = saved.Links
	return links, nil
}

func (s *userLinks) account(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil || !validUserKey(key) {
		return ""
	}
	if root := s.links[key]; root != "" {
		return root
	}
	return key
}

// link joins two proven accounts, retaining every prior alias of each account.
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
	next := make(map[string]string, len(s.links)+1)
	for alias, root := range s.links {
		if root == second {
			root = first
		}
		next[alias] = root
	}
	next[second] = first
	data, err := json.Marshal(struct {
		Version int               `json:"version"`
		Links   map[string]string `json:"links"`
	}{Version: 1, Links: next})
	if err != nil || len(data) > maxUserLinksBytes {
		return errors.New("user links exceed limit")
	}
	if err := workspace.WriteAtomic(s.path, data); err != nil {
		s.bad = fmt.Errorf("save user links: %w", err)
		return s.bad
	}
	s.links = next
	return nil
}
