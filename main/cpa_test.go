package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestComputeCPA_HeadOnUrgentAndWarningAndAdvisory(t *testing.T) {
	cfg := DefaultCPAConfig()

	// Ownship at (47.0, 8.0), 5000 ft, heading North (0 deg), 100 kt (~51.44 m/s).
	// Target directly north, heading South (180 deg), 100 kt -> relative closing speed ~102.89 m/s.
	// 1 degree latitude ~= 111,195 m.
	// At ~1030 m north (~0.00926 deg), tCPA ~= 10s (< 12s), miss ~= 0m -> Urgent!
	urgentTarget := TrafficInfo{
		Icao_addr:      0x111111,
		Position_valid: true,
		Lat:            47.00926,
		Lng:            8.0,
		Alt:            5000,
		Track:          180,
		Speed:          100,
		Speed_valid:    true,
		Age:            1.0,
	}
	res := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, urgentTarget, cfg)
	if !res.Valid {
		t.Fatalf("expected valid CPAResult")
	}
	if res.AlertLevel != CPAAlertUrgent {
		t.Errorf("AlertLevel = %d, want Urgent (%d) [tCPA=%.1fs horiz=%.1fm vert=%.1fft]", res.AlertLevel, CPAAlertUrgent, res.TimeToCPASec, res.HorizMissMeters, res.VertMissFeet)
	}
	if math.Abs(res.TimeToCPASec-10.0) > 1.0 {
		t.Errorf("TimeToCPASec = %.2f, want ~10s", res.TimeToCPASec)
	}
	if res.HorizMissMeters > 5.0 {
		t.Errorf("HorizMissMeters = %.2f, want ~0m", res.HorizMissMeters)
	}
	if res.ClockPosition != 12 {
		t.Errorf("ClockPosition = %d, want 12", res.ClockPosition)
	}
	if res.ClosureRateMps < 95.0 {
		t.Errorf("ClosureRateMps = %.2f, want ~102.9 m/s", res.ClosureRateMps)
	}
	if !strings.Contains(res.Callout, "12 o'clock") || !strings.Contains(res.Callout, "same altitude") || !strings.Contains(res.Callout, "converging") {
		t.Errorf("unexpected urgent callout: %q", res.Callout)
	}

	// At ~2060 m north (~0.01852 deg), tCPA ~= 20s (12 < tCPA <= 25) -> Warning!
	warningTarget := urgentTarget
	warningTarget.Lat = 47.01852
	warningTarget.Alt = 5300
	resW := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, warningTarget, cfg)
	if resW.AlertLevel != CPAAlertWarning {
		t.Errorf("Warning AlertLevel = %d, want %d [tCPA=%.1fs]", resW.AlertLevel, CPAAlertWarning, resW.TimeToCPASec)
	}
	if !strings.Contains(resW.Callout, "300 feet high") {
		t.Errorf("expected '300 feet high' in callout: %q", resW.Callout)
	}

	// At ~3600 m north (~0.0324 deg), tCPA ~= 35s (25 < tCPA <= 45) -> Advisory!
	advisoryTarget := urgentTarget
	advisoryTarget.Lat = 47.0324
	advisoryTarget.Alt = 4500
	resA := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, advisoryTarget, cfg)
	if resA.AlertLevel != CPAAlertAdvisory {
		t.Errorf("Advisory AlertLevel = %d, want %d [tCPA=%.1fs]", resA.AlertLevel, CPAAlertAdvisory, resA.TimeToCPASec)
	}
	if !strings.Contains(resA.Callout, "500 feet low") {
		t.Errorf("expected '500 feet low' in callout: %q", resA.Callout)
	}
}

func TestComputeCPA_VerticalConvergenceAndSeparation(t *testing.T) {
	cfg := DefaultCPAConfig()
	// Target is 1000 ft higher right now, descending at -2000 fpm while ownship climbs at +1000 fpm.
	// Relative vertical closure is 3000 fpm = 50 ft/sec -> reaches same altitude in 20s!
	// Horizontal separation ~2060m head-on at 102.9 m/s -> tCPA ~20s -> vertical miss near 0 ft -> Warning!
	ti := TrafficInfo{
		Position_valid: true,
		Lat:            47.01852,
		Lng:            8.0,
		Alt:            6000,
		Track:          180,
		Speed:          100,
		Speed_valid:    true,
		Vvel:           -2000,
		Age:            0.5,
	}
	res := ComputeCPA(47.0, 8.0, 5000, 0, 100, 1000, false, ti, cfg)
	if res.AlertLevel != CPAAlertWarning {
		t.Fatalf("vertical convergence expected Warning, got %d (vertMiss=%.1fft tCPA=%.1fs)", res.AlertLevel, res.VertMissFeet, res.TimeToCPASec)
	}
	if res.VertMissFeet > 100 {
		t.Errorf("VertMissFeet = %.1f, want <100 ft", res.VertMissFeet)
	}

	// Same geometry but level 1500 ft above -> VertMiss = 1500 ft > 800 ft -> No Alert!
	ti.Alt = 6500
	ti.Vvel = 0
	resSep := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, ti, cfg)
	if resSep.AlertLevel != CPAAlertNone {
		t.Errorf("vertically separated target alerted: level=%d vertMiss=%.1f", resSep.AlertLevel, resSep.VertMissFeet)
	}
}

func TestComputeCPA_StaticZeroRelativeVelocityAndDiverging(t *testing.T) {
	cfg := DefaultCPAConfig()

	// Parallel same-speed aircraft 300m to the right (zero relative velocity).
	parallel := TrafficInfo{
		Position_valid: true,
		Lat:            47.0,
		Lng:            8.00395, // ~300 m east at lat 47
		Alt:            5000,
		Track:          0,
		Speed:          100,
		Speed_valid:    true,
		Age:            1.0,
	}
	res := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, parallel, cfg)
	if !res.Valid || res.TimeToCPASec != 0 || res.AlertLevel != CPAAlertUrgent {
		t.Errorf("static zero-rel-vel inside 300m: valid=%v tCPA=%.1f level=%d (horiz=%.1f)", res.Valid, res.TimeToCPASec, res.AlertLevel, res.HorizMissMeters)
	}
	if res.ClockPosition != 3 {
		t.Errorf("east target when heading north: clock=%d, want 3", res.ClockPosition)
	}

	// Diverging target 1200m behind us flying opposite direction -> suppressed (no nuisance alert).
	diverging := TrafficInfo{
		Position_valid: true,
		Lat:            46.9892, // ~1200 m south
		Lng:            8.0,
		Alt:            5000,
		Track:          180,
		Speed:          100,
		Speed_valid:    true,
		Age:            1.0,
	}
	resDiv := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, diverging, cfg)
	if resDiv.AlertLevel != CPAAlertNone {
		t.Errorf("diverging target outside urgent bubble should not alert, got level=%d closure=%.1f", resDiv.AlertLevel, resDiv.ClosureRateMps)
	}
}

func TestComputeCPA_SuppressionCases(t *testing.T) {
	cfg := DefaultCPAConfig()
	base := TrafficInfo{
		Position_valid: true,
		Lat:            47.005,
		Lng:            8.0,
		Alt:            5000,
		Track:          180,
		Speed:          100,
		Speed_valid:    true,
		Age:            1.0,
	}

	// Ownship on ground.
	if r := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, true, base, cfg); r.AlertLevel != 0 {
		t.Errorf("ownOnGround should suppress alert, got %d", r.AlertLevel)
	}
	// Target on ground.
	onGnd := base
	onGnd.OnGround = true
	if r := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, onGnd, cfg); r.AlertLevel != 0 {
		t.Errorf("target OnGround should suppress alert, got %d", r.AlertLevel)
	}
	// Taxi speed (< 20 kt).
	if r := ComputeCPA(47.0, 8.0, 5000, 0, 15, 0, false, base, cfg); r.AlertLevel != 0 {
		t.Errorf("taxi speed should suppress alert, got %d", r.AlertLevel)
	}
	// Position invalid.
	noPos := base
	noPos.Position_valid = false
	if r := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, noPos, cfg); r.Valid || r.AlertLevel != 0 {
		t.Errorf("Position_valid=false should return invalid/level=0")
	}
	// Stale (> 15s).
	stale := base
	stale.Age = 16.0
	if r := ComputeCPA(47.0, 8.0, 5000, 0, 100, 0, false, stale, cfg); r.Valid || r.AlertLevel != 0 {
		t.Errorf("stale target (Age>15) should return invalid/level=0")
	}
}

func TestSelectHighestThreatAndCalloutDebouncer(t *testing.T) {
	results := map[uint32]CPAResult{
		0x100001: {Valid: true, AlertLevel: CPAAlertAdvisory, TimeToCPASec: 30, HorizMissMeters: 1500, CurrentDistMeters: 3000},
		0x100002: {Valid: true, AlertLevel: CPAAlertWarning, TimeToCPASec: 18, HorizMissMeters: 800, CurrentDistMeters: 2000},
		0x100003: {Valid: true, AlertLevel: CPAAlertWarning, TimeToCPASec: 14, HorizMissMeters: 900, CurrentDistMeters: 1800},
		0x100004: {Valid: true, AlertLevel: CPAAlertNone, TimeToCPASec: 5, HorizMissMeters: 100, CurrentDistMeters: 500},
	}

	key, res, ok := SelectHighestThreat(results)
	if !ok || key != 0x100003 || res.AlertLevel != CPAAlertWarning {
		t.Fatalf("SelectHighestThreat = 0x%X (level=%d, ok=%v), want 0x100003 Warning", key, res.AlertLevel, ok)
	}

	// Adding an Urgent threat overrides Warning.
	results[0x100005] = CPAResult{Valid: true, AlertLevel: CPAAlertUrgent, TimeToCPASec: 11, HorizMissMeters: 400, CurrentDistMeters: 1100}
	key, res, ok = SelectHighestThreat(results)
	if !ok || key != 0x100005 || res.AlertLevel != CPAAlertUrgent {
		t.Fatalf("SelectHighestThreat with Urgent = 0x%X (level=%d, ok=%v), want 0x100005 Urgent", key, res.AlertLevel, ok)
	}

	// Empty / no-alert map returns false.
	if _, _, ok := SelectHighestThreat(map[uint32]CPAResult{0x1: {Valid: true, AlertLevel: 0}}); ok {
		t.Fatalf("expected no threat when all AlertLevel == 0")
	}

	// CalloutDebouncer cooldown + escalation tests.
	deb := NewCalloutDebouncer(15 * time.Second)
	t0 := time.Now()
	adv := CPAResult{Valid: true, AlertLevel: CPAAlertAdvisory, Callout: "Traffic, 11 o'clock, 1 mile, 300 feet high, converging"}
	warn := CPAResult{Valid: true, AlertLevel: CPAAlertWarning, Callout: "Traffic, 11 o'clock, half a mile, 200 feet high, converging"}

	if !deb.ShouldAnnounce(0x100001, adv, t0) {
		t.Fatalf("first alert must announce")
	}
	if deb.ShouldAnnounce(0x100001, adv, t0.Add(5*time.Second)) {
		t.Fatalf("repeat alert within 15s cooldown must be suppressed")
	}
	// Escalation bypasses cooldown!
	if !deb.ShouldAnnounce(0x100001, warn, t0.Add(6*time.Second)) {
		t.Fatalf("escalation from Advisory to Warning must bypass cooldown")
	}
	// Same level within 15s after escalation is suppressed.
	if deb.ShouldAnnounce(0x100001, warn, t0.Add(18*time.Second)) {
		t.Fatalf("repeat Warning within 15s of escalation must be suppressed")
	}
	// After 15s cooldown expires, announces again.
	if !deb.ShouldAnnounce(0x100001, warn, t0.Add(22*time.Second)) {
		t.Fatalf("alert after cooldown expiry must announce")
	}
}

func TestFormatTrafficCallout(t *testing.T) {
	if got := FormatTrafficCallout(11, 1852, 310, 40); got != "Traffic, 11 o'clock, 1 mile, 300 feet high, converging" {
		t.Errorf("got %q", got)
	}
	if got := FormatTrafficCallout(12, 400, -20, 20); got != "Traffic, 12 o'clock, less than half a mile, same altitude, converging" {
		t.Errorf("got %q", got)
	}
	if got := FormatTrafficCallout(3, 3704, -520, 0); got != "Traffic, 3 o'clock, 2 miles, 500 feet low" {
		t.Errorf("got %q", got)
	}
}
