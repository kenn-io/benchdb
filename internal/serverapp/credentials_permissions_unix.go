//go:build !windows

package serverapp

import (
	"errors"
	"os"
)

func ensureOwnerOnly(_ string, info os.FileInfo, isDir bool) error {
	if info == nil || info.IsDir() != isDir || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential permissions do not match the expected file type")
	}
	return ownerOnlyMode(info.Mode())
}

func verifyOwnerOnly(file *os.File, isDir bool) error {
	if file == nil {
		return errors.New("credential file cannot be verified")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return ensureOwnerOnly("", info, isDir)
}

func ownerOnlyMode(mode os.FileMode) error {
	if mode.Perm()&0077 != 0 {
		return errors.New("credential permissions are not owner-only")
	}
	return nil
}
