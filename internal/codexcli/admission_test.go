package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestSessionUnlaunchedCallsHaveCompleteZeroUsage(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resumed"}[resumed], func(t *testing.T) {
			binary := installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			dir := t.TempDir()
			wantCalls := ""
			if resumed {
				if _, err := session.Run(context.Background(), dir, "first", false, "", config.CodexConfig{}); err != nil {
					t.Fatal(err)
				}
				wantCalls = "called"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := session.Run(ctx, dir, "cancelled before start", false, "", config.CodexConfig{})
			if !errors.Is(err, context.Canceled) || result.Answer != "" || !result.UsageComplete || result.Tokens != (usage.Tokens{}) {
				t.Fatalf("unlaunched call: %+v error=%v", result, err)
			}
			data, err := os.ReadFile(binary + ".calls")
			if string(data) != wantCalls || (err != nil && !(wantCalls == "" && errors.Is(err, os.ErrNotExist))) {
				t.Fatalf("cancelled call reached CLI: %q %v", data, err)
			}
			if _, err := session.Run(context.Background(), dir, "retry after no launch", false, "", config.CodexConfig{}); err != nil {
				t.Fatalf("unlaunched call poisoned session: %v", err)
			}
		})
	}
}

func TestSessionStartFailureDoesNotPoisonFreshRetry(t *testing.T) {
	binary := installFakeCodex(t, "#!/missing-minion-fixture-interpreter\n")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	result, err := session.Run(context.Background(), dir, "first", false, "", config.CodexConfig{})
	if !errors.Is(err, os.ErrNotExist) || result.Answer != "" || !result.UsageComplete || result.Tokens != (usage.Tokens{}) {
		t.Fatalf("unstarted exec: %+v error=%v", result, err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if result, err := session.Run(context.Background(), dir, "retry", false, "", config.CodexConfig{}); err != nil || result.Answer != "done" {
		t.Fatalf("fresh retry: %+v %v", result, err)
	}
}

func TestSessionRejectsOutputOverlapBeforeBinding(t *testing.T) {
	binary := installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	dir := t.TempDir()
	output := filepath.Join(dir, "reports")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(context.Background(), dir, "invalid", false, output, config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "output directory must be separate") {
		t.Fatalf("overlapping output accepted: %v", err)
	}
	if _, err := os.Stat(binary + ".calls"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid input reached CLI: %v", err)
	}
	if result, err := session.Run(context.Background(), dir, "corrected", false, t.TempDir(), config.CodexConfig{}); err != nil || result.Answer != "done" {
		t.Fatalf("invalid input bound session: %+v %v", result, err)
	}
}

func TestSessionRejectsReadGrantBeforeBinding(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	settings := config.CodexConfig{Model: "first", ReadOnlyDirs: []string{filepath.Join(t.TempDir(), "missing")}}
	if _, err := session.Run(context.Background(), dir, "invalid", false, "", settings); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing input accepted: %v", err)
	}
	settings.Model, settings.ReadOnlyDirs = "corrected", []string{t.TempDir()}
	if result, err := session.Run(context.Background(), dir, "corrected", false, "", settings); err != nil || result.Answer != "done" {
		t.Fatalf("invalid read grant bound session: %+v %v", result, err)
	}
}

func TestSessionRejectsSkillBeforeBinding(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	skill := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skill, []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), dir, "invalid", false, "", config.CodexConfig{Skills: []string{skill}}); err == nil || !strings.Contains(err.Error(), "skill is inside the untrusted source snapshot") {
		t.Fatalf("source skill accepted: %v", err)
	}
	if session.bound || session.runtimeDir != "" {
		t.Fatalf("invalid skill bound session or allocated runtime scratch: bound=%t runtime=%q", session.bound, session.runtimeDir)
	}
	if result, err := session.Run(context.Background(), dir, "corrected", false, "", config.CodexConfig{}); err != nil || result.Answer != "done" {
		t.Fatalf("invalid skill blocked corrected retry: %+v %v", result, err)
	}
}
