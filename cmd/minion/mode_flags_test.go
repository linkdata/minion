package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"
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
