//go:build darwin

package config

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func newConfigTemp(target string) (*os.File, error) {
	dir, err := os.MkdirTemp(filepath.Dir(target), ".config-*")
	if err != nil {
		return nil, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return nil, errors.Join(err, os.Remove(dir))
	}
	if err := RemoveInheritedConfigACL(directory); err != nil {
		return nil, errors.Join(err, directory.Close(), os.Remove(dir))
	}
	info, statErr := directory.Stat()
	closeErr := directory.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return nil, errors.Join(err, os.Remove(dir))
	}
	if info.Mode().Perm() != 0o700 {
		return nil, errors.Join(errors.New("config temporary directory grants other users access"), os.Remove(dir))
	}
	name := filepath.Join(dir, "config.json")
	if output, err := exec.Command("/bin/cp", "-p", target, name).CombinedOutput(); err != nil {
		return nil, errors.Join(fmt.Errorf("copy config metadata: %w: %s", err, bytes.TrimSpace(output)), cleanupConfigTemp(name))
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return nil, errors.Join(err, cleanupConfigTemp(name))
	}
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err == nil {
		err = f.Truncate(0)
	}
	if err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return nil, errors.Join(err, cleanupConfigTemp(name))
	}
	return f, nil
}

func cleanupConfigTemp(name string) error {
	fileErr := os.Remove(name)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	return errors.Join(fileErr, os.Remove(filepath.Dir(name)))
}

// RemoveInheritedConfigACL removes a parent ACL before a temporary config can contain secrets.
func RemoveInheritedConfigACL(file *os.File) error {
	_, hasACL, err := macOSACLBytes(file)
	if err != nil || !hasACL {
		return err
	}
	if output, err := exec.Command("/bin/chmod", "-N", file.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("remove inherited config ACL: %w: %s", err, bytes.TrimSpace(output))
	}
	_, hasACL, err = macOSACLBytes(file)
	if err != nil {
		return err
	}
	if hasACL {
		return errors.New("temporary config still has an inherited ACL")
	}
	return nil
}

func macOSACLBytes(file *os.File) ([]byte, bool, error) {
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY}
	buf := make([]byte, 64<<10)
	_, _, callErr := unix.Syscall6(unix.SYS_FGETATTRLIST, file.Fd(), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), unix.FSOPT_REPORT_FULLSIZE, 0)
	runtime.KeepAlive(&attrs)
	runtime.KeepAlive(buf)
	if callErr != 0 {
		return nil, false, callErr
	}
	total := int(binary.LittleEndian.Uint32(buf[:4]))
	if total < 24 || total > len(buf) || binary.LittleEndian.Uint32(buf[4:8])&unix.ATTR_CMN_RETURNED_ATTRS == 0 {
		return nil, false, errors.New("config ACL metadata is incomplete")
	}
	if binary.LittleEndian.Uint32(buf[4:8])&unix.ATTR_CMN_EXTENDED_SECURITY == 0 {
		return nil, false, nil
	}
	if total < 32 {
		return nil, false, errors.New("config ACL reference is incomplete")
	}
	start := 24 + int(int32(binary.LittleEndian.Uint32(buf[24:28])))
	end := start + int(binary.LittleEndian.Uint32(buf[28:32]))
	if start < 32 || end < start || end > total {
		return nil, false, errors.New("config ACL reference is invalid")
	}
	return bytes.Clone(buf[start:end]), true, nil
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
	if newOwner.Uid != owner.Uid || newOwner.Gid != owner.Gid {
		if err := replacement.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			return fmt.Errorf("preserve config owner and group: %w", err)
		}
	}
	if err := replacement.Chmod(info.Mode() & configPermissionBits); err != nil {
		return err
	}
	newInfo, err = replacement.Stat()
	if err != nil {
		return err
	}
	newOwner = newInfo.Sys().(*syscall.Stat_t)
	oldACL, oldHasACL, err := macOSACLBytes(original)
	if err != nil {
		return fmt.Errorf("read config ACL: %w", err)
	}
	newACL, newHasACL, err := macOSACLBytes(replacement)
	if err != nil {
		return fmt.Errorf("read replacement ACL: %w", err)
	}
	if newOwner.Uid != owner.Uid || newOwner.Gid != owner.Gid || newInfo.Mode()&configPermissionBits != info.Mode()&configPermissionBits || oldHasACL != newHasACL || !bytes.Equal(newACL, oldACL) {
		return errors.New("replacement config permissions differ from original")
	}
	return nil
}
