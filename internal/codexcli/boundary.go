package codexcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/processgroup"
)

const (
	probePrompt = "MINION_TRUSTED_PROMPT_78"
	probeAgent  = "MINION_UNTRUSTED_AGENTS_78"
	probeSkill  = "MINION_UNTRUSTED_SKILL_78"
)

func instructionBoundaryConfig() []string {
	return []string{
		"-c", "project_doc_max_bytes=0",
		"-c", `project_root_markers=["` + instructionRoot + `"]`,
	}
}

// ValidateInstructionBoundary checks the installed Codex CLI's instruction isolation.
//
// It renders inert prompts from a source fixture and an isolated runtime without
// starting an inference turn, then fails if the CLI cannot verify separation.
func ValidateInstructionBoundary(ctx context.Context) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	binary, err := ExecutablePath()
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "minion-codex-boundary-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(root)) }()
	repo := filepath.Join(root, "repo")
	runtimeDir := filepath.Join(repo, "var", "work", "runtime")
	skillDir := filepath.Join(repo, ".agents", "skills", "untrusted-canary")
	probeHome := filepath.Join(root, "home")
	for _, dir := range []string{runtimeDir, skillDir, probeHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(repo, "AGENTS.md"):           probeAgent + "\n",
		filepath.Join(skillDir, "SKILL.md"):        "---\nname: untrusted-canary\ndescription: " + probeSkill + "\n---\nInert test skill.\n",
		filepath.Join(runtimeDir, instructionRoot): "",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return err
		}
	}
	// #nosec G204 -- Git receives one generated path as an argument, without a shell.
	git := exec.CommandContext(ctx, "git", "init", "-q", repo)
	git.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + probeHome, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if err := git.Run(); err != nil {
		return fmt.Errorf("create Codex instruction probe repository: %w", err)
	}
	// A visible control proves this CLI exposes both instruction sources to
	// prompt-input. The isolated renders then check project-root isolation and
	// the production skill-catalogue setting separately.
	for _, check := range []struct {
		dir        string
		config     []string
		wantCanary bool
	}{
		{dir: repo, config: []string{"-c", "skills.include_instructions=true"}, wantCanary: true},
		{dir: runtimeDir, config: append(instructionBoundaryConfig(), "-c", "skills.include_instructions=false")},
		{dir: runtimeDir, config: instructionBoundaryConfig()},
	} {
		args := append([]string{"debug", "prompt-input"}, check.config...)
		args = append(args, probePrompt)
		runCtx, cancelRun := context.WithCancel(ctx)
		// #nosec G204 -- Codex receives only fixed config flags and an inert prompt.
		cmd := exec.CommandContext(runCtx, binary, args...)
		cmd.Args[0] = "codex"
		cmd.Dir = check.dir
		cmd.Env = childEnvironment([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + probeHome, "CODEX_HOME=" + probeHome}, runtimeDir, runtimeDir, false)
		processgroup.Configure(cmd)
		output := bounded.Buffer{Limit: maxEventBytes, Cancel: cancelRun}
		diagnostics := bounded.Tail{}
		cmd.Stdout, cmd.Stderr = &output, &diagnostics
		err := cmd.Run()
		cancelRun()
		probeErr := errors.Join(ctx.Err(), err)
		if output.Exceeded {
			probeErr = errors.Join(aicli.ErrOutputLimit, probeErr)
		}
		if probeErr != nil {
			return fmt.Errorf("render Codex instruction probe: %w: %q", probeErr, strings.TrimSpace(diagnostics.String()))
		}
		foundAgent := bytes.Contains(output.Bytes(), []byte(probeAgent))
		foundSkill := bytes.Contains(output.Bytes(), []byte(probeSkill))
		if check.wantCanary && (!foundAgent || !foundSkill) {
			return errors.New("codex instruction probe cannot observe repository instructions and skills")
		}
		if !check.wantCanary && (foundAgent || foundSkill) {
			return errors.New("codex loaded source snapshot instructions or repository skills")
		}
		var messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(output.Bytes(), &messages); err != nil {
			return fmt.Errorf("decode Codex instruction probe: %w", err)
		}
		trusted := false
		for _, message := range messages {
			for _, content := range message.Content {
				if message.Role == "user" && strings.Contains(content.Text, probePrompt) {
					trusted = true
				}
			}
		}
		if !trusted {
			return errors.New("codex omitted the trusted instruction probe prompt")
		}
	}
	return nil
}
