package main

import (
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"
)

func buildODIDBasicID(idType, uaType uint8, uasID string) []byte {
	msg := make([]byte, ODIDMessageSize)
	msg[0] = (ODIDMsgTypeBasicID << 4) | 0x02 // proto v2
	msg[1] = ((idType & 0x0F) << 4) | (uaType & 0x0F)
	copy(msg[2:22], []byte(uasID))
	return msg
}

func buildODIDLocation(opStatus uint8, trackDeg, speedMps, vvelMps, lat, lon, altBaroM, altGeodM, heightM float64) []byte {
	msg := make([]byte, ODIDMessageSize)
	msg[0] = (ODIDMsgTypeLocation << 4) | 0x02

	var ewBit byte
	dirVal := math.Mod(trackDeg, 360.0)
	if dirVal < 0 {
		dirVal += 360.0
	}
	if dirVal >= 180.0 {
		ewBit = 1
		dirVal -= 180.0
	}

	var multBit byte
	var rawSpeed byte
	if speedMps <= 254*0.25 {
		multBit = 0
		rawSpeed = byte(math.Round(speedMps / 0.25))
	} else {
		multBit = 1
		rawSpeed = byte(math.Round((speedMps - 255*0.25) / 0.75))
	}

	msg[1] = ((opStatus & 0x0F) << 4) | (1 << 2) | (ewBit << 1) | multBit
	msg[2] = byte(math.Round(dirVal))
	msg[3] = rawSpeed
	msg[4] = byte(int8(math.Round(vvelMps / 0.5)))

	binary.LittleEndian.PutUint32(msg[5:9], uint32(int32(math.Round(lat*1e7))))
	binary.LittleEndian.PutUint32(msg[9:13], uint32(int32(math.Round(lon*1e7))))

	encodeAlt := func(m float64) uint16 {
		if math.IsNaN(m) {
			return ODIDAltInvalidRaw
		}
		return uint16(math.Round((m + 1000.0) * 2.0))
	}
	binary.LittleEndian.PutUint16(msg[13:15], encodeAlt(altBaroM))
	binary.LittleEndian.PutUint16(msg[15:17], encodeAlt(altGeodM))
	binary.LittleEndian.PutUint16(msg[17:19], encodeAlt(heightM))
	return msg
}

func buildODIDSystem(opLat, opLon float64) []byte {
	msg := make([]byte, ODIDMessageSize)
	msg[0] = (ODIDMsgTypeSystem << 4) | 0x02
	binary.LittleEndian.PutUint32(msg[2:6], uint32(int32(math.Round(opLat*1e7))))
	binary.LittleEndian.PutUint32(msg[6:10], uint32(int32(math.Round(opLon*1e7))))
	return msg
}

func buildODIDOperatorID(opID string) []byte {
	msg := make([]byte, ODIDMessageSize)
	msg[0] = (ODIDMsgTypeOperatorID << 4) | 0x02
	copy(msg[2:22], []byte(opID))
	return msg
}

func TestParseOpenDroneIDSingleMessages(t *testing.T) {
	// Basic ID
	basic := buildODIDBasicID(1, 2, "1581F45T211234567890")
	tgt, err := ParseOpenDroneIDFrame(basic)
	if err != nil {
		t.Fatalf("BasicID parse failed: %v", err)
	}
	if !tgt.HasBasicID || tgt.IDType != 1 || tgt.UAType != 2 || tgt.UASID != "1581F45T211234567890" {
		t.Errorf("BasicID mismatch: %+v", tgt)
	}

	// Location/Vector (West track 275 deg, high-speed multiplier 75 m/s, climb +2.5 m/s)
	loc := buildODIDLocation(2, 275, 75.0, 2.5, 37.6188056, -122.3754167, 120.0, 135.0, 50.0)
	tgt, err = ParseOpenDroneIDFrame(loc)
	if err != nil {
		t.Fatalf("Location parse failed: %v", err)
	}
	if !tgt.HasLocation || !tgt.HasBaroAlt || !tgt.HasGeodAlt || !tgt.HasHeightAGL {
		t.Fatalf("expected location and altitude flags set: %+v", tgt)
	}
	if math.Abs(tgt.Lat-37.6188056) > 1e-6 || math.Abs(tgt.Lon-(-122.3754167)) > 1e-6 {
		t.Errorf("lat/lon mismatch: %f, %f", tgt.Lat, tgt.Lon)
	}
	if math.Abs(tgt.TrackDeg-275.0) > 1.0 {
		t.Errorf("TrackDeg = %.1f, want 275", tgt.TrackDeg)
	}
	wantKt := 75.0 * odidMpsToKnots
	if math.Abs(tgt.SpeedKt-wantKt) > 2.0 {
		t.Errorf("SpeedKt = %.2f, want ~%.2f", tgt.SpeedKt, wantKt)
	}
	wantFpm := 2.5 * odidMpsToFpm
	if math.Abs(tgt.VvelFpm-wantFpm) > 5.0 {
		t.Errorf("VvelFpm = %.1f, want ~%.1f", tgt.VvelFpm, wantFpm)
	}
	if math.Abs(tgt.AltBaroFt-120.0*odidMetersToFeet) > 2.0 {
		t.Errorf("AltBaroFt = %.1f, want ~%.1f", tgt.AltBaroFt, 120.0*odidMetersToFeet)
	}

	// System
	sys := buildODIDSystem(37.615, -122.370)
	tgt, err = ParseOpenDroneIDFrame(sys)
	if err != nil {
		t.Fatalf("System parse failed: %v", err)
	}
	if !tgt.HasSystem || math.Abs(tgt.OperatorLat-37.615) > 1e-6 || math.Abs(tgt.OperatorLon-(-122.370)) > 1e-6 {
		t.Errorf("System mismatch: %+v", tgt)
	}

	// Operator ID
	op := buildODIDOperatorID("FA3X9K2M11")
	tgt, err = ParseOpenDroneIDFrame(op)
	if err != nil {
		t.Fatalf("OperatorID parse failed: %v", err)
	}
	if !tgt.HasOperatorID || tgt.OperatorID != "FA3X9K2M11" {
		t.Errorf("OperatorID mismatch: %+v", tgt)
	}
}

func TestParseOpenDroneIDMessagePackAndBLEAdv(t *testing.T) {
	basic := buildODIDBasicID(1, 2, "DJI-MAVIC-01")
	loc := buildODIDLocation(2, 90, 15.0, -1.0, 48.1351, 11.5820, math.NaN(), 550.0, 40.0)
	sys := buildODIDSystem(48.1340, 11.5810)
	op := buildODIDOperatorID("DEU-OP-99123")

	pack := []byte{(ODIDMsgTypeMessagePack << 4) | 0x02, ODIDMessageSize, 4}
	pack = append(pack, basic...)
	pack = append(pack, loc...)
	pack = append(pack, sys...)
	pack = append(pack, op...)

	// Wrap in BLE Service Data: AppCode=0x0D, Counter=0x07, payload=pack
	bleAdv := append([]byte{ODIDBLEAppCode, 0x07}, pack...)

	tgt, err := ParseOpenDroneIDBLEAdv(bleAdv)
	if err != nil {
		t.Fatalf("BLE MessagePack parse failed: %v", err)
	}
	if tgt.UASID != "DJI-MAVIC-01" || !tgt.HasLocation || tgt.HasBaroAlt || !tgt.HasGeodAlt || tgt.OperatorID != "DEU-OP-99123" {
		t.Errorf("unexpected merged pack target: %+v", tgt)
	}
	if math.Abs(tgt.OperatorLat-48.1340) > 1e-6 {
		t.Errorf("OperatorLat = %f, want 48.1340", tgt.OperatorLat)
	}
}

func TestParseOpenDroneIDMalformedNeverPanics(t *testing.T) {
	badFrames := [][]byte{
		nil,
		{},
		{0x00, 0x01, 0x02},
		{(ODIDMsgTypeMessagePack << 4), 25},     // short header
		{(ODIDMsgTypeMessagePack << 4), 25, 0},  // zero count
		{(ODIDMsgTypeMessagePack << 4), 25, 11}, // count > max
		{(ODIDMsgTypeMessagePack << 4), 24, 1},  // bad item size
		{(ODIDMsgTypeMessagePack << 4), 25, 5, 0, 0, 0, 0}, // truncated pack
		append([]byte{(ODIDMsgTypeMessagePack << 4), 25, 1}, // nested pack
			append([]byte{(ODIDMsgTypeMessagePack << 4), 25, 1}, make([]byte, 22)...)...),
		append([]byte{0xE0}, make([]byte, 24)...), // unsupported msg type
	}
	for i, f := range badFrames {
		if _, err := ParseOpenDroneIDFrame(f); err == nil {
			t.Errorf("case %d: expected error for malformed frame", i)
		}
	}

	// Bad BLE app code and truncated BLE adv.
	badBLE := append([]byte{0x0E, 0x00}, buildODIDBasicID(1, 2, "TEST")...)
	if _, err := ParseOpenDroneIDBLEAdv(badBLE); err == nil {
		t.Errorf("expected error for wrong BLE app code")
	}
	if _, err := ParseOpenDroneIDBLEAdv([]byte{ODIDBLEAppCode, 0x01, 0x00}); err == nil {
		t.Errorf("expected error for short BLE adv")
	}
}

func TestRemoteIDState_MergePublishAndExpire(t *testing.T) {
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
	trafficMutex = &sync.Mutex{}
	traffic = make(map[uint32]TrafficInfo)
	seenTraffic = make(map[uint32]bool)

	state := NewRemoteIDState()
	t0 := time.Now().UTC()
	mac := "AA:BB:CC:DD:EE:FF"

	// Frame 1: Basic ID only (no location yet -> should not publish to traffic map).
	basicTgt, err := ParseOpenDroneIDFrame(buildODIDBasicID(1, 2, "1581F45T2100001"))
	if err != nil {
		t.Fatalf("basic parse failed: %v", err)
	}
	if _, _, ok := state.IngestAndPublish(mac, basicTgt, -72, t0); ok {
		t.Fatalf("BasicID-only frame should not publish TrafficInfo without location")
	}

	// Frame 2: Location/Vector from same MAC -> merges with Basic ID and publishes!
	locTgt, err := ParseOpenDroneIDFrame(buildODIDLocation(2, 180, 10.0, 1.0, 47.45, 8.55, 450.0, 460.0, 30.0))
	if err != nil {
		t.Fatalf("location parse failed: %v", err)
	}
	key, ti, ok := state.IngestAndPublish(mac, locTgt, -68, t0.Add(2*time.Second))
	if !ok {
		t.Fatalf("expected valid TrafficInfo after Location frame merged")
	}
	if ti.Emitter_category != 14 {
		t.Errorf("Emitter_category = %d, want 14 (UAV)", ti.Emitter_category)
	}
	if ti.TargetType != TARGET_TYPE_REMOTEID || ti.Last_source != TRAFFIC_SOURCE_REMOTEID {
		t.Errorf("TargetType=%d Last_source=%d, want %d/%d", ti.TargetType, ti.Last_source, TARGET_TYPE_REMOTEID, TRAFFIC_SOURCE_REMOTEID)
	}
	if ti.Tail != "1581F45T" {
		t.Errorf("Tail = %q, want truncated 8-char UAS ID '1581F45T'", ti.Tail)
	}
	if !ti.Position_valid || ti.AltIsGNSS {
		t.Errorf("expected Position_valid=true and baro AltIsGNSS=false, got %v/%v", ti.Position_valid, ti.AltIsGNSS)
	}

	trafficMutex.Lock()
	stored, exists := traffic[key]
	trafficMutex.Unlock()
	if !exists || stored.Tail != "1581F45T" {
		t.Fatalf("target not stored in global traffic map under key 0x%X", key)
	}

	// Expiry check: after 31s target expires.
	if expired := state.Expire(t0.Add(20 * time.Second)); len(expired) != 0 {
		t.Errorf("target expired too early: %v", expired)
	}
	if expired := state.Expire(t0.Add(33 * time.Second)); len(expired) != 1 || expired[0] != mac {
		t.Errorf("expected target %s to expire at +31s, got %v", mac, expired)
	}
	if len(state.Snapshot()) != 0 {
		t.Errorf("expected empty snapshot after expiry")
	}
}
