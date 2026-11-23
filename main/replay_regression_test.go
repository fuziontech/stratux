package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/common"
)

func initReplayTestEnv() {
	initTestGlobals()
	crcInit()

	if weatherUpdate == nil {
		weatherUpdate = NewUIBroadcaster()
	}
	if trafficUpdate == nil {
		trafficUpdate = NewUIBroadcaster()
	}
	if radarUpdate == nil {
		radarUpdate = NewUIBroadcaster()
	}
	if situationUpdate == nil {
		situationUpdate = NewUIBroadcaster()
	}
	if weatherRawUpdate == nil {
		weatherRawUpdate = NewUIBroadcaster()
	}
	if gdl90Update == nil {
		gdl90Update = NewUIBroadcaster()
	}
	if netMutex == nil {
		netMutex = &sync.Mutex{}
	}
	if clientConnections == nil {
		clientConnections = make(map[string]connection)
	}
	if networkGDL90Chan == nil {
		networkGDL90Chan = make(chan []byte, 256)
	}
	if systemErrs == nil {
		systemErrs = make(map[string]string)
	}

	globalSettingsMutex.Lock()
	globalSettings.CPA_Alerts_Enabled = true
	globalSettings.UAT_Enabled = true
	globalSettings.ES_Enabled = true
	globalSettings.OGN_Enabled = true
	globalSettings.APRS_Enabled = true
	globalSettings.RemoteID_Enabled = true
	globalSettings.AutoGain_Enabled = true
	globalSettings.GPS_Enabled = true
	globalSettingsMutex.Unlock()
}

// verifyGDL90Frame validates flag delimiters (0x7E), control-escape unescaping (0x7D),
// CRC-16 integrity, and the expected GDL90 message ID.
func verifyGDL90Frame(t *testing.T, frame []byte, expectedMsgID byte) []byte {
	t.Helper()
	if len(frame) < 5 {
		t.Fatalf("GDL90 frame too short: len=%d", len(frame))
	}
	if frame[0] != 0x7E || frame[len(frame)-1] != 0x7E {
		t.Fatalf("GDL90 frame missing 0x7E flags: start=0x%02X end=0x%02X", frame[0], frame[len(frame)-1])
	}

	unescaped := make([]byte, 0, len(frame)-2)
	body := frame[1 : len(frame)-1]
	for i := 0; i < len(body); i++ {
		b := body[i]
		if b == 0x7E {
			t.Fatalf("GDL90 frame contains unescaped 0x7E at index %d", i)
		}
		if b == 0x7D {
			i++
			if i >= len(body) {
				t.Fatalf("GDL90 frame ends with dangling 0x7D escape")
			}
			unescaped = append(unescaped, body[i]^0x20)
		} else {
			unescaped = append(unescaped, b)
		}
	}

	if len(unescaped) < 3 {
		t.Fatalf("GDL90 unescaped frame too short: len=%d", len(unescaped))
	}
	payload := unescaped[:len(unescaped)-2]
	gotCRC := uint16(unescaped[len(unescaped)-2]) | uint16(unescaped[len(unescaped)-1])<<8
	wantCRC := crcCompute(payload)
	if gotCRC != wantCRC {
		t.Fatalf("GDL90 CRC-16 mismatch: got=0x%04X want=0x%04X (msgID=0x%02X)", gotCRC, wantCRC, payload[0])
	}
	if payload[0] != expectedMsgID {
		t.Fatalf("GDL90 message ID mismatch: got=0x%02X want=0x%02X", payload[0], expectedMsgID)
	}
	return payload
}

// verifyNMEASentence validates $...*XX\r\n framing and XOR checksum.
func verifyNMEASentence(t *testing.T, sentence, expectedPrefix string) {
	t.Helper()
	if !strings.HasPrefix(sentence, expectedPrefix) {
		t.Fatalf("NMEA sentence %q missing prefix %q", sentence, expectedPrefix)
	}
	if !strings.HasSuffix(sentence, "\r\n") {
		t.Fatalf("NMEA sentence %q missing CRLF terminator", sentence)
	}
	trimmed := strings.TrimSuffix(sentence, "\r\n")
	starIdx := strings.LastIndexByte(trimmed, '*')
	if starIdx < 1 || starIdx+3 != len(trimmed) {
		t.Fatalf("NMEA sentence %q missing valid *XX checksum suffix", sentence)
	}
	var xor byte
	for i := 1; i < starIdx; i++ {
		xor ^= trimmed[i]
	}
	wantCS := fmt.Sprintf("%02X", xor)
	gotCS := strings.ToUpper(trimmed[starIdx+1:])
	if gotCS != wantCS {
		t.Fatalf("NMEA checksum mismatch for %q: got=%s want=%s", sentence, gotCS, wantCS)
	}
	if _, ok := validateNMEAChecksum(trimmed); !ok {
		t.Fatalf("validateNMEAChecksum rejected %q", trimmed)
	}
}

func buildTestOpenDroneIDPack(uasID string, lat, lon, altBaroM, altGeodM, heightAGLM, speedMps, trackDeg, vvelMps float64) []byte {
	// Message 0: Basic ID (25 bytes)
	basicID := make([]byte, ODIDMessageSize)
	basicID[0] = (ODIDMsgTypeBasicID << 4) | 0x02
	basicID[1] = (1 << 4) | 2 // IDType=1 (Serial Number), UAType=2 (Helicopter/Multirotor)
	copy(basicID[2:22], []byte(uasID))

	// Message 1: Location/Vector (25 bytes)
	loc := make([]byte, ODIDMessageSize)
	loc[0] = (ODIDMsgTypeLocation << 4) | 0x02

	normTrack := math.Mod(trackDeg, 360.0)
	if normTrack < 0 {
		normTrack += 360.0
	}
	var ewDir byte
	var rawDir byte
	if normTrack >= 180.0 {
		ewDir = 1
		rawDir = byte(math.Round(normTrack - 180.0))
	} else {
		ewDir = 0
		rawDir = byte(math.Round(normTrack))
	}
	loc[1] = (2 << 4) | (ewDir << 1) // OpStatus=2 (Airborne), speedMult=0
	loc[2] = rawDir
	loc[3] = byte(math.Round(speedMps / 0.25))
	loc[4] = byte(int8(math.Round(vvelMps / 0.5)))

	binary.LittleEndian.PutUint32(loc[5:9], uint32(int32(math.Round(lat*1e7))))
	binary.LittleEndian.PutUint32(loc[9:13], uint32(int32(math.Round(lon*1e7))))
	binary.LittleEndian.PutUint16(loc[13:15], uint16(math.Round((altBaroM+1000.0)*2.0)))
	binary.LittleEndian.PutUint16(loc[15:17], uint16(math.Round((altGeodM+1000.0)*2.0)))
	binary.LittleEndian.PutUint16(loc[17:19], uint16(math.Round((heightAGLM+1000.0)*2.0)))

	// Message Pack (0xF): 3-byte header + 2 * 25-byte messages
	pack := make([]byte, 0, 3+2*ODIDMessageSize)
	pack = append(pack, (ODIDMsgTypeMessagePack<<4)|0x02, ODIDMessageSize, 2)
	pack = append(pack, basicID...)
	pack = append(pack, loc...)
	return pack
}

func TestMultiBandReplayRegression(t *testing.T) {
	initReplayTestEnv()

	// Measure baseline goroutines after one-time broadcaster initialization.
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	const (
		ownLat      = 37.5000
		ownLon      = -122.2500
		ownAltFt    = 3000.0
		ownTrackDeg = 90.0
		ownSpeedKt  = 100.0
	)

	// 1. Ownship GPS NMEA Replay
	t.Run("OwnshipGPSNMEAReplay", func(t *testing.T) {
		globalStatusMutex.Lock()
		globalStatus.GPS_connected = true
		globalStatusMutex.Unlock()

		// 37.5000N = 3730.0000,N; 122.2500W = 12215.0000,W; 3000 ft MSL = 914.4 m.
		gga := appendNmeaChecksum("$GPGGA,120000.00,3730.0000,N,12215.0000,W,2,10,0.8,914.4,M,-30.0,M,,")
		rmc := appendNmeaChecksum("$GPRMC,120000.00,A,3730.0000,N,12215.0000,W,100.0,090.0,231126,,,A")

		if !processNMEALineLow(gga, true) {
			t.Fatalf("processNMEALineLow rejected valid GPGGA: %s", gga)
		}
		if !processNMEALineLow(rmc, true) {
			t.Fatalf("processNMEALineLow rejected valid GPRMC: %s", rmc)
		}

		// Provide realistic horizontal accuracy so isGPSGroundTrackValid() is true.
		mySituation.muGPS.Lock()
		mySituation.GPSHorizontalAccuracy = 5.0
		mySituation.GPSNACp = 10
		lat := mySituation.GPSLatitude
		lon := mySituation.GPSLongitude
		alt := mySituation.GPSAltitudeMSL
		trk := mySituation.GPSTrueCourse
		spd := mySituation.GPSGroundSpeed
		mySituation.muGPS.Unlock()

		if !isGPSValid() {
			t.Fatalf("expected isGPSValid() == true after GPGGA+GPRMC replay")
		}
		if !isGPSGroundTrackValid() {
			t.Fatalf("expected isGPSGroundTrackValid() == true after GPRMC replay")
		}
		if math.Abs(float64(lat)-ownLat) > 0.001 {
			t.Errorf("GPSLatitude = %f, want ~%f", lat, ownLat)
		}
		if math.Abs(float64(lon)-ownLon) > 0.001 {
			t.Errorf("GPSLongitude = %f, want ~%f", lon, ownLon)
		}
		if math.Abs(float64(alt)-ownAltFt) > 5.0 {
			t.Errorf("GPSAltitudeMSL = %f, want ~%f", alt, ownAltFt)
		}
		if math.Abs(float64(trk)-ownTrackDeg) > 0.5 {
			t.Errorf("GPSTrueCourse = %f, want ~%f", trk, ownTrackDeg)
		}
		if math.Abs(spd-ownSpeedKt) > 0.5 {
			t.Errorf("GPSGroundSpeed = %f, want ~%f", spd, ownSpeedKt)
		}
	})

	// 2. 978 UAT Replay (../test-data/example.dump978)
	t.Run("UAT978Replay", func(t *testing.T) {
		f, err := os.Open(filepath.Join("..", "test-data", "example.dump978"))
		if err != nil {
			t.Fatalf("failed to open ../test-data/example.dump978: %v", err)
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		// Increase buffer in case lines are long.
		scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)

		const maxFrames = 500
		linesRead := 0
		decodedFrames := 0
		for scanner.Scan() && linesRead < maxFrames {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			linesRead++
			frame, msgType := parseInput(line)
			if len(frame) > 0 && msgType != 0 {
				decodedFrames++
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("error scanning example.dump978: %v", err)
		}
		if decodedFrames == 0 {
			t.Fatalf("expected >0 decoded UAT frames from example.dump978, got 0 (lines=%d)", linesRead)
		}

		// Verify graphical weather snapshot marshals to valid JSON with zero NaN/Inf.
		products := getGraphicalProductsSnapshot()
		if products == nil {
			t.Errorf("getGraphicalProductsSnapshot() returned nil, want non-nil slice")
		}
		b, err := json.Marshal(products)
		if err != nil {
			t.Fatalf("json.Marshal(getGraphicalProductsSnapshot()) failed: %v", err)
		}
		if !json.Valid(b) {
			t.Fatalf("graphical products JSON is invalid: %s", string(b))
		}
	})

	// 3. Cross-Source Traffic Fusion Replay
	t.Run("CrossSourceTrafficFusionReplay", func(t *testing.T) {
		// Refresh ownship GPS timestamp so isGPSValid() remains true.
		mySituation.muGPS.Lock()
		mySituation.GPSLastFixLocalTime = stratuxClock.GetTime()
		mySituation.GPSLastGroundTrackTime = stratuxClock.GetTime()
		mySituation.GPSFixQuality = 2
		mySituation.muGPS.Unlock()

		// Clear traffic map for deterministic fusion assertion.
		trafficMutex.Lock()
		traffic = make(map[uint32]TrafficInfo)
		trafficMutex.Unlock()

		// Register DDB alias (FLARM 0xDDAABB -> ICAO 0xA1B2C3) via JSON loader too.
		ddbPayload := []byte(`{"devices":[
			{"device_type":"I","device_id":"A1B2C3","registration":"N123AB"},
			{"device_type":"F","device_id":"DDAABB","registration":"N123AB"}
		]}`)
		if n := globalTrafficFusion.LoadDDBAliasesFromJSON(ddbPayload); n != 1 {
			t.Fatalf("LoadDDBAliasesFromJSON registered %d aliases, want 1", n)
		}
		if mapped, ok := globalTrafficFusion.LookupICAO(0xDDAABB); !ok || mapped != 0xA1B2C3 {
			t.Fatalf("LookupICAO(0xDDAABB) = (0x%X, %v), want (0xA1B2C3, true)", mapped, ok)
		}

		// Step A: Feed live direct 1090ES ADS-B target (0xA1B2C3) at 37.5200, -122.2000, 3500 ft.
		const (
			adsbLat = 37.5200
			adsbLng = -122.2000
		)
		esJSON := fmt.Sprintf(
			`{"Icao_addr":%d,"DF":17,"CA":5,"TypeCode":11,"SubtypeCode":0,"SignalLevel":0.25,"Tail":"N123AB","Emitter_category":1,"OnGround":false,"Lat":%.4f,"Lng":%.4f,"Position_valid":true,"NACp":9,"Alt":3500,"AltIsGNSS":false,"Speed_valid":true,"Speed":120,"Track":180,"Vvel":0,"Timestamp":"%s"}`,
			0xA1B2C3, adsbLat, adsbLng, time.Now().UTC().Format(time.RFC3339Nano),
		)
		parseDump1090Message(esJSON)

		// Step B: Feed the same aircraft via FLARM PFLAA using its aliased FLARM ID DDAABB.
		// Position is ~2222m N, ~4418m E, +152m vertical relative to ownship (37.5000, -122.2500, 3000 ft).
		pflaa := appendNmeaChecksum("$PFLAA,0,2222,4418,152,2,DDAABB,180,,62,0.0,8")
		if !processNMEALineLow(pflaa, true) {
			t.Fatalf("processNMEALineLow rejected valid PFLAA: %s", pflaa)
		}

		// Step C: Feed a delayed/offset internet APRS packet (FLRA1B2C3) at 37°40'N, 122°30'W (far from 37.52, -122.20).
		aprsPkt := "FLRA1B2C3>APRS,qAS,TEST:/120000h3740.00N/12230.00W'180/120/A=003500 !W00! id05A1B2C3"
		parseAprsMessage(aprsPkt, true)

		trafficMutex.Lock()
		numTargets := len(traffic)
		canonical, hasCanonical := traffic[0xA1B2C3]
		trafficMutex.Unlock()

		if numTargets != 1 {
			t.Fatalf("expected 1 canonical fused target in traffic map, got %d", numTargets)
		}
		if !hasCanonical {
			t.Fatalf("expected canonical target under ICAO key 0xA1B2C3")
		}
		if math.Abs(float64(canonical.Lat)-adsbLat) > 0.005 || math.Abs(float64(canonical.Lng)-adsbLng) > 0.005 {
			t.Errorf("stale APRS overwrote live 1090ES position: got (%.4f, %.4f), want ~(%.4f, %.4f)",
				canonical.Lat, canonical.Lng, adsbLat, adsbLng)
		}
	})

	// 4. 3D CPA Convergence & Alerting + GDL90/FLARM Output Validation
	t.Run("CPAConvergenceAndProtocolOutput", func(t *testing.T) {
		// Refresh ownship fix so makeOwnshipReport() and FLARM helpers see valid GPS.
		mySituation.muGPS.Lock()
		mySituation.GPSLatitude = ownLat
		mySituation.GPSLongitude = ownLon
		mySituation.GPSAltitudeMSL = ownAltFt
		mySituation.GPSHeightAboveEllipsoid = ownAltFt - 98.4
		mySituation.GPSTrueCourse = ownTrackDeg
		mySituation.GPSGroundSpeed = ownSpeedKt
		mySituation.GPSFixQuality = 2
		mySituation.GPSHorizontalAccuracy = 5.0
		mySituation.GPSNACp = 10
		mySituation.GPSLastFixLocalTime = stratuxClock.GetTime()
		mySituation.GPSLastGroundTrackTime = stratuxClock.GetTime()
		mySituation.muGPS.Unlock()

		// Head-on converging aircraft ahead on 090° bearing at 3000 ft, 270° track, 100 kt.
		// At 1.25 NM (~2315 m) with 200 kt closing speed (~102.9 m/s), t_cpa ≈ 22.5s (Warning <= 25s).
		// Also test 1.5 NM (~2778 m) at 120 kt target (closing 220 kt, t_cpa ≈ 24.5s -> Warning).
		lonPerMeter := 1.0 / (111120.0 * math.Cos(common.Radians(ownLat)))
		target15NMLon := ownLon + (1.5*1852.0)*lonPerMeter
		target12NMLon := ownLon + (1.2*1852.0)*lonPerMeter

		headOnTarget := TrafficInfo{
			Icao_addr:        0xBEEF01,
			Addr_type:        0,
			TargetType:       TARGET_TYPE_ADSB,
			Last_source:      TRAFFIC_SOURCE_1090ES,
			Tail:             "N999CP",
			Emitter_category: 1,
			OnGround:         false,
			Position_valid:   true,
			Lat:              float32(ownLat),
			Lng:              float32(target12NMLon),
			Alt:              3000,
			Track:            270,
			Speed:            100,
			Speed_valid:      true,
			Vvel:             0,
			Last_seen:        stratuxClock.GetTime(),
			Age:              0.2,
		}
		cfg := DefaultCPAConfig()
		cpaRes := ComputeCPA(ownLat, ownLon, ownAltFt, ownTrackDeg, ownSpeedKt, 0, false, headOnTarget, cfg)
		if !cpaRes.Valid {
			t.Fatalf("expected valid CPA result for head-on converging aircraft")
		}
		if cpaRes.AlertLevel < CPAAlertWarning {
			t.Errorf("head-on converging AlertLevel = %d, want >= %d (CPAAlertWarning); tCPA=%.1fs horiz=%.0fm vert=%.0fft",
				cpaRes.AlertLevel, CPAAlertWarning, cpaRes.TimeToCPASec, cpaRes.HorizMissMeters, cpaRes.VertMissFeet)
		}
		if strings.TrimSpace(cpaRes.Callout) == "" {
			t.Errorf("expected non-empty spoken Callout for converging threat")
		}

		// Verify 1.5 NM head-on at 100 kt produces at least an advisory with non-empty callout
		// (t_cpa = 27.0s, horiz miss ~0m, vert miss 0ft).
		headOn15 := headOnTarget
		headOn15.Lng = float32(target15NMLon)
		cpa15 := ComputeCPA(ownLat, ownLon, ownAltFt, ownTrackDeg, ownSpeedKt, 0, false, headOn15, cfg)
		if !cpa15.Valid || cpa15.AlertLevel == CPAAlertNone || strings.TrimSpace(cpa15.Callout) == "" {
			t.Errorf("1.5 NM head-on target expected valid alert+callout, got %+v", cpa15)
		}

		// Verify a taxi/ground target gets AlertLevel == 0.
		groundTarget := headOnTarget
		groundTarget.Icao_addr = 0xBEEF02
		groundTarget.OnGround = true
		groundCPA := ComputeCPA(ownLat, ownLon, ownAltFt, ownTrackDeg, ownSpeedKt, 0, false, groundTarget, cfg)
		if groundCPA.AlertLevel != CPAAlertNone {
			t.Errorf("ground/taxi target AlertLevel = %d, want 0 (CPAAlertNone)", groundCPA.AlertLevel)
		}

		// Verify SelectHighestThreat picks the converging airborne threat over the ground target.
		bestKey, bestRes, hasThreat := SelectHighestThreat(map[uint32]CPAResult{
			headOnTarget.Icao_addr: cpaRes,
			groundTarget.Icao_addr: groundCPA,
		})
		if !hasThreat || bestKey != headOnTarget.Icao_addr || bestRes.AlertLevel < CPAAlertWarning {
			t.Errorf("SelectHighestThreat() = (icao=0x%X, level=%d, ok=%v), want 0x%X >= Warning",
				bestKey, bestRes.AlertLevel, hasThreat, headOnTarget.Icao_addr)
		}
		trafficMutex.Lock()
		traffic[headOnTarget.Icao_addr] = headOnTarget
		trafficMutex.Unlock()

		// Drain networkGDL90Chan before testing makeOwnshipReport().
		for len(networkGDL90Chan) > 0 {
			<-networkGDL90Chan
		}
		if !makeOwnshipReport() {
			t.Fatalf("makeOwnshipReport() returned false with valid ownship GPS")
		}
		select {
		case ownFrame := <-networkGDL90Chan:
			verifyGDL90Frame(t, ownFrame, 0x0A)
		default:
			t.Fatalf("makeOwnshipReport() did not emit GDL90 frame to networkGDL90Chan")
		}

		// Validate GDL90 Traffic Report, Heartbeat, and Stratux Status frames.
		verifyGDL90Frame(t, makeTrafficReportMsg(headOnTarget), 0x14)
		verifyGDL90Frame(t, makeHeartbeat(), 0x00)
		verifyGDL90Frame(t, makeStratuxStatus(), 'S')

		// Validate FLARM PFLAA sentence framing and checksum.
		pflaaOut, valid, _ := makeFlarmPFLAAString(headOnTarget)
		if !valid {
			t.Fatalf("makeFlarmPFLAAString returned valid=false")
		}
		verifyNMEASentence(t, pflaaOut, "$PFLAA,")
	})

	// 5. Drone Remote ID (ASTM F3411) Replay
	t.Run("RemoteIDASTM3411Replay", func(t *testing.T) {
		pack := buildTestOpenDroneIDPack(
			"UAS12345",
			37.5050, -122.2450,
			150.0, 155.0, 120.0,
			10.0, 90.0, 1.0,
		)

		// Verify direct frame decoding and BLE 0xFFFA Service Data envelope decoding.
		parsed, err := ParseOpenDroneIDFrame(pack)
		if err != nil {
			t.Fatalf("ParseOpenDroneIDFrame failed: %v", err)
		}
		bleAdv := append([]byte{ODIDBLEAppCode, 0x01}, pack...)
		if _, err := ParseOpenDroneIDBLEAdv(bleAdv); err != nil {
			t.Fatalf("ParseOpenDroneIDBLEAdv failed: %v", err)
		}

		key, ti, ok := globalRemoteIDState.IngestAndPublish("AA:BB:CC:DD:EE:FF", parsed, -62, time.Now().UTC())
		if !ok {
			t.Fatalf("globalRemoteIDState.IngestAndPublish returned ok=false")
		}

		trafficMutex.Lock()
		stored, exists := traffic[key]
		trafficMutex.Unlock()
		if !exists {
			t.Fatalf("Remote ID target key 0x%X not found in global traffic map", key)
		}
		if stored.Emitter_category != 14 {
			t.Errorf("Remote ID Emitter_category = %d, want 14 (UAV)", stored.Emitter_category)
		}
		if stored.TargetType != TARGET_TYPE_REMOTEID {
			t.Errorf("Remote ID TargetType = %d, want %d (TARGET_TYPE_REMOTEID)", stored.TargetType, TARGET_TYPE_REMOTEID)
		}

		pflaaDrone, valid, _ := makeFlarmPFLAAString(ti)
		if !valid {
			t.Fatalf("makeFlarmPFLAAString failed for Remote ID target")
		}
		verifyNMEASentence(t, pflaaDrone, "$PFLAA,")
		// AcftType is the last field before *XX; Emitter_category 14 maps to FLARM type "D".
		starIdx := strings.LastIndexByte(pflaaDrone, '*')
		fields := strings.Split(pflaaDrone[:starIdx], ",")
		if len(fields) < 12 || fields[11] != "D" {
			t.Errorf("Remote ID FLARM PFLAA AcftType = %q in %q, want \"D\"", fields[len(fields)-1], pflaaDrone)
		}
	})

	// 6. JSON Serialization & Goroutine Stability Check
	t.Run("JSONSerializationAndGoroutineStability", func(t *testing.T) {
		trafficMutex.Lock()
		trafficSnap := make(map[uint32]TrafficInfo, len(traffic))
		for k, v := range traffic {
			trafficSnap[k] = v
		}
		trafficMutex.Unlock()

		trafficJSON, err := json.Marshal(trafficSnap)
		if err != nil {
			t.Fatalf("json.Marshal(traffic) failed (possible NaN/Inf): %v", err)
		}
		if !json.Valid(trafficJSON) {
			t.Fatalf("traffic JSON invalid")
		}

		wxJSON, err := json.Marshal(getGraphicalProductsSnapshot())
		if err != nil {
			t.Fatalf("json.Marshal(getGraphicalProductsSnapshot()) failed: %v", err)
		}
		if !json.Valid(wxJSON) {
			t.Fatalf("graphical weather JSON invalid")
		}

		healthSnap := hwHealth.Snapshot()
		healthJSON, err := json.Marshal(&healthSnap)
		if err != nil {
			t.Fatalf("json.Marshal(hwHealth.Snapshot()) failed: %v", err)
		}
		if !json.Valid(healthJSON) {
			t.Fatalf("hardware health JSON invalid")
		}

		runtime.GC()
		delta := runtime.NumGoroutine() - baseGoroutines
		if delta > 8 {
			t.Errorf("goroutine leak detected during replay regression: delta=%d (base=%d, now=%d)",
				delta, baseGoroutines, runtime.NumGoroutine())
		}
	})
}

func TestManagementNewEndpoints(t *testing.T) {
	initReplayTestEnv()

	t.Run("GraphicalWeatherAlwaysJSONArray", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/getGraphicalWeather", nil)
		rec := httptest.NewRecorder()
		handleGraphicalWeather(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := strings.TrimSpace(rec.Body.String())
		if body == "null" || !strings.HasPrefix(body, "[") {
			t.Fatalf("expected JSON array from /getGraphicalWeather, got %q", body)
		}
	})

	t.Run("CheckUpdateOfflineAndMockGitHub", func(t *testing.T) {
		origURL := githubLatestReleaseURL
		origPaths := otaStatusPaths
		origVer := stratuxVersion
		defer func() {
			githubLatestReleaseURL = origURL
			otaStatusPaths = origPaths
			stratuxVersion = origVer
		}()

		tmpDir := t.TempDir()
		otaFile := filepath.Join(tmpDir, "ota_last_status")
		if err := os.WriteFile(otaFile, []byte("committed\n"), 0644); err != nil {
			t.Fatalf("write ota status: %v", err)
		}
		otaStatusPaths = []string{otaFile}
		stratuxVersion = "v3.6"

		// Offline mode returns UpdateAvailable=false gracefully without error.
		githubLatestReleaseURL = ""
		rec := httptest.NewRecorder()
		handleCheckUpdate(rec, httptest.NewRequest(http.MethodGet, "/checkUpdate", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("offline /checkUpdate status = %d, want 200", rec.Code)
		}
		var offlineResp updateCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &offlineResp); err != nil {
			t.Fatalf("unmarshal offline response: %v", err)
		}
		if offlineResp.UpdateAvailable || offlineResp.CurrentVersion != "v3.6" || offlineResp.OTAStatus != "committed" {
			t.Errorf("unexpected offline checkUpdate response: %+v", offlineResp)
		}

		// Mock GitHub release server with newer tag v3.7.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v3.7","html_url":"https://github.com/stratux/stratux/releases/tag/v3.7"}`))
		}))
		defer srv.Close()

		githubLatestReleaseURL = srv.URL
		rec2 := httptest.NewRecorder()
		handleCheckUpdate(rec2, httptest.NewRequest(http.MethodGet, "/checkUpdate", nil))
		if rec2.Code != http.StatusOK {
			t.Fatalf("online /checkUpdate status = %d, want 200", rec2.Code)
		}
		var onlineResp updateCheckResponse
		if err := json.Unmarshal(rec2.Body.Bytes(), &onlineResp); err != nil {
			t.Fatalf("unmarshal online response: %v", err)
		}
		if !onlineResp.UpdateAvailable || onlineResp.LatestVersion != "v3.7" || onlineResp.ReleaseURL == "" {
			t.Errorf("unexpected online checkUpdate response: %+v", onlineResp)
		}
	})

	t.Run("FirmwareListAndFlashValidation", func(t *testing.T) {
		origBase := firmwareBaseDir
		defer func() { firmwareBaseDir = origBase }()

		tmpDir := t.TempDir()
		firmwareBaseDir = tmpDir

		// Empty/missing directories should gracefully return non-nil empty slices.
		rec := httptest.NewRecorder()
		handleGetFirmwareList(rec, httptest.NewRequest(http.MethodGet, "/getFirmwareList", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/getFirmwareList status = %d, want 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "null") {
			t.Fatalf("/getFirmwareList returned null slices: %s", rec.Body.String())
		}

		// Create dummy firmware zip files.
		ognDir := filepath.Join(tmpDir, "ogn")
		softrfDir := filepath.Join(tmpDir, "softrf")
		_ = os.MkdirAll(ognDir, 0755)
		_ = os.MkdirAll(softrfDir, 0755)
		_ = os.WriteFile(filepath.Join(ognDir, "ogn-tracker-test.zip"), []byte("PK"), 0644)
		_ = os.WriteFile(filepath.Join(softrfDir, "SoftRF.test.zip"), []byte("PK"), 0644)

		rec2 := httptest.NewRecorder()
		handleGetFirmwareList(rec2, httptest.NewRequest(http.MethodGet, "/getFirmwareList", nil))
		var fwList firmwareListResponse
		if err := json.Unmarshal(rec2.Body.Bytes(), &fwList); err != nil {
			t.Fatalf("unmarshal firmware list: %v", err)
		}
		if len(fwList.OGN) != 1 || fwList.OGN[0] != "ogn-tracker-test.zip" {
			t.Errorf("unexpected OGN firmware list: %+v", fwList.OGN)
		}
		if len(fwList.SoftRF) != 1 || fwList.SoftRF[0] != "SoftRF.test.zip" {
			t.Errorf("unexpected SoftRF firmware list: %+v", fwList.SoftRF)
		}

		// Reject GET, cross-origin, path traversal, and command injection on /flashFirmware.
		recGet := httptest.NewRecorder()
		handleFlashFirmware(recGet, httptest.NewRequest(http.MethodGet, "/flashFirmware", nil))
		if recGet.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET /flashFirmware status = %d, want 405", recGet.Code)
		}

		badPayloads := []string{
			`{"Target":"ogn","Firmware":"../etc/passwd.zip"}`,
			`{"Target":"ogn","Firmware":"ogn-tracker-test.zip; id"}`,
			`{"Target":"ogn","Firmware":"nonexistent.zip"}`,
			`{"Target":"invalid","Firmware":"ogn-tracker-test.zip"}`,
		}
		for _, payload := range badPayloads {
			req := httptest.NewRequest(http.MethodPost, "http://192.168.10.1/flashFirmware", bytes.NewBufferString(payload))
			w := httptest.NewRecorder()
			handleFlashFirmware(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("payload %s returned status %d, want 400", payload, w.Code)
			}
		}

		// Cross-origin POST must be rejected with 403.
		reqXOrigin := httptest.NewRequest(http.MethodPost, "http://192.168.10.1/flashFirmware", bytes.NewBufferString(`{"Target":"ogn","Firmware":"ogn-tracker-test.zip"}`))
		reqXOrigin.Header.Set("Origin", "https://evil.example.com")
		wXOrigin := httptest.NewRecorder()
		handleFlashFirmware(wXOrigin, reqXOrigin)
		if wXOrigin.Code != http.StatusForbidden {
			t.Errorf("cross-origin /flashFirmware status = %d, want 403", wXOrigin.Code)
		}
	})
}
