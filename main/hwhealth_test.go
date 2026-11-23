package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestDecodePiThrottle(t *testing.T) {
	cases := []struct {
		name string
		raw  uint32
		want PiThrottleStatus
	}{
		{
			name: "zero",
			raw:  0x0,
			want: PiThrottleStatus{Raw: 0x0, Available: true},
		},
		{
			name: "undervoltage_now_only",
			raw:  0x00001,
			want: PiThrottleStatus{Raw: 0x00001, Available: true, UnderVoltageNow: true},
		},
		{
			name: "freq_capped_now_only",
			raw:  0x00002,
			want: PiThrottleStatus{Raw: 0x00002, Available: true, FreqCappedNow: true},
		},
		{
			name: "throttled_now_only",
			raw:  0x00004,
			want: PiThrottleStatus{Raw: 0x00004, Available: true, ThrottledNow: true},
		},
		{
			name: "soft_temp_now_only",
			raw:  0x00008,
			want: PiThrottleStatus{Raw: 0x00008, Available: true, SoftTempLimitNow: true},
		},
		{
			name: "historical_uv_and_throttled_0x50000",
			raw:  0x50000,
			want: PiThrottleStatus{
				Raw:                  0x50000,
				Available:            true,
				UnderVoltageOccurred: true,
				ThrottledOccurred:    true,
			},
		},
		{
			name: "active_and_historical_uv_and_throttled_0x50005",
			raw:  0x50005,
			want: PiThrottleStatus{
				Raw:                  0x50005,
				Available:            true,
				UnderVoltageNow:      true,
				ThrottledNow:         true,
				UnderVoltageOccurred: true,
				ThrottledOccurred:    true,
			},
		},
		{
			name: "soft_temp_now_and_occurred_0x80008",
			raw:  0x80008,
			want: PiThrottleStatus{
				Raw:                   0x80008,
				Available:             true,
				SoftTempLimitNow:      true,
				SoftTempLimitOccurred: true,
			},
		},
		{
			name: "freq_capped_occurred_0x20000",
			raw:  0x20000,
			want: PiThrottleStatus{
				Raw:                0x20000,
				Available:          true,
				FreqCappedOccurred: true,
			},
		},
		{
			name: "all_bits_0xF000F",
			raw:  0xF000F,
			want: PiThrottleStatus{
				Raw:                   0xF000F,
				Available:             true,
				UnderVoltageNow:       true,
				FreqCappedNow:         true,
				ThrottledNow:          true,
				SoftTempLimitNow:      true,
				UnderVoltageOccurred:  true,
				FreqCappedOccurred:    true,
				ThrottledOccurred:     true,
				SoftTempLimitOccurred: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodePiThrottle(tc.raw)
			if got != tc.want {
				t.Errorf("DecodePiThrottle(0x%x) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseAndReadPiThrottleStatus(t *testing.T) {
	if raw, err := parsePiThrottleString("throttled=0x50005\n"); err != nil || raw != 0x50005 {
		t.Fatalf("parsePiThrottleString(vcgencmd) = 0x%x, %v; want 0x50005, nil", raw, err)
	}
	if raw, err := parsePiThrottleString("50000\n"); err != nil || raw != 0x50000 {
		t.Fatalf("parsePiThrottleString(sysfs) = 0x%x, %v; want 0x50000, nil", raw, err)
	}
	if _, err := parsePiThrottleString("not-hex"); err == nil {
		t.Fatalf("expected error on invalid hex")
	}

	tmpDir := t.TempDir()
	fakeSysfs := filepath.Join(tmpDir, "get_throttled")
	if err := os.WriteFile(fakeSysfs, []byte("50005\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origPath := piThrottleSysfsPath
	origBin := vcgencmdBinary
	defer func() {
		piThrottleSysfsPath = origPath
		vcgencmdBinary = origBin
	}()

	piThrottleSysfsPath = fakeSysfs
	vcgencmdBinary = ""
	st := ReadPiThrottleStatus()
	if !st.Available || !st.UnderVoltageNow || !st.ThrottledNow || st.Raw != 0x50005 {
		t.Fatalf("ReadPiThrottleStatus() = %+v, want Available=true Raw=0x50005", st)
	}

	// Non-Pi host: missing sysfs and missing vcgencmd must cleanly return Available=false.
	piThrottleSysfsPath = filepath.Join(tmpDir, "does-not-exist")
	vcgencmdBinary = "nonexistent-vcgencmd-binary"
	st = ReadPiThrottleStatus()
	if st.Available {
		t.Fatalf("expected Available=false on non-Pi host, got %+v", st)
	}
}

func TestReadPowerTelemetrySysfs(t *testing.T) {
	origPS := powerSupplySysfsDir
	origHM := hwmonSysfsDir
	defer func() {
		powerSupplySysfsDir = origPS
		hwmonSysfsDir = origHM
	}()

	// Case 1: absent directories return Available=false cleanly.
	emptyDir := t.TempDir()
	powerSupplySysfsDir = filepath.Join(emptyDir, "missing_ps")
	hwmonSysfsDir = filepath.Join(emptyDir, "missing_hm")
	if pt := ReadPowerTelemetry(); pt.Available {
		t.Fatalf("expected Available=false when sysfs absent, got %+v", pt)
	}

	// Case 2: Linux power_supply battery.
	psDir := filepath.Join(t.TempDir(), "power_supply")
	batDir := filepath.Join(psDir, "BAT0")
	if err := os.MkdirAll(batDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(batDir, "type"), []byte("Battery\n"), 0644)
	_ = os.WriteFile(filepath.Join(batDir, "voltage_now"), []byte("5050000\n"), 0644)
	_ = os.WriteFile(filepath.Join(batDir, "current_now"), []byte("1250000\n"), 0644)
	_ = os.WriteFile(filepath.Join(batDir, "capacity"), []byte("84\n"), 0644)

	powerSupplySysfsDir = psDir
	pt := ReadPowerTelemetry()
	if !pt.Available || pt.VoltageV != 5.05 || pt.CurrentMa != 1250 || pt.BatteryPct != 84 || pt.Source != "power_supply/BAT0" {
		t.Fatalf("power_supply ReadPowerTelemetry() = %+v", pt)
	}

	// Case 3: fallback to INA219 hwmon.
	powerSupplySysfsDir = filepath.Join(emptyDir, "missing_ps")
	hmDir := filepath.Join(t.TempDir(), "hwmon")
	inaDir := filepath.Join(hmDir, "hwmon0")
	if err := os.MkdirAll(inaDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(inaDir, "name"), []byte("ina219\n"), 0644)
	_ = os.WriteFile(filepath.Join(inaDir, "in1_input"), []byte("5120\n"), 0644)
	_ = os.WriteFile(filepath.Join(inaDir, "curr1_input"), []byte("980\n"), 0644)

	hwmonSysfsDir = hmDir
	pt = ReadPowerTelemetry()
	if !pt.Available || pt.VoltageV != 5.12 || pt.CurrentMa != 980 || pt.Source != "hwmon/ina219" {
		t.Fatalf("hwmon ReadPowerTelemetry() = %+v", pt)
	}
}

func TestSparklineRingBufferOrderingAndWrapAround(t *testing.T) {
	m := NewHardwareHealthMonitor()

	// Partial fill (10 samples): chronological 1..10.
	for i := 1; i <= 10; i++ {
		m.RecordBandRate("uat", i)
	}
	snap := m.Snapshot()
	want10 := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if !reflect.DeepEqual(snap.UAT.History60s, want10) {
		t.Fatalf("partial History60s = %v, want %v", snap.UAT.History60s, want10)
	}
	if snap.UAT.MsgsPerSec != 10 {
		t.Fatalf("MsgsPerSec = %d, want 10", snap.UAT.MsgsPerSec)
	}

	// Mutating returned snapshot must not affect internal ring buffer.
	snap.UAT.History60s[0] = 9999
	if m.Snapshot().UAT.History60s[0] != 1 {
		t.Fatalf("Snapshot did not deep-copy History60s")
	}

	// Fill past 60 (total 95 samples -> window should be 36..95).
	for i := 11; i <= 95; i++ {
		m.RecordBandRate("uat", i)
	}
	snap = m.Snapshot()
	if len(snap.UAT.History60s) != 60 {
		t.Fatalf("wrapped History60s len = %d, want 60", len(snap.UAT.History60s))
	}
	for i := 0; i < 60; i++ {
		want := 36 + i
		if snap.UAT.History60s[i] != want {
			t.Fatalf("History60s[%d] = %d, want %d", i, snap.UAT.History60s[i], want)
		}
	}
	if snap.UAT.MsgsPerSec != 95 {
		t.Fatalf("MsgsPerSec after wrap = %d, want 95", snap.UAT.MsgsPerSec)
	}
}

func TestUATIQClippingHysteresisAndESOverload(t *testing.T) {
	m := NewHardwareHealthMonitor()

	// Normal baseline: 0.5% clipping should NOT trigger overload initially.
	m.RecordUATIQStats(0.5, 42.0, 0)
	snap := m.Snapshot()
	if snap.UAT.OverloadAlert {
		t.Fatalf("0.5%% clipping should not trigger OverloadAlert initially")
	}
	if snap.UAT.RecommendedGain != "optimal" {
		t.Fatalf("RecommendedGain = %q, want optimal", snap.UAT.RecommendedGain)
	}

	// Cross > 1.0% threshold -> overload triggers, recommend "reduce".
	m.RecordUATIQStats(1.4, 90.0, 7)
	snap = m.Snapshot()
	if !snap.UAT.OverloadAlert || snap.UAT.RecommendedGain != "reduce" || snap.UAT.DroppedFrames != 7 {
		t.Fatalf("overload state = %+v, want OverloadAlert=true RecommendedGain=reduce DroppedFrames=7", snap.UAT)
	}

	// Hysteresis: drop to 0.5% (>= 0.3%) -> OverloadAlert must REMAIN true.
	m.RecordUATIQStats(0.5, 60.0, 7)
	snap = m.Snapshot()
	if !snap.UAT.OverloadAlert || snap.UAT.RecommendedGain != "reduce" {
		t.Fatalf("hysteresis failed at 0.5%%: %+v", snap.UAT)
	}

	// Drop below 0.3% -> OverloadAlert clears.
	m.RecordUATIQStats(0.2, 40.0, 7)
	snap = m.Snapshot()
	if snap.UAT.OverloadAlert || snap.UAT.RecommendedGain != "optimal" {
		t.Fatalf("expected overload cleared at 0.2%%, got %+v", snap.UAT)
	}

	// Weak signal with no clipping -> recommend "increase".
	m.RecordUATIQStats(0.0, 3.5, 7)
	snap = m.Snapshot()
	if snap.UAT.OverloadAlert || snap.UAT.RecommendedGain != "increase" {
		t.Fatalf("expected RecommendedGain=increase for low meanAmp, got %+v", snap.UAT)
	}

	// 1090ES strong signal overload hysteresis.
	m.SetSuggestedESGain(37.2)
	m.RecordESSignalSample(6.5)
	snap = m.Snapshot()
	if !snap.ES.OverloadAlert || snap.ES.RecommendedGain != "reduce" {
		t.Fatalf("ES overload failed at 6.5%%: %+v", snap.ES)
	}
	if m.SuggestedESGain() >= 37.2 {
		t.Fatalf("expected SuggestedESGain < 37.2 after 6.5%% strong signals, got %v", m.SuggestedESGain())
	}

	// ES hysteresis at 2.5% stays active.
	m.RecordESSignalSample(2.5)
	if !m.Snapshot().ES.OverloadAlert {
		t.Fatalf("ES overload cleared too early at 2.5%%")
	}

	// ES clears below 1.5%.
	m.RecordESSignalSample(1.0)
	snap = m.Snapshot()
	if snap.ES.OverloadAlert || snap.ES.RecommendedGain != "optimal" {
		t.Fatalf("ES overload failed to clear at 1.0%%: %+v", snap.ES)
	}
}

func TestComputeGainStepBoundsAndHysteresis(t *testing.T) {
	// Step down 1 notch when clipping > 1.0% (37.2 -> 36.4).
	g, changed := ComputeGainStep(37.2, 2.0, false)
	if !changed || g != 36.4 {
		t.Fatalf("ComputeGainStep(37.2, 2.0, false) = (%v, %v), want (36.4, true)", g, changed)
	}

	// Step down 2 notches on severe clipping > 5.0% (37.2 -> 33.8).
	g, changed = ComputeGainStep(37.2, 6.0, false)
	if !changed || g != 33.8 {
		t.Fatalf("ComputeGainStep(37.2, 6.0, false) = (%v, %v), want (33.8, true)", g, changed)
	}

	// Hysteresis deadband [0.3%, 1.0%]: even with lowSignal=true, do not change.
	g, changed = ComputeGainStep(37.2, 0.5, true)
	if changed || g != 37.2 {
		t.Fatalf("ComputeGainStep(37.2, 0.5, true) = (%v, %v), want (37.2, false)", g, changed)
	}

	// Step up 1 notch when lowSignal && clippedPct < 0.3% (37.2 -> 38.6).
	g, changed = ComputeGainStep(37.2, 0.1, true)
	if !changed || g != 38.6 {
		t.Fatalf("ComputeGainStep(37.2, 0.1, true) = (%v, %v), want (38.6, true)", g, changed)
	}

	// Lower bound clamp at 0.0 dB.
	g, changed = ComputeGainStep(0.0, 3.0, false)
	if changed || g != 0.0 {
		t.Fatalf("ComputeGainStep(0.0, 3.0, false) = (%v, %v), want (0.0, false)", g, changed)
	}

	// Upper bound clamp at 49.6 dB.
	g, changed = ComputeGainStep(49.6, 0.0, true)
	if changed || g != 49.6 {
		t.Fatalf("ComputeGainStep(49.6, 0.0, true) = (%v, %v), want (49.6, false)", g, changed)
	}

	// Out-of-range inputs clamp into valid table range.
	g, changed = ComputeGainStep(-5.0, 0.0, false)
	if !changed || g != 0.0 {
		t.Fatalf("ComputeGainStep(-5.0, 0, false) = (%v, %v), want (0.0, true)", g, changed)
	}
	g, changed = ComputeGainStep(99.0, 0.0, false)
	if !changed || g != 49.6 {
		t.Fatalf("ComputeGainStep(99.0, 0, false) = (%v, %v), want (49.6, true)", g, changed)
	}
}

func TestHardwareHealthConcurrentSnapshotAndHTTP(t *testing.T) {
	m := NewHardwareHealthMonitor()

	var wg sync.WaitGroup
	const workers = 6
	const iters = 200

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m.RecordRates(i%20, (i+1)%50, i%5, i%3)
				m.RecordUATIQStats(float64(i%30)*0.1, float64(i%100), uint64(i))
				m.RecordESSignalSample(float64(i%10) * 0.8)
				m.RecordEstimatedPPM("uat", id-2)
				m.SetBandStatus("es", true, i%2 == 0)
				m.UpdateSystemTelemetry(52.5+float32(i%10), DecodePiThrottle(uint32(i&0x50005)), PowerTelemetry{
					Available: true,
					VoltageV:  5.05,
					CurrentMa: 1100,
					Source:    "test",
				})
				snap := m.Snapshot()
				if len(snap.UAT.History60s) > 60 {
					t.Errorf("History60s exceeded 60: %d", len(snap.UAT.History60s))
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify HTTP handler produces valid JSON matching HardwareHealthSnapshot.
	req := httptest.NewRequest(http.MethodGet, "/getHealth", nil)
	rec := httptest.NewRecorder()
	handleHealthRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /getHealth status = %d, want 200", rec.Code)
	}
	var decoded HardwareHealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("GET /getHealth returned invalid JSON: %v", err)
	}
	if decoded.Warnings == nil {
		t.Fatalf("expected non-nil Warnings slice in JSON response")
	}
}
