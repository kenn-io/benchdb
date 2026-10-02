//go:build windows

package serverapp

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS is the file-object mapping of GENERIC_ALL. Store and verify
// the mapped rights so ACL readback has a stable object-specific mask.
const ownerOnlyFileAccess = windows.ACCESS_MASK(windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff)

func ensureOwnerOnly(path string, info os.FileInfo, isDir bool) error {
	if info == nil || info.IsDir() != isDir || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential permissions do not match the expected file type")
	}

	userSID, err := currentProcessSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if ownerOnlyWindowsDescriptor(descriptor, userSID, isDir) {
		return nil
	}

	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION |
		windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if descriptor == nil {
		owner = userSID
		securityInfo |= windows.OWNER_SECURITY_INFORMATION
	} else {
		currentOwner, _, ownerErr := descriptor.Owner()
		if ownerErr != nil || currentOwner == nil || !currentOwner.Equals(userSID) {
			owner = userSID
			securityInfo |= windows.OWNER_SECURITY_INFORMATION
		}
	}

	var pinner runtime.Pinner
	pinner.Pin(userSID)
	defer pinner.Unpin()
	acl, err := ownerOnlyWindowsACL(userSID, isDir)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, securityInfo, owner, nil, acl, nil); err != nil {
		return err
	}
	if !ownerOnlyWindowsPath(path, userSID, isDir) {
		return errors.New("credential ACL could not be verified")
	}
	return nil
}

func verifyOwnerOnly(file *os.File, isDir bool) error {
	if file == nil {
		return errors.New("credential file cannot be verified")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() != isDir || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential permissions do not match the expected file type")
	}

	userSID, err := currentProcessSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if !ownerOnlyWindowsDescriptor(descriptor, userSID, isDir) {
		return errors.New("credential ACL could not be verified")
	}
	return nil
}

func currentProcessSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func ownerOnlyWindowsACL(userSID *windows.SID, isDir bool) (*windows.ACL, error) {
	inheritance := uint32(windows.NO_INHERITANCE)
	if isDir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: ownerOnlyFileAccess,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(userSID),
		},
	}}
	return windows.ACLFromEntries(access, nil)
}

func ownerOnlyWindowsPath(path string, userSID *windows.SID, isDir bool) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return err == nil && ownerOnlyWindowsDescriptor(descriptor, userSID, isDir)
}

func ownerOnlyWindowsDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, userSID *windows.SID, isDir bool) bool {
	if descriptor == nil || userSID == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(userSID) {
		return false
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil {
		return false
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != ownerOnlyFileAccess {
		return false
	}
	expectedFlags := uint8(windows.NO_INHERITANCE)
	if isDir {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	if ace.Header.AceFlags != expectedFlags {
		return false
	}
	aclUserSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return aclUserSID.Equals(userSID)
}
