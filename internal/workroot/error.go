// Package workroot reports unsafe placement of executor workspaces.
package workroot

import "errors"

// ErrUnsafe indicates that the work root cannot safely accept or continue job work.
var ErrUnsafe = errors.New("unsafe or unavailable work root")
