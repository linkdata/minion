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
	"syscall"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	gitAttributes = "* -text -ident -filter -working-tree-encoding !diff\n"

	// maxPatchBytes bounds a published fix's binary patch independently of model context.
	maxPatchBytes = 1 << 20
	// maxChangedPathListBytes bounds a changed-path listing with Git's record metadata.
	maxChangedPathListBytes = (workspace.MaxChanges + 1) * (workspace.MaxPathBytes + 128)

	// maxPathListBytes bounds source and PR path listings including Git record metadata.
	maxPathListBytes = workspace.MaxFiles * (workspace.MaxPathBytes + 128)
	// Bound local metadata independently of source contents and repository history.
	maxSourceTreeListBytes = workspace.MaxBytes
)

var (
	ErrNoChanges         = errors.New("no changes to commit")
	ErrTooManyFiles      = errors.New("too many changed files")
	ErrDiffTooLarge      = errors.New("staged diff exceeds size limit")
	ErrCheckoutTooLarge  = errors.New("source tree exceeds checkout limits")
	ErrUnsupportedSource = errors.New("source tree is unsupported")
)

func resolveCredentialHelperGH() string {
	path, err := aicli.AbsoluteExecutable("gh")
	if err != nil {
		return "/bin/false"
	}
	// Multicall launchers can select an app from the symlink name in argv[0].
	return path
}

// Checkout owns the trusted orchestration clone; models receive separate source snapshots.
type Checkout struct {
	path           string
	configDigest   [sha256.Size]byte
	expectedHead   string
	comparisonBase string
	sourceTrees    map[string][]sourceEntry
}

// Clone pins the repository's default branch in an isolated directory.
// The Git remote is always derived from a validated owner/name; GitHub CLI's
// credential helper supplies authentication without embedding a token in URLs.
// Clones complete history; checkoutFiles materializes the trusted fix worktree.
func Clone(ctx context.Context, workRoot, repo, defaultBranch string, checkoutFiles bool) (*Checkout, error) {
	url, err := repositoryURL(repo)
	if err != nil {
		return nil, err
	}
	return cloneFromURL(ctx, workRoot, url, defaultBranch, checkoutFiles)
}

// ClonePullRequest pins the verified PR head without materializing its worktree. Full base history
// is cloned for comparison and model use, without validating or materializing the unused base tip tree.
func ClonePullRequest(ctx context.Context, workRoot, repo string, number int, baseRef, expectedHeadSHA string) (*Checkout, error) {
	url, err := repositoryURL(repo)
	if err != nil {
		return nil, err
	}
	return clonePullRequestFromURL(ctx, workRoot, url, number, baseRef, expectedHeadSHA)
}

// URL constructors let local fixtures exercise production preparation without network calls.
func cloneFromURL(ctx context.Context, workRoot, url, defaultBranch string, checkoutFiles bool) (*Checkout, error) {
	c, err := cloneRepository(ctx, workRoot, url, defaultBranch)
	if err != nil {
		return nil, err
	}
	head, err := runGit(ctx, c.path, "rev-parse", "--verify", "refs/remotes/origin/"+defaultBranch+"^{commit}")
	if err == nil {
		err = c.checkoutSource(ctx, head, checkoutFiles)
	}
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func clonePullRequestFromURL(ctx context.Context, workRoot, url string, number int, baseRef, expectedHeadSHA string) (*Checkout, error) {
	if number <= 0 {
		return nil, fmt.Errorf("invalid pull request number %d", number)
	}
	if !workspace.ValidSHA(expectedHeadSHA) {
		return nil, fmt.Errorf("invalid expected pull request SHA %q", expectedHeadSHA)
	}
	c, err := cloneRepository(ctx, workRoot, url, baseRef)
	if err != nil {
		return nil, err
	}
	if err := c.checkoutPullRequest(ctx, number, baseRef, expectedHeadSHA); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// cloneRepository fetches complete history and configures a trusted checkout before materialization.
func cloneRepository(ctx context.Context, workRoot, url, defaultBranch string) (_ *Checkout, err error) {
	if err := validateBranch(defaultBranch); err != nil {
		return nil, err
	}
	workRoot, err = workspace.CanonicalPath(workRoot)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(workRoot, "minion-checkout-")
	if err != nil {
		return nil, fmt.Errorf("create checkout directory: %w", err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			err = errors.Join(err, cleanup.RemoveAll(path))
		}
	}()

	// Disable local clone shortcuts so file inputs (including fixtures) cannot share objects with the checkout.
	if _, err := runGit(ctx, workRoot, "clone", "--template=", "--reject-shallow", "--no-local", "--no-checkout", "--branch", defaultBranch, "--", url, path); err != nil {
		return nil, err
	}
	c := &Checkout{path: path}
	if err := disableAttributes(filepath.Join(c.path, ".git")); err != nil {
		return nil, err
	}
	if err := c.recordConfig(); err != nil {
		return nil, err
	}
	removeOnError = false
	return c, nil
}

// verifiedHead returns the checkout commit only while Git config and HEAD still match
// the values verified during clone or pull request checkout.
func (c *Checkout) verifiedHead(ctx context.Context) (string, error) {
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
	if c == nil || c.path == "" {
		return nil
	}
	if err := cleanup.RemoveAll(c.path); err != nil {
		return fmt.Errorf("remove checkout: %w", err)
	}
	c.path = ""
	return nil
}

// checkoutPullRequest uses the freshly cloned base and fetches GitHub's PR head ref.
// The fetched head must match the API observation, so a changed PR is not reviewed as another commit.
func (c *Checkout) checkoutPullRequest(ctx context.Context, number int, baseRef, expectedHeadSHA string) error {
	const headDest = "refs/remotes/minion/pr-head"
	if _, err := runGit(ctx, c.path, "fetch", "--no-tags", "--force", "--", "origin",
		"+refs/pull/"+strconv.Itoa(number)+"/head:"+headDest); err != nil {
		return err
	}
	base, err := runGit(ctx, c.path, "rev-parse", "--verify", "refs/remotes/origin/"+baseRef+"^{commit}")
	if err != nil {
		return err
	}
	head, err := runGit(ctx, c.path, "rev-parse", "--verify", headDest+"^{commit}")
	if err != nil {
		return err
	}
	if head != expectedHeadSHA {
		return fmt.Errorf("pull request head changed: expected %s, fetched %s", expectedHeadSHA, head)
	}
	if err := c.checkoutSource(ctx, head, false); err != nil {
		return err
	}
	c.comparisonBase = base
	return nil
}

func (c *Checkout) checkoutSource(ctx context.Context, head string, checkoutFiles bool) error {
	if _, err := c.sourceTree(ctx, head); err != nil {
		return err
	}
	args := []string{"update-ref", "--no-deref", "HEAD", head}
	if checkoutFiles {
		args = []string{"checkout", "--detach", head}
	}
	if _, err := runGit(ctx, c.path, args...); err != nil {
		return err
	}
	c.expectedHead = head
	return nil
}

// NewBranch creates a branch at the current, verified checkout commit.
func (c *Checkout) NewBranch(ctx context.Context, name string) error {
	if err := validateBranch(name); err != nil {
		return err
	}
	if _, err := c.verifiedHead(ctx); err != nil {
		return err
	}
	if _, err := runGit(ctx, c.path, "switch", "--create", name); err != nil {
		return err
	}
	return nil
}

// CommitAndPush commits changes as botLogin and pushes one branch.
//
// Import must have successfully validated and staged the reviewed changes.
// It bounds the staged patch, commits without hooks or signing, and uses the
// validated repository URL. Unstaged files are never added here.
// beforePush must persist the new commit SHA for later recovery.
func (c *Checkout) CommitAndPush(ctx context.Context, repo, branch, botLogin, message string, beforePush func(string) error) error {
	url, err := repositoryURL(repo)
	if err != nil {
		return err
	}
	return c.commitAndPush(ctx, url, branch, botLogin, message, beforePush)
}

// The URL entry point lets local fixtures exercise publication without network calls.
func (c *Checkout) commitAndPush(ctx context.Context, pushURL, branch, botLogin, message string, beforePush func(string) error) error {
	if err := c.ready(); err != nil {
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
	if err := c.assertHead(ctx); err != nil {
		return err
	}
	currentBranch, err := runGit(ctx, c.path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	if currentBranch != "refs/heads/"+branch {
		return fmt.Errorf("current branch %q does not match push branch %q", currentBranch, branch)
	}

	count, err := c.stagedPathCount(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNoChanges
	}
	if count > workspace.MaxChanges {
		return ErrTooManyFiles
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
	_, err = runGit(ctx, c.path, "push", "--no-verify", "--", pushURL, "HEAD:refs/heads/"+branch)
	return err
}

func (c *Checkout) stagedPathCount(ctx context.Context) (int, error) {
	args := []string{"diff", "--cached", "--name-only", "-z", "--no-renames", "--no-ext-diff"}
	data, truncated, err := c.boundedGit(ctx, nil, maxChangedPathListBytes, true, args...)
	if err != nil {
		return 0, err
	}
	if truncated {
		return 0, ErrTooManyFiles
	}
	parts, err := splitNUL(data)
	if err != nil {
		return 0, fmt.Errorf("invalid Git changed path output: %w", err)
	}
	return len(parts), nil
}

// splitNUL splits terminated Git records, accepting an empty result.
func splitNUL(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("unterminated Git records")
	}
	return bytes.Split(data[:len(data)-1], []byte{0}), nil
}

func (c *Checkout) checkPatchSize(ctx context.Context) error {
	_, truncated, err := c.boundedGit(ctx, nil, maxPatchBytes, true, patchDiffArgs()...)
	if truncated {
		return ErrDiffTooLarge
	}
	return err
}

func patchDiffArgs() []string {
	return []string{"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short"}
}

func (c *Checkout) head(ctx context.Context) (string, error) {
	return runGit(ctx, c.path, "rev-parse", "--verify", "HEAD^{commit}")
}

func (c *Checkout) assertHead(ctx context.Context) error {
	head, err := c.head(ctx)
	if err != nil {
		return err
	}
	if c.expectedHead == "" || head != c.expectedHead {
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
	if c == nil || c.path == "" {
		return errors.New("checkout is closed or uninitialized")
	}
	// Defense in depth for the trusted checkout retained across preparation and
	// publication: refuse changes to its Git config. Models receive snapshots
	// and cannot access this checkout.
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
	if err := runGitInputTo(ctx, dir, nil, &output, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

// Credentials and network policy are supplied explicitly by each trusted/offline caller.
func gitCommand(ctx context.Context, binary, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, append(hardeningArgs(), args...)...)
	processgroup.Configure(cmd)
	cmd.Dir, cmd.Env = dir, env
	return cmd
}

func runGitInputTo(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, args ...string) error {
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		return err
	}
	gitArgs := []string{
		"-c", "user.name=Minion",
		"-c", "user.email=minion@localhost",
		"-c", "credential.helper=",
		"-c", credentialHelperConfig(resolveCredentialHelperGH()),
		"-c", "push.gpgSign=false",
		"-c", "submodule.recurse=false",
	}
	cmd := gitCommand(ctx, binary, dir, cleanGitEnvironment(), append(gitArgs, args...)...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	diagnostic := bounded.Buffer{Limit: bounded.DiagnosticBytes}
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		operation := args[0]
		for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
			operation = args[i+2]
		}
		return gitFailure("git", operation, diagnostic.String(), ctx.Err(), err)
	}
	return nil
}

func gitFailure(prefix, operation, message string, contextErr, err error) error {
	message = strings.TrimSpace(message)
	if contextErr != nil {
		if !errors.Is(err, contextErr) {
			err = errors.Join(contextErr, err)
		}
	} else {
		// Best-effort C-locale errno suffixes avoid matching phrases embedded in quoted paths.
		for _, line := range strings.Split(message, "\n") {
			if strings.HasSuffix(line, ": No space left on device") {
				err = fmt.Errorf("%w: %w", syscall.ENOSPC, err)
				break
			}
			if strings.HasSuffix(line, ": Disk quota exceeded") || strings.HasSuffix(line, ": Quota exceeded") {
				err = fmt.Errorf("%w: %w", syscall.EDQUOT, err)
				break
			}
		}
		err = classifyPathError(operation, message, err)
	}
	if message != "" {
		return fmt.Errorf("%s %s: %w: %q", prefix, operation, err, message)
	}
	return fmt.Errorf("%s %s: %w", prefix, operation, err)
}

// classifyPathError preserves a Git failure's cause while identifying protected
// path rejections from add/checkout. Diagnostics must use Git's C locale.
func classifyPathError(operation, message string, err error) error {
	if (operation == "add" || operation == "checkout") && (strings.HasPrefix(message, "error: invalid path ") || strings.Contains(message, "\nerror: invalid path ")) {
		return fmt.Errorf("%w: %w", ErrUnsupportedSource, err)
	}
	return err
}

// disableAttributes disables repository transformations on trusted and offline indexes.
func disableAttributes(gitDir string) error {
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, "info", "attributes"), []byte(gitAttributes), 0600)
}

// hardeningArgs returns shared Git safeguards. Callers select credentials and
// network access separately; each call receives an independent argument slice.
func hardeningArgs() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.fileMode=true",
		"-c", "core.protectHFS=true", "-c", "core.protectNTFS=true",
		"-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null",
		"-c", "commit.gpgsign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
	}
}

func credentialHelperConfig(path string) string {
	return "credential.helper=!" + aicli.ShellQuote(path) + " auth git-credential"
}

func cleanGitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(key, "GIT_") || key == "GCM_INTERACTIVE" || key == "GH_PROMPT_DISABLED" || key == "LC_ALL" || key == "PATH" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PATH="+safePath(os.Getenv("PATH")))
	return append(env, hardeningEnv()...)
}

// hardeningEnv adds prompt and locale settings to Minion-run Git.
func hardeningEnv() []string {
	return append(aicli.ModelGitEnvironment(),
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false",
		"GCM_INTERACTIVE=never", "GH_PROMPT_DISABLED=1", "LC_ALL=C",
	)
}

// safePath excludes checkout-relative command lookup, including credential helpers.
func safePath(value string) string {
	var absolute []string
	for _, dir := range filepath.SplitList(value) {
		if filepath.IsAbs(dir) {
			absolute = append(absolute, dir)
		}
	}
	if len(absolute) == 0 {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return strings.Join(absolute, string(os.PathListSeparator))
}
