package workroot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckRefusesUnboundedDefaultLayout(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Check(work, filepath.Join(root, "state.json"), DefaultMaxBytes); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("same-filesystem work root accepted: %v", err)
	}
	if err := Check(work, filepath.Join(root, "new", "nested", "state.json"), DefaultMaxBytes); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("missing state directory bypassed same-filesystem check: %v", err)
	}
	if err := Check(filepath.Join(root, "missing"), filepath.Join(root, "state.json"), DefaultMaxBytes); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("missing work mount accepted: %v", err)
	}
}

func TestExistingStateDirResolvesMissingChildrenAndSymlinks(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "state-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := existingStateDir(filepath.Join(link, "new", "nested"))
	if err != nil || resolved != root {
		t.Fatalf("state filesystem ancestor = %q, %v; want %q", resolved, err, root)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created state directory: %v", err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := existingStateDir(filepath.Join(dangling, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling state directory symlink was accepted: %v", err)
	}
}

func TestDeviceAndCapacityRequirements(t *testing.T) {
	for _, tc := range []struct {
		name                string
		work, parent, state uint64
		blocks              uint64
		blockSize, maxBytes int64
		want                bool
	}{
		{"separate bounded mount", 2, 1, 1, 512, 4096, 2 << 20, true},
		{"unmounted", 1, 1, 2, 512, 4096, 2 << 20, false},
		{"shares state filesystem", 2, 1, 2, 512, 4096, 2 << 20, false},
		{"oversized", 2, 1, 1, 513, 4096, 2 << 20, false},
		{"unknown capacity", 2, 1, 1, 0, 4096, 2 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDevices(tc.work, tc.parent, tc.state)
			if err == nil {
				err = validateCapacity(tc.blocks, tc.blockSize, tc.maxBytes)
			}
			if (err == nil) != tc.want {
				t.Fatalf("validation = %v, want pass=%v", err, tc.want)
			}
		})
	}
}

func TestFreeSpaceRequirement(t *testing.T) {
	const blockSize = 4096
	required := uint64(minFreeBytes / blockSize)
	if err := validateFreeSpace(required, blockSize); err != nil {
		t.Fatalf("64 MiB free was refused: %v", err)
	}
	if err := validateFreeSpace(required-1, blockSize); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("less than 64 MiB free was accepted: %v", err)
	}
	if err := validateFreeSpace(0, blockSize); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("full filesystem was accepted: %v", err)
	}
}
