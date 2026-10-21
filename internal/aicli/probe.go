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
// A nonempty secret is redacted before diagnostics are quoted.
func Probe(ctx context.Context, binary, argv0 string, args []string, dir string, env []string, input, secret string) ([]byte, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// #nosec G204 -- Callers supply trusted installed executables and fixed argument shapes without a shell.
	cmd := exec.CommandContext(runCtx, binary, args...)
	if argv0 != "" {
		cmd.Args[0] = argv0
	}
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(input)
	output := bounded.Buffer{Limit: MaxEventBytes, Cancel: cancel}
	diagnostics := bounded.Tail{}
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	err := processgroup.Run(cmd)
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(err, contextErr) {
		err = errors.Join(contextErr, err)
	}
	if output.Exceeded {
		err = errors.Join(ErrOutputLimit, err)
	}
	if err != nil {
		if text := RedactTail(&diagnostics, secret); text != "" {
			err = fmt.Errorf("%w: %q", err, text)
		}
		return nil, err
	}
	return output.Bytes(), nil
}
