//go:build windows

package requestsign

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateFilePermissions(file *os.File, _ os.FileInfo) error {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(token.User.Sid) {
		return errors.New("private file must be owned by the current user")
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return errors.New("private file must have an owner-only DACL")
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("private file has unsupported ACL entries")
		}
		// Windows returns an ACE whose trailing bytes contain its SID.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !sid.Equals(token.User.Sid) {
			return errors.New("private file grants access to another identity")
		}
	}
	return nil
}
