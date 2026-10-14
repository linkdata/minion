package skills

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	maxArchiveBytes     = 64 << 20
	maxExpandedBytes    = 256 << 20
	maxArchiveEntries   = 100000
	maxSkillFiles       = 256
	maxSkillBytes       = 32 << 20
	maxSingleFileBytes  = 8 << 20
	installMetadataName = ".minion-install.json"
)

type skillFile struct {
	name string
	data []byte
	mode os.FileMode
	dir  bool
}

type skillBundle struct {
	source source
	commit string
	digest string
	files  []skillFile
	bytes  int64
	seen   map[string]bool
	cases  map[string]string
}

type installer struct {
	client *http.Client
	apiURL string
	home   string
}

type stagedInstall struct {
	target, stage, backup string
	hadOld                bool
	installed             bool
}

// Install installs configured skills for Codex and Claude.
//
// It replaces same-name skill directories only when explicitly called. Each
// GitHub source is resolved to one commit before downloading. Embedded skills
// need no network access. No skill source code is run.
func Install(ctx context.Context, urls []string) error {
	home, err := userHome()
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" && req.URL.Host != "codeload.github.com" {
				return fmt.Errorf("refuse skill download redirect to %s", req.URL.Host)
			}
			return nil
		},
	}
	return (&installer{client: client, apiURL: "https://api.github.com", home: home}).install(ctx, urls)
}

func (i *installer) install(ctx context.Context, urls []string) (err error) {
	byName := make(map[string]*skillBundle)
	caseNames := make(map[string]string)
	groups := make(map[string][]*skillBundle)
	for _, rawURL := range urls {
		src, err := parse(rawURL)
		if err != nil {
			return err
		}
		if prior, exists := caseNames[strings.ToLower(src.name)]; exists && prior != src.name {
			return fmt.Errorf("skill names %q and %q collide on case-insensitive filesystems", prior, src.name)
		}
		caseNames[strings.ToLower(src.name)] = src.name
		if old, exists := byName[src.name]; exists {
			if old.source.url != src.url {
				return fmt.Errorf("skill name %q has multiple sources", src.name)
			}
			continue
		}
		bundle := &skillBundle{source: src, seen: make(map[string]bool), cases: make(map[string]string)}
		byName[src.name] = bundle
		if src.builtin {
			content, _ := BuiltinContent(src.url)
			bundle.files = []skillFile{{name: "SKILL.md", data: content, mode: 0o600}}
			bundle.digest = fmt.Sprintf("sha256:%x", sha256.Sum256(content))
			continue
		}
		key := src.owner + "/" + src.repo + "/" + src.ref
		groups[key] = append(groups[key], bundle)
	}
	if len(byName) == 0 {
		return nil
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		group := groups[key]
		first := group[0].source
		sha, err := i.commit(ctx, first)
		if err != nil {
			return err
		}
		for _, bundle := range group {
			bundle.commit = sha
		}
		if err := i.archive(ctx, first, sha, group); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	var staged []*stagedInstall
	var workDirs []string
	defer func() {
		for _, item := range staged {
			if item.stage != "" {
				err = errors.Join(err, os.RemoveAll(item.stage))
			}
		}
		for _, dir := range workDirs {
			if err != nil {
				preserve := false
				for _, item := range staged {
					if item.hadOld && filepath.Dir(item.backup) == dir {
						preserve = true
						break
					}
				}
				if preserve {
					continue
				}
			}
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	for _, root := range []string{filepath.Join(i.home, ".agents", "skills"), filepath.Join(i.home, ".claude", "skills")} {
		if err := ensureRoot(i.home, root); err != nil {
			return err
		}
		workDir, err := os.MkdirTemp(filepath.Dir(root), ".minion-skill-install-")
		if err != nil {
			return err
		}
		workDirs = append(workDirs, workDir)
		for _, name := range names {
			item, err := stage(root, workDir, byName[name])
			if err != nil {
				return err
			}
			staged = append(staged, item)
		}
	}
	for _, item := range staged {
		if err := replace(item); err != nil {
			for j := len(staged) - 1; j >= 0; j-- {
				if staged[j].installed {
					err = errors.Join(err, restore(staged[j]))
				}
			}
			return err
		}
	}
	return err
}

func (i *installer) get(ctx context.Context, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "minion-skill-installer")
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("skill download %s: HTTP %d", target, resp.StatusCode), resp.Body.Close())
	}
	return resp, nil
}

func (i *installer) commit(ctx context.Context, src source) (sha string, err error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", i.apiURL, src.owner, src.repo, src.ref)
	resp, err := i.get(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 {
		return "", fmt.Errorf("GitHub commit response is too large")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &commit); err != nil {
		return "", err
	}
	if !validSHA(commit.SHA) {
		return "", fmt.Errorf("GitHub returned an invalid commit SHA")
	}
	return commit.SHA, nil
}

func (i *installer) archive(ctx context.Context, src source, sha string, bundles []*skillBundle) (err error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", i.apiURL, src.owner, src.repo, sha)
	resp, err := i.get(ctx, endpoint)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	limited := &io.LimitedReader{R: resp.Body, N: maxArchiveBytes + 1}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	reader := tar.NewReader(gz)
	var count, expanded int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		count++
		if count > maxArchiveEntries || header.Size < 0 || header.Size > maxExpandedBytes-expanded {
			return fmt.Errorf("skill archive exceeds size limits")
		}
		expanded += header.Size
		archiveName := strings.TrimSuffix(header.Name, "/")
		separator := strings.IndexByte(archiveName, '/')
		if separator < 0 {
			continue
		}
		relative := archiveName[separator+1:]
		var matches []*skillBundle
		for _, bundle := range bundles {
			if relative != bundle.source.path && !strings.HasPrefix(relative, bundle.source.path+"/") {
				continue
			}
			if _, err := archivePath(header.Name); err != nil {
				return err
			}
			if relative == bundle.source.path {
				if header.Typeflag != tar.TypeDir {
					return fmt.Errorf("skill archive root %q is not a directory", relative)
				}
				continue
			}
			name := strings.TrimPrefix(relative, bundle.source.path+"/")
			folded := strings.ToLower(name)
			if name == "" || bundle.seen[folded] {
				return fmt.Errorf("skill archive contains duplicate entry %q", relative)
			}
			var prefix string
			for _, part := range strings.Split(name, "/") {
				if prefix != "" {
					prefix += "/"
				}
				prefix += part
				key := strings.ToLower(prefix)
				if old, exists := bundle.cases[key]; exists && old != prefix {
					return fmt.Errorf("skill archive contains case-colliding paths %q and %q", old, prefix)
				}
				bundle.cases[key] = prefix
			}
			bundle.seen[folded] = true
			if len(bundle.files) >= maxSkillFiles || header.Size > maxSingleFileBytes || header.Size > maxSkillBytes-bundle.bytes {
				return fmt.Errorf("skill %q exceeds file limits", bundle.source.name)
			}
			matches = append(matches, bundle)
		}
		if len(matches) != 0 {
			if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg || header.Mode&0o7000 != 0 {
				return fmt.Errorf("skill archive contains an unsafe entry %q", header.Name)
			}
			var data []byte
			if header.Typeflag != tar.TypeDir {
				data = make([]byte, int(header.Size))
				if _, err := io.ReadFull(reader, data); err != nil {
					return err
				}
			}
			for _, bundle := range matches {
				name := strings.TrimPrefix(relative, bundle.source.path+"/")
				// #nosec G115 -- only low permission bits remain after masking the archive mode.
				bundle.files = append(bundle.files, skillFile{name: name, data: data, mode: os.FileMode(header.Mode) & 0o777, dir: header.Typeflag == tar.TypeDir})
				bundle.bytes += header.Size
			}
		}
	}
	if limited.N == 0 {
		return fmt.Errorf("compressed skill archive exceeds size limit")
	}
	for _, bundle := range bundles {
		found := false
		for _, file := range bundle.files {
			if file.name == "SKILL.md" && !file.dir && strings.TrimSpace(string(file.data)) != "" {
				found = true
			}
			if file.name == installMetadataName {
				return fmt.Errorf("skill %q conflicts with installer metadata", bundle.source.name)
			}
		}
		if !found {
			return fmt.Errorf("skill %q has no nonempty SKILL.md", bundle.source.name)
		}
	}
	return nil
}

func archivePath(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name {
		return "", fmt.Errorf("skill archive contains an unsafe path %q", name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("skill archive contains an unsafe path %q", name)
		}
	}
	if len(parts) == 1 {
		return "", nil
	}
	return strings.Join(parts[1:], "/"), nil
}

func ensureRoot(home, root string) error {
	for _, path := range []string{home, filepath.Dir(root), root} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != home {
			if err = os.Mkdir(path, 0o700); err == nil || os.IsExist(err) {
				info, err = os.Lstat(path)
			}
		}
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

func stage(root, workDir string, bundle *skillBundle) (item *stagedInstall, err error) {
	target := filepath.Join(root, bundle.source.name)
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("existing skill %q is not a directory", target)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	dir, err := os.MkdirTemp(workDir, bundle.source.name+"-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	for _, file := range bundle.files {
		path := filepath.Join(dir, filepath.FromSlash(file.name))
		if file.dir {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o600)
		if file.mode&0o111 != 0 {
			mode = 0o700
		}
		if err := os.WriteFile(path, file.data, mode); err != nil {
			return nil, err
		}
	}
	metadata, err := json.MarshalIndent(struct {
		URL    string `json:"url"`
		Commit string `json:"commit,omitempty"`
		Digest string `json:"digest,omitempty"`
	}{bundle.source.url, bundle.commit, bundle.digest}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, installMetadataName), append(metadata, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := checkSkillDir(dir); err != nil {
		return nil, err
	}
	return &stagedInstall{target: target, stage: dir}, nil
}

func replace(item *stagedInstall) error {
	info, err := os.Lstat(item.target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("existing skill %q is not a directory", item.target)
		}
		backup, err := os.MkdirTemp(filepath.Dir(item.stage), ".backup-")
		if err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		if err := os.Rename(item.target, backup); err != nil {
			return err
		}
		item.backup, item.hadOld = backup, true
	}
	if err := os.Rename(item.stage, item.target); err != nil {
		if item.hadOld {
			restoreErr := os.Rename(item.backup, item.target)
			if restoreErr == nil {
				item.hadOld = false
			} else {
				err = errors.Join(err, fmt.Errorf("restore %q from %q: %w", item.target, item.backup, restoreErr))
			}
		}
		return err
	}
	item.stage = ""
	item.installed = true
	return nil
}

func restore(item *stagedInstall) error {
	if err := os.RemoveAll(item.target); err != nil {
		return err
	}
	item.installed = false
	if item.hadOld {
		if err := os.Rename(item.backup, item.target); err != nil {
			return fmt.Errorf("restore %q from %q: %w", item.target, item.backup, err)
		}
		item.hadOld = false
	}
	return nil
}
