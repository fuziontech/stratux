package main

import (
	"testing"
	"time"
)

func TestTrafficSourcePriorityTiers(t *testing.T) {
	cases := []struct {
		name         string
		source       int
		targetType   uint8
		extrapolated bool
		isAPRS       bool
		want         int
	}{
		{"1090ES ADS-B", TRAFFIC_SOURCE_1090ES, TARGET_TYPE_ADSB, false, false, 100},
		{"978 UAT ADS-B", TRAFFIC_SOURCE_UAT, TARGET_TYPE_ADSB, false, false, 100},
		{"Direct RF OGN", TRAFFIC_SOURCE_OGN, TARGET_TYPE_MODE_S, false, false, 95},
		{"Direct RF FLARM", TRAFFIC_SOURCE_FLARM, 0, false, false, 95},
		{"Remote ID UAS", TRAFFIC_SOURCE_REMOTEID, TARGET_TYPE_REMOTEID, false, false, 85},
		{"ADS-R", TRAFFIC_SOURCE_UAT, TARGET_TYPE_ADSR, false, false, 75},
		{"TIS-B", TRAFFIC_SOURCE_UAT, TARGET_TYPE_TISB, false, false, 75},
		{"TIS-B Mode-S", TRAFFIC_SOURCE_UAT, TARGET_TYPE_TISB_S, false, false, 75},
		{"Direct RF AIS", TRAFFIC_SOURCE_AIS, TARGET_TYPE_AIS, false, false, 70},
		{"CoT", TRAFFIC_SOURCE_COT, 0, false, false, 50},
		{"Mode-S bearingless", TRAFFIC_SOURCE_1090ES, TARGET_TYPE_MODE_S, false, false, 40},
		{"Internet APRS", TRAFFIC_SOURCE_OGN, 0, false, true, 30},
		{"Extrapolated ADS-B", TRAFFIC_SOURCE_1090ES, TARGET_TYPE_ADSB, true, false, 10},
		{"Extrapolated APRS", TRAFFIC_SOURCE_OGN, 0, true, true, 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			if tc.isAPRS {
				got = TrafficSourcePriorityWithAPRS(tc.source, tc.targetType, tc.extrapolated, true)
			} else {
				got = TrafficSourcePriority(tc.source, tc.targetType, tc.extrapolated)
			}
			if got != tc.want {
				t.Errorf("priority = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTrafficFusionIndex_DirectAndDDBAlias(t *testing.T) {
	idx := NewTrafficFusionIndex()
	const (
		icaoAddr  = uint32(0x3D1234)
		flarmAddr = uint32(0xDD9988)
	)
	idx.RegisterDDBAlias(flarmAddr, icaoAddr)

	now := time.Now()
	existing := map[uint32]TrafficInfo{
		icaoAddr: {
			Icao_addr:      icaoAddr,
			Position_valid: true,
			Last_source:    TRAFFIC_SOURCE_1090ES,
			TargetType:     TARGET_TYPE_ADSB,
			Lat:            48.1234,
			Lng:            11.5678,
			Alt:            4500,
			Track:          90,
			Last_seen:      now,
		},
	}

	// Step 1: Direct exact match on ICAO key.
	key, matched := idx.ResolveCanonicalKey(icaoAddr, 0, 48.1234, 11.5678, 4500, 90, true, existing)
	if !matched || key != icaoAddr {
		t.Fatalf("direct match failed: key=0x%X matched=%v, want 0x%X/true", key, matched, icaoAddr)
	}

	// Step 2: FLARM ID with addrType=1 resolves onto existing ICAO key via DDB alias.
	key, matched = idx.ResolveCanonicalKey(flarmAddr, 1, 48.2000, 11.6000, 5000, 180, false, existing)
	if !matched || key != icaoAddr {
		t.Fatalf("DDB cross-link failed: key=0x%X matched=%v, want 0x%X/true", key, matched, icaoAddr)
	}

	// Unknown address with no spatial match creates new typed key.
	unknownAddr := uint32(0xAA0001)
	key, matched = idx.ResolveCanonicalKey(unknownAddr, 1, 49.0, 12.0, 8000, 270, true, existing)
	wantKey := uint32(1)<<24 | unknownAddr
	if matched || key != wantKey {
		t.Fatalf("unknown target = 0x%X/%v, want 0x%X/false", key, matched, wantKey)
	}
}

func TestTrafficFusionIndex_LoadDDBAliasesFromJSON(t *testing.T) {
	idx := NewTrafficFusionIndex()
	sample := []byte(`{
		"devices": [
			{"device_type":"I","device_id":"3D1111","registration":"D-KABC"},
			{"device_type":"F","device_id":"DD2222","registration":"D-KABC"},
			{"device_type":"O","device_id":"003333","registration":"D-KABC"},
			{"device_type":"F","device_id":"DD4444","registration":"D-ONLYFLARM"}
		]
	}`)
	n := idx.LoadDDBAliasesFromJSON(sample)
	if n != 2 {
		t.Fatalf("LoadDDBAliasesFromJSON registered %d aliases, want 2", n)
	}
	if icao, ok := idx.LookupICAO(0xDD2222); !ok || icao != 0x3D1111 {
		t.Errorf("FLARM alias lookup = 0x%X/%v, want 0x3D1111/true", icao, ok)
	}
	if icao, ok := idx.LookupICAO(0x003333); !ok || icao != 0x3D1111 {
		t.Errorf("OGN alias lookup = 0x%X/%v, want 0x3D1111/true", icao, ok)
	}
	if _, ok := idx.LookupICAO(0xDD4444); ok {
		t.Errorf("unexpected alias for FLARM-only aircraft")
	}
}

func TestTrafficFusionIndex_SpatialVelocityCorrelation(t *testing.T) {
	idx := NewTrafficFusionIndex()
	now := time.Now()
	const icaoKey = uint32(0xA12345)

	existing := map[uint32]TrafficInfo{
		icaoKey: {
			Icao_addr:      icaoKey,
			Position_valid: true,
			Last_source:    TRAFFIC_SOURCE_1090ES,
			TargetType:     TARGET_TYPE_ADSB,
			Lat:            47.5000,
			Lng:            8.5000,
			Alt:            5000,
			Track:          355,
			Speed:          110,
			Speed_valid:    true,
			Last_seen:      now.Add(-2 * time.Second),
		},
	}

	// Within 150m (~55m north), 200 ft vertically, 10 deg wrap-around track delta (355 vs 5), 10 kt speed delta, different source -> fuse!
	key, matched := idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5005, 8.5000, 5200, 5, 100, true, true, now, existing,
	)
	if !matched || key != icaoKey {
		t.Fatalf("expected 3D spatial/velocity fusion onto 0x%X, got 0x%X matched=%v", icaoKey, key, matched)
	}

	// Same source (1090ES formation flight) -> MUST NOT fuse!
	key, matched = idx.ResolveCanonicalKeyWithContext(
		0xA99999, 0, TRAFFIC_SOURCE_1090ES,
		47.5005, 8.5000, 5050, 358, 110, true, true, now, existing,
	)
	if matched {
		t.Fatalf("same-source formation aircraft should not fuse, matched key=0x%X", key)
	}

	// Exceeds horizontal limit (~550m away) -> no fusion.
	_, matched = idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5050, 8.5000, 5000, 355, 110, true, true, now, existing,
	)
	if matched {
		t.Fatalf("target >150m away should not fuse")
	}

	// Exceeds vertical limit (400 ft) -> no fusion.
	_, matched = idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5002, 8.5000, 5400, 355, 110, true, true, now, existing,
	)
	if matched {
		t.Fatalf("target >300ft away should not fuse")
	}

	// Exceeds track delta (40 deg) -> no fusion.
	_, matched = idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5002, 8.5000, 5000, 35, 110, true, true, now, existing,
	)
	if matched {
		t.Fatalf("target with >25deg track delta should not fuse")
	}

	// Exceeds speed delta (25 kt) -> no fusion.
	_, matched = idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5002, 8.5000, 5000, 355, 135, true, true, now, existing,
	)
	if matched {
		t.Fatalf("target with >15kt speed delta should not fuse")
	}

	// Existing target stale (6s >= 5s) -> no fusion.
	stale := existing[icaoKey]
	stale.Last_seen = now.Add(-6 * time.Second)
	existing[icaoKey] = stale
	_, matched = idx.ResolveCanonicalKeyWithContext(
		0xDD7777, 1, TRAFFIC_SOURCE_OGN,
		47.5002, 8.5000, 5000, 355, 110, true, true, now, existing,
	)
	if matched {
		t.Fatalf("stale target (>=5s) should not fuse spatially")
	}
}

func TestShouldAcceptPositionUpdate(t *testing.T) {
	now := time.Now()
	freshADSB := TrafficInfo{
		Position_valid: true,
		Last_source:    TRAFFIC_SOURCE_1090ES,
		TargetType:     TARGET_TYPE_ADSB,
		Last_seen:      now.Add(-2 * time.Second),
	}

	// Lower-priority internet APRS rejected while direct RF ADS-B is fresh (< 6s).
	if ShouldAcceptPositionUpdate(freshADSB, TRAFFIC_SOURCE_OGN, 0, true, now) {
		t.Errorf("APRS should be rejected when fresh ADS-B exists")
	}
	// Lower-priority TIS-B rejected while direct RF ADS-B is fresh (< 6s).
	if ShouldAcceptPositionUpdate(freshADSB, TRAFFIC_SOURCE_UAT, TARGET_TYPE_TISB, false, now) {
		t.Errorf("TIS-B should be rejected when fresh ADS-B exists")
	}
	// Equal-priority UAT ADS-B accepted.
	if !ShouldAcceptPositionUpdate(freshADSB, TRAFFIC_SOURCE_UAT, TARGET_TYPE_ADSB, false, now) {
		t.Errorf("equal-priority UAT ADS-B should be accepted")
	}

	// Stale ADS-B (>= 6s) allows lower-priority fallback.
	staleADSB := freshADSB
	staleADSB.Last_seen = now.Add(-7 * time.Second)
	if !ShouldAcceptPositionUpdate(staleADSB, TRAFFIC_SOURCE_UAT, TARGET_TYPE_TISB, false, now) {
		t.Errorf("TIS-B fallback should be accepted when ADS-B is stale (>=6s)")
	}

	// Extrapolated existing position always accepts real update.
	extrap := freshADSB
	extrap.ExtrapolatedPosition = true
	if !ShouldAcceptPositionUpdate(extrap, TRAFFIC_SOURCE_OGN, 0, true, now) {
		t.Errorf("real update should replace extrapolated position")
	}
}

func TestMergeAltitudePreventsPingPong(t *testing.T) {
	t0 := time.Now()
	ti := &TrafficInfo{}

	// 1. Baro arrives first: 5000 ft.
	MergeAltitude(ti, 5000, false, TRAFFIC_SOURCE_1090ES, t0)
	if ti.Alt != 5000 || ti.AltIsGNSS {
		t.Fatalf("after baro: Alt=%d AltIsGNSS=%v, want 5000/false", ti.Alt, ti.AltIsGNSS)
	}

	// 2. GNSS arrives 3s later: 5175 ft -> must NOT overwrite baro Alt, must record +175 offset.
	MergeAltitude(ti, 5175, true, TRAFFIC_SOURCE_OGN, t0.Add(3*time.Second))
	if ti.Alt != 5000 || ti.AltIsGNSS {
		t.Fatalf("GNSS overwrote fresh baro: Alt=%d AltIsGNSS=%v, want 5000/false", ti.Alt, ti.AltIsGNSS)
	}
	if ti.GnssDiffFromBaroAlt != 175 {
		t.Fatalf("GnssDiffFromBaroAlt=%d, want 175", ti.GnssDiffFromBaroAlt)
	}

	// 3. Baro disappears for 12s (> 10s), GNSS arrives: falls back to GNSS.
	MergeAltitude(ti, 5200, true, TRAFFIC_SOURCE_OGN, t0.Add(12*time.Second))
	if ti.Alt != 5200 || !ti.AltIsGNSS {
		t.Fatalf("expected fallback to GNSS after >10s: Alt=%d AltIsGNSS=%v", ti.Alt, ti.AltIsGNSS)
	}

	// 4. Baro returns 4s later at 5025 ft -> switches back to baro and computes offset (+175).
	MergeAltitude(ti, 5025, false, TRAFFIC_SOURCE_1090ES, t0.Add(16*time.Second))
	if ti.Alt != 5025 || ti.AltIsGNSS {
		t.Fatalf("expected baro priority: Alt=%d AltIsGNSS=%v", ti.Alt, ti.AltIsGNSS)
	}
	if ti.GnssDiffFromBaroAlt != 175 {
		t.Fatalf("GnssDiffFromBaroAlt=%d, want 175", ti.GnssDiffFromBaroAlt)
	}
}
