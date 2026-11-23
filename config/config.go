// Package config provides pure, zero-CGO configuration validation, template
// string sanitization, and atomic file persistence helpers for Stratux.
package config

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ValidateDirectPin reports whether pin is an exact 8-digit numeric Wi-Fi
// Direct PIN (matching ^[0-9]{8}$).
func ValidateDirectPin(pin string) bool {
	if len(pin) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}
	return true
}

// ValidateCountry trims whitespace, uppercases code, and validates that it is
// a 2-letter ASCII ISO-3166 country code (matching ^[A-Z]{2}$).
func ValidateCountry(code string) (string, bool) {
	clean := strings.ToUpper(strings.TrimSpace(code))
	if len(clean) != 2 {
		return "", false
	}
	if clean[0] < 'A' || clean[0] > 'Z' || clean[1] < 'A' || clean[1] > 'Z' {
		return "", false
	}
	return clean, true
}

// ValidateIPv4 validates ip and returns its canonical dotted-quad IPv4 string.
func ValidateIPv4(ip string) (string, bool) {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return "", false
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", false
	}
	return v4.String(), true
}

// IsSafeWpaString reports whether s contains no characters that could inject
// directives or break quoting in wpa_supplicant configuration files (newlines,
// NUL, ASCII control characters, DEL, double quotes, or backslashes).
func IsSafeWpaString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7F || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// SanitizeWpaString strips newlines, NUL, ASCII control characters (< 0x20),
// DEL (0x7F), double quotes ('"'), and backslashes ('\') to prevent directive
// injection in wpa_supplicant configuration files.
func SanitizeWpaString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7F || c == '"' || c == '\\' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// AtomicWriteFile writes data to path atomically by creating a temporary file
// in the same directory, writing all bytes, applying perm via Chmod, syncing
// to disk, closing, and renaming over path. Any temporary file is removed on
// error.
func AtomicWriteFile(path string, data []byte, perm fs.FileMode) error {
	if path == "" {
		return errors.New("config: empty path")
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpFile, err := os.CreateTemp(dir, "."+base+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
