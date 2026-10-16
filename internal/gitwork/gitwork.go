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
	"time"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	maxPathListBytes = workspace.MaxFiles * (workspace.MaxPathBytes + 128)
	maxGitDiagnostic = 8 << 10
)

var (
	ErrNoChanges         = errors.New("no changes to commit")
	ErrTooManyFiles      = errors.New("too many changed files")
	ErrDiffTooLarge      = errors.New("staged diff exceeds size limit")
	ErrChangedFileLarge  = errors.New("changed file exceeds size limit")
	ErrHeadMoved         = errors.New("pull request head changed")
	ErrCheckoutTooLarge  = errors.New("source tree exceeds checkout limits")
	ErrUnsupportedSource = errors.New("source tree is unsupported")
	credentialHelperGH   = resolveCredentialHelperGH()
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

// Checkout owns the trusted orchestration clone; models receive separate source snapshots.
type Checkout struct {
	path         string
	url          string
	pushURL      string
	configDigest [sha256.Size]byte
	expectedHead string
	branch       string
	closed       bool
	lookupTree   func(context.Context, string) (github.GitTree, error)
	sourceTrees  map[string][]sourceEntry
}

// SetPushRepository selects a separately validated destination for fix output.
//
// A normal clone cannot publish changes until this is set.
func (c *Checkout) SetPushRepository(repo string) error {
	if err := c.ready(); err != nil {
		return err
	}
	url, err := repositoryURL(repo)
	if err != nil {
		return err
	}
	c.pushURL = url
	return nil
}

// Clone checks out the repository's default branch in an isolated directory.
// The Git remote is always derived from a validated owner/name; GitHub CLI's
// credential helper supplies authentication without embedding a token in URLs.
// Blob contents are fetched on demand.
func Clone(ctx context.Context, workRoot, repo, defaultBranch string, lookupTree func(context.Context, string) (github.GitTree, error)) (*Checkout, error) {
	url, err := repositoryURL(repo)
	if err != nil {
		return nil, err
	}
	if err := validateBranch(defaultBranch); err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	return cloneFromURL(ctx, workRoot, url, defaultBranch, lookupTree)
}

// cloneFromURL is separate so local Git fixtures can exercise the same clone
// sequence without a network call. Production callers always pass the URL
// returned by repositoryURL.
func cloneFromURL(ctx context.Context, workRoot, url, defaultBranch string, lookupTree func(context.Context, string) (github.GitTree, error)) (*Checkout, error) {
	if lookupTree == nil {
		return nil, errors.New("source tree metadata lookup is missing")
	}
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
	workRoot, err = filepath.EvalSymlinks(workRoot)
	if err != nil {
		return nil, err
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
	c := &Checkout{path: path, url: url, lookupTree: lookupTree}
	head, err := runGit(ctx, path, "rev-parse", "--verify", "refs/remotes/origin/"+defaultBranch+"^{commit}")
	if err != nil {
		return nil, err
	}
	if _, err := c.sourceTree(ctx, head); err != nil {
		return nil, err
	}
	if err := c.disableAttributes(); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, path, "checkout", "--detach", head); err != nil {
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

// Close removes the isolated checkout. It is safe to call more than once.
func (c *Checkout) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	path := c.path
	c.path = ""
	if err := cleanup.RemoveAll(path); err != nil {
		return fmt.Errorf("remove checkout: %w", err)
	}
	return nil
}

// CheckoutPullRequest fetches the base branch and GitHub's PR head ref from the
// trusted repository URL. The fetched head must match the SHA observed through
// the GitHub API, preventing a changed PR from being reviewed as another commit.
func (c *Checkout) CheckoutPullRequest(ctx context.Context, number int, baseRef, expectedHeadSHA string) error {
	if err := c.ready(); err != nil {
		return err
	}
	if number <= 0 {
		return fmt.Errorf("invalid pull request number %d", number)
	}
	if err := validateBranch(baseRef); err != nil {
		return fmt.Errorf("pull request base: %w", err)
	}
	if !workspace.ValidSHA(expectedHeadSHA) {
		return fmt.Errorf("invalid expected pull request SHA %q", expectedHeadSHA)
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
	if _, err := c.sourceTree(ctx, head); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "checkout", "--detach", head); err != nil {
		return err
	}
	c.expectedHead = strings.ToLower(head)
	c.branch = ""
	return c.recordConfig()
}

// Check expanded blobs and gitlinks, including repeated object references,
// against GitHub metadata before fetching source contents.
func checkSourceTree(tree github.GitTree, expectedSHA string) error {
	if tree.Truncated != nil && *tree.Truncated {
		return fmt.Errorf("%w: GitHub tree metadata is truncated", ErrCheckoutTooLarge)
	}
	if !strings.EqualFold(tree.SHA, expectedSHA) || !workspace.ValidSHA(tree.SHA) || tree.Truncated == nil || tree.Tree == nil {
		return fmt.Errorf("%w: incomplete or mismatched tree metadata", ErrUnsupportedSource)
	}
	var files int
	var total int64
	for _, entry := range tree.Tree {
		if !workspace.ValidSourcePath(entry.Path) {
			return fmt.Errorf("%w: unsafe source path %q", ErrUnsupportedSource, entry.Path)
		}
		switch entry.Type {
		case "tree":
			if entry.Mode != "040000" {
				return fmt.Errorf("%w: unsupported tree mode %q at %q", ErrUnsupportedSource, entry.Mode, entry.Path)
			}
			continue
		case "commit": // A submodule gitlink writes no blob into the checkout.
			if entry.Mode != "160000" {
				return fmt.Errorf("%w: unsupported gitlink mode %q at %q", ErrUnsupportedSource, entry.Mode, entry.Path)
			}
		case "blob":
			// Snapshots declare symlinks as omitted and never follow their targets.
			if entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" {
				return fmt.Errorf("%w: unsupported blob mode %q at %q", ErrUnsupportedSource, entry.Mode, entry.Path)
			}
			if entry.Size == nil || *entry.Size < 0 {
				return fmt.Errorf("%w: blob size is unavailable", ErrUnsupportedSource)
			}
			if *entry.Size > workspace.MaxBytes-total {
				return fmt.Errorf("%w: more than %d bytes", ErrCheckoutTooLarge, workspace.MaxBytes)
			}
			total += *entry.Size
		default:
			return fmt.Errorf("%w: unknown Git tree entry type %q", ErrUnsupportedSource, entry.Type)
		}
		files++
		if files > workspace.MaxFiles {
			return fmt.Errorf("%w: more than %d paths", ErrCheckoutTooLarge, workspace.MaxFiles)
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

// CommitAndPush commits changes as botLogin and pushes one branch.
//
// It stages the working tree, bounds the changed file count and patch size,
// commits without hooks or signing, and uses the validated repository URL.
// beforePush must persist the new commit SHA for later recovery.
func (c *Checkout) CommitAndPush(ctx context.Context, branch, botLogin, message string, beforePush func(string) error) error {
	if err := c.ready(); err != nil {
		return err
	}
	if c.pushURL == "" {
		return errors.New("push repository is not configured")
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

	if _, err := runGit(ctx, c.path, "add", "--all", "--", "."); err != nil {
		return err
	}
	paths, err := c.stagedPaths(ctx, "")
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return ErrNoChanges
	}
	if len(paths) > workspace.MaxChanges {
		return fmt.Errorf("%w: %d > %d", ErrTooManyFiles, len(paths), workspace.MaxChanges)
	}
	// Deleted paths may now be directories or lie beneath replacement files.
	// They contribute no new blob, so exclude them before examining the worktree.
	paths, err = c.stagedPaths(ctx, "d")
	if err != nil {
		return err
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
	if _, err := runGit(ctx, c.path, "push", "--porcelain", "--no-verify", "--", c.pushURL, "HEAD:refs/heads/"+branch); err != nil {
		return err
	}
	return c.recordConfig()
}

func (c *Checkout) stagedPaths(ctx context.Context, filter string) ([]string, error) {
	const limit = (workspace.MaxChanges + 1) * (workspace.MaxPathBytes + 128)
	args := []string{"diff", "--cached", "--name-only", "-z", "--no-renames", "--no-ext-diff"}
	if filter != "" {
		args = append(args, "--diff-filter="+filter)
	}
	data, truncated, err := c.boundedGit(ctx, limit, true, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrTooManyFiles
	}
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
		if err != nil {
			return fmt.Errorf("stat changed file %q: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("changed path %q is a directory or submodule", path)
		}
		total += info.Size()
		if info.Size() > workspace.MaxChangeBytes || total > workspace.MaxChangeBytes {
			return fmt.Errorf("%w: %q (%d bytes; %d bytes total)", ErrChangedFileLarge, path, info.Size(), total)
		}
	}
	return nil
}

func (c *Checkout) checkPatchSize(ctx context.Context) error {
	_, truncated, err := c.boundedGit(ctx, workspace.MaxContextBytes, true, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short")
	if truncated {
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

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	var output bytes.Buffer
	if err := runGitTo(ctx, dir, &output, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

func runGitTo(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	return runGitInputTo(ctx, dir, nil, stdout, args...)
}

func runGitInputTo(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, args ...string) error {
	gitArgs := []string{
		"-c", "user.name=Minion",
		"-c", "user.email=minion@localhost",
		"-c", "credential.helper=",
		"-c", credentialHelperConfig(credentialHelperGH),
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.protectHFS=true",
		"-c", "core.protectNTFS=true",
		"-c", "core.excludesFile=/dev/null",
		"-c", "core.attributesFile=/dev/null",
		"-c", "commit.gpgsign=false",
		"-c", "push.gpgSign=false",
		"-c", "submodule.recurse=false",
	}
	cmd := exec.CommandContext(ctx, "git", append(gitArgs, args...)...)
	configureGitCancellation(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = cleanGitEnvironment()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	diagnostic := bounded.Buffer{Limit: maxGitDiagnostic}
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		operation := args[0]
		for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
			operation = args[i+2]
		}
		message := strings.TrimSpace(diagnostic.String())
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", ctx.Err(), err)
		} else if (operation == "add" || operation == "checkout") && strings.HasPrefix(message, "error: invalid path ") {
			err = fmt.Errorf("%w: %w", ErrUnsupportedSource, err)
		}
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
	env := make([]string, 0, len(os.Environ())+7)
	hasPath := false
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(key, "GIT_") || key == "GCM_INTERACTIVE" || key == "GH_PROMPT_DISABLED" || key == "LC_ALL" {
			continue
		}
		if key == "PATH" {
			hasPath = true
			// Git runs its credential helper through a shell in the checkout.
			// Relative or empty entries could resolve untrusted repository commands.
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
		"LC_ALL=C",
	)
}
