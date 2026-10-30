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
		{"-mode=execute", "-loop"},
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
	if code != 2 || !strings.Contains(output, `unsupported mode "in-process"; use orchestrate, execute, or remote-execute`) {
		t.Fatalf("retired mode: exit=%d log=%q", code, output)
	}
}
