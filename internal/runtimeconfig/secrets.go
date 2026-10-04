// Package runtimeconfig resolves deployment secrets without exposing their values.
package runtimeconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxSecretBytes = 64 << 10

// Secret reads NAME or NAME_FILE, rejecting conflicting nonempty selectors.
// File secrets may end in one LF or CRLF; all other bytes are preserved.
func Secret(name string) (string, error) {
	if value := os.Getenv(name); value != "" {
		if os.Getenv(name+"_FILE") != "" {
			return "", fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
		}
		return validate(name, value)
	}
	if file := os.Getenv(name + "_FILE"); file != "" {
		setting := name + "_FILE"
		// Reject FIFOs/devices before opening. Mounted-secret symlinks are supported;
		// source paths and their parents must be controlled by the deployer.
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s must name a readable regular file", setting)
		}
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("%s cannot be read", setting)
		}
		defer func() { _ = f.Close() }()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s must name a readable regular file", setting)
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
		if err != nil {
			return "", fmt.Errorf("%s cannot be read", setting)
		}
		if len(data) > maxSecretBytes {
			return "", fmt.Errorf("%s exceeds 64 KiB", setting)
		}
		value := strings.TrimSuffix(string(data), "\n")
		if len(data) > 0 && data[len(data)-1] == '\n' {
			value = strings.TrimSuffix(value, "\r")
		}
		return validate(setting, value)
	}
	return "", nil
}

func validate(setting, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s resolved to an empty secret", setting)
	}
	if len(value) > maxSecretBytes {
		return "", fmt.Errorf("%s exceeds 64 KiB", setting)
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%s contains a NUL byte", setting)
	}
	return value, nil
}

// DatabaseURL requires a BENCHDB_DB_URL source. Only callers that historically
// accepted DATABASE_URL should opt into that existing inline fallback.
func DatabaseURL(allowFallback bool) (string, error) {
	value, err := Secret("BENCHDB_DB_URL")
	if err != nil {
		return "", err
	}
	if value == "" && allowFallback {
		if fallback := os.Getenv("DATABASE_URL"); fallback != "" {
			return validate("DATABASE_URL", fallback)
		}
	}
	if value == "" {
		if allowFallback {
			return "", errors.New("BENCHDB_DB_URL (or DATABASE_URL) is required")
		}
		return "", errors.New("BENCHDB_DB_URL is required")
	}
	return value, nil
}
