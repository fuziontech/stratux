package uatparse

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"
)

// GraphicalProduct is a structured, JSON-serializable representation of a decoded
// FIS-B graphical or advisory weather product (AIRMET, G-AIRMET, SIGMET, Convective SIGMET,
// SUA, TFR/NOTAM, CWA, Turbulence, Icing, Lightning, Cloud Tops, PIREP, Winds Aloft).
// Coordinates are ordered [lon, lat] (GeoJSON / OpenLayers order).
type GraphicalProduct struct {
	ProductID    uint16      `json:"ProductID"`
	ProductName  string      `json:"ProductName"`
	ReportNumber uint16      `json:"ReportNumber"`
	Location     string      `json:"Location"`
	ValidStart   time.Time   `json:"ValidStart"`
	ValidEnd     time.Time   `json:"ValidEnd"`
	AltLowFt     int32       `json:"AltLowFt"`
	AltHighFt    int32       `json:"AltHighFt"`
	Text         string      `json:"Text"`
	GeometryType string      `json:"GeometryType"` // "polygon", "circle", "point", "text"
	Coordinates  [][]float64 `json:"Coordinates"`  // [lon, lat] GeoJSON order
	RadiusNM     float64     `json:"RadiusNM"`
	ReceivedAt   time.Time   `json:"ReceivedAt"`
	Key          string      `json:"Key"`
}

// FISBProductName returns the human-readable product name for a FIS-B product ID.
func FISBProductName(productID uint16) string {
	switch productID {
	case 8:
		return "TFR/NOTAM"
	case 11:
		return "AIRMET"
	case 12:
		return "SIGMET"
	case 13:
		return "SUA"
	case 14:
		return "G-AIRMET"
	case 15:
		return "Center Weather Advisory"
	case 16:
		return "Turbulence"
	case 17:
		return "Icing"
	case 20:
		return "Lightning"
	case 25, 26:
		return "Cloud Tops"
	case 413:
		return "Text"
	default:
		return fmt.Sprintf("FIS-B %d", productID)
	}
}

func isGraphicalProductID(productID uint32) bool {
	switch productID {
	case 8, 11, 12, 13, 14, 15, 16, 17, 20, 25, 26:
		return true
	default:
		return false
	}
}

func classifyProductName(productID uint16, text string) string {
	if productID == 12 {
		upper := strings.ToUpper(text)
		if strings.Contains(upper, "CONVECTIVE") || strings.Contains(upper, "WST") {
			return "Convective SIGMET"
		}
	}
	return FISBProductName(productID)
}

func cleanDLACString(s string) string {
	s = strings.ReplaceAll(s, "\x03", "")
	s = strings.ReplaceAll(s, "\x1a", "")
	s = strings.ReplaceAll(s, "\x1e", "\n")
	return strings.TrimSpace(s)
}

func cleanTextLines(lines []string) string {
	cleaned := make([]string, 0, len(lines))
	for _, l := range lines {
		t := cleanDLACString(l)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	return strings.Join(cleaned, "\n")
}

func sanitizeASCIIText(b []byte) []string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c == 0x03 || c == 0x00 {
			sb.WriteByte('\n')
		} else if c == '\n' || c == '\r' || c == '\t' || (c >= 0x20 && c <= 0x7e) {
			sb.WriteByte(c)
		}
	}
	raw := strings.ReplaceAll(sb.String(), "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func hashString32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// airmetParseDate parses a FIS-B graphical date/time field into a formatted string
// and returns the number of bytes consumed (or -1 if truncated).
func airmetParseDate(b []byte, date_time_format uint8) (string, int) {
	switch date_time_format {
	case 0: // No date/time used.
		return "", 0
	case 1: // Month, Day, Hours, Minutes.
		if len(b) < 4 {
			return "", -1
		}
		month := uint8(b[0])
		day := uint8(b[1])
		hours := uint8(b[2])
		minutes := uint8(b[3])
		return fmt.Sprintf("%02d-%02d %02d:%02d", month, day, hours, minutes), 4
	case 2: // Day, Hours, Minutes.
		if len(b) < 3 {
			return "", -1
		}
		day := uint8(b[0])
		hours := uint8(b[1])
		minutes := uint8(b[2])
		return fmt.Sprintf("%02d %02d:%02d", day, hours, minutes), 3
	case 3: // Hours, Minutes.
		if len(b) < 2 {
			return "", -1
		}
		hours := uint8(b[0])
		minutes := uint8(b[1])
		return fmt.Sprintf("%02d:%02d", hours, minutes), 2
	}

	return "", 0
}

// airmetParseTime parses a FIS-B graphical date/time field into both a UTC time.Time
// (resolved relative to refTime) and the formatted string, returning bytes consumed or -1 on truncation.
func airmetParseTime(b []byte, dateTimeFormat uint8, refTime time.Time) (time.Time, string, int) {
	str, n := airmetParseDate(b, dateTimeFormat)
	if n <= 0 {
		return time.Time{}, str, n
	}

	ref := refTime.UTC()
	if ref.IsZero() {
		ref = time.Now().UTC()
	}

	switch dateTimeFormat {
	case 1: // Month, Day, Hours, Minutes
		month, day, hours, minutes := int(b[0]), int(b[1]), int(b[2]), int(b[3])
		if month < 1 || month > 12 || day < 1 || day > 31 || hours > 23 || minutes > 59 {
			return time.Time{}, str, n
		}
		year := ref.Year()
		if month == 1 && ref.Month() == time.December {
			year++
		} else if month == 12 && ref.Month() == time.January {
			year--
		}
		return time.Date(year, time.Month(month), day, hours, minutes, 0, 0, time.UTC), str, n
	case 2: // Day, Hours, Minutes
		day, hours, minutes := int(b[0]), int(b[1]), int(b[2])
		if day < 1 || day > 31 || hours > 23 || minutes > 59 {
			return time.Time{}, str, n
		}
		year, month := ref.Year(), ref.Month()
		return time.Date(year, month, day, hours, minutes, 0, 0, time.UTC), str, n
	case 3: // Hours, Minutes
		hours, minutes := int(b[0]), int(b[1])
		if hours > 23 || minutes > 59 {
			return time.Time{}, str, n
		}
		return time.Date(ref.Year(), ref.Month(), ref.Day(), hours, minutes, 0, 0, time.UTC), str, n
	default:
		return time.Time{}, str, n
	}
}

// airmetLatLng converts raw 19-bit (alt=false) or 18-bit (alt=true) FIS-B graphical
// coordinates into WGS-84 degrees latitude and longitude.
func airmetLatLng(lat_raw, lng_raw int32, alt bool) (float64, float64) {
	fct := 360.0 / 524288.0 // 360 / 2^19 (~0.0006866455 deg)
	if alt {
		fct = 360.0 / 262144.0 // 360 / 2^18 (~0.0013732910 deg)
	}
	lat := fct * float64(lat_raw)
	lng := fct * float64(lng_raw)
	if lat > 90.0 {
		lat = lat - 180.0
	}
	if lng > 180.0 {
		lng = lng - 360.0
	}
	return lat, lng
}

// decode3DVertex extracts a 6-byte Extended Range 3D vertex (19-bit lon, 19-bit lat, 10-bit alt*100ft).
func decode3DVertex(b []byte) (lat, lon float64, alt int32) {
	lngRaw := (int32(b[0]) << 11) | (int32(b[1]) << 3) | ((int32(b[2]) & 0xE0) >> 5)
	latRaw := ((int32(b[2]) & 0x1F) << 14) | (int32(b[3]) << 6) | ((int32(b[4]) & 0xFC) >> 2)
	altRaw := ((int32(b[4]) & 0x03) << 8) | int32(b[5])
	lat, lon = airmetLatLng(latRaw, lngRaw, false)
	return lat, lon, altRaw * 100
}

// decode2DVertex extracts a 5-byte Extended Range 2D vertex (19-bit lon, 19-bit lat).
func decode2DVertex(b []byte) (lat, lon float64) {
	lngRaw := (int32(b[0]) << 11) | (int32(b[1]) << 3) | ((int32(b[2]) & 0xE0) >> 5)
	latRaw := ((int32(b[2]) & 0x1F) << 14) | (int32(b[3]) << 6) | ((int32(b[4]) & 0xFC) >> 2)
	return airmetLatLng(latRaw, lngRaw, false)
}

// collapse3DRingPair detects FAA 3D airspace polygons where vertices 0..k-1 and k..2k-1
// have identical 2D coordinates (representing top and bottom faces of a 3D prism) and
// returns a single clean 2D [lon, lat] ring.
func collapse3DRingPair(coords [][]float64) [][]float64 {
	n := len(coords)
	if n < 6 || n%2 != 0 {
		return coords
	}
	half := n / 2
	for i := 0; i < half; i++ {
		if math.Abs(coords[i][0]-coords[half+i][0]) > 1e-4 || math.Abs(coords[i][1]-coords[half+i][1]) > 1e-4 {
			return coords
		}
	}
	return coords[:half]
}

// decodeAirmet decodes FIS-B Aerodrome/Airspace & Graphical products (IDs 8, 11-17, 20, 25, 26)
// using the current UTC time as reference.
func (f *UATFrame) decodeAirmet() {
	f.decodeAirmetAt(time.Now().UTC())
}

// decodeAirmetAt decodes FIS-B Aerodrome/Airspace & Graphical products (IDs 8, 11-17, 20, 25, 26)
// safely with full bounds checking, populating both legacy UATFrame fields and GraphicalProducts.
func (f *UATFrame) decodeAirmetAt(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	data := f.FISB_data
	if f.FISB_length > 0 && int(f.FISB_length) < len(data) {
		data = data[:f.FISB_length]
	}

	// APDU payload header: 48 bits (6 bytes).
	if len(data) < 6 {
		return
	}

	recordFormat := (data[0] & 0xF0) >> 4
	f.RecordFormat = recordFormat
	recordCount := int((data[1] & 0xF0) >> 4)
	rawLoc := dlac_decode(data[2:5], 3)
	f.LocationIdentifier = rawLoc
	cleanLoc := cleanDLACString(rawLoc)

	if recordCount == 0 {
		return
	}

	recData := data[6:]
	prodID := uint16(f.Product_id)

	switch recordFormat {
	case 1, 2: // 1 = Unformatted ASCII Text, 2 = Unformatted DLAC Text
		for recIdx := 0; recIdx < recordCount && len(recData) >= 5; recIdx++ {
			recLen := int((uint16(recData[0]) << 8) | uint16(recData[1]))
			if recLen < 5 || len(recData) < recLen {
				break
			}

			reportNumber := (uint16(recData[2]) << 6) | ((uint16(recData[3]) & 0xFC) >> 2)
			reportYear := ((uint16(recData[3]) & 0x03) << 5) | ((uint16(recData[4]) & 0xF8) >> 3)
			f.ReportNumber = reportNumber
			f.ReportYear = reportYear

			textPayload := recData[5:recLen]
			var lines []string
			if recordFormat == 2 {
				decoded := dlac_decode(textPayload, uint32(len(textPayload)))
				lines = formatDLACData(decoded)
			} else {
				lines = sanitizeASCIIText(textPayload)
			}
			f.Text_data = append(f.Text_data, lines...)

			cleanText := cleanTextLines(lines)
			if cleanText != "" {
				key := fmt.Sprintf("fisb:%d:%d:%d:text:%d", prodID, reportYear, reportNumber, recIdx)
				if reportNumber == 0 {
					key = fmt.Sprintf("fisb:%d:%s:text:%08x", prodID, cleanLoc, hashString32(cleanText))
				}
				f.GraphicalProducts = append(f.GraphicalProducts, GraphicalProduct{
					ProductID:    prodID,
					ProductName:  classifyProductName(prodID, cleanText),
					ReportNumber: reportNumber,
					Location:     cleanLoc,
					Text:         cleanText,
					GeometryType: "text",
					ReceivedAt:   now,
					Key:          key,
				})
			}

			recData = recData[recLen:]
		}

	case 8: // Graphical Overlay Record Format (Aero_FISB_ProdDef_Rev4 §6.22)
		for recIdx := 0; recIdx < recordCount && len(recData) >= 5; recIdx++ {
			recLen := int((uint16(recData[0]) << 2) | ((uint16(recData[1]) & 0xC0) >> 6))

			var thisRec []byte
			var nextRec []byte
			if recLen >= 5 && recLen <= len(recData) {
				thisRec = recData[:recLen]
				nextRec = recData[recLen:]
			} else if recordCount == 1 {
				// Fallback for single-record payloads where record_length is zero or omitted.
				thisRec = recData
				nextRec = nil
			} else {
				break
			}

			reportNumber := ((uint16(thisRec[1]) & 0x3F) << 8) | uint16(thisRec[2])
			reportYear := (uint16(thisRec[3]) & 0xFE) >> 1
			overlayID := ((thisRec[4] & 0x1E) >> 1) + 1
			objectLabelFlag := thisRec[4] & 0x01

			f.ReportNumber = reportNumber
			f.ReportYear = reportYear

			loc := cleanLoc
			var cur []byte
			if objectLabelFlag == 0 {
				if len(thisRec) < 7 {
					recData = nextRec
					continue
				}
				cur = thisRec[7:]
			} else {
				if len(thisRec) < 14 {
					recData = nextRec
					continue
				}
				if lbl := cleanDLACString(dlac_decode(thisRec[5:14], 9)); lbl != "" && loc == "" {
					loc = lbl
				}
				cur = thisRec[14:]
			}

			if len(cur) < 2 {
				recData = nextRec
				continue
			}
			qualifierFlag := (cur[0] & 0x40) >> 6
			if qualifierFlag == 0 {
				cur = cur[2:]
			} else {
				if len(cur) < 5 {
					recData = nextRec
					continue
				}
				cur = cur[5:]
			}

			if len(cur) < 2 {
				recData = nextRec
				continue
			}
			applicability := (cur[0] & 0xC0) >> 6
			dateTimeFormat := (cur[0] & 0x30) >> 4
			geomOption := cur[0] & 0x0F
			vertexCount := int(cur[1]&0x3F) + 1
			cur = cur[2:]

			var validStart, validEnd time.Time
			var startStr, endStr string
			truncatedDate := false
			switch applicability {
			case 0: // No times given (UFN).
			case 1: // Start time only (WEF).
				t1, s1, n1 := airmetParseTime(cur, dateTimeFormat, now)
				if n1 < 0 {
					truncatedDate = true
					break
				}
				validStart, startStr = t1, s1
				f.ReportStart = startStr
				cur = cur[n1:]
			case 2: // End time only (TIL).
				t2, s2, n2 := airmetParseTime(cur, dateTimeFormat, now)
				if n2 < 0 {
					truncatedDate = true
					break
				}
				validEnd, endStr = t2, s2
				f.ReportEnd = endStr
				cur = cur[n2:]
			case 3: // Both start and end times.
				t1, s1, n1 := airmetParseTime(cur, dateTimeFormat, now)
				if n1 < 0 {
					truncatedDate = true
					break
				}
				cur = cur[n1:]
				t2, s2, n2 := airmetParseTime(cur, dateTimeFormat, now)
				if n2 < 0 {
					truncatedDate = true
					break
				}
				validStart, startStr = t1, s1
				validEnd, endStr = t2, s2
				if !validStart.IsZero() && !validEnd.IsZero() && validEnd.Before(validStart) {
					switch dateTimeFormat {
					case 3:
						validEnd = validEnd.Add(24 * time.Hour)
					case 2:
						validEnd = validEnd.AddDate(0, 1, 0)
					case 1:
						validEnd = validEnd.AddDate(1, 0, 0)
					}
				}
				f.ReportStart = startStr
				f.ReportEnd = endStr
				cur = cur[n2:]
			}
			if truncatedDate {
				recData = nextRec
				continue
			}

			baseProduct := GraphicalProduct{
				ProductID:    prodID,
				ProductName:  FISBProductName(prodID),
				ReportNumber: reportNumber,
				Location:     loc,
				ValidStart:   validStart,
				ValidEnd:     validEnd,
				ReceivedAt:   now,
			}

			switch geomOption {
			case 1, 2, 5, 6: // 2D Polygon or Polyline (5 bytes per vertex)
				if len(cur) < 5*vertexCount {
					break
				}
				points := make([]GeoPoint, 0, vertexCount)
				coords := make([][]float64, 0, vertexCount)
				for i := 0; i < vertexCount; i++ {
					lat, lon := decode2DVertex(cur[5*i : 5*i+5])
					points = append(points, GeoPoint{Lat: lat, Lon: lon})
					coords = append(coords, []float64{lon, lat})
				}
				f.Points = append(f.Points, points...)
				gp := baseProduct
				gp.GeometryType = "polygon"
				gp.Coordinates = coords
				gp.Key = fmt.Sprintf("fisb:%d:%d:%d:poly2d:%d:%d", prodID, reportYear, reportNumber, overlayID, recIdx)
				f.GraphicalProducts = append(f.GraphicalProducts, gp)

			case 3, 4, 12, 13: // 3D Polygon or Polyline (6 bytes per vertex)
				if len(cur) < 6*vertexCount {
					break
				}
				points := make([]GeoPoint, 0, vertexCount)
				coords := make([][]float64, 0, vertexCount)
				var altLow, altHigh int32
				for i := 0; i < vertexCount; i++ {
					lat, lon, alt := decode3DVertex(cur[6*i : 6*i+6])
					if i == 0 || alt < altLow {
						altLow = alt
					}
					if i == 0 || alt > altHigh {
						altHigh = alt
					}
					points = append(points, GeoPoint{Lat: lat, Lon: lon, Alt: alt})
					coords = append(coords, []float64{lon, lat})
				}
				f.Points = append(f.Points, points...)
				gp := baseProduct
				gp.GeometryType = "polygon"
				gp.Coordinates = collapse3DRingPair(coords)
				gp.AltLowFt = altLow
				gp.AltHighFt = altHigh
				gp.Key = fmt.Sprintf("fisb:%d:%d:%d:poly3d:%d:%d", prodID, reportYear, reportNumber, overlayID, recIdx)
				f.GraphicalProducts = append(f.GraphicalProducts, gp)

			case 7, 8: // Extended Range Circular Prism (7 = MSL, 8 = AGL; 14 bytes per prism)
				for p := 0; p < vertexCount; p++ {
					if len(cur) < 14*(p+1) {
						break
					}
					seg := cur[14*p : 14*(p+1)]
					lngBotRaw := (int32(seg[0]) << 10) | (int32(seg[1]) << 2) | ((int32(seg[2]) & 0xC0) >> 6)
					latBotRaw := ((int32(seg[2]) & 0x3F) << 12) | (int32(seg[3]) << 4) | ((int32(seg[4]) & 0xF0) >> 4)

					altBotRaw := (int32(seg[9]) & 0xFE) >> 1
					altTopRaw := ((int32(seg[9]) & 0x01) << 6) | ((int32(seg[10]) & 0xFC) >> 2)

					rLngRaw := ((int32(seg[10]) & 0x03) << 7) | ((int32(seg[11]) & 0xFE) >> 1)
					rLatRaw := ((int32(seg[11]) & 0x01) << 8) | int32(seg[12])

					latBot, lngBot := airmetLatLng(latBotRaw, lngBotRaw, true)
					altBot := altBotRaw * 500
					altTop := altTopRaw * 500
					altLow, altHigh := altBot, altTop
					if altLow > altHigh {
						altLow, altHigh = altHigh, altLow
					}

					rLng := float64(rLngRaw) * 0.2
					rLat := float64(rLatRaw) * 0.2
					radiusNM := (rLng + rLat) / 2.0
					if rLng == 0 {
						radiusNM = rLat
					} else if rLat == 0 {
						radiusNM = rLng
					}

					f.Points = append(f.Points, GeoPoint{Lat: latBot, Lon: lngBot, Alt: altLow})
					gp := baseProduct
					gp.GeometryType = "circle"
					gp.Coordinates = [][]float64{{lngBot, latBot}}
					gp.RadiusNM = math.Round(radiusNM*100) / 100
					gp.AltLowFt = altLow
					gp.AltHighFt = altHigh
					gp.Key = fmt.Sprintf("fisb:%d:%d:%d:circle:%d:%d:%d", prodID, reportYear, reportNumber, overlayID, recIdx, p)
					f.GraphicalProducts = append(f.GraphicalProducts, gp)
				}

			case 9, 10: // Extended Range 3D Point (9 = AGL, 10 = MSL; 6 bytes per point)
				for p := 0; p < vertexCount; p++ {
					if len(cur) < 6*(p+1) {
						break
					}
					lat, lon, alt := decode3DVertex(cur[6*p : 6*p+6])
					f.Points = append(f.Points, GeoPoint{Lat: lat, Lon: lon, Alt: alt})
					gp := baseProduct
					gp.GeometryType = "point"
					gp.Coordinates = [][]float64{{lon, lat}}
					gp.AltLowFt = alt
					gp.AltHighFt = alt
					gp.Key = fmt.Sprintf("fisb:%d:%d:%d:point3d:%d:%d:%d", prodID, reportYear, reportNumber, overlayID, recIdx, p)
					f.GraphicalProducts = append(f.GraphicalProducts, gp)
				}

			case 11: // Extended Range 2D Point (5 bytes per point)
				for p := 0; p < vertexCount; p++ {
					if len(cur) < 5*(p+1) {
						break
					}
					lat, lon := decode2DVertex(cur[5*p : 5*p+5])
					f.Points = append(f.Points, GeoPoint{Lat: lat, Lon: lon})
					gp := baseProduct
					gp.GeometryType = "point"
					gp.Coordinates = [][]float64{{lon, lat}}
					gp.Key = fmt.Sprintf("fisb:%d:%d:%d:point2d:%d:%d:%d", prodID, reportYear, reportNumber, overlayID, recIdx, p)
					f.GraphicalProducts = append(f.GraphicalProducts, gp)
				}
			}

			recData = nextRec
		}
	}
}

// extractTextGraphicalProducts converts PIREP and Winds Aloft text reports decoded from
// Product 413 frames into structured GraphicalProduct entries.
func (f *UATFrame) extractTextGraphicalProducts(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	for _, raw := range f.Text_data {
		line := cleanDLACString(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		var productName string
		var keyTag string
		switch fields[0] {
		case "PIREP", "UA", "UUA":
			productName = "PIREP"
			keyTag = "pirep"
		case "WINDS":
			productName = "Winds Aloft"
			keyTag = "winds"
		default:
			continue
		}

		loc := fields[1]
		var altFt int32
		if productName == "PIREP" {
			altFt = parsePIREPAltitudeFt(line)
		}

		f.GraphicalProducts = append(f.GraphicalProducts, GraphicalProduct{
			ProductID:    uint16(f.Product_id),
			ProductName:  productName,
			Location:     loc,
			AltLowFt:     altFt,
			AltHighFt:    altFt,
			Text:         line,
			GeometryType: "text",
			ReceivedAt:   now,
			Key:          fmt.Sprintf("fisb:%d:%s:%s:%08x", f.Product_id, keyTag, loc, hashString32(line)),
		})
	}
}

// parsePIREPAltitudeFt extracts flight level / altitude in feet from a PIREP `/FLnnn` group.
func parsePIREPAltitudeFt(text string) int32 {
	idx := strings.Index(text, "/FL")
	if idx < 0 {
		return 0
	}
	rest := text[idx+3:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	fl, err := strconv.Atoi(rest[:end])
	if err != nil || fl < 0 || fl > 999 {
		return 0
	}
	return int32(fl * 100)
}

// GetGraphicalProducts aggregates all decoded structured graphical FIS-B products
// across all frames in the uplink message.
func (u *UATMsg) GetGraphicalProducts() ([]GraphicalProduct, error) {
	ret := make([]GraphicalProduct, 0)
	if !u.decoded {
		if err := u.DecodeUplink(); err != nil {
			return ret, err
		}
	}

	for _, f := range u.Frames {
		if len(f.GraphicalProducts) > 0 {
			ret = append(ret, f.GraphicalProducts...)
		}
	}

	return ret, nil
}
