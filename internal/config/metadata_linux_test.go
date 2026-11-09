//go:build linux

package config

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestUpdatePreservesPOSIXACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range []struct {
		tag, permissions uint16
		id               uint32
	}{
		{1, 6, ^uint32(0)},          // owner: read, write
		{2, 4, uint32(os.Getuid())}, // named user: read
		{4, 0, ^uint32(0)},          // owning group: none
		{16, 4, ^uint32(0)},         // mask: read
		{32, 0, ^uint32(0)},         // others: none
	} {
		at := 4 + i*8
		binary.LittleEndian.PutUint16(acl[at:], entry.tag)
		binary.LittleEndian.PutUint16(acl[at+2:], entry.permissions)
		binary.LittleEndian.PutUint32(acl[at+4:], entry.id)
	}
	if err := unix.Fsetxattr(int(file.Fd()), configACL, acl, 0); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			t.Skipf("filesystem cannot set POSIX ACL: %v", err)
		}
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := AddRepository(path, Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "owner/second" {
		t.Fatalf("repository change was not saved: %+v, %v", cfg, err)
	}
	updated, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer updated.Close()
	got, err := configACLBytes(updated)
	if err != nil || !bytes.Equal(got, acl) {
		t.Fatalf("config ACL changed: got %v, want %v: %v", got, acl, err)
	}
	info, err := updated.Stat()
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("config ACL mask or mode changed: %v, %v", info, err)
	}
}
