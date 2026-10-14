// Package skills installs and locates operator-selected AI skills.
package skills

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type source struct {
	url, owner, repo, ref, path, name string
	builtin                           bool
}

func parse(rawURL string) (source, error) {
	if rawURL == BuiltinAntiPromptInjection {
		return source{url: rawURL, name: "anti-prompt-injection", builtin: true}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return source{}, fmt.Errorf("skill URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(rawURL, "%") {
		return source{}, fmt.Errorf("skill URL %q must be an HTTPS github.com tree URL without escapes, query, or fragment", rawURL)
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	if len(parts) < 6 || parts[2] != "tree" || parts[4] != "skills" {
		return source{}, fmt.Errorf("skill URL %q must name a GitHub skills/<name> directory with a single-segment ref", rawURL)
	}
	for _, part := range parts {
		if !safeSegment(part) {
			return source{}, fmt.Errorf("skill URL %q contains an unsafe path segment", rawURL)
		}
	}
	return source{
		url:   "https://github.com/" + strings.Join(parts, "/"),
		owner: parts[0], repo: parts[1], ref: parts[3],
		path: strings.Join(parts[4:], "/"), name: parts[len(parts)-1],
	}, nil
}

func safeSegment(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, c := range part {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// Name returns the skill directory name in a supported source URL.
func Name(rawURL string) (string, error) {
	src, err := parse(rawURL)
	return src.name, err
}

// InstalledPath returns a Codex-readable reference for an available skill.
//
// Missing skills or install metadata return installed false. Malformed,
// mismatched, or unreadable installations return an error; callers must not
// invoke a command that depends on one. The embedded anti-prompt-injection
// skill is always available and returns its built-in URL for the runner.
func InstalledPath(rawURL string) (path string, installed bool, err error) {
	src, err := parse(rawURL)
	if err != nil {
		return "", false, err
	}
	if src.builtin {
		return src.url, true, nil
	}
	home, err := userHome()
	if err != nil {
		return "", false, err
	}
	root := filepath.Join(home, ".agents", "skills")
	if err := checkParents(home, ".agents", "skills"); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	dir := filepath.Join(root, src.name)
	if err := checkSkillDir(dir); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	// #nosec G304 -- the path uses a validated skill name under the operator's home.
	metadata, err := os.ReadFile(filepath.Join(dir, ".minion-install.json"))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var recorded struct {
		URL    string `json:"url"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(metadata, &recorded); err != nil || recorded.URL != src.url || !validSHA(recorded.Commit) {
		return "", false, fmt.Errorf("skill %q has invalid or mismatched install metadata", src.name)
	}
	return filepath.Join(dir, "SKILL.md"), true, nil
}

func userHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(home)
}

func validSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func checkParents(home string, parts ...string) error {
	info, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill path %q must not be a symlink", home)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill directory %q is not a directory", home)
	}
	path := home
	for _, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill path %q must not be a symlink", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("skill directory %q is not a directory", path)
		}
	}
	return nil
}

func checkSkillDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill path %q must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill directory %q is not a directory", dir)
	}
	var found bool
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("skill %q contains a non-regular file", dir)
		}
		if path == filepath.Join(dir, "SKILL.md") {
			if !info.Mode().IsRegular() || info.Size() > 8<<20 {
				return fmt.Errorf("skill %q has an invalid SKILL.md", dir)
			}
			// #nosec G122 G304 -- only the operator can mutate this installed skill directory.
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			content, readErr := io.ReadAll(io.LimitReader(file, 8<<20+1))
			closeErr := file.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if strings.TrimSpace(string(content)) == "" {
				return fmt.Errorf("skill %q has an empty SKILL.md", dir)
			}
			found = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("skill %q has no SKILL.md", dir)
	}
	return nil
}
