//go:build linux

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const configACL = "system.posix_acl_access"
const configSELinuxLabel = "security.selinux"

func newConfigTemp(target string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(target), ".config-*")
}

func cleanupConfigTemp(name string) error {
	if err := os.Remove(name); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func configACLBytes(file *os.File) ([]byte, error) {
	return configAttributeBytes(file, configACL)
}

func configAttributeBytes(file *os.File, name string) ([]byte, error) {
	size, err := unix.Fgetxattr(int(file.Fd()), name, nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	if _, err := unix.Fgetxattr(int(file.Fd()), name, value); err != nil {
		return nil, err
	}
	return value, nil
}

func preserveConfigMetadata(target string, replacement *os.File, info os.FileInfo) error {
	original, err := os.Open(target)
	if err != nil {
		return err
	}
	defer original.Close()
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read config owner")
	}
	newInfo, err := replacement.Stat()
	if err != nil {
		return err
	}
	newOwner, ok := newInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read replacement owner")
	}
	if owner.Uid != newOwner.Uid || owner.Gid != newOwner.Gid {
		if err := replacement.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			return fmt.Errorf("preserve config owner and group: %w", err)
		}
	}
	acl, err := configACLBytes(original)
	if err != nil {
		return fmt.Errorf("read config ACL: %w", err)
	}
	if acl == nil {
		if err := unix.Fremovexattr(int(replacement.Fd()), configACL); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("clear inherited config ACL: %w", err)
		}
	} else if err := unix.Fsetxattr(int(replacement.Fd()), configACL, acl, 0); err != nil {
		return fmt.Errorf("preserve config ACL: %w", err)
	}
	if err := replacement.Chmod(info.Mode() & configPermissionBits); err != nil {
		return err
	}
	newInfo, err = replacement.Stat()
	if err != nil {
		return err
	}
	newOwner = newInfo.Sys().(*syscall.Stat_t)
	newACL, err := configACLBytes(replacement)
	if err != nil {
		return err
	}
	oldLabel, err := configAttributeBytes(original, configSELinuxLabel)
	if err != nil {
		return fmt.Errorf("read config SELinux label: %w", err)
	}
	newLabel, err := configAttributeBytes(replacement, configSELinuxLabel)
	if err != nil {
		return fmt.Errorf("read replacement SELinux label: %w", err)
	}
	if newOwner.Uid != owner.Uid || newOwner.Gid != owner.Gid || newInfo.Mode()&configPermissionBits != info.Mode()&configPermissionBits || !bytes.Equal(newACL, acl) || !bytes.Equal(newLabel, oldLabel) {
		return errors.New("replacement config permissions differ from original")
	}
	return nil
}
