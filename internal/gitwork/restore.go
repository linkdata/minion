package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/workspace"
)

// RestoreCheckout recreates a trusted fix checkout from the pinned Job source.
// It does not fetch a branch that may have moved since the action ran.
func RestoreCheckout(ctx context.Context, workRoot string, snapshot workspace.Snapshot) (_ *Checkout, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("restore checkout: %w", err)
		}
	}()
	if err := workspace.ValidateSnapshot(snapshot); err != nil {
		return nil, err
	}
	root, err := workspace.CanonicalPath(workRoot)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(root, "minion-checkout-")
	if err != nil {
		return nil, fmt.Errorf("create restored checkout: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup.RemoveAll(path))
		}
	}()
	if err := workspace.Materialize(path, snapshot.Files); err != nil {
		return nil, err
	}
	if _, err := MaterializeRepository(ctx, path, snapshot); err != nil {
		return nil, err
	}
	// The imported repository starts with the model-facing offline protocol ban.
	// This private checkout may push only through CommitAndPush's validated URL.
	if _, err := runGit(ctx, path, "config", "--local", "--unset", "protocol.allow"); err != nil {
		return nil, err
	}
	checkout := &Checkout{path: path, expectedHead: snapshot.Head}
	if err := checkout.recordConfig(); err != nil {
		return nil, err
	}
	if _, err := checkout.verifiedHead(ctx); err != nil {
		return nil, err
	}
	return checkout, nil
}
