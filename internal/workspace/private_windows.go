package workspace

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkPrivateRoot(root string, info os.FileInfo) (resultErr error) {
	dir, err := openPath(root, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, dir.Close()) }()
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.Join(errors.New("workspace root changed while checking access"), err)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(dir.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("workspace root %q must be owned by the service user", root)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("workspace root %q ACL: %w", root, err)
	}
	if dacl == nil {
		return fmt.Errorf("workspace root %q has no restrictive ACL", root)
	}
	if err := privateWindowsDACL(dacl, user.User.Sid); err != nil {
		return fmt.Errorf("workspace root %q: %w", root, err)
	}
	return nil
}

func privateWindowsDACL(dacl *windows.ACL, user *windows.SID) error {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if ace.Mask == 0 || sid.Equals(user) || sid.Equals(admins) || sid.Equals(system) {
				continue
			}
		}
		return errors.New("ACL grants access to another principal")
	}
	return nil
}
