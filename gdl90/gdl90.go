// Package gdl90 implements pure, zero-CGO GDL90 framing, CRC-16 CCITT
// calculation, coordinate/callsign encoding, and message builders matching
// the FAA GDL90 Public ICD Rev A and Stratux wire formats.
package gdl90

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// FlagByte is the HDLC framing delimiter byte (0x7E) used at the start and
	// end of every GDL90 message.
	FlagByte byte = 0x7E
	// ControlEscape is the HDLC control-escape byte (0x7D) used to escape
	// FlagByte and ControlEscape occurrences inside a GDL90 frame.
	ControlEscape byte = 0x7D

	// LonLatResolution is the angular resolution (degrees per LSB) for 24-bit
	// signed GDL90 latitude/longitude semicircles (180 / 2^23).
	LonLatResolution = float32(180.0 / 8388608.0)
	// TrackResolution is the angular resolution (degrees per LSB) for 8-bit
	// GDL90 track/heading values (360 / 256).
	TrackResolution = float32(360.0 / 256.0)

	// GDL90 message IDs.
	MsgIDHeartbeat     byte = 0x00
	MsgIDOwnshipReport byte = 0x0A
	MsgIDOwnshipGeoAlt byte = 0x0B
	MsgIDTrafficReport byte = 0x14

	// VFOMNotAvailable is the 15-bit Vertical Figure of Merit sentinel indicating
	// that VFOM is not available (GDL90 ICD Rev A, section 3.8).
	VFOMNotAvailable uint16 = 0x7FFF
)

var crc16Table [256]uint16

func init() {
	for i := uint16(0); i < 256; i++ {
		crc := i << 8
		for bitctr := 0; bitctr < 8; bitctr++ {
			var z uint16
			if (crc & 0x8000) != 0 {
				z = 0x1021
			}
			crc = (crc << 1) ^ z
		}
		crc16Table[i] = crc
	}
}

// ComputeCRC computes the GDL90 CRC-16 CCITT checksum over data using the
// table algorithm defined in FAA GDL90 Public ICD Rev A, section 2.2.3.
func ComputeCRC(data []byte) uint16 {
	var ret uint16
	for _, b := range data {
		ret = crc16Table[ret>>8] ^ (ret << 8) ^ uint16(b)
	}
	return ret
}

// PrepareMessage frames an unescaped GDL90 payload with leading and trailing
// 0x7E flag bytes, appends the little-endian 16-bit CRC, and escapes any
// occurrences of 0x7E or 0x7D in both payload and CRC. It does not mutate data.
func PrepareMessage(data []byte) []byte {
	crc := ComputeCRC(data)

	out := make([]byte, 0, len(data)+6)
	out = append(out, FlagByte)

	appendEscaped := func(b byte) {
		if b == FlagByte || b == ControlEscape {
			out = append(out, ControlEscape, b^0x20)
		} else {
			out = append(out, b)
		}
	}

	for _, b := range data {
		appendEscaped(b)
	}
	// Append CRC-16 LSB first, then MSB, escaping control bytes.
	appendEscaped(byte(crc & 0xFF))
	appendEscaped(byte(crc >> 8))

	out = append(out, FlagByte)
	return out
}

// UnframeMessage validates and unframes a single GDL90 message bounded by
// leading and trailing 0x7E flag bytes. It unescapes 0x7D sequences and
// verifies the trailing 2-byte little-endian CRC-16. On failure, it returns
// (nil, false).
func UnframeMessage(framed []byte) (payload []byte, ok bool) {
	if len(framed) < 4 || framed[0] != FlagByte || framed[len(framed)-1] != FlagByte {
		return nil, false
	}

	raw := make([]byte, 0, len(framed)-2)
	escaped := false
	for _, b := range framed[1 : len(framed)-1] {
		if b == FlagByte {
			// Unescaped flag byte inside a frame is invalid.
			return nil, false
		}
		if escaped {
			raw = append(raw, b^0x20)
			escaped = false
			continue
		}
		if b == ControlEscape {
			escaped = true
			continue
		}
		raw = append(raw, b)
	}
	if escaped || len(raw) < 2 {
		return nil, false
	}

	bodyLen := len(raw) - 2
	gotCRC := uint16(raw[bodyLen]) | (uint16(raw[bodyLen+1]) << 8)
	body := raw[:bodyLen]
	if ComputeCRC(body) != gotCRC {
		return nil, false
	}

	res := make([]byte, bodyLen)
	copy(res, body)
	return res, true
}

// EncodeLatLon encodes a latitude or longitude in degrees into the 24-bit
// big-endian two's-complement semicircle representation specified by GDL90.
// Non-finite inputs (NaN/Inf) are encoded as zero.
func EncodeLatLon(v float32) [3]byte {
	var ret [3]byte
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return ret
	}
	wk := int32(v / LonLatResolution)
	ret[0] = byte((wk & 0xFF0000) >> 16)
	ret[1] = byte((wk & 0x00FF00) >> 8)
	ret[2] = byte(wk & 0x0000FF)
	return ret
}

// ParseStratuxVersion parses a Stratux version string (e.g. "v3.6", "v1.6r1",
// "v3.5rc2-dirty", "v1.4b3") into the 4-byte SX status version tuple:
// [major, minor, releaseType, build] where releaseType is 0 (plain), 1 ("b"),
// 2 ("r"), or 3 ("rc"). Safe on arbitrary strings.
func ParseStratuxVersion(ver string) [4]byte {
	var out [4]byte
	v := strings.TrimPrefix(ver, "v")
	dotIdx := strings.Index(v, ".")
	if dotIdx < 0 {
		return out
	}
	mStr := v[:dotIdx]
	mibStr := v[dotIdx+1:]

	tp := 0
	miStr := mibStr
	bStr := ""
	if idx := strings.Index(mibStr, "rc"); idx != -1 {
		tp = 3
		miStr = mibStr[:idx]
		bStr = mibStr[idx+2:]
	} else if idx := strings.Index(mibStr, "r"); idx != -1 {
		tp = 2
		miStr = mibStr[:idx]
		bStr = mibStr[idx+1:]
	} else if idx := strings.Index(mibStr, "b"); idx != -1 {
		tp = 1
		miStr = mibStr[:idx]
		bStr = mibStr[idx+1:]
	}
	if dash := strings.Index(miStr, "-"); dash != -1 {
		miStr = miStr[:dash]
	}
	if dash := strings.Index(bStr, "-"); dash != -1 {
		bStr = bStr[:dash]
	}

	m, _ := strconv.Atoi(mStr)
	mi, _ := strconv.Atoi(miStr)
	b, _ := strconv.Atoi(bStr)
	out[0] = byte(m)
	out[1] = byte(mi)
	out[2] = byte(tp)
	out[3] = byte(b)
	return out
}

// NormalizeAngle360 normalizes an angle in degrees into [0, 360) in O(1) time.
// Non-finite values (NaN, +Inf, -Inf) return 0.
func NormalizeAngle360(deg float64) float64 {
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return 0
	}
	deg = math.Mod(deg, 360.0)
	if deg < 0 {
		deg += 360.0
	}
	if deg == 0 || deg >= 360.0 {
		return 0
	}
	return deg
}

// FormatCallsign8 formats a callsign into an 8-byte GDL90 callsign field,
// space-padded with 0x20, truncated to 8 bytes, and sanitized so that any
// non-printable or non-ASCII byte (< 0x20 or > 0x7E) is replaced with 0x20.
func FormatCallsign8(s string) [8]byte {
	out := [8]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
	for i := 0; i < len(s) && i < len(out); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E {
			c = 0x20
		}
		out[i] = c
	}
	return out
}

// EncodeHeartbeat builds and frames a GDL90 Heartbeat message (Message ID 0x00)
// matching Stratux's status bits and FAA GDL90 Public ICD Rev A, section 3.1.
func EncodeHeartbeat(gpsValid, maintReq, utcOK bool, now time.Time, msgCountUplink, msgCountBasicLong uint32) []byte {
	msg := make([]byte, 7)
	msg[0] = MsgIDHeartbeat
	msg[1] = 0x01 // UAT Initialized.
	if gpsValid {
		msg[1] |= 0x80 // GPS Pos Valid.
	}
	msg[1] |= 0x10 // Addr talkback (Stratux convention).
	if maintReq {
		msg[1] |= 0x40 // Maintenance Req'd.
	}

	nowUTC := now.UTC()
	midnightUTC := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
	secondsSinceMidnightUTC := uint32(nowUTC.Sub(midnightUTC).Seconds())

	var utcBit byte
	if utcOK {
		utcBit = 0x01
	}
	msg[2] = byte(((secondsSinceMidnightUTC>>16)&0x01)<<7) | utcBit
	msg[3] = byte(secondsSinceMidnightUTC & 0xFF)
	msg[4] = byte((secondsSinceMidnightUTC >> 8) & 0xFF)

	// GDL90 ICD Rev A, section 3.1.4:
	// - Byte 5 bits 7..3: 5-bit Uplink message count (clamped to 31).
	// - Byte 5 bits 1..0 + Byte 6: 10-bit Basic & Long message count (clamped to 1023).
	if msgCountUplink > 31 {
		msgCountUplink = 31
	}
	if msgCountBasicLong > 1023 {
		msgCountBasicLong = 1023
	}
	msg[5] = byte(msgCountUplink<<3) | byte((msgCountBasicLong>>8)&0x03)
	msg[6] = byte(msgCountBasicLong & 0xFF)

	return PrepareMessage(msg)
}

// EncodeOwnshipGeoAltitude builds and frames a GDL90 Ownship Geometric Altitude
// message (Message ID 0x0B) per FAA GDL90 Public ICD Rev A, section 3.8.
// Altitude is encoded in 5-foot increments as a signed 16-bit big-endian value,
// clamped to [math.MinInt16, math.MaxInt16] (non-finite inputs become 0).
// vfomMeters uses the 15-bit GDL90 VFOM encoding (0x7FFF = not available; values
// above 0x7FFF are clamped to 0x7FFE).
func EncodeOwnshipGeoAltitude(altFt float32, verticalWarning bool, vfomMeters uint16) []byte {
	msg := make([]byte, 5)
	msg[0] = MsgIDOwnshipGeoAlt

	scaled := float64(altFt) / 5.0
	var encodedAlt int16
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) {
		encodedAlt = 0
	} else if scaled > math.MaxInt16 {
		encodedAlt = math.MaxInt16
	} else if scaled < math.MinInt16 {
		encodedAlt = math.MinInt16
	} else {
		encodedAlt = int16(scaled)
	}
	uAlt := uint16(encodedAlt)
	msg[1] = byte(uAlt >> 8)
	msg[2] = byte(uAlt & 0xFF)

	vfom := vfomMeters
	if vfom > VFOMNotAvailable {
		vfom = 0x7FFE
	}
	metrics := vfom & VFOMNotAvailable
	if verticalWarning {
		metrics |= 0x8000
	}
	msg[3] = byte(metrics >> 8)
	msg[4] = byte(metrics & 0xFF)

	return PrepareMessage(msg)
}
