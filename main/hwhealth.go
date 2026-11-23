/*
	Copyright (c) 2026 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	hwhealth.go: Hardware health telemetry — Raspberry Pi undervoltage/throttling,
	optional power/battery sensors, RF band 60-second sparklines, IQ clipping
	overload detection, and RTL-SDR gain/PPM advisory telemetry.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Raspberry Pi firmware get_throttled bitmask constants.
const (
	piThrottleUnderVoltageNow       uint32 = 0x00001
	piThrottleFreqCappedNow         uint32 = 0x00002
	piThrottleThrottledNow          uint32 = 0x00004
	piThrottleSoftTempLimitNow      uint32 = 0x00008
	piThrottleUnderVoltageOccurred  uint32 = 0x10000
	piThrottleFreqCappedOccurred    uint32 = 0x20000
	piThrottleThrottledOccurred     uint32 = 0x40000
	piThrottleSoftTempLimitOccurred uint32 = 0x80000
)

// RF clipping / overload hysteresis thresholds.
const (
	uatClipTriggerPct = 1.0
	uatClipClearPct   = 0.3
	uatLowAmpThresh   = 8.0

	esStrongTriggerPct = 5.0
	esStrongClearPct   = 1.5

	sparklineWindowSec = 60
)

// Standard RTL-SDR R820T/R820T2 gain table (dB), matching sdr.go.
var rtlsdrValidGains = []float64{
	0.0, 0.9, 1.4, 2.7, 3.7, 7.7, 8.7, 12.5, 14.4, 15.7,
	16.6, 19.7, 20.7, 22.9, 25.4, 28.0, 29.7, 32.8, 33.8, 36.4,
	37.2, 38.6, 40.2, 42.1, 43.4, 43.9, 44.5, 48.0, 49.6,
}

// Overridable sysfs paths for deterministic unit testing.
var (
	piThrottleSysfsPath = "/sys/devices/platform/soc/soc:firmware/get_throttled"
	powerSupplySysfsDir = "/sys/class/power_supply"
	hwmonSysfsDir       = "/sys/class/hwmon"
	vcgencmdBinary      = "vcgencmd"
)

// PiThrottleStatus decodes the Raspberry Pi firmware get_throttled bitmask.
type PiThrottleStatus struct {
	Raw                   uint32 `json:"Raw"`
	Available             bool   `json:"Available"`
	UnderVoltageNow       bool   `json:"UnderVoltageNow"`
	FreqCappedNow         bool   `json:"FreqCappedNow"`
	ThrottledNow          bool   `json:"ThrottledNow"`
	SoftTempLimitNow      bool   `json:"SoftTempLimitNow"`
	UnderVoltageOccurred  bool   `json:"UnderVoltageOccurred"`
	FreqCappedOccurred    bool   `json:"FreqCappedOccurred"`
	ThrottledOccurred     bool   `json:"ThrottledOccurred"`
	SoftTempLimitOccurred bool   `json:"SoftTempLimitOccurred"`
}

// PowerTelemetry holds optional Linux power_supply or INA219 hwmon readings.
type PowerTelemetry struct {
	Available  bool    `json:"Available"`
	VoltageV   float64 `json:"VoltageV"`
	CurrentMa  float64 `json:"CurrentMa"`
	BatteryPct float64 `json:"BatteryPct"`
	Source     string  `json:"Source"`
}

// RFBandHealth reports per-band RF reception rate, 60 s sparkline, clipping/overload, and advisory gain/PPM.
type RFBandHealth struct {
	Enabled         bool    `json:"Enabled"`
	Connected       bool    `json:"Connected"`
	MsgsPerSec      int     `json:"MsgsPerSec"`
	History60s      []int   `json:"History60s"`
	ClippedPercent  float64 `json:"ClippedPercent"`
	OverloadAlert   bool    `json:"OverloadAlert"`
	RecommendedGain string  `json:"RecommendedGain"`
	EstimatedPPM    int     `json:"EstimatedPPM"`
	DroppedFrames   uint64  `json:"DroppedFrames"`
}

// HardwareHealthSnapshot is the JSON payload returned by GET /getHealth.
type HardwareHealthSnapshot struct {
	Timestamp time.Time        `json:"Timestamp"`
	CPUTempC  float32          `json:"CPUTempC"`
	Throttle  PiThrottleStatus `json:"Throttle"`
	Power     PowerTelemetry   `json:"Power"`
	UAT       RFBandHealth     `json:"UAT"`
	ES        RFBandHealth     `json:"ES"`
	OGN       RFBandHealth     `json:"OGN"`
	AIS       RFBandHealth     `json:"AIS"`
	Warnings  []string         `json:"Warnings"`
}

// DecodePiThrottle decodes a raw Raspberry Pi firmware get_throttled bitmask.
func DecodePiThrottle(raw uint32) PiThrottleStatus {
	return PiThrottleStatus{
		Raw:                   raw,
		Available:             true,
		UnderVoltageNow:       raw&piThrottleUnderVoltageNow != 0,
		FreqCappedNow:         raw&piThrottleFreqCappedNow != 0,
		ThrottledNow:          raw&piThrottleThrottledNow != 0,
		SoftTempLimitNow:      raw&piThrottleSoftTempLimitNow != 0,
		UnderVoltageOccurred:  raw&piThrottleUnderVoltageOccurred != 0,
		FreqCappedOccurred:    raw&piThrottleFreqCappedOccurred != 0,
		ThrottledOccurred:     raw&piThrottleThrottledOccurred != 0,
		SoftTempLimitOccurred: raw&piThrottleSoftTempLimitOccurred != 0,
	}
}

func parsePiThrottleString(s string) (uint32, error) {
	trimmed := strings.TrimSpace(s)
	if idx := strings.Index(trimmed, "="); idx >= 0 {
		trimmed = strings.TrimSpace(trimmed[idx+1:])
	}
	trimmed = strings.TrimPrefix(trimmed, "0x")
	trimmed = strings.TrimPrefix(trimmed, "0X")
	if trimmed == "" {
		return 0, fmt.Errorf("empty throttle value")
	}
	v, err := strconv.ParseUint(trimmed, 16, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// ReadPiThrottleStatus reads Raspberry Pi undervoltage/throttling flags from sysfs,
// falling back to vcgencmd if present, and cleanly returning Available: false on non-Pi hosts.
func ReadPiThrottleStatus() PiThrottleStatus {
	if piThrottleSysfsPath != "" {
		if data, err := os.ReadFile(piThrottleSysfsPath); err == nil {
			if raw, parseErr := parsePiThrottleString(string(data)); parseErr == nil {
				return DecodePiThrottle(raw)
			}
		}
	}

	if vcgencmdBinary != "" {
		if binPath, err := exec.LookPath(vcgencmdBinary); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, cmdErr := exec.CommandContext(ctx, binPath, "get_throttled").Output()
			if cmdErr == nil {
				if raw, parseErr := parsePiThrottleString(string(out)); parseErr == nil {
					return DecodePiThrottle(raw)
				}
			}
		}
	}

	return PiThrottleStatus{Available: false}
}

func readSysfsFloat(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ReadPowerTelemetry reads optional Linux power_supply or INA219/power hwmon telemetry.
// Returns Available: false cleanly when no power telemetry hardware is present.
func ReadPowerTelemetry() PowerTelemetry {
	if pt, ok := readPowerSupplyDir(powerSupplySysfsDir); ok {
		return pt
	}
	if pt, ok := readHwmonPowerDir(hwmonSysfsDir); ok {
		return pt
	}
	return PowerTelemetry{Available: false}
}

func readPowerSupplyDir(dir string) (PowerTelemetry, bool) {
	if dir == "" {
		return PowerTelemetry{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return PowerTelemetry{}, false
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var best PowerTelemetry
	bestScore := 0

	for _, name := range names {
		base := filepath.Join(dir, name)
		voltageUV, hasVolt := readSysfsFloat(filepath.Join(base, "voltage_now"))
		currentUA, hasCurr := readSysfsFloat(filepath.Join(base, "current_now"))
		capPct, hasCap := readSysfsFloat(filepath.Join(base, "capacity"))
		if !hasVolt && !hasCap {
			continue
		}

		score := 1
		if hasCap {
			score += 2
		}
		if hasVolt && voltageUV > 0 {
			score += 2
		}
		if hasCurr {
			score++
		}
		if typeBytes, err := os.ReadFile(filepath.Join(base, "type")); err == nil {
			if strings.EqualFold(strings.TrimSpace(string(typeBytes)), "Battery") {
				score += 2
			}
		}

		if score > bestScore {
			bestScore = score
			best = PowerTelemetry{
				Available: true,
				Source:    "power_supply/" + name,
			}
			if hasVolt {
				best.VoltageV = voltageUV / 1e6
			}
			if hasCurr {
				best.CurrentMa = currentUA / 1e3
			}
			if hasCap {
				best.BatteryPct = capPct
			}
		}
	}

	return best, bestScore > 0
}

func readHwmonPowerDir(dir string) (PowerTelemetry, bool) {
	if dir == "" {
		return PowerTelemetry{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return PowerTelemetry{}, false
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, entry := range names {
		base := filepath.Join(dir, entry)
		nameBytes, _ := os.ReadFile(filepath.Join(base, "name"))
		sensorName := strings.TrimSpace(string(nameBytes))
		lowerName := strings.ToLower(sensorName)

		voltMV, hasVolt := readSysfsFloat(filepath.Join(base, "in1_input"))
		currMA, hasCurr := readSysfsFloat(filepath.Join(base, "curr1_input"))
		if !hasVolt {
			continue
		}

		isPowerMonitor := strings.HasPrefix(lowerName, "ina") ||
			strings.Contains(lowerName, "power") ||
			strings.Contains(lowerName, "volt") ||
			strings.Contains(lowerName, "ups") ||
			strings.Contains(lowerName, "battery") ||
			hasCurr
		if !isPowerMonitor {
			continue
		}

		source := "hwmon/" + entry
		if sensorName != "" {
			source = "hwmon/" + sensorName
		}
		return PowerTelemetry{
			Available: true,
			VoltageV:  voltMV / 1e3,
			CurrentMa: currMA,
			Source:    source,
		}, true
	}

	return PowerTelemetry{}, false
}

// ComputeGainStep adjusts an RTL-SDR gain along the valid R820T gain table (0.0..49.6 dB)
// using hysteresis so gain does not oscillate when clipping sits in [0.3%, 1.0%].
func ComputeGainStep(currentGain float64, clippedPct float64, lowSignal bool) (newGain float64, changed bool) {
	if math.IsNaN(currentGain) || math.IsInf(currentGain, 0) {
		currentGain = 37.2
	}
	if math.IsNaN(clippedPct) || math.IsInf(clippedPct, 0) || clippedPct < 0 {
		clippedPct = 0
	}

	// Locate nearest entry in the valid RTL-SDR gain table.
	idx := 0
	bestDiff := math.Abs(currentGain - rtlsdrValidGains[0])
	for i := 1; i < len(rtlsdrValidGains); i++ {
		if d := math.Abs(currentGain - rtlsdrValidGains[i]); d < bestDiff {
			bestDiff = d
			idx = i
		}
	}

	// Clamp out-of-bounds inputs into the valid gain range.
	if currentGain < rtlsdrValidGains[0] {
		return rtlsdrValidGains[0], true
	}
	if currentGain > rtlsdrValidGains[len(rtlsdrValidGains)-1] {
		return rtlsdrValidGains[len(rtlsdrValidGains)-1], true
	}

	targetIdx := idx
	switch {
	case clippedPct > 5.0:
		targetIdx -= 2
	case clippedPct > uatClipTriggerPct:
		targetIdx--
	case lowSignal && clippedPct < uatClipClearPct:
		targetIdx++
	default:
		// Inside hysteresis deadband [0.3%, 1.0%] or optimal signal level: keep gain unchanged.
		return rtlsdrValidGains[idx], false
	}

	if targetIdx < 0 {
		targetIdx = 0
	}
	if targetIdx >= len(rtlsdrValidGains) {
		targetIdx = len(rtlsdrValidGains) - 1
	}

	next := rtlsdrValidGains[targetIdx]
	if targetIdx == idx {
		return next, false
	}
	return next, true
}

// rateRing60 is a fixed 60-sample circular buffer for 1 Hz message-rate sparklines.
type rateRing60 struct {
	buf   [sparklineWindowSec]int
	head  int // index of next write (and oldest element when count == sparklineWindowSec)
	count int
}

func (r *rateRing60) Push(v int) {
	if v < 0 {
		v = 0
	}
	r.buf[r.head] = v
	r.head = (r.head + 1) % sparklineWindowSec
	if r.count < sparklineWindowSec {
		r.count++
	}
}

func (r *rateRing60) Values() []int {
	if r.count == 0 {
		return []int{}
	}
	out := make([]int, r.count)
	if r.count < sparklineWindowSec {
		copy(out, r.buf[:r.count])
		return out
	}
	for i := 0; i < sparklineWindowSec; i++ {
		out[i] = r.buf[(r.head+i)%sparklineWindowSec]
	}
	return out
}

type bandState struct {
	health  RFBandHealth
	ring    rateRing60
	meanAmp float64
}

// HardwareHealthMonitor aggregates Pi hardware telemetry and RF receiver health safely across goroutines.
type HardwareHealthMonitor struct {
	mu        sync.RWMutex
	timestamp time.Time
	cpuTempC  float32
	throttle  PiThrottleStatus
	power     PowerTelemetry

	uat bandState
	es  bandState
	ogn bandState
	ais bandState

	esSuggestedGain float64

	// Previous total message counters for computing 1-second message rates.
	totalsInit bool
	prevUAT    uint64
	prevES     uint64
	prevOGN    uint64
	prevAIS    uint64
}

// NewHardwareHealthMonitor creates an initialized, thread-safe HardwareHealthMonitor.
func NewHardwareHealthMonitor() *HardwareHealthMonitor {
	m := &HardwareHealthMonitor{
		esSuggestedGain: 37.2,
	}
	m.uat.health.RecommendedGain = "optimal"
	m.es.health.RecommendedGain = "optimal"
	m.ogn.health.RecommendedGain = "optimal"
	m.ais.health.RecommendedGain = "optimal"
	return m
}

func (m *HardwareHealthMonitor) bandLocked(band string) *bandState {
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "uat", "978":
		return &m.uat
	case "es", "1090", "1090es":
		return &m.es
	case "ogn", "868":
		return &m.ogn
	case "ais", "162":
		return &m.ais
	default:
		return nil
	}
}

// RecordBandRate appends a 1-second message rate sample to the specified band's 60 s sparkline.
func (m *HardwareHealthMonitor) RecordBandRate(band string, msgsPerSec int) {
	if msgsPerSec < 0 {
		msgsPerSec = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.bandLocked(band); b != nil {
		b.health.MsgsPerSec = msgsPerSec
		b.ring.Push(msgsPerSec)
	}
}

// RecordRates appends 1-second message rates for all four RF bands atomically.
func (m *HardwareHealthMonitor) RecordRates(uat, es, ogn, ais int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	push := func(b *bandState, v int) {
		if v < 0 {
			v = 0
		}
		b.health.MsgsPerSec = v
		b.ring.Push(v)
	}
	push(&m.uat, uat)
	push(&m.es, es)
	push(&m.ogn, ogn)
	push(&m.ais, ais)
}

// RecordUATIQStats records dump978 IQ clipping statistics and applies overload hysteresis.
func (m *HardwareHealthMonitor) RecordUATIQStats(clippedPct, meanAmp float64, dropped uint64) {
	if math.IsNaN(clippedPct) || math.IsInf(clippedPct, 0) || clippedPct < 0 {
		clippedPct = 0
	}
	if math.IsNaN(meanAmp) || math.IsInf(meanAmp, 0) || meanAmp < 0 {
		meanAmp = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.uat.health.ClippedPercent = clippedPct
	m.uat.health.DroppedFrames = dropped
	m.uat.meanAmp = meanAmp

	if clippedPct > uatClipTriggerPct {
		m.uat.health.OverloadAlert = true
	} else if clippedPct < uatClipClearPct {
		m.uat.health.OverloadAlert = false
	}

	switch {
	case m.uat.health.OverloadAlert || clippedPct > uatClipTriggerPct:
		m.uat.health.RecommendedGain = "reduce"
	case meanAmp > 0 && meanAmp < uatLowAmpThresh && clippedPct < uatClipClearPct:
		m.uat.health.RecommendedGain = "increase"
	default:
		m.uat.health.RecommendedGain = "optimal"
	}
}

// RecordESSignalSample records 1090ES strong-signal percentage and updates overload & gain advisory.
func (m *HardwareHealthMonitor) RecordESSignalSample(strongPct float64) {
	if math.IsNaN(strongPct) || math.IsInf(strongPct, 0) || strongPct < 0 {
		strongPct = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.es.health.ClippedPercent = strongPct
	if strongPct > esStrongTriggerPct {
		m.es.health.OverloadAlert = true
	} else if strongPct < esStrongClearPct {
		m.es.health.OverloadAlert = false
	}

	if m.es.health.OverloadAlert || strongPct > esStrongTriggerPct {
		m.es.health.RecommendedGain = "reduce"
		// Map ES strong-signal percentage (>5%) into a step-down suggestion.
		nextGain, changed := ComputeGainStep(m.esSuggestedGain, strongPct, false)
		if changed {
			m.esSuggestedGain = nextGain
		}
	} else {
		m.es.health.RecommendedGain = "optimal"
	}
}

// SuggestedESGain returns the current auto-adjusted 1090ES gain suggestion (dB).
func (m *HardwareHealthMonitor) SuggestedESGain() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.esSuggestedGain
}

// SetSuggestedESGain seeds the baseline 1090ES gain used by RecordESSignalSample.
func (m *HardwareHealthMonitor) SetSuggestedESGain(gain float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !math.IsNaN(gain) && !math.IsInf(gain, 0) && gain >= 0 {
		m.esSuggestedGain = gain
	}
}

// RecordEstimatedPPM updates the estimated oscillator PPM drift for a band.
func (m *HardwareHealthMonitor) RecordEstimatedPPM(band string, ppm int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.bandLocked(band); b != nil {
		b.health.EstimatedPPM = ppm
	}
}

// SetBandStatus updates the enabled and connected flags for a band.
func (m *HardwareHealthMonitor) SetBandStatus(band string, enabled, connected bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.bandLocked(band); b != nil {
		b.health.Enabled = enabled
		b.health.Connected = connected
	}
}

// UpdateSystemTelemetry updates the CPU temperature, Raspberry Pi throttle flags, and power readings.
func (m *HardwareHealthMonitor) UpdateSystemTelemetry(cpuTemp float32, throttle PiThrottleStatus, power PowerTelemetry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timestamp = time.Now().UTC()
	m.cpuTempC = cpuTemp
	m.throttle = throttle
	m.power = power
}

// SampleTotals computes 1-second message rates from cumulative totals and appends them to the 60 s sparklines.
func (m *HardwareHealthMonitor) SampleTotals(uatTot, esTot, ognTot, aisTot uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.totalsInit {
		m.prevUAT = uatTot
		m.prevES = esTot
		m.prevOGN = ognTot
		m.prevAIS = aisTot
		m.totalsInit = true
		return
	}

	delta := func(cur, prev uint64) int {
		if cur <= prev {
			return 0
		}
		diff := cur - prev
		if diff > math.MaxInt32 {
			return math.MaxInt32
		}
		return int(diff)
	}

	uatRate := delta(uatTot, m.prevUAT)
	esRate := delta(esTot, m.prevES)
	ognRate := delta(ognTot, m.prevOGN)
	aisRate := delta(aisTot, m.prevAIS)

	m.prevUAT = uatTot
	m.prevES = esTot
	m.prevOGN = ognTot
	m.prevAIS = aisTot

	m.uat.health.MsgsPerSec = uatRate
	m.uat.ring.Push(uatRate)
	m.es.health.MsgsPerSec = esRate
	m.es.ring.Push(esRate)
	m.ogn.health.MsgsPerSec = ognRate
	m.ogn.ring.Push(ognRate)
	m.ais.health.MsgsPerSec = aisRate
	m.ais.ring.Push(aisRate)
}

func copyBandHealthLocked(b *bandState) RFBandHealth {
	cp := b.health
	cp.History60s = b.ring.Values()
	if cp.RecommendedGain == "" {
		cp.RecommendedGain = "optimal"
	}
	return cp
}

// Snapshot returns a race-free, deep-copied HardwareHealthSnapshot ready for JSON marshaling.
func (m *HardwareHealthMonitor) Snapshot() HardwareHealthSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ts := m.timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	snap := HardwareHealthSnapshot{
		Timestamp: ts,
		CPUTempC:  m.cpuTempC,
		Throttle:  m.throttle,
		Power:     m.power,
		UAT:       copyBandHealthLocked(&m.uat),
		ES:        copyBandHealthLocked(&m.es),
		OGN:       copyBandHealthLocked(&m.ogn),
		AIS:       copyBandHealthLocked(&m.ais),
		Warnings:  make([]string, 0),
	}

	if snap.Throttle.Available {
		if snap.Throttle.UnderVoltageNow {
			snap.Warnings = append(snap.Warnings, "Raspberry Pi undervoltage active — check power supply and USB cable")
		} else if snap.Throttle.UnderVoltageOccurred {
			snap.Warnings = append(snap.Warnings, "Raspberry Pi undervoltage occurred since boot")
		}
		if snap.Throttle.ThrottledNow {
			snap.Warnings = append(snap.Warnings, "Raspberry Pi CPU is currently throttled")
		}
		if snap.Throttle.FreqCappedNow {
			snap.Warnings = append(snap.Warnings, "Raspberry Pi ARM frequency is currently capped")
		}
		if snap.Throttle.SoftTempLimitNow {
			snap.Warnings = append(snap.Warnings, "Raspberry Pi soft temperature limit active")
		}
	}

	if snap.Power.Available && snap.Power.VoltageV > 0 && snap.Power.VoltageV < 4.75 {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("Low supply voltage detected (%.2f V)", snap.Power.VoltageV))
	}

	if snap.UAT.OverloadAlert {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("978 UAT RF overload (%.1f%% IQ samples clipped) — reduce gain", snap.UAT.ClippedPercent))
	}
	if snap.ES.OverloadAlert {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("1090 ES RF overload (%.1f%% strong signals) — reduce gain", snap.ES.ClippedPercent))
	}

	return snap
}

// Global hardware health monitor instance.
var hwHealth = NewHardwareHealthMonitor()

func hwHealthSampleGlobals() {
	globalStatusMutex.RLock()
	uatTot := globalStatus.UAT_messages_total
	esTot := globalStatus.ES_messages_total
	ognTot := globalStatus.OGN_messages_total
	aisTot := globalStatus.AIS_messages_total
	globalStatusMutex.RUnlock()

	hwHealth.SampleTotals(uatTot, esTot, ognTot, aisTot)
}

func hwHealthPollOnce() {
	throttle := ReadPiThrottleStatus()
	power := ReadPowerTelemetry()

	globalSettingsMutex.RLock()
	uatEnabled := globalSettings.UAT_Enabled || globalSettings.Ping_Enabled || globalSettings.Pong_Enabled
	esEnabled := globalSettings.ES_Enabled || globalSettings.Ping_Enabled || globalSettings.Pong_Enabled
	ognEnabled := globalSettings.OGN_Enabled
	aisEnabled := globalSettings.AIS_Enabled
	esGain := globalSettings.Dump1090Gain
	globalSettingsMutex.RUnlock()

	globalStatusMutex.RLock()
	cpuTemp := globalStatus.CPUTemp
	uatRadioConn := globalStatus.UATRadio_connected
	pingConn := globalStatus.Ping_connected
	pongConn := globalStatus.Pong_connected
	ognConn := globalStatus.OGN_connected
	aisConn := globalStatus.AIS_connected
	globalStatusMutex.RUnlock()

	sdrDevMutex.RLock()
	hasUAT := UATDev != nil
	hasES := ESDev != nil
	hasOGN := OGNDev != nil
	hasAIS := AISDev != nil
	sdrDevMutex.RUnlock()

	hwHealth.SetBandStatus("uat", uatEnabled, hasUAT || uatRadioConn || pingConn || pongConn)
	hwHealth.SetBandStatus("es", esEnabled, hasES || pingConn || pongConn)
	hwHealth.SetBandStatus("ogn", ognEnabled, hasOGN || ognConn)
	hwHealth.SetBandStatus("ais", aisEnabled, hasAIS || aisConn)
	if esGain > 0 {
		hwHealth.SetSuggestedESGain(esGain)
	}
	hwHealth.UpdateSystemTelemetry(cpuTemp, throttle, power)

	snap := hwHealth.Snapshot()
	syncHwHealthSystemErrors(snap)
}

func syncHwHealthSystemErrors(snap HardwareHealthSnapshot) {
	if systemErrsMutex == nil || systemErrs == nil {
		return
	}

	if snap.Throttle.Available && snap.Throttle.UnderVoltageNow {
		addSingleSystemErrorf("hw-undervoltage", "Hardware warning: Raspberry Pi undervoltage detected (0x%x) — check power supply", snap.Throttle.Raw)
	} else {
		removeSingleSystemError("hw-undervoltage")
	}

	if snap.Throttle.Available && snap.Throttle.ThrottledNow {
		addSingleSystemErrorf("hw-throttled", "Hardware warning: Raspberry Pi CPU throttling active (0x%x)", snap.Throttle.Raw)
	} else {
		removeSingleSystemError("hw-throttled")
	}

	if snap.UAT.OverloadAlert {
		addSingleSystemErrorf("hw-uat-overload", "RF warning: 978 UAT IQ clipping overload (%.1f%%) — reduce gain", snap.UAT.ClippedPercent)
	} else {
		removeSingleSystemError("hw-uat-overload")
	}

	if snap.ES.OverloadAlert {
		addSingleSystemErrorf("hw-es-overload", "RF warning: 1090 ES strong-signal overload (%.1f%%) — reduce gain", snap.ES.ClippedPercent)
	} else {
		removeSingleSystemError("hw-es-overload")
	}
}

// hwHealthWatcher polls Pi hardware telemetry every 5 seconds, updates 1 Hz RF sparklines,
// and surfaces active undervoltage/throttling/clipping conditions via system errors.
func hwHealthWatcher() {
	hwHealthSampleGlobals()
	hwHealthPollOnce()

	rateTicker := time.NewTicker(1 * time.Second)
	defer rateTicker.Stop()

	hwTicker := time.NewTicker(5 * time.Second)
	defer hwTicker.Stop()

	for {
		select {
		case <-rateTicker.C:
			hwHealthSampleGlobals()
		case <-hwTicker.C:
			hwHealthPollOnce()
		}
	}
}

// handleHealthRequest serves GET /getHealth with the current HardwareHealthSnapshot as JSON.
func handleHealthRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)
	snap := hwHealth.Snapshot()
	healthJSON, err := json.Marshal(&snap)
	if err != nil {
		http.Error(w, "failed to marshal hardware health", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", healthJSON)
}
