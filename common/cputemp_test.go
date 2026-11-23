package common

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIsCPUTempValid(t *testing.T) {
	if IsCPUTempValid(InvalidCpuTemp) {
		t.Errorf("InvalidCpuTemp (%v) should not be valid", InvalidCpuTemp)
	}
	if IsCPUTempValid(0) {
		t.Errorf("0 should not be valid")
	}
	if IsCPUTempValid(-1.5) {
		t.Errorf("-1.5 should not be valid")
	}
	if !IsCPUTempValid(0.5) {
		t.Errorf("0.5 should be valid")
	}
	if !IsCPUTempValid(52.3) {
		t.Errorf("52.3 should be valid")
	}
}

func TestParseCPUTemp(t *testing.T) {
	cases := []struct {
		raw  string
		want float32
	}{
		{"48123\n", 48.123},
		{"  55000\r\n", 55.0},
		{"42\n", 42.0},
		{"0\n", 0.0},
		{"-10\n", -10.0},
		{"", InvalidCpuTemp},
		{"not-a-number\n", InvalidCpuTemp},
	}
	for _, tc := range cases {
		got := parseCPUTemp([]byte(tc.raw))
		if got != tc.want {
			t.Errorf("parseCPUTemp(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestCpuTempMonitorSkipsInvalidAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	tempFile := filepath.Join(dir, "temp")

	// Start with an invalid / missing file — updater must NOT be called with InvalidCpuTemp.
	ctx, cancel := context.WithCancel(context.Background())
	var (
		mu      sync.Mutex
		updates []float32
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		cpuTempMonitor(ctx, tempFile, 10*time.Millisecond, func(temp float32) {
			mu.Lock()
			updates = append(updates, temp)
			mu.Unlock()
		})
	}()

	time.Sleep(35 * time.Millisecond)
	mu.Lock()
	if len(updates) != 0 {
		t.Fatalf("expected 0 updates for missing file, got %v", updates)
	}
	mu.Unlock()

	// Write a non-positive value (still invalid).
	if err := os.WriteFile(tempFile, []byte("0\n"), 0600); err != nil {
		t.Fatalf("write tempFile: %v", err)
	}
	time.Sleep(35 * time.Millisecond)
	mu.Lock()
	if len(updates) != 0 {
		t.Fatalf("expected 0 updates for 0 temp, got %v", updates)
	}
	mu.Unlock()

	// Write a valid millidegree temperature.
	if err := os.WriteFile(tempFile, []byte("54321\r\n"), 0600); err != nil {
		t.Fatalf("write tempFile: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(updates)
		var last float32
		if n > 0 {
			last = updates[n-1]
		}
		mu.Unlock()
		if n > 0 {
			if last != 54.321 {
				t.Fatalf("got temp %v, want 54.321", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for valid CPU temp update")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("cpuTempMonitor did not stop after context cancellation")
	}
}

func TestIsRunningAsRoot(t *testing.T) {
	if got, want := IsRunningAsRoot(), os.Geteuid() == 0; got != want {
		t.Errorf("IsRunningAsRoot() = %v, want %v", got, want)
	}
}
