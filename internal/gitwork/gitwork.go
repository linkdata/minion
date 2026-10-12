// Package gitwork creates isolated Git checkouts and publishes vetted changes.
package gitwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/github"
)

const (
	maxChangedFiles    = 100
	maxPatchBytes      = 1 << 20
	maxChangedFileSize = 1 << 20
	maxPathListBytes   = 1 << 20
	maxGitDiagnostic   = 8 << 10
	maxWorktreeEntries = 100_000
	maxCheckoutFiles   = 10_000
	maxCheckoutBytes   = 64 << 20
	maxTreeEntryBytes  = 1 << 20
	checkoutAttributes = "* -text -ident -filter -working-tree-encoding\n"
)

var (
	ErrNoChanges        = errors.New("no changes to commit")
	ErrTooManyFiles     = errors.New("too many changed files")
	ErrDiffTooLarge     = errors.New("staged diff exceeds size limit")
	ErrChangedFileLarge = errors.New("changed file exceeds size limit")
	ErrHeadMoved        = errors.New("pull request head changed")
	ErrCheckoutTooLarge = errors.New("pull request tree exceeds checkout limits")
	credentialHelperGH  = resolveCredentialHelperGH()
)

func resolveCredentialHelperGH() string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "/bin/false"
	}
	if !filepath.IsAbs(path) {
		return "/bin/false"
	}
	// Multicall launchers can select an app from the symlink name in argv[0].
	return path
}

// Checkout owns a temporary clone. Git operations are deliberately kept here,
// outside the Codex process, which receives only Path().
type Checkout struct {
	path         string
	url          string
	configDigest [sha256.Size]byte
	expectedHead string
	branch       string
	closed       bool
}

// Clone checks out the repository's default branch in an isolated directory.
// The Git remote is always derived from a validated owner/name; GitHub CLI's
// credential helper supplies authentication without embedding a token in URLs.
// Blob contents are fetched on demand, while tags are fetched for release notes.
func Clone(ctx context.Context, workRoot, repo, defaultBranch string) (*Checkout, error) {
	url, err := repositoryURL(repo)
	if err != nil {
		return nil, err
	}
	if err := validateBranch(defaultBranch); err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	return cloneFromURL(ctx, workRoot, url, defaultBranch)
}

// cloneFromURL is separate so local Git fixtures can exercise the same clone
// sequence without a network call. Production callers always pass the URL
// returned by repositoryURL.
func cloneFromURL(ctx context.Context, workRoot, url, defaultBranch string) (*Checkout, error) {
	if strings.TrimSpace(workRoot) == "" {
		return nil, errors.New("work root is empty")
	}
	workRoot, err := filepath.Abs(workRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve work root: %w", err)
	}
	if err := os.MkdirAll(workRoot, 0700); err != nil {
		return nil, fmt.Errorf("create work root: %w", err)
	}
	path, err := os.MkdirTemp(workRoot, "minion-")
	if err != nil {
		return nil, fmt.Errorf("create checkout directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(path)
		}
	}()

	if _, err := runGit(ctx, workRoot, "clone", "--filter=blob:none", "--single-branch", "--no-checkout", "--branch", defaultBranch, "--", url, path); err != nil {
		return nil, err
	}
	c := &Checkout{path: path, url: url}
	// origin was created by the trusted clone operation and has a fixed URL.
	// Fetching through its name keeps partial-clone promisor settings valid.
	if _, err := runGit(ctx, path, "fetch", "--tags", "--filter=blob:none", "--no-recurse-submodules", "origin"); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, path, "checkout", "--detach", "refs/remotes/origin/"+defaultBranch); err != nil {
		return nil, err
	}
	if c.expectedHead, err = c.head(ctx); err != nil {
		return nil, err
	}
	if err := c.recordConfig(); err != nil {
		return nil, err
	}
	cleanup = false
	return c, nil
}

// Path is the checkout directory given to Codex. It becomes empty after Close.
func (c *Checkout) Path() string {
	if c == nil || c.closed {
		return ""
	}
	return c.path
}

// Head returns the checkout commit only while Git config and HEAD still match
// the values verified during clone or pull request checkout.
func (c *Checkout) Head(ctx context.Context) (string, error) {
	if err := c.ready(); err != nil {
		return "", err
	}
	if err := c.assertHead(ctx); err != nil {
		return "", err
	}
	return c.expectedHead, nil
}

// HasTag reports whether an exact Git tag already exists in the checkout.
func (c *Checkout) HasTag(ctx context.Context, tag string) (bool, error) {
	if err := c.ready(); err != nil {
		return false, err
	}
	if err := validateBranch(tag); err != nil {
		return false, fmt.Errorf("invalid tag %q: %w", tag, err)
	}
	_, err := runGit(ctx, c.path, "show-ref", "--verify", "--quiet", "refs/tags/"+tag)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// Close removes the isolated checkout. It is safe to call more than once.
func (c *Checkout) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	path := c.path
	c.path = ""
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove checkout: %w", err)
	}
	return nil
}

// CheckoutPullRequest fetches the base branch and GitHub's PR head ref from the
// trusted repository URL. The fetched head must match the SHA observed through
// the GitHub API, preventing a changed PR from being reviewed as another commit.
func (c *Checkout) CheckoutPullRequest(ctx context.Context, number int, baseRef, expectedHeadSHA string, lookupTree func(context.Context, string) (github.GitTree, error)) error {
	if err := c.ready(); err != nil {
		return err
	}
	if number <= 0 {
		return fmt.Errorf("invalid pull request number %d", number)
	}
	if err := validateBranch(baseRef); err != nil {
		return fmt.Errorf("pull request base: %w", err)
	}
	if !validSHA(expectedHeadSHA) {
		return fmt.Errorf("invalid expected pull request SHA %q", expectedHeadSHA)
	}
	if lookupTree == nil {
		return errors.New("pull request tree metadata lookup is missing")
	}
	const baseDest = "refs/remotes/minion/base"
	const headDest = "refs/remotes/minion/pr-head"
	_, err := runGit(ctx, c.path, "fetch", "--no-tags", "--filter=blob:none", "--force", "--", c.url,
		"+refs/heads/"+baseRef+":"+baseDest,
		"+refs/pull/"+strconv.Itoa(number)+"/head:"+headDest)
	if err != nil {
		return err
	}
	// A filtered fetch may record promisor settings in the local config.
	// Remember Git's own update even when the fetched head has moved.
	if err := c.recordConfig(); err != nil {
		return err
	}
	head, err := runGit(ctx, c.path, "rev-parse", "--verify", headDest+"^{commit}")
	if err != nil {
		return err
	}
	if !strings.EqualFold(head, expectedHeadSHA) {
		return fmt.Errorf("%w: expected %s, fetched %s", ErrHeadMoved, expectedHeadSHA, head)
	}
	treeSHA, err := runGit(ctx, c.path, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return err
	}
	tree, err := lookupTree(ctx, treeSHA)
	if err != nil {
		return fmt.Errorf("load pull request tree metadata: %w", err)
	}
	if err := checkPullRequestTree(tree, treeSHA); err != nil {
		return err
	}
	// Repository attributes cannot expand blobs or run a checkout filter.
	// info/attributes takes precedence over every in-tree .gitattributes.
	if err := os.WriteFile(filepath.Join(c.path, ".git", "info", "attributes"), []byte(checkoutAttributes), 0600); err != nil {
		return fmt.Errorf("set pull request checkout attributes: %w", err)
	}
	if _, err := runGit(ctx, c.path, "checkout", "--detach", head); err != nil {
		return err
	}
	c.expectedHead = strings.ToLower(head)
	c.branch = ""
	return c.recordConfig()
}

// Check every expanded blob and gitlink, including repeated object references,
// against GitHub metadata for the exact tree fetched from the PR head.
func checkPullRequestTree(tree github.GitTree, expectedSHA string) error {
	if !strings.EqualFold(tree.SHA, expectedSHA) || !validSHA(tree.SHA) || tree.Truncated == nil || *tree.Truncated || tree.Tree == nil {
		return fmt.Errorf("%w: incomplete or mismatched tree metadata", ErrCheckoutTooLarge)
	}
	var files int
	var total int64
	for _, entry := range tree.Tree {
		if entry.Path == "" || len(entry.Path) > maxTreeEntryBytes {
			return fmt.Errorf("%w: invalid or oversized tree path", ErrCheckoutTooLarge)
		}
		switch entry.Type {
		case "tree":
			continue
		case "commit": // A submodule gitlink writes no blob into the checkout.
		case "blob":
			if entry.Size == nil || *entry.Size < 0 {
				return fmt.Errorf("%w: blob size is unavailable", ErrCheckoutTooLarge)
			}
			if *entry.Size > maxCheckoutBytes-total {
				return fmt.Errorf("%w: more than %d bytes", ErrCheckoutTooLarge, maxCheckoutBytes)
			}
			total += *entry.Size
		default:
			return fmt.Errorf("%w: unknown Git tree entry type %q", ErrCheckoutTooLarge, entry.Type)
		}
		files++
		if files > maxCheckoutFiles {
			return fmt.Errorf("%w: more than %d paths", ErrCheckoutTooLarge, maxCheckoutFiles)
		}
	}
	return nil
}

// NewBranch creates a branch at the current, verified checkout commit.
func (c *Checkout) NewBranch(ctx context.Context, name string) error {
	if err := c.ready(); err != nil {
		return err
	}
	if err := validateBranch(name); err != nil {
		return err
	}
	if err := c.assertHead(ctx); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "switch", "--create", name); err != nil {
		return err
	}
	c.branch = name
	return c.recordConfig()
}

// HasChanges includes untracked files and never runs repository hooks.
func (c *Checkout) HasChanges(ctx context.Context) (bool, error) {
	if err := c.ready(); err != nil {
		return false, err
	}
	if err := rejectNestedGitMetadata(ctx, c.path, maxWorktreeEntries); err != nil {
		return false, err
	}
	var presence presenceWriter
	if err := runGitTo(ctx, c.path, &presence, "status", "--porcelain=v1", "-z", "--untracked-files=all"); err != nil {
		return false, err
	}
	return presence.present, nil
}

// CommitAndPush commits changes as botLogin and pushes one branch.
//
// It stages the working tree, bounds the changed file count and patch size,
// commits without hooks or signing, and uses the validated repository URL.
// beforePush must persist the new commit SHA for later recovery.
func (c *Checkout) CommitAndPush(ctx context.Context, branch, botLogin, message string, beforePush func(string) error) error {
	if err := c.ready(); err != nil {
		return err
	}
	if err := rejectNestedGitMetadata(ctx, c.path, maxWorktreeEntries); err != nil {
		return err
	}
	if beforePush == nil {
		return errors.New("before-push recorder is required")
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	if strings.TrimSpace(message) == "" || strings.ContainsRune(message, 0) {
		return errors.New("commit message is empty or contains NUL")
	}
	if c.branch != branch {
		return fmt.Errorf("checkout branch %q does not match push branch %q", c.branch, branch)
	}
	if err := c.assertHead(ctx); err != nil {
		return err
	}
	currentBranch, err := runGit(ctx, c.path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}
	if currentBranch != branch {
		return fmt.Errorf("current branch %q does not match push branch %q", currentBranch, branch)
	}

	// Ignore any index state left by Codex. The commit must be derived from
	// the working tree, not an independently staged object.
	if _, err := runGit(ctx, c.path, "reset", "--mixed", "--quiet", "HEAD"); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "add", "--all", "--", "."); err != nil {
		return err
	}
	paths, err := c.stagedPaths(ctx)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return ErrNoChanges
	}
	if len(paths) > maxChangedFiles {
		return fmt.Errorf("%w: %d > %d", ErrTooManyFiles, len(paths), maxChangedFiles)
	}
	if err := c.checkChangedFileSizes(paths); err != nil {
		return err
	}
	if err := c.checkPatchSize(ctx); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "-c", "user.name="+botLogin, "-c", "user.email="+botLogin+"@users.noreply.github.com", "commit", "--no-verify", "-m", message); err != nil {
		return err
	}
	c.expectedHead, err = c.head(ctx)
	if err != nil {
		return err
	}
	if err := beforePush(c.expectedHead); err != nil {
		return fmt.Errorf("record commit before push: %w", err)
	}
	if _, err := runGit(ctx, c.path, "push", "--porcelain", "--no-verify", "--", c.url, "HEAD:refs/heads/"+branch); err != nil {
		return err
	}
	return c.recordConfig()
}

// Reject nested Git metadata before invoking credentialed Git on a workspace
// that Codex could write. ReadDir batches bound the scan even for wide trees;
// Info reports symlinks themselves, so they are never traversed.
func rejectNestedGitMetadata(ctx context.Context, root string, limit int) error {
	dirs := []string{root}
	entries := 0
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		err := func() error {
			f, err := os.Open(dir)
			if err != nil {
				return err
			}
			defer f.Close()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				batch, readErr := f.ReadDir(128)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				for _, entry := range batch {
					if dir == root && entry.Name() == ".git" {
						continue
					}
					entries++
					if entries > limit {
						return fmt.Errorf("checkout scan exceeds %d entries", limit)
					}
					path := filepath.Join(dir, entry.Name())
					if strings.EqualFold(entry.Name(), ".git") {
						return fmt.Errorf("nested Git metadata at %q", path)
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					if info.IsDir() {
						dirs = append(dirs, path)
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Checkout) stagedPaths(ctx context.Context) ([]string, error) {
	var output cappedBuffer
	output.limit = maxPathListBytes
	if err := runGitTo(ctx, c.path, &output, "diff", "--cached", "--name-only", "-z", "--no-renames", "--no-ext-diff"); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, ErrTooManyFiles
	}
	data := output.Bytes()
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("invalid Git changed path output")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, len(parts))
	for i, part := range parts {
		paths[i] = string(part)
	}
	return paths, nil
}

func (c *Checkout) checkChangedFileSizes(paths []string) error {
	var total int64
	for _, path := range paths {
		if path == "" || path == "." || !filepath.IsLocal(path) || filepath.Clean(path) != path {
			return fmt.Errorf("unsafe changed path %q", path)
		}
		info, err := os.Lstat(filepath.Join(c.path, filepath.FromSlash(path)))
		if errors.Is(err, os.ErrNotExist) {
			continue // A deletion contributes no new blob.
		}
		if err != nil {
			return fmt.Errorf("stat changed file %q: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("changed path %q is a directory or submodule", path)
		}
		total += info.Size()
		if info.Size() > maxChangedFileSize || total > maxChangedFileSize {
			return fmt.Errorf("%w: %q (%d bytes; %d bytes total)", ErrChangedFileLarge, path, info.Size(), total)
		}
	}
	return nil
}

func (c *Checkout) checkPatchSize(ctx context.Context) error {
	diffCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	counter := &byteLimitWriter{limit: maxPatchBytes, cancel: cancel}
	err := runGitTo(diffCtx, c.path, counter, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short")
	if counter.exceeded {
		return ErrDiffTooLarge
	}
	return err
}

func (c *Checkout) head(ctx context.Context) (string, error) {
	return runGit(ctx, c.path, "rev-parse", "--verify", "HEAD^{commit}")
}

func (c *Checkout) assertHead(ctx context.Context) error {
	head, err := c.head(ctx)
	if err != nil {
		return err
	}
	if c.expectedHead == "" || !strings.EqualFold(head, c.expectedHead) {
		return fmt.Errorf("checkout HEAD moved: expected %s, found %s", c.expectedHead, head)
	}
	return nil
}

func (c *Checkout) recordConfig() error {
	data, err := os.ReadFile(filepath.Join(c.path, ".git", "config"))
	if err != nil {
		return fmt.Errorf("read checkout Git config: %w", err)
	}
	c.configDigest = sha256.Sum256(data)
	return nil
}

func (c *Checkout) ready() error {
	if c == nil || c.closed || c.path == "" || c.url == "" {
		return errors.New("checkout is closed or uninitialized")
	}
	data, err := os.ReadFile(filepath.Join(c.path, ".git", "config"))
	if err != nil {
		return fmt.Errorf("read checkout Git config: %w", err)
	}
	if sha256.Sum256(data) != c.configDigest {
		return errors.New("checkout Git config changed; refusing Git operation")
	}
	return nil
}

type presenceWriter struct{ present bool }

func (w *presenceWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.present = true
	}
	return len(p), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.Len()
	if len(p) > remaining {
		w.exceeded = true
		p = p[:remaining]
	}
	_, _ = w.Buffer.Write(p)
	return n, nil
}

type byteLimitWriter struct {
	bytes    int64
	limit    int64
	exceeded bool
	cancel   context.CancelFunc
}

func (w *byteLimitWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	if w.bytes > w.limit && !w.exceeded {
		w.exceeded = true
		w.cancel()
	}
	return len(p), nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	var output bytes.Buffer
	if err := runGitTo(ctx, dir, &output, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

func runGitTo(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	gitArgs := []string{
		"-c", "credential.helper=",
		"-c", credentialHelperConfig(credentialHelperGH),
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgsign=false",
		"-c", "push.gpgSign=false",
		"-c", "submodule.recurse=false",
	}
	cmd := exec.CommandContext(ctx, "git", append(gitArgs, args...)...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnvironment()
	cmd.Stdout = stdout
	var diagnostic cappedBuffer
	diagnostic.limit = maxGitDiagnostic
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		operation := args[0]
		for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
			operation = args[i+2]
		}
		message := strings.TrimSpace(diagnostic.String())
		if message != "" {
			return fmt.Errorf("git %s: %w: %s", operation, err, message)
		}
		return fmt.Errorf("git %s: %w", operation, err)
	}
	return nil
}

func credentialHelperConfig(path string) string {
	return "credential.helper=!'" + strings.ReplaceAll(path, "'", "'\\''") + "' auth git-credential"
}

func cleanGitEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+6)
	hasPath := false
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(key, "GIT_") || key == "GCM_INTERACTIVE" || key == "GH_PROMPT_DISABLED" {
			continue
		}
		if key == "PATH" {
			hasPath = true
			// Git runs its credential helper through a shell in the checkout.
			// Relative or empty entries could resolve model-written commands there.
			var absolute []string
			for _, dir := range filepath.SplitList(value) {
				if filepath.IsAbs(dir) {
					absolute = append(absolute, dir)
				}
			}
			if len(absolute) == 0 {
				entry = "PATH=/usr/local/bin:/usr/bin:/bin"
			} else {
				entry = "PATH=" + strings.Join(absolute, string(os.PathListSeparator))
			}
		}
		env = append(env, entry)
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GCM_INTERACTIVE=never",
		"GH_PROMPT_DISABLED=1",
	)
}
