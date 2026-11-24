package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/gpsnmea"
)

func initTestGPSSituation(t *testing.T) {
	t.Helper()
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
		time.Sleep(25 * time.Millisecond)
	}
	mySituation.muGPS = &sync.Mutex{}
	mySituation.muGPSPerformance = &sync.Mutex{}
	mySituation.muSatellite = &sync.Mutex{}
	mySituation.muBaro = &sync.Mutex{}
	mySituation.muAttitude = &sync.Mutex{}
	Satellites = make(map[string]SatelliteInfo)

	mySituation.muGPS.Lock()
	mySituation.GPSFixQuality = 0
	mySituation.GPSLatitude = 0
	mySituation.GPSLongitude = 0
	mySituation.GPSAltitudeMSL = 0
	mySituation.GPSHorizontalAccuracy = 0
	mySituation.GPSVerticalAccuracy = 0
	mySituation.GPSNACp = 0
	mySituation.GPSSatellites = 0
	mySituation.GPSSatellitesTracked = 0
	mySituation.GPSSatellitesSeen = 0
	mySituation.GPSLastFixLocalTime = time.Time{}
	mySituation.GPSLastValidNMEAMessageTime = time.Time{}
	mySituation.GPSLastAccuracyTime = time.Time{}
	mySituation.GPSGroundSpeed = 0
	mySituation.GPSTrueCourse = 0
	mySituation.muGPS.Unlock()

	mySituation.muBaro.Lock()
	mySituation.BaroLastMeasurementTime = time.Time{}
	mySituation.muBaro.Unlock()

	mySituation.muAttitude.Lock()
	mySituation.AHRSLastAttitudeTime = time.Time{}
	mySituation.muAttitude.Unlock()

	setGPSConnected(false)
	setGPSDetectedType(0)
	setSerialPort(nil)
}

func TestIsGPSValid_ResetsZeroStartupAccuracy(t *testing.T) {
	initTestGPSSituation(t)

	// Immediately after boot, timestamps are zero and accuracies start at 0.
	if isGPSConnected() {
		t.Fatalf("isGPSConnected() should be false when GPSLastValidNMEAMessageTime is zero")
	}
	if isTempPressValid() {
		t.Fatalf("isTempPressValid() should be false when BaroLastMeasurementTime is zero")
	}
	if isAHRSValid() {
		t.Fatalf("isAHRSValid() should be false when AHRSLastAttitudeTime is zero")
	}
	if isGPSValid() {
		t.Fatalf("isGPSValid() should be false when disconnected")
	}

	mySituation.muGPS.Lock()
	hAcc := mySituation.GPSHorizontalAccuracy
	vAcc := mySituation.GPSVerticalAccuracy
	mySituation.muGPS.Unlock()
	if hAcc != 999999 || vAcc != 999999 {
		t.Fatalf("expected startup accuracies reset to 999999, got horiz=%v vert=%v", hAcc, vAcc)
	}
}

func TestProcessNMEA_SixCharTimestampAndEmptyGeoid(t *testing.T) {
	initTestGPSSituation(t)
	setGPSConnected(true)

	// 6-char hhmmss timestamp (no fractional seconds) and blank geoid separation (field 11).
	gga := gpsnmea.AppendChecksum("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,,M,,")
	if !processNMEALineLow(gga, true) {
		t.Fatalf("expected GGA with 6-char timestamp and empty geoid separation to parse")
	}

	mySituation.muGPS.Lock()
	fixQ := mySituation.GPSFixQuality
	sats := mySituation.GPSSatellites
	lat := mySituation.GPSLatitude
	mySituation.muGPS.Unlock()

	if fixQ != 1 {
		t.Fatalf("GPSFixQuality = %d, want 1", fixQ)
	}
	if sats != 8 {
		t.Fatalf("GPSSatellites fallback from GGA = %d, want 8", sats)
	}
	if lat < 48.11 || lat > 48.12 {
		t.Fatalf("GPSLatitude = %f, want ~48.1173", lat)
	}

	// 6-char RMC timestamp.
	rmc := gpsnmea.AppendChecksum("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230324,003.1,W")
	if !processNMEALineLow(rmc, true) {
		t.Fatalf("expected RMC with 6-char timestamp to parse")
	}
}

func TestProcessNMEA_StationaryVTGAndGSTGuard(t *testing.T) {
	initTestGPSSituation(t)
	setGPSConnected(true)

	// GST while GPSFixQuality == 0 must be ignored so accuracy stays at 999999.
	isGPSValid() // sets 999999
	gstNoFix := gpsnmea.AppendChecksum("$GNGST,205246.00,1.19,0.02,0.01,-2.4501,0.50,0.50,1.00")
	if processNMEALineLow(gstNoFix, true) {
		t.Fatalf("expected GST with no fix to be rejected")
	}
	mySituation.muGPS.Lock()
	hAcc := mySituation.GPSHorizontalAccuracy
	mySituation.muGPS.Unlock()
	if hAcc != 999999 {
		t.Fatalf("GST without fix overwrote GPSHorizontalAccuracy to %v", hAcc)
	}

	// Stationary VTG with empty COG (field 1) and 0.1 kt groundspeed must still succeed.
	vtg := gpsnmea.AppendChecksum("$GNVTG,,T,,M,0.12,N,0.22,K,A")
	if !processNMEALineLow(vtg, true) {
		t.Fatalf("expected stationary VTG with empty COG to parse")
	}
}

func TestProcessNMEA_GSAStartupFallbackAnd2DBlankVDOP(t *testing.T) {
	initTestGPSSituation(t)
	setGPSConnected(true)

	// Give a valid fix first, then send a 2D GSA with blank VDOP immediately after boot (GPSLastAccuracyTime is zero).
	gga := gpsnmea.AppendChecksum("$GNGGA,123519.00,4807.038,N,01131.000,E,1,04,1.2,545.4,M,46.9,M,,")
	processNMEALineLow(gga, true)

	gsa := gpsnmea.AppendChecksum("$GNGSA,A,2,03,14,22,31,,,,,,,,,2.5,1.2,,1")
	if !processNMEALineLow(gsa, true) {
		t.Fatalf("expected 2D GSA with blank VDOP to parse")
	}

	mySituation.muGPS.Lock()
	hAcc := mySituation.GPSHorizontalAccuracy
	vAcc := mySituation.GPSVerticalAccuracy
	mySituation.muGPS.Unlock()
	if hAcc <= 0 || hAcc >= 100 {
		t.Fatalf("expected HDOP-derived horizontal accuracy on startup, got %v", hAcc)
	}
	if vAcc != 999999 {
		t.Fatalf("expected 999999 vertical accuracy for blank VDOP, got %v", vAcc)
	}
}

func TestProcessNMEA_TalkerScopedGSV(t *testing.T) {
	initTestGPSSituation(t)
	setGPSConnected(true)

	// NMEA 4.10/4.11 Galileo GAGSV with local SV 12 must NOT collide with GPS G12!
	gpGsv := gpsnmea.AppendChecksum("$GPGSV,1,1,01,12,45,120,38")
	gaGsv := gpsnmea.AppendChecksum("$GAGSV,1,1,01,12,30,210,34")
	gbGsv := gpsnmea.AppendChecksum("$GBGSV,1,1,01,19,55,090,41")
	if !processNMEALineLow(gpGsv, true) || !processNMEALineLow(gaGsv, true) || !processNMEALineLow(gbGsv, true) {
		t.Fatalf("expected GPGSV/GAGSV/GBGSV sentences to parse")
	}

	mySituation.muSatellite.Lock()
	_, hasG12 := Satellites["G12"]
	_, hasE12 := Satellites["E12"]
	_, hasB19 := Satellites["B19"]
	mySituation.muSatellite.Unlock()
	if !hasG12 || !hasE12 || !hasB19 {
		t.Fatalf("expected G12=%v E12=%v B19=%v to all exist distinctly", hasG12, hasE12, hasB19)
	}
}

func TestMakeUBXCfgValset(t *testing.T) {
	var kv []byte
	kv = appendUBXValU1(kv, 0x20110021, 7)
	pkt := makeUBXCfgValset(kv)
	// 6 header + 4 valset hdr + 5 kv + 2 checksum = 17 bytes
	if len(pkt) != 17 {
		t.Fatalf("UBX-CFG-VALSET length = %d, want 17", len(pkt))
	}
	if pkt[0] != 0xB5 || pkt[1] != 0x62 || pkt[2] != 0x06 || pkt[3] != 0x8A {
		t.Fatalf("unexpected UBX-CFG-VALSET header: %x", pkt[:4])
	}
}
