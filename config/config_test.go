package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestValidateDirectPin(t *testing.T) {
	valid := []string{"00000000", "12345678", "98765432"}
	for _, pin := range valid {
		if !ValidateDirectPin(pin) {
			t.Errorf("ValidateDirectPin(%q) = false, want true", pin)
		}
	}

	invalid := []string{
		"",
		"1234567",
		"123456789",
		"1234567a",
		"1234 567",
		" 12345678",
		"12345678\n",
		"12345678\nctrl_interface=/tmp",
		"1234-678",
	}
	for _, pin := range invalid {
		if ValidateDirectPin(pin) {
			t.Errorf("ValidateDirectPin(%q) = true, want false", pin)
		}
	}
}

func TestValidateCountry(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"US", "US"},
		{"us", "US"},
		{"  de  ", "DE"},
		{"Gb", "GB"},
		{"FR\r\n", "FR"},
	}
	for _, tc := range valid {
		got, ok := ValidateCountry(tc.in)
		if !ok || got != tc.want {
			t.Errorf("ValidateCountry(%q) = (%q, %v), want (%q, true)", tc.in, got, ok, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"U",
		"USA",
		"U1",
		"12",
		"U\nS",
		"US\nnetwork={}",
		"ÜS",
		"--",
	}
	for _, code := range invalid {
		if got, ok := ValidateCountry(code); ok || got != "" {
			t.Errorf("ValidateCountry(%q) = (%q, %v), want (\"\", false)", code, got, ok)
		}
	}
}

func TestValidateIPv4(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"192.168.10.1", "192.168.10.1"},
		{"  10.0.0.1  ", "10.0.0.1"},
		{"0.0.0.0", "0.0.0.0"},
		{"255.255.255.255", "255.255.255.255"},
	}
	for _, tc := range valid {
		got, ok := ValidateIPv4(tc.in)
		if !ok || got != tc.want {
			t.Errorf("ValidateIPv4(%q) = (%q, %v), want (%q, true)", tc.in, got, ok, tc.want)
		}
	}

	invalid := []string{
		"",
		"not-an-ip",
		"192.168.10.256",
		"192.168.10",
		"192.168.10.1/24",
		"::1",
		"2001:db8::1",
		"192.168.10.1; rm -rf /",
		"192.168.10.1\n192.168.10.2",
	}
	for _, ip := range invalid {
		if got, ok := ValidateIPv4(ip); ok || got != "" {
			t.Errorf("ValidateIPv4(%q) = (%q, %v), want (\"\", false)", ip, got, ok)
		}
	}
}

func TestSanitizeWpaString(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		wantSafe bool
	}{
		{"clean_ssid", "Stratux-5G_Test!", "Stratux-5G_Test!", true},
		{"empty", "", "", true},
		{"newline_directive_injection", "MyNet\"\n}\nnetwork={\nssid=\"evil", "MyNet}network={ssid=evil", false},
		{"crlf_and_tab", "Net\r\n\tName", "NetName", false},
		{"nul_and_ctrl", "A\x00B\x01C\x1fD\x7fE", "ABCDE", false},
		{"quotes_and_backslashes", `pass"word\secret`, "passwordsecret", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if gotSafe := IsSafeWpaString(tc.in); gotSafe != tc.wantSafe {
				t.Errorf("IsSafeWpaString(%q) = %v, want %v", tc.in, gotSafe, tc.wantSafe)
			}
			got := SanitizeWpaString(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeWpaString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !IsSafeWpaString(got) {
				t.Errorf("SanitizeWpaString output %q is not safe", got)
			}
		})
	}
}

func TestAtomicWriteFile_BasicAndPermissions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "stratux.conf")

	first := []byte(`{"UAT_Enabled": true}` + "\n")
	if err := AtomicWriteFile(target, first, 0600); err != nil {
		t.Fatalf("AtomicWriteFile(first) failed: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file perm = %04o, want 0600", info.Mode().Perm())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("content = %q, want %q", got, first)
	}

	// Overwrite atomically with new content and mode 0644.
	second := []byte(`{"UAT_Enabled": false, "ES_Enabled": true}` + "\n")
	if err := AtomicWriteFile(target, second, 0644); err != nil {
		t.Fatalf("AtomicWriteFile(second) failed: %v", err)
	}
	info, err = os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(second) failed: %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("updated file perm = %04o, want 0644", info.Mode().Perm())
	}
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(second) failed: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("updated content = %q, want %q", got, second)
	}

	// Verify no leftover temp files remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "stratux.conf" {
		t.Fatalf("unexpected directory entries after write: %v", entries)
	}
}

func TestAtomicWriteFile_ErrorCleanup(t *testing.T) {
	if err := AtomicWriteFile("", []byte("x"), 0644); err == nil {
		t.Fatalf("AtomicWriteFile(empty path) succeeded, want error")
	}

	dir := t.TempDir()
	// Non-existent parent directory fails cleanly.
	if err := AtomicWriteFile(filepath.Join(dir, "missing", "file.conf"), []byte("x"), 0644); err == nil {
		t.Fatalf("AtomicWriteFile(missing parent) succeeded, want error")
	}

	// Rename over an existing directory fails and cleans up temp file.
	subDir := filepath.Join(dir, "asdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	if err := AtomicWriteFile(subDir, []byte("payload"), 0644); err == nil {
		t.Fatalf("AtomicWriteFile(over directory) succeeded, want error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "asdir" {
		t.Fatalf("temp file leaked after rename error; entries = %v", entries)
	}
}

func TestAtomicWriteFile_Concurrent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "concurrent.conf")

	payloads := make(map[string]struct{})
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("worker-%02d:%s\n", i, bytes.Repeat([]byte{byte('A' + i)}, 4096))
		payloads[p] = struct{}{}
	}
	if err := AtomicWriteFile(target, []byte(fmt.Sprintf("worker-00:%s\n", bytes.Repeat([]byte{'A'}, 4096))), 0644); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}

	var wg sync.WaitGroup
	// Concurrent writers.
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := []byte(fmt.Sprintf("worker-%02d:%s\n", i, bytes.Repeat([]byte{byte('A' + i)}, 4096)))
			for j := 0; j < 20; j++ {
				if err := AtomicWriteFile(target, data, 0644); err != nil {
					t.Errorf("concurrent write failed: %v", err)
					return
				}
			}
		}()
	}

	// Concurrent readers verifying no torn/partial reads ever occur.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				data, err := os.ReadFile(target)
				if err != nil {
					t.Errorf("concurrent read failed: %v", err)
					return
				}
				if _, ok := payloads[string(data)]; !ok {
					t.Errorf("observed torn/unexpected read of length %d", len(data))
					return
				}
			}
		}()
	}

	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "concurrent.conf" {
		t.Fatalf("temp files leaked after concurrent writes: %v", entries)
	}
}
