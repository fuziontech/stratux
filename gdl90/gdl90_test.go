package gdl90

import (
	"bytes"
	"math"
	"sync"
	"testing"
	"time"
)

func TestComputeCRC_OfficialICDVector(t *testing.T) {
	// FAA GDL90 Public ICD Rev A, Section 2.2.3 official Heartbeat test vector:
	// Payload: 00 81 41 DB D0 08 02
	// Expected CRC-16: 0x8BB3 (transmitted LSB first: B3 8B)
	// Expected framed message: 7E 00 81 41 DB D0 08 02 B3 8B 7E
	payload := []byte{0x00, 0x81, 0x41, 0xDB, 0xD0, 0x08, 0x02}
	const wantCRC uint16 = 0x8BB3

	if got := ComputeCRC(payload); got != wantCRC {
		t.Fatalf("ComputeCRC(ICD vector) = 0x%04X, want 0x%04X", got, wantCRC)
	}

	wantFramed := []byte{0x7E, 0x00, 0x81, 0x41, 0xDB, 0xD0, 0x08, 0x02, 0xB3, 0x8B, 0x7E}
	gotFramed := PrepareMessage(payload)
	if !bytes.Equal(gotFramed, wantFramed) {
		t.Fatalf("PrepareMessage(ICD vector) = %X, want %X", gotFramed, wantFramed)
	}

	gotPayload, ok := UnframeMessage(gotFramed)
	if !ok {
		t.Fatalf("UnframeMessage(ICD vector) returned ok=false")
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("UnframeMessage(ICD vector) = %X, want %X", gotPayload, payload)
	}
}

func TestComputeCRC_EdgeCases(t *testing.T) {
	if got := ComputeCRC(nil); got != 0 {
		t.Fatalf("ComputeCRC(nil) = 0x%04X, want 0x0000", got)
	}
	if got := ComputeCRC([]byte{}); got != 0 {
		t.Fatalf("ComputeCRC(empty) = 0x%04X, want 0x0000", got)
	}
	// Verify concurrent CRC calls are race-free under -race.
	var wg sync.WaitGroup
	sample := []byte{0x00, 0x81, 0x41, 0xDB, 0xD0, 0x08, 0x02}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if ComputeCRC(sample) != 0x8BB3 {
					t.Errorf("unexpected CRC in concurrent goroutine")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPrepareAndUnframe_EscapingAndNonMutating(t *testing.T) {
	// Payload containing 0x7E, 0x7D, 0x5E, 0x5D in multiple positions.
	original := []byte{0x7E, 0x7D, 0x00, 0x5E, 0x5D, 0x7E, 0x7D, 0xFF}
	snapshot := append([]byte(nil), original...)

	framed := PrepareMessage(original)
	if !bytes.Equal(original, snapshot) {
		t.Fatalf("PrepareMessage mutated caller slice: got %X, want %X", original, snapshot)
	}

	if framed[0] != FlagByte || framed[len(framed)-1] != FlagByte {
		t.Fatalf("framed message missing 0x7E delimiters: %X", framed)
	}
	// Interior bytes must never contain raw 0x7E.
	if bytes.Contains(framed[1:len(framed)-1], []byte{FlagByte}) {
		t.Fatalf("framed interior contains unescaped 0x7E: %X", framed)
	}
	// Must contain escaped sequences 7D 5E and 7D 5D.
	if !bytes.Contains(framed[1:len(framed)-1], []byte{0x7D, 0x5E}) {
		t.Fatalf("framed interior missing escaped 0x7E (7D 5E): %X", framed)
	}
	if !bytes.Contains(framed[1:len(framed)-1], []byte{0x7D, 0x5D}) {
		t.Fatalf("framed interior missing escaped 0x7D (7D 5D): %X", framed)
	}

	unframed, ok := UnframeMessage(framed)
	if !ok {
		t.Fatalf("UnframeMessage failed for escaped payload")
	}
	if !bytes.Equal(unframed, original) {
		t.Fatalf("round-trip mismatch: got %X, want %X", unframed, original)
	}
}

func TestPrepareAndUnframe_EscapedCRCBytes(t *testing.T) {
	// Search for small payloads whose CRC low or high byte is 0x7E or 0x7D
	// to verify escaping of CRC bytes themselves.
	found7E := false
	found7D := false
	for a := 0; a < 256 && (!found7E || !found7D); a++ {
		for b := 0; b < 256 && (!found7E || !found7D); b++ {
			p := []byte{byte(a), byte(b)}
			crc := ComputeCRC(p)
			lo, hi := byte(crc&0xFF), byte(crc>>8)
			if lo == 0x7E || hi == 0x7E {
				found7E = true
				f := PrepareMessage(p)
				if bytes.Contains(f[1:len(f)-1], []byte{0x7E}) {
					t.Fatalf("CRC byte 0x7E was not escaped: %X", f)
				}
				got, ok := UnframeMessage(f)
				if !ok || !bytes.Equal(got, p) {
					t.Fatalf("round-trip failed when CRC contains 0x7E: ok=%v got=%X want=%X", ok, got, p)
				}
			}
			if lo == 0x7D || hi == 0x7D {
				found7D = true
				f := PrepareMessage(p)
				got, ok := UnframeMessage(f)
				if !ok || !bytes.Equal(got, p) {
					t.Fatalf("round-trip failed when CRC contains 0x7D: ok=%v got=%X want=%X", ok, got, p)
				}
			}
		}
	}
	if !found7E || !found7D {
		t.Fatalf("did not find test vectors exercising 0x7E (%v) and 0x7D (%v) in CRC", found7E, found7D)
	}
}

func TestUnframeMessage_InvalidInputs(t *testing.T) {
	valid := PrepareMessage([]byte{0x0A, 0x01, 0x02, 0x03})
	corruptCRC := append([]byte(nil), valid...)
	corruptCRC[len(corruptCRC)-2] ^= 0x01

	corruptBody := append([]byte(nil), valid...)
	corruptBody[2] ^= 0xFF

	cases := []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"too_short_2", []byte{0x7E, 0x7E}},
		{"too_short_3", []byte{0x7E, 0x00, 0x7E}},
		{"missing_start_flag", valid[1:]},
		{"missing_end_flag", valid[:len(valid)-1]},
		{"unescaped_interior_flag", []byte{0x7E, 0x00, 0x7E, 0x00, 0x00, 0x7E}},
		{"dangling_escape", []byte{0x7E, 0x00, 0x00, 0x7D, 0x7E}},
		{"escape_followed_by_flag", []byte{0x7E, 0x7D, 0x7E, 0x00, 0x00, 0x7E}},
		{"short_after_unescape", []byte{0x7E, 0x7D, 0x5E, 0x7E}},
		{"bad_crc", corruptCRC},
		{"corrupt_body", corruptBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := UnframeMessage(tc.in); ok || got != nil {
				t.Fatalf("UnframeMessage(%s) = (%X, %v), want (nil, false)", tc.name, got, ok)
			}
		})
	}

	// Empty payload framed is valid and round-trips to empty slice.
	emptyFramed := PrepareMessage([]byte{})
	if got, ok := UnframeMessage(emptyFramed); !ok || len(got) != 0 {
		t.Fatalf("UnframeMessage(PrepareMessage(empty)) = (%X, %v), want ([], true)", got, ok)
	}
}

func TestEncodeLatLon(t *testing.T) {
	cases := []struct {
		name string
		deg  float32
		want [3]byte
	}{
		{"zero", 0, [3]byte{0x00, 0x00, 0x00}},
		{"plus_45", 45.0, [3]byte{0x20, 0x00, 0x00}},
		{"minus_45", -45.0, [3]byte{0xE0, 0x00, 0x00}},
		{"plus_90", 90.0, [3]byte{0x40, 0x00, 0x00}},
		{"minus_90", -90.0, [3]byte{0xC0, 0x00, 0x00}},
		{"plus_180", 180.0, [3]byte{0x80, 0x00, 0x00}},
		{"minus_180", -180.0, [3]byte{0x80, 0x00, 0x00}},
		// FAA GDL90 ICD Rev A Section 3.5.1 example: +44.90708 -> 0x1FF072 (approx; verify within 1 LSB)
		{"nan", float32(math.NaN()), [3]byte{0x00, 0x00, 0x00}},
		{"pos_inf", float32(math.Inf(1)), [3]byte{0x00, 0x00, 0x00}},
		{"neg_inf", float32(math.Inf(-1)), [3]byte{0x00, 0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeLatLon(tc.deg); got != tc.want {
				t.Fatalf("EncodeLatLon(%v) = %X, want %X", tc.deg, got, tc.want)
			}
		})
	}

	// Verify decode round-trip accuracy for representative coordinates.
	for _, coord := range []float32{37.7749, -122.4194, 48.1351, 11.5820, -33.8688, 151.2093} {
		enc := EncodeLatLon(coord)
		raw := int32(enc[0])<<16 | int32(enc[1])<<8 | int32(enc[2])
		if raw&0x800000 != 0 {
			raw |= ^0xFFFFFF // sign-extend 24-bit two's complement
		}
		decoded := float32(raw) * LonLatResolution
		if math.Abs(float64(decoded-coord)) > float64(LonLatResolution)*2 {
			t.Fatalf("EncodeLatLon(%v) decoded to %v (diff > 2 LSB)", coord, decoded)
		}
	}
}

func TestParseStratuxVersion(t *testing.T) {
	cases := []struct {
		ver  string
		want [4]byte
	}{
		{"v3.6", [4]byte{3, 6, 0, 0}},
		{"3.6", [4]byte{3, 6, 0, 0}},
		{"v1.6r1", [4]byte{1, 6, 2, 1}},
		{"v1.6r1-eu032", [4]byte{1, 6, 2, 1}},
		{"v3.5rc2", [4]byte{3, 5, 3, 2}},
		{"v3.5rc2-dirty", [4]byte{3, 5, 3, 2}},
		{"v1.4b3", [4]byte{1, 4, 1, 3}},
		{"v1.4b3-abcdef", [4]byte{1, 4, 1, 3}},
		{"v2.10-dev", [4]byte{2, 10, 0, 0}},
		{"", [4]byte{0, 0, 0, 0}},
		{"dev", [4]byte{0, 0, 0, 0}},
		{"v", [4]byte{0, 0, 0, 0}},
		{"v.", [4]byte{0, 0, 0, 0}},
		{"vabc.def", [4]byte{0, 0, 0, 0}},
		{"v1.2rc", [4]byte{1, 2, 3, 0}},
	}
	for _, tc := range cases {
		if got := ParseStratuxVersion(tc.ver); got != tc.want {
			t.Errorf("ParseStratuxVersion(%q) = %v, want %v", tc.ver, got, tc.want)
		}
	}
}

func TestNormalizeAngle360(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{-0.0, 0},
		{90, 90},
		{359.5, 359.5},
		{360, 0},
		{-360, 0},
		{720.25, 0.25},
		{-90, 270},
		{-450, 270},
		{1e9 + 45, math.Mod(1e9+45, 360.0)},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
		{-1e-17, 0},
	}
	for _, tc := range cases {
		got := NormalizeAngle360(tc.in)
		if math.IsNaN(got) || got < 0 || got >= 360 {
			t.Fatalf("NormalizeAngle360(%v) = %v out of [0, 360)", tc.in, got)
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("NormalizeAngle360(%v) = %v, want %v", tc.in, got, tc.want)
		}
		if got == 0 && math.Signbit(got) {
			t.Errorf("NormalizeAngle360(%v) returned negative zero", tc.in)
		}
	}
}

func TestFormatCallsign8(t *testing.T) {
	cases := []struct {
		in   string
		want [8]byte
	}{
		{"", [8]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}},
		{"Stratux", [8]byte{'S', 't', 'r', 'a', 't', 'u', 'x', ' '}},
		{"N12345", [8]byte{'N', '1', '2', '3', '4', '5', ' ', ' '}},
		{"N1234567", [8]byte{'N', '1', '2', '3', '4', '5', '6', '7'}},
		{"N123456789", [8]byte{'N', '1', '2', '3', '4', '5', '6', '7'}},
		{"A\x00B\r\n\t\x7f\x80", [8]byte{'A', ' ', 'B', ' ', ' ', ' ', ' ', ' '}},
	}
	for _, tc := range cases {
		if got := FormatCallsign8(tc.in); got != tc.want {
			t.Errorf("FormatCallsign8(%q) = %q (%X), want %q (%X)", tc.in, got, got, tc.want, tc.want)
		}
	}
}

func TestEncodeHeartbeat(t *testing.T) {
	// Test afternoon timestamp (< 65536s, bit 16 = 0): 12:34:56 UTC = 45296s = 0x0B0F0.
	ts1 := time.Date(2026, 6, 15, 12, 34, 56, 0, time.UTC)
	framed := EncodeHeartbeat(true, false, true, ts1, 5, 300)
	payload, ok := UnframeMessage(framed)
	if !ok {
		t.Fatalf("UnframeMessage(EncodeHeartbeat) failed")
	}
	if len(payload) != 7 {
		t.Fatalf("heartbeat payload len = %d, want 7", len(payload))
	}
	if payload[0] != MsgIDHeartbeat {
		t.Errorf("msg ID = 0x%02X, want 0x00", payload[0])
	}
	// Status byte 1: GPS valid (0x80) | Addr talkback (0x10) | UAT init (0x01) = 0x91
	if payload[1] != 0x91 {
		t.Errorf("status byte 1 = 0x%02X, want 0x91", payload[1])
	}
	// Status byte 2: bit 16 of 45296 is 0; UTC OK = 0x01
	if payload[2] != 0x01 {
		t.Errorf("status byte 2 = 0x%02X, want 0x01", payload[2])
	}
	gotSec := uint32(payload[3]) | (uint32(payload[4]) << 8) | (uint32(payload[2]>>7) << 16)
	if gotSec != 45296 {
		t.Errorf("timestamp seconds = %d, want 45296", gotSec)
	}
	gotUplink := uint32(payload[5] >> 3)
	gotBasicLong := (uint32(payload[5]&0x03) << 8) | uint32(payload[6])
	if gotUplink != 5 || gotBasicLong != 300 {
		t.Errorf("counts = (uplink=%d, basicLong=%d), want (5, 300)", gotUplink, gotBasicLong)
	}

	// Test evening timestamp (>= 65536s, bit 16 = 1): 23:59:59 UTC = 86399s = 0x1517F,
	// with maintReq=true, gpsValid=false, utcOK=false, and overflowing counts.
	ts2 := time.Date(2026, 6, 15, 23, 59, 59, 0, time.FixedZone("UTC+2", 2*3600))
	// In UTC this is 21:59:59 = 79199s = 0x1355F.
	framed2 := EncodeHeartbeat(false, true, false, ts2, 999, 5000)
	payload2, ok := UnframeMessage(framed2)
	if !ok {
		t.Fatalf("UnframeMessage(EncodeHeartbeat #2) failed")
	}
	// Status byte 1: MaintReq (0x40) | Addr talkback (0x10) | UAT init (0x01) = 0x51
	if payload2[1] != 0x51 {
		t.Errorf("status byte 1 = 0x%02X, want 0x51", payload2[1])
	}
	// Status byte 2: bit 16 of 79199 is 1 (0x80), utcOK = 0 -> 0x80
	if payload2[2] != 0x80 {
		t.Errorf("status byte 2 = 0x%02X, want 0x80", payload2[2])
	}
	gotSec2 := uint32(payload2[3]) | (uint32(payload2[4]) << 8) | (uint32(payload2[2]>>7) << 16)
	if gotSec2 != 79199 {
		t.Errorf("timestamp seconds = %d, want 79199", gotSec2)
	}
	gotUplink2 := uint32(payload2[5] >> 3)
	gotBasicLong2 := (uint32(payload2[5]&0x03) << 8) | uint32(payload2[6])
	if gotUplink2 != 31 || gotBasicLong2 != 1023 {
		t.Errorf("clamped counts = (uplink=%d, basicLong=%d), want (31, 1023)", gotUplink2, gotBasicLong2)
	}
}

func TestEncodeOwnshipGeoAltitude(t *testing.T) {
	cases := []struct {
		name        string
		altFt       float32
		vertWarn    bool
		vfom        uint16
		wantAlt     int16
		wantMetrics uint16
	}{
		{"nominal_5000ft", 5000, false, 10, 1000, 0x000A},
		{"negative_alt", -250, false, 10, -50, 0x000A},
		{"vertical_warning", 12000, true, 25, 2400, 0x8019},
		{"vfom_not_available", 0, false, VFOMNotAvailable, 0, 0x7FFF},
		{"vfom_not_available_with_warn", 500, true, VFOMNotAvailable, 100, 0xFFFF},
		{"vfom_overflow_clamped", 100, false, 40000, 20, 0x7FFE},
		{"alt_overflow_pos", 500000, false, 10, math.MaxInt16, 0x000A},
		{"alt_overflow_neg", -500000, false, 10, math.MinInt16, 0x000A},
		{"alt_nan", float32(math.NaN()), false, 10, 0, 0x000A},
		{"alt_inf", float32(math.Inf(1)), false, 10, 0, 0x000A},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			framed := EncodeOwnshipGeoAltitude(tc.altFt, tc.vertWarn, tc.vfom)
			payload, ok := UnframeMessage(framed)
			if !ok {
				t.Fatalf("UnframeMessage failed")
			}
			if len(payload) != 5 {
				t.Fatalf("payload len = %d, want 5", len(payload))
			}
			if payload[0] != MsgIDOwnshipGeoAlt {
				t.Fatalf("msg ID = 0x%02X, want 0x0B", payload[0])
			}
			gotAlt := int16(uint16(payload[1])<<8 | uint16(payload[2]))
			if gotAlt != tc.wantAlt {
				t.Errorf("encoded alt = %d, want %d", gotAlt, tc.wantAlt)
			}
			gotMetrics := uint16(payload[3])<<8 | uint16(payload[4])
			if gotMetrics != tc.wantMetrics {
				t.Errorf("metrics = 0x%04X, want 0x%04X", gotMetrics, tc.wantMetrics)
			}
		})
	}
}
