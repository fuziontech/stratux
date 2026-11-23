package main

import (
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func initTestGlobals() {
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
	if trafficMutex == nil {
		trafficMutex = &sync.Mutex{}
	}
	if traffic == nil {
		traffic = make(map[uint32]TrafficInfo)
	}
	if seenTraffic == nil {
		seenTraffic = make(map[uint32]bool)
	}
	if systemErrsMutex == nil {
		systemErrsMutex = &sync.Mutex{}
	}
	if ADSBTowerMutex == nil {
		ADSBTowerMutex = &sync.Mutex{}
	}
	if ADSBTowers == nil {
		ADSBTowers = make(map[string]ADSBTower)
	}
	if mySituation.muGPS == nil {
		mySituation.muGPS = &sync.Mutex{}
	}
	if mySituation.muBaro == nil {
		mySituation.muBaro = &sync.Mutex{}
	}
	if mySituation.muAttitude == nil {
		mySituation.muAttitude = &sync.Mutex{}
	}
	if mySituation.muSatellite == nil {
		mySituation.muSatellite = &sync.Mutex{}
	}
	if mySituation.muGPSPerformance == nil {
		mySituation.muGPSPerformance = &sync.Mutex{}
	}
}

func TestParseStratuxVersion(t *testing.T) {
	cases := []struct {
		in   string
		want [4]byte
	}{
		{"v3.6", [4]byte{3, 6, 0, 0}},
		{"v1.6r1", [4]byte{1, 6, 2, 1}},
		{"v2.0b3", [4]byte{2, 0, 1, 3}},
		{"v2.0rc4", [4]byte{2, 0, 3, 4}},
		{"", [4]byte{0, 0, 0, 0}},
		{"devbuild", [4]byte{0, 0, 0, 0}},
		{"v", [4]byte{0, 0, 0, 0}},
	}
	for _, tc := range cases {
		got := parseStratuxVersion(tc.in)
		if got != tc.want {
			t.Errorf("parseStratuxVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeAngle360(t *testing.T) {
	if got := normalizeAngle360(math.Inf(1)); got != 0 {
		t.Errorf("Inf -> %v, want 0", got)
	}
	if got := normalizeAngle360(math.NaN()); got != 0 {
		t.Errorf("NaN -> %v, want 0", got)
	}
	if got := normalizeAngle360(1e25); math.IsNaN(got) || got < 0 || got >= 360 {
		t.Errorf("1e25 -> %v, want [0,360)", got)
	}
	if got := normalizeAngle360(-90); math.Abs(got-270) > 1e-9 {
		t.Errorf("-90 -> %v, want 270", got)
	}
	if got := normalizeAngle360(725); math.Abs(got-5) > 1e-9 {
		t.Errorf("725 -> %v, want 5", got)
	}
}

func TestNetworkSettingsSanitization(t *testing.T) {
	initTestGlobals()

	globalSettingsMutex.Lock()
	globalSettings.WiFiDirectPin = "12345678"
	globalSettings.WiFiCountry = "US"
	globalSettings.WiFiIPAddress = "192.168.10.1"
	globalSettingsMutex.Unlock()

	// Command injection attempt in WiFiDirectPin must be rejected.
	setWifiDirectPin("12345678; id > /tmp/pwn")
	globalSettingsMutex.RLock()
	pin := globalSettings.WiFiDirectPin
	globalSettingsMutex.RUnlock()
	if pin != "12345678" {
		t.Errorf("injection pin accepted: %q", pin)
	}

	setWifiDirectPin("87654321")
	globalSettingsMutex.RLock()
	pin = globalSettings.WiFiDirectPin
	globalSettingsMutex.RUnlock()
	if pin != "87654321" {
		t.Errorf("valid pin rejected: %q", pin)
	}

	// Config directive injection in wpa_supplicant strings must be stripped.
	cleaned := sanitizeWpaString("MySSID\"\nnetwork={\n}\r\x00\\")
	if strings.ContainsAny(cleaned, "\"\r\n\\\x00") {
		t.Errorf("sanitizeWpaString failed to strip metacharacters: %q", cleaned)
	}

	// Invalid IPv4 must be rejected.
	if _, ok := validateIPv4("192.168"); ok {
		t.Errorf("short IPv4 should be rejected")
	}
	if got, ok := validateIPv4("192.168.10.1"); !ok || got != "192.168.10.1" {
		t.Errorf("valid IPv4 rejected: %q, %v", got, ok)
	}
}

func TestValidateSameOrigin(t *testing.T) {
	// No Origin header (e.g., curl / EFB local client) allowed.
	req := httptest.NewRequest("POST", "http://192.168.10.1/reboot", nil)
	if !validateSameOrigin(req) {
		t.Errorf("request without Origin should be allowed")
	}
	// Matching Origin allowed.
	req2 := httptest.NewRequest("POST", "http://192.168.10.1/reboot", nil)
	req2.Header.Set("Origin", "http://192.168.10.1")
	if !validateSameOrigin(req2) {
		t.Errorf("matching Origin should be allowed")
	}
	// Cross-origin rejected.
	req3 := httptest.NewRequest("POST", "http://192.168.10.1/reboot", nil)
	req3.Header.Set("Origin", "https://evil.example.com")
	if validateSameOrigin(req3) {
		t.Errorf("cross-origin request should be rejected")
	}
}

func TestParseInputAndDownlinkNoPanic(t *testing.T) {
	initTestGlobals()

	malformed := []string{
		"",
		"+",
		"-",
		"+;",
		"-;",
		"-010203;",                            // too short downlink
		"-08112233445566778899aabbccddeeff;",  // msg_type=1 but only 16 bytes
		"+" + strings.Repeat("aa", 500) + ";", // >432 bytes uplink (was negative strings.Repeat panic)
		"-" + strings.Repeat("bb", 500) + ";", // >432 bytes downlink (was hex.Decode OOB panic)
		"-zzzz;",                              // invalid hex
	}
	for _, m := range malformed {
		// Must not panic.
		_, _ = parseInput(m)
		parseDownlinkReport(m, 0)
	}

	// Valid 18-byte basic downlink must return 18 bytes, not 432 zero-padded bytes.
	shortHex := "-" + strings.Repeat("00", 18) + ";"
	frame, msgType := parseInput(shortHex)
	if msgType != 0x1e || len(frame) != 18 {
		t.Errorf("short downlink returned msgType=0x%x len=%d, want 0x1e len=18", msgType, len(frame))
	}
}

func TestCalcLocationForBearingDistanceAntimeridian(t *testing.T) {
	// Crossing 180° westward from -179.9° should wrap cleanly to ~+179.x°, not -180.x°.
	lat, lon := calcLocationForBearingDistance(0, -179.9, 270, 20)
	if math.IsNaN(lat) || math.IsNaN(lon) {
		t.Fatalf("got NaN: lat=%v lon=%v", lat, lon)
	}
	if lon < -180 || lon > 180 {
		t.Errorf("longitude out of [-180, 180]: %v", lon)
	}
	if lon <= 0 {
		t.Errorf("expected positive eastern longitude after crossing -180 westward, got %v", lon)
	}
}

func TestSanitizeNmeaAndCotTail(t *testing.T) {
	if got := sanitizeNmeaTail("N123,45*AB\r\n"); strings.ContainsAny(got, ",*\r\n") {
		t.Errorf("sanitizeNmeaTail leaked NMEA delimiters: %q", got)
	}
	if got := sanitizeCotTail("UID,1*2\n"); strings.ContainsAny(got, ",*\r\n") {
		t.Errorf("sanitizeCotTail leaked delimiters: %q", got)
	}
}

func TestParseAprsMessageNoPanicAndSouthWestHemisphere(t *testing.T) {
	initTestGlobals()

	// Malformed/minimal inputs must not panic or fatal.
	parseAprsMessage("", true)
	parseAprsMessage("ICA123456>APRS:/120000h3352.12S/15112.34E'090/050/A=001000 id06123456", true)

	// Verify Southern/Western hemisphere coordinate sign: 3352.12S / 15112.34W
	// Expected: -(33 + 52.12/60) = -33.86867, -(151 + 12.34/60) = -151.20567
	// Reset ownship GPS fix so distant Southern/Western hemisphere coordinates aren't filtered by prior test state.
	mySituation.muGPS.Lock()
	mySituation.GPSFixQuality = 0
	mySituation.GPSLastFixLocalTime = time.Time{}
	mySituation.muGPS.Unlock()

	trafficMutex.Lock()
	traffic = make(map[uint32]TrafficInfo)
	trafficMutex.Unlock()

	pkt := "FLRABCDEF>APRS,qAS,TEST:/120000h3352.12S/15112.34W'090/050/A=001000 !W00! id06ABCDEF"
	parseAprsMessage(pkt, true)

	trafficMutex.Lock()
	var found *TrafficInfo
	for _, ti := range traffic {
		if ti.Icao_addr == 0xABCDEF {
			cp := ti
			found = &cp
			break
		}
	}
	trafficMutex.Unlock()

	if found == nil {
		t.Fatalf("expected APRS target 0xABCDEF to be stored in traffic map")
	}
	wantLat := -(33.0 + 52.12/60.0)
	wantLon := -(151.0 + 12.34/60.0)
	if math.Abs(float64(found.Lat)-wantLat) > 0.01 {
		t.Errorf("Southern hemisphere Lat = %f, want ~%f", found.Lat, wantLat)
	}
	if math.Abs(float64(found.Lng)-wantLon) > 0.01 {
		t.Errorf("Western hemisphere Lng = %f, want ~%f", found.Lng, wantLon)
	}
}
