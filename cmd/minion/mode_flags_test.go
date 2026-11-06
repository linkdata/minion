package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestExecuteModeRejectsServiceFlagsBeforeStartup(t *testing.T) {
	for _, args := range [][]string{
		{"-mode=execute", "-config=unused.json"},
		{"-mode=execute", "-oneshot"},
		{"-mode=execute", "-check"},
		{"-mode=execute", "-install-skills"},
		{"-mode=unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			oldFlags, oldArgs := flag.CommandLine, os.Args
			defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
			flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
			os.Args = append([]string{"minion"}, args...)
			if code := runWithGitCheck(func(context.Context, string) error {
				t.Fatal("invalid mode reached Git preflight")
				return nil
			}); code != 2 {
				t.Fatalf("invalid mode returned %d; want usage error 2", code)
			}
		})
	}
}

func TestInProcessModeRejectedBeforeStartup(t *testing.T) {
	code, output := runStartupFixtureWithRun(t, config.Config{}, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			t.Fatal("retired mode reached Git preflight")
			return nil
		})
	}, "-mode=in-process")
	if code != 2 || !strings.Contains(output, `unsupported mode "in-process"; use orchestrate, execute, remote-execute, or hash`) {
		t.Fatalf("retired mode: exit=%d log=%q", code, output)
	}
}

func TestOneShotAndCheckRejectedBeforeStartup(t *testing.T) {
	code, output := runStartupFixtureWithRun(t, config.Config{}, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			t.Fatal("conflicting flags reached Git preflight")
			return nil
		})
	}, "-oneshot", "-check")
	if code != 2 || !strings.Contains(output, "-oneshot cannot be combined with -check") {
		t.Fatalf("conflicting flags: exit=%d log=%q", code, output)
	}
}

func TestInstallSkillsConflictRejectedBeforeConfigLoad(t *testing.T) {
	for _, other := range []string{"-oneshot", "-check"} {
		t.Run(other, func(t *testing.T) {
			code, output := runStartupFixture(t, config.Config{}, "-install-skills", other)
			if code != 2 || !strings.Contains(output, "-install-skills cannot be combined with -oneshot or -check") {
				t.Fatalf("conflicting flags: exit=%d log=%q", code, output)
			}
		})
	}
}
