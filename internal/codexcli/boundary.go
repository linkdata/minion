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
	"github.com/linkdata/minion/internal/executil"
)

const (
	probePrompt       = "MINION_TRUSTED_PROMPT_78"
	probeAgent        = "MINION_UNTRUSTED_AGENTS_78"
	probeRuntimeAgent = "MINION_UNTRUSTED_RUNTIME_AGENTS_78"
	probeSkill        = "MINION_UNTRUSTED_SKILL_78"
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
	binary, err := aicli.ExecutablePath("codex")
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "minion-codex-boundary-")
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, os.RemoveAll(root))
	}()
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
		filepath.Join(runtimeDir, "AGENTS.md"):     probeRuntimeAgent + "\n",
		filepath.Join(skillDir, "SKILL.md"):        "---\nname: untrusted-canary\ndescription: " + probeSkill + "\n---\nInert test skill.\n",
		filepath.Join(runtimeDir, instructionRoot): "",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return err
		}
	}
	gitBinary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		return err
	}
	// #nosec G204 -- Git receives one generated path as an argument, without a shell.
	git := exec.CommandContext(ctx, gitBinary, "init", "-q", repo)
	git.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + probeHome}, aicli.ModelGitEnvironment()...)
	if err := executil.Run(git); err != nil {
		return fmt.Errorf("create Codex instruction probe repository: %w", err)
	}
	// A visible control proves this CLI exposes both instruction sources to
	// prompt-input. The isolated renders then check project-root isolation and
	// the production skill-catalogue setting separately.
	env := childEnvironment([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + probeHome, "CODEX_HOME=" + probeHome}, runtimeDir, runtimeDir, probeHome, false)
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
		output, err := aicli.Probe(ctx, binary, "codex", args, check.dir, env, "", "")
		if err != nil {
			return fmt.Errorf("render Codex instruction probe: %w", err)
		}
		foundAgent := bytes.Contains(output, []byte(probeAgent))
		foundRuntimeAgent := bytes.Contains(output, []byte(probeRuntimeAgent))
		foundSkill := bytes.Contains(output, []byte(probeSkill))
		if check.wantCanary && (!foundAgent || !foundSkill) {
			return errors.New("codex instruction probe cannot observe repository instructions and skills")
		}
		if !check.wantCanary && (foundAgent || foundRuntimeAgent || foundSkill) {
			return errors.New("codex loaded untrusted instructions or repository skills")
		}
		var messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(output, &messages); err != nil {
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
