package aicli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/processgroup"
)

// Probe captures bounded output from an installed CLI without model inference.
// A nonempty argv0 preserves the executable's logical name for multicall binaries.
func Probe(ctx context.Context, binary, argv0 string, args []string, dir string, env []string, input string) ([]byte, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// #nosec G204 -- Callers supply trusted installed executables and fixed argument shapes without a shell.
	cmd := exec.CommandContext(runCtx, binary, args...)
	if argv0 != "" {
		cmd.Args[0] = argv0
	}
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(input)
	processgroup.Configure(cmd)
	output := bounded.Buffer{Limit: MaxEventBytes, Cancel: cancel}
	diagnostics := bounded.Tail{}
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	runErr := cmd.Run()
	err := errors.Join(ctx.Err(), runErr)
	if output.Exceeded {
		err = errors.Join(ErrOutputLimit, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, strings.TrimSpace(diagnostics.String()))
	}
	return output.Bytes(), nil
}
