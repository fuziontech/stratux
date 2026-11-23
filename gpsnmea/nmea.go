// Package gpsnmea implements pure, zero-CGO NMEA 0183 checksum validation and
// generation, callsign/tail sanitization for FLARM NMEA output, and GNSS
// satellite classification matching Stratux's GPS and FLARM parsers.
package gpsnmea

import (
	"fmt"
	"strconv"
	"strings"
)

// GNSS constellation identifiers returned by ClassifySatellite.
const (
	ConstellationUnknown = "UNKNOWN"
	ConstellationGPS     = "GPS"
	ConstellationSBAS    = "SBAS"
	ConstellationGLONASS = "GLONASS"
	ConstellationQZSS    = "QZSS"
	ConstellationGalileo = "GALILEO"
	ConstellationBeidou  = "BEIDOU"
)

// ValidateChecksum verifies an NMEA 0183 sentence of the form "$<body>*<XX>"
// (with optional trailing CR/LF). The checksum is the bitwise XOR of all bytes
// strictly between '$' and '*'. If valid, it returns (body, true); otherwise
// it returns ("", false).
func ValidateChecksum(sentence string) (body string, valid bool) {
	if !strings.HasPrefix(sentence, "$") {
		return "", false
	}
	trimmed := strings.TrimRight(sentence[1:], "\r\n")
	starIdx := strings.IndexByte(trimmed, '*')
	if starIdx < 0 {
		return "", false
	}
	// Reject additional '*' characters that indicate malformed or injected frames.
	if strings.IndexByte(trimmed[starIdx+1:], '*') >= 0 {
		return "", false
	}

	rawBody := trimmed[:starIdx]
	csStr := trimmed[starIdx+1:]
	if len(csStr) != 2 {
		return "", false
	}

	csExpected, err := strconv.ParseUint(csStr, 16, 8)
	if err != nil {
		return "", false
	}

	var calc byte
	for i := 0; i < len(rawBody); i++ {
		calc ^= rawBody[i]
	}
	if calc != byte(csExpected) {
		return "", false
	}
	return rawBody, true
}

// AppendChecksum computes the NMEA 0183 XOR checksum for sentence and appends
// "*XX" in uppercase hex. If sentence begins with '$', the leading '$' is
// excluded from the XOR calculation; if sentence is empty, it returns "*00".
func AppendChecksum(sentence string) string {
	if len(sentence) == 0 {
		return "*00"
	}
	start := 0
	if sentence[0] == '$' {
		start = 1
	}
	var checksum byte
	for i := start; i < len(sentence); i++ {
		checksum ^= sentence[i]
	}
	return fmt.Sprintf("%s*%02X", sentence, checksum)
}

// SanitizeTail strips NMEA delimiter characters (',', '*', '$') and all
// non-printable/non-ASCII bytes (< 0x20 or >= 0x7F, including '\r' and '\n')
// from aircraft callsigns before embedding them in NMEA sentences such as
// $PFLAA or $PFLAU.
func SanitizeTail(tail string) string {
	var b strings.Builder
	b.Grow(len(tail))
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c < 0x20 || c >= 0x7F || c == ',' || c == '*' || c == '$' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ClassifySatellite maps a numeric NMEA satellite ID (SV) to its GNSS
// constellation constant and constellation-local display ID, matching
// Stratux's NMEA GSV/GSA classification rules in main/gps.go.
func ClassifySatellite(sv int) (constellation string, displayID int) {
	if sv <= 0 {
		return ConstellationUnknown, sv
	} else if sv <= 32 {
		return ConstellationGPS, sv // GPS 1-32
	} else if sv <= 64 {
		return ConstellationSBAS, sv + 87 // SBAS 33-64 -> PRN 120-151
	} else if sv <= 96 {
		return ConstellationGLONASS, sv - 64 // GLONASS 65-96 -> 1-32
	} else if sv <= 158 {
		return ConstellationSBAS, sv // SBAS 152-158
	} else if sv <= 202 {
		return ConstellationQZSS, sv - 192 // QZSS 193-202 -> 1-10
	} else if sv <= 336 {
		return ConstellationGalileo, sv - 300 // Galileo 301-336 -> 1-36
	} else if sv <= 463 {
		return ConstellationBeidou, sv - 400 // BeiDou 401-463 -> 1-63
	}
	return ConstellationUnknown, sv
}

// FormatSatelliteID returns the short prefixed Stratux satellite label
// (e.g. "G12", "S138", "R5", "Q1", "E12", "B5", "U0") for an NMEA SV ID.
func FormatSatelliteID(sv int) string {
	c, id := ClassifySatellite(sv)
	prefix := "U"
	switch c {
	case ConstellationGPS:
		prefix = "G"
	case ConstellationSBAS:
		prefix = "S"
	case ConstellationGLONASS:
		prefix = "R"
	case ConstellationQZSS:
		prefix = "Q"
	case ConstellationGalileo:
		prefix = "E"
	case ConstellationBeidou:
		prefix = "B"
	}
	return fmt.Sprintf("%s%d", prefix, id)
}
