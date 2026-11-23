package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadSettingsLargeFileOver4KB(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "stratux.conf")

	// Build a JSON config larger than 4096 bytes with fan settings at the very end.
	padding := strings.Repeat("x", 8192)
	raw := `{"Padding":"` + padding + `","TempTarget":62.5,"PWMDutyMin":25,"PWMFrequency":32000,"PWMPin":19}`
	if len(raw) <= 4096 {
		t.Fatalf("test config must be > 4096 bytes, got %d", len(raw))
	}
	if err := os.WriteFile(cfgPath, []byte(raw), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Set runtime state to verify reload does not clobber live readings.
	fanControlMu.Lock()
	myFanControl.TempCurrent = 47.5
	myFanControl.PWMDutyCurrent = 40
	fanControlMu.Unlock()

	readSettingsFromFile(cfgPath)

	fanControlMu.RLock()
	got := myFanControl
	fanControlMu.RUnlock()

	if got.TempTarget != 62.5 || got.PWMDutyMin != 25 || got.PWMFrequency != 32000 || got.PWMPin != 19 {
		t.Errorf("unexpected parsed config: %+v", got)
	}
	if got.TempCurrent != 47.5 || got.PWMDutyCurrent != 40 {
		t.Errorf("runtime readings were clobbered on reload: %+v", got)
	}
}

func TestReadSettingsMissingAndMalformedFallback(t *testing.T) {
	dir := t.TempDir()
	readSettingsFromFile(filepath.Join(dir, "does-not-exist.conf"))

	fanControlMu.RLock()
	got := myFanControl
	fanControlMu.RUnlock()
	if got.TempTarget != defaultTempTarget || got.PWMPin != defaultPin {
		t.Errorf("missing file did not apply defaults: %+v", got)
	}

	badPath := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(badPath, []byte("{bad json"), 0600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	readSettingsFromFile(badPath)
	fanControlMu.RLock()
	got = myFanControl
	fanControlMu.RUnlock()
	if got.TempTarget != defaultTempTarget || got.PWMFrequency != defaultPwmFrequency {
		t.Errorf("malformed file did not apply defaults: %+v", got)
	}
}

func TestConcurrentStatusReadsAndConfigReloads(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "stratux.conf")
	if err := os.WriteFile(cfgPath, []byte(`{"TempTarget":55,"PWMDutyMin":15,"PWMFrequency":64000,"PWMPin":18}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	quit := make(chan struct{})
	go updateStats(quit)

	var wg sync.WaitGroup
	// Concurrent HTTP status readers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				handleStatusRequest(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("status code = %d, want 200", rec.Code)
					return
				}
				var fc FanControl
				if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
					t.Errorf("invalid JSON response: %v", err)
					return
				}
			}
		}()
	}

	// Concurrent config reloaders.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				readSettingsFromFile(cfgPath)
			}
		}()
	}

	// Concurrent temperature & PWM updaters.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			fanControlMu.Lock()
			myFanControl.TempCurrent = 40.0 + float64(j%20)
			myFanControl.PWMDutyCurrent = uint32(j % 100)
			fanControlMu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	close(quit)
}

func TestListenAddrIsLoopback(t *testing.T) {
	if addr != "127.0.0.1:9977" {
		t.Errorf("addr = %q, want 127.0.0.1:9977", addr)
	}
}
