/*
	Copyright (c) 2025 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	cpa.go: True 3D Closest Point of Approach (CPA) traffic conflict detection,
	highest-threat selection for FLARM PFLAU, and debounced aviation audio callouts.
*/

package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/common"
)

const (
	CPAAlertNone     uint8 = 0
	CPAAlertAdvisory uint8 = 1
	CPAAlertWarning  uint8 = 2
	CPAAlertUrgent   uint8 = 3

	cpaFeetToMeters    = 0.3048
	cpaKnotsToMps      = 1852.0 / 3600.0
	cpaFpmToMps        = 0.3048 / 60.0
	cpaMetersPerNM     = 1852.0
	cpaMaxTargetAgeSec = 15.0
	// Diverging targets outside the urgent horizontal bubble separating faster than
	// ~4 kt do not raise future conflict alerts.
	cpaDivergeSuppressMps = -2.0
)

// CPAThreshold defines the time-to-CPA, horizontal miss, and vertical miss limits for an alert tier.
type CPAThreshold struct {
	TimeSec float64
	HorizM  float64
	VertFt  float64
}

// CPAConfig controls the 3D CPA prediction horizon, ground-taxi suppression, and alert envelopes.
type CPAConfig struct {
	HorizonSec        float64
	TaxiGroundSpeedKt float64
	Advisory          CPAThreshold
	Warning           CPAThreshold
	Urgent            CPAThreshold
}

// DefaultCPAConfig returns standard GA/glider 3D CPA alerting thresholds.
func DefaultCPAConfig() CPAConfig {
	return CPAConfig{
		HorizonSec:        60,
		TaxiGroundSpeedKt: 20,
		Advisory:          CPAThreshold{TimeSec: 45, HorizM: 2500, VertFt: 800},
		Warning:           CPAThreshold{TimeSec: 25, HorizM: 1200, VertFt: 500},
		Urgent:            CPAThreshold{TimeSec: 12, HorizM: 500, VertFt: 300},
	}
}

// CPAResult holds the 3D CPA solution, threat level, relative clock bearing, and formatted audio callout.
type CPAResult struct {
	Valid              bool
	TimeToCPASec       float64
	HorizMissMeters    float64
	VertMissFeet       float64
	CurrentDistMeters  float64
	ClosureRateMps     float64
	AlertLevel         uint8   // 0=None, 1=Advisory, 2=Warning, 3=Urgent
	ClockPosition      int     // 1..12
	RelativeBearingDeg float64 // -180..+180 (positive = right of ownship track)
	Callout            string
}

// ComputeCPA solves the linear 3D Closest Point of Approach between ownship and a traffic target
// in a local ENU tangent plane and classifies the resulting threat level.
func ComputeCPA(
	ownLat, ownLon, ownAltFt, ownTrackDeg, ownSpeedKt, ownVvelFpm float64,
	ownOnGround bool,
	ti TrafficInfo,
	cfg CPAConfig,
) CPAResult {
	cfg = normalizeCPAConfig(cfg)

	if !ti.Position_valid || ti.Age > cpaMaxTargetAgeSec {
		return CPAResult{}
	}
	tgtLat := float64(ti.Lat)
	tgtLon := float64(ti.Lng)
	if !isFinite(ownLat) || !isFinite(ownLon) || !isFinite(ownAltFt) ||
		!isFinite(ownTrackDeg) || !isFinite(ownSpeedKt) || !isFinite(ownVvelFpm) ||
		!isFinite(tgtLat) || !isFinite(tgtLon) || (tgtLat == 0 && tgtLon == 0) {
		return CPAResult{}
	}

	// Local ENU relative position vector r0 = target - ownship (meters).
	distM, trueBearingDeg := common.Distance(ownLat, ownLon, tgtLat, tgtLon)
	if !isFinite(distM) || !isFinite(trueBearingDeg) {
		return CPAResult{}
	}
	brgRad := common.Radians(trueBearingDeg)
	rE := distM * math.Sin(brgRad)
	rN := distM * math.Cos(brgRad)
	altDiffFt := float64(ti.Alt) - ownAltFt
	rU := altDiffFt * cpaFeetToMeters

	// Ownship ENU velocity vector (m/s).
	ownSpd := ownSpeedKt
	if ownSpd < 0 {
		ownSpd = 0
	}
	ownSpdMps := ownSpd * cpaKnotsToMps
	ownTrackRad := common.Radians(ownTrackDeg)
	vOwnE := ownSpdMps * math.Sin(ownTrackRad)
	vOwnN := ownSpdMps * math.Cos(ownTrackRad)
	vOwnU := ownVvelFpm * cpaFpmToMps

	// Target ENU velocity vector (m/s).
	var vTgtE, vTgtN float64
	if ti.Speed_valid {
		tgtSpdMps := float64(ti.Speed) * cpaKnotsToMps
		tgtTrackRad := common.Radians(float64(ti.Track))
		vTgtE = tgtSpdMps * math.Sin(tgtTrackRad)
		vTgtN = tgtSpdMps * math.Cos(tgtTrackRad)
	}
	vTgtU := float64(ti.Vvel) * cpaFpmToMps

	// Relative velocity v_rel = v_target - v_ownship.
	vRelE := vTgtE - vOwnE
	vRelN := vTgtN - vOwnN
	vRelU := vTgtU - vOwnU

	vRelSq := vRelE*vRelE + vRelN*vRelN + vRelU*vRelU
	dotRV := rE*vRelE + rN*vRelN + rU*vRelU

	// Solve t_cpa = -dot(r0, v_rel) / dot(v_rel, v_rel) clamped to [0, HorizonSec].
	tCPA := 0.0
	if vRelSq > 1e-6 {
		tCPA = -dotRV / vRelSq
		if tCPA < 0 {
			tCPA = 0
		} else if tCPA > cfg.HorizonSec {
			tCPA = cfg.HorizonSec
		}
	}

	cpaE := rE + vRelE*tCPA
	cpaN := rN + vRelN*tCPA
	cpaU := rU + vRelU*tCPA
	horizMissM := math.Hypot(cpaE, cpaN)
	vertMissFt := math.Abs(cpaU) / cpaFeetToMeters

	// Closure rate (m/s): positive when 3D range is decreasing.
	r3d := math.Sqrt(rE*rE + rN*rN + rU*rU)
	closureRateMps := 0.0
	if r3d > 1e-6 {
		closureRateMps = -dotRV / r3d
	}

	relBearing := normalizeRelBearing180(trueBearingDeg - ownTrackDeg)
	clockPos := bearingToClockPosition(relBearing)

	res := CPAResult{
		Valid:              true,
		TimeToCPASec:       tCPA,
		HorizMissMeters:    horizMissM,
		VertMissFeet:       vertMissFt,
		CurrentDistMeters:  distM,
		ClosureRateMps:     closureRateMps,
		AlertLevel:         CPAAlertNone,
		ClockPosition:      clockPos,
		RelativeBearingDeg: relBearing,
	}

	// Suppress alerts on ground or below taxi speed threshold.
	if ownOnGround || ti.OnGround || ownSpeedKt < cfg.TaxiGroundSpeedKt {
		return res
	}

	// Suppress alerts when clearly diverging outside the urgent horizontal bubble.
	if closureRateMps < cpaDivergeSuppressMps && distM > cfg.Urgent.HorizM {
		return res
	}

	switch {
	case matchesCPAThreshold(tCPA, horizMissM, vertMissFt, cfg.Urgent):
		res.AlertLevel = CPAAlertUrgent
	case matchesCPAThreshold(tCPA, horizMissM, vertMissFt, cfg.Warning):
		res.AlertLevel = CPAAlertWarning
	case matchesCPAThreshold(tCPA, horizMissM, vertMissFt, cfg.Advisory):
		res.AlertLevel = CPAAlertAdvisory
	}

	if res.AlertLevel >= CPAAlertAdvisory {
		res.Callout = FormatTrafficCallout(clockPos, distM, altDiffFt, closureRateMps)
	}
	return res
}

// FormatTrafficCallout formats a concise aviation traffic advisory string, e.g.
// "Traffic, 11 o'clock, 1 mile, 300 feet high, converging".
func FormatTrafficCallout(clockPos int, currentDistMeters float64, altDiffFeet float64, closureRateMps float64) string {
	if clockPos < 1 || clockPos > 12 {
		clockPos = 12
	}

	nm := currentDistMeters / cpaMetersPerNM
	var distStr string
	switch {
	case nm < 0.5:
		distStr = "less than half a mile"
	case nm < 0.75:
		distStr = "half a mile"
	default:
		miles := int(math.Round(nm))
		if miles <= 1 {
			distStr = "1 mile"
		} else {
			distStr = fmt.Sprintf("%d miles", miles)
		}
	}

	roundedAlt := int(math.Round(math.Abs(altDiffFeet)/100.0)) * 100
	var altStr string
	switch {
	case roundedAlt < 100:
		altStr = "same altitude"
	case altDiffFeet > 0:
		altStr = fmt.Sprintf("%d feet high", roundedAlt)
	default:
		altStr = fmt.Sprintf("%d feet low", roundedAlt)
	}

	parts := []string{
		"Traffic",
		fmt.Sprintf("%d o'clock", clockPos),
		distStr,
		altStr,
	}
	if closureRateMps > 5.0 {
		parts = append(parts, "converging")
	}
	return strings.Join(parts, ", ")
}

// SelectHighestThreat selects the highest-priority active threat from a map of CPA results
// (for driving FLARM PFLAU alarm level, relative bearing, distance, and vertical separation).
func SelectHighestThreat(results map[uint32]CPAResult) (uint32, CPAResult, bool) {
	var bestKey uint32
	var bestRes CPAResult
	found := false

	for k, r := range results {
		if !r.Valid || r.AlertLevel == CPAAlertNone {
			continue
		}
		if !found || isHigherThreat(k, r, bestKey, bestRes) {
			bestKey = k
			bestRes = r
			found = true
		}
	}
	return bestKey, bestRes, found
}

func isHigherThreat(kA uint32, a CPAResult, kB uint32, b CPAResult) bool {
	if a.AlertLevel != b.AlertLevel {
		return a.AlertLevel > b.AlertLevel
	}
	if math.Abs(a.TimeToCPASec-b.TimeToCPASec) > 1e-6 {
		return a.TimeToCPASec < b.TimeToCPASec
	}
	if math.Abs(a.HorizMissMeters-b.HorizMissMeters) > 1e-6 {
		return a.HorizMissMeters < b.HorizMissMeters
	}
	if math.Abs(a.CurrentDistMeters-b.CurrentDistMeters) > 1e-6 {
		return a.CurrentDistMeters < b.CurrentDistMeters
	}
	return kA < kB
}

type calloutRecord struct {
	lastTime time.Time
	level    uint8
}

// CalloutDebouncer suppresses repetitive per-target audio callouts for a cooldown interval
// unless the target's alert level escalates.
type CalloutDebouncer struct {
	mu       sync.Mutex
	cooldown time.Duration
	history  map[uint32]calloutRecord
}

// NewCalloutDebouncer constructs a CalloutDebouncer. If cooldown <= 0, defaults to 15 seconds.
func NewCalloutDebouncer(cooldown time.Duration) *CalloutDebouncer {
	if cooldown <= 0 {
		cooldown = 15 * time.Second
	}
	return &CalloutDebouncer{
		cooldown: cooldown,
		history:  make(map[uint32]calloutRecord),
	}
}

// ShouldAnnounce returns true if the callout for `icao` should be spoken at `now`.
// It returns true on first alert, on any AlertLevel escalation, or once the cooldown has elapsed.
func (d *CalloutDebouncer) ShouldAnnounce(icao uint32, res CPAResult, now time.Time) bool {
	if d == nil || !res.Valid || res.AlertLevel == CPAAlertNone || res.Callout == "" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if prev, ok := d.history[icao]; ok {
		escalated := res.AlertLevel > prev.level
		cooledDown := now.Sub(prev.lastTime) >= d.cooldown
		if !escalated && !cooledDown {
			return false
		}
	}
	d.history[icao] = calloutRecord{
		lastTime: now,
		level:    res.AlertLevel,
	}
	return true
}

// Prune removes callout records older than maxAge to keep memory bounded.
func (d *CalloutDebouncer) Prune(now time.Time, maxAge time.Duration) {
	if d == nil || maxAge <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, rec := range d.history {
		if now.Sub(rec.lastTime) > maxAge {
			delete(d.history, k)
		}
	}
}

func normalizeCPAConfig(cfg CPAConfig) CPAConfig {
	def := DefaultCPAConfig()
	if cfg.HorizonSec <= 0 {
		cfg.HorizonSec = def.HorizonSec
	}
	if cfg.TaxiGroundSpeedKt == 0 && cfg.Advisory == (CPAThreshold{}) && cfg.Warning == (CPAThreshold{}) && cfg.Urgent == (CPAThreshold{}) {
		return def
	}
	if cfg.Advisory == (CPAThreshold{}) {
		cfg.Advisory = def.Advisory
	}
	if cfg.Warning == (CPAThreshold{}) {
		cfg.Warning = def.Warning
	}
	if cfg.Urgent == (CPAThreshold{}) {
		cfg.Urgent = def.Urgent
	}
	return cfg
}

func matchesCPAThreshold(tCPA, horizM, vertFt float64, th CPAThreshold) bool {
	return tCPA <= th.TimeSec && horizM <= th.HorizM && vertFt <= th.VertFt
}

func normalizeRelBearing180(deg float64) float64 {
	deg = math.Mod(deg, 360.0)
	if deg > 180.0 {
		deg -= 360.0
	} else if deg <= -180.0 {
		deg += 360.0
	}
	return deg
}

func bearingToClockPosition(relBearingDeg float64) int {
	b360 := math.Mod(relBearingDeg+360.0, 360.0)
	hour := int(math.Floor((b360+15.0)/30.0)) % 12
	if hour == 0 {
		return 12
	}
	return hour
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
