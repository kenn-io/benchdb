package serverapp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// persistedSecret publishes a complete credential without replacing an existing
// one. Hard-link creation elects a winner across concurrent server processes.
func persistedSecret(dir, name string) (string, error) {
	created := false
	if err := os.Mkdir(dir, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return "", errors.New("BENCHDB_DATA_DIR cannot be created; its parent must exist and be writable")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("BENCHDB_DATA_DIR must be an owner-only directory, not a symlink")
	}
	if err := ensureOwnerOnly(dir, info, true); err != nil {
		return "", errors.New("BENCHDB_DATA_DIR must be an owner-only directory, not a symlink")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", errors.New("BENCHDB_DATA_DIR cannot be opened")
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("BENCHDB_DATA_DIR changed during startup")
	}
	dirFile, err := root.Open(".")
	if err != nil {
		return "", errors.New("BENCHDB_DATA_DIR cannot be verified")
	}
	verifyErr := verifyOwnerOnly(dirFile, true)
	closeErr := dirFile.Close()
	if verifyErr != nil || closeErr != nil {
		return "", errors.New("BENCHDB_DATA_DIR permissions cannot be verified")
	}
	if created {
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return "", errors.New("BENCHDB_DATA_DIR creation cannot be synchronized")
		}
	}
	if value, err := readPersistedSecret(root, dir, name); !errors.Is(err, os.ErrNotExist) {
		return value, err
	}

	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate %s failed", name)
	}
	value := hex.EncodeToString(entropy[:])
	tmp := "." + name + "-" + rand.Text()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("persist %s: BENCHDB_DATA_DIR is not writable", name)
	}
	defer func() { _ = root.Remove(tmp) }()
	info, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("persist %s: credential permissions cannot be verified", name)
	}
	tmpPath := filepath.Join(dir, tmp)
	if err := ensureOwnerOnly(tmpPath, info, false); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("persist %s: credential permissions cannot be secured", name)
	}
	if err := verifyOwnerOnly(f, false); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("persist %s: credential permissions cannot be verified", name)
	}
	if _, err = f.WriteString(value); err == nil {
		err = f.Sync()
	}
	closeErr = f.Close()
	if err != nil || closeErr != nil {
		return "", fmt.Errorf("persist %s: credential cannot be synchronized", name)
	}
	if err := root.Link(tmp, name); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("persist %s: atomic publication failed", name)
	}
	d, err := root.Open(".")
	if err != nil {
		return "", fmt.Errorf("persist %s: directory cannot be opened", name)
	}
	err = syncFile(d)
	closeErr = d.Close()
	if err != nil || closeErr != nil {
		return "", fmt.Errorf("persist %s: directory cannot be synchronized", name)
	}
	// The winner may be another process. Validate and return what was published.
	return readPersistedSecret(root, dir, name)
}

func readPersistedSecret(root *os.Root, dir, name string) (string, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != 64 {
		return "", fmt.Errorf("persisted %s must be an owner-only regular 64-character credential", name)
	}
	if err := ensureOwnerOnly(filepath.Join(dir, name), info, false); err != nil {
		return "", fmt.Errorf("persisted %s must be an owner-only regular 64-character credential", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return "", fmt.Errorf("persisted %s cannot be read", name)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fmt.Errorf("persisted %s changed during startup", name)
	}
	if err := verifyOwnerOnly(f, false); err != nil {
		return "", fmt.Errorf("persisted %s must be an owner-only regular 64-character credential", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(data) != 64 {
		return "", fmt.Errorf("persisted %s cannot be read completely", name)
	}
	value := string(data)
	if _, err := hex.DecodeString(value); err != nil || strings.ToLower(value) != value {
		return "", fmt.Errorf("persisted %s must contain lowercase hexadecimal", name)
	}
	return value, nil
}

func syncFile(f *os.File) error {
	if runtime.GOOS == "windows" {
		return nil
	} // Directory fsync is a Unix durability contract.
	return f.Sync()
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return syncFile(f)
}
