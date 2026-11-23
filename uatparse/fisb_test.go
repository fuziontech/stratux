package uatparse

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// packBits writes bitLen bits of val into dst starting at big-endian bitOffset.
func packBits(dst []byte, bitOffset, bitLen int, val uint32) {
	for i := 0; i < bitLen; i++ {
		bit := (val >> uint(bitLen-1-i)) & 1
		pos := bitOffset + i
		if bit == 1 {
			dst[pos/8] |= 1 << uint(7-(pos%8))
		}
	}
}

// buildSegmentedInfoFrame constructs a complete FIS-B info frame (Raw_data) with s_f=1.
func buildSegmentedInfoFrame(productID uint16, tOpt uint32, hdr SegHeader, payload []byte) []byte {
	var timeEndBit int
	switch tOpt {
	case 0:
		timeEndBit = 28
	case 1:
		timeEndBit = 34
	case 2:
		timeEndBit = 37
	case 3:
		timeEndBit = 43
	}
	hdrBytes := (timeEndBit + 28 + 7) / 8
	buf := make([]byte, hdrBytes)
	packBits(buf, 3, 11, uint32(productID))
	packBits(buf, 14, 1, 1) // s_f = 1
	packBits(buf, 15, 2, tOpt)
	// Put a valid non-zero timestamp.
	switch tOpt {
	case 0:
		packBits(buf, 17, 5, 14) // hours
		packBits(buf, 22, 6, 30) // minutes
	case 1:
		packBits(buf, 17, 5, 14)
		packBits(buf, 22, 6, 30)
		packBits(buf, 28, 6, 15)
	case 2:
		packBits(buf, 17, 4, 9)  // month
		packBits(buf, 21, 5, 6)  // day
		packBits(buf, 26, 5, 14) // hours
		packBits(buf, 31, 6, 30) // minutes
	case 3:
		packBits(buf, 17, 4, 9)
		packBits(buf, 21, 5, 6)
		packBits(buf, 26, 5, 14)
		packBits(buf, 31, 6, 30)
		packBits(buf, 37, 6, 45)
	}
	packBits(buf, timeEndBit, 10, uint32(hdr.ProductFileID))
	packBits(buf, timeEndBit+10, 9, uint32(hdr.ProductFileLength))
	packBits(buf, timeEndBit+19, 9, uint32(hdr.APDUNumber))
	return append(buf, payload...)
}

// buildUplinkMsg wraps one or more FIS-B info frame Raw_data blobs into a 432-byte UAT uplink.
func buildUplinkMsg(infoFrames ...[]byte) *UATMsg {
	raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
	raw[6] = 0x20 // app_data_valid = true
	pos := 8
	for _, fr := range infoFrames {
		flen := len(fr)
		raw[pos] = byte(flen >> 1)
		raw[pos+1] = byte((flen & 1) << 7) // frame_type = 0 (FIS-B)
		copy(raw[pos+2:], fr)
		pos += 2 + flen
	}
	return &UATMsg{msg: raw}
}

// encode3DVertex encodes lat/lon (degrees) and altFt (feet) into 6-byte Extended Range 3D format.
func encode3DVertex(lat, lon float64, altFt int32) []byte {
	if lat < 0 {
		lat += 180.0
	}
	if lon < 0 {
		lon += 360.0
	}
	latRaw := uint32(math.Round(lat * 524288.0 / 360.0))
	lngRaw := uint32(math.Round(lon * 524288.0 / 360.0))
	altRaw := uint32(altFt / 100)
	b := make([]byte, 6)
	packBits(b, 0, 19, lngRaw)
	packBits(b, 19, 19, latRaw)
	packBits(b, 38, 10, altRaw)
	return b
}

// encode2DVertex encodes lat/lon (degrees) into 5-byte Extended Range 2D format.
func encode2DVertex(lat, lon float64) []byte {
	if lat < 0 {
		lat += 180.0
	}
	if lon < 0 {
		lon += 360.0
	}
	latRaw := uint32(math.Round(lat * 524288.0 / 360.0))
	lngRaw := uint32(math.Round(lon * 524288.0 / 360.0))
	b := make([]byte, 5)
	packBits(b, 0, 19, lngRaw)
	packBits(b, 19, 19, latRaw)
	return b
}

// encodeCircularPrism encodes a 14-byte Extended Range Circular Prism.
func encodeCircularPrism(lat, lon float64, altBotFt, altTopFt int32, radiusNM float64) []byte {
	if lat < 0 {
		lat += 180.0
	}
	if lon < 0 {
		lon += 360.0
	}
	latRaw := uint32(math.Round(lat * 262144.0 / 360.0))
	lngRaw := uint32(math.Round(lon * 262144.0 / 360.0))
	altBotRaw := uint32(altBotFt / 500)
	altTopRaw := uint32(altTopFt / 500)
	rRaw := uint32(math.Round(radiusNM / 0.2))
	b := make([]byte, 14)
	packBits(b, 0, 18, lngRaw)
	packBits(b, 18, 18, latRaw)
	packBits(b, 36, 18, lngRaw)
	packBits(b, 54, 18, latRaw)
	packBits(b, 72, 7, altBotRaw)
	packBits(b, 79, 7, altTopRaw)
	packBits(b, 86, 9, rRaw)
	packBits(b, 95, 9, rRaw)
	return b
}

// buildTextRecordAPDU builds a record_format=2 (DLAC) APDU payload.
func buildTextRecordAPDU(loc string, reportNum, reportYear uint16, text string) []byte {
	hdr := make([]byte, 6)
	hdr[0] = 0x22 // record_format=2, version=2
	hdr[1] = 0x10 // record_count=1
	if loc != "" {
		encLoc := dlacEncode(loc)
		for i := 0; i < 3 && i < len(encLoc); i++ {
			hdr[2+i] = encLoc[i]
		}
		hdr[5] = 0x00
	} else {
		hdr[5] = 0xFF
	}

	dlacPayload := dlacEncode(text + "\x03")
	recLen := uint16(5 + len(dlacPayload))
	recHdr := make([]byte, 5)
	recHdr[0] = byte(recLen >> 8)
	recHdr[1] = byte(recLen & 0xFF)
	recHdr[2] = byte(reportNum >> 6)
	recHdr[3] = byte(((reportNum & 0x3F) << 2) | ((reportYear >> 5) & 0x03))
	recHdr[4] = byte(((reportYear & 0x1F) << 3) | 0x04) // active=1

	out := append(hdr, recHdr...)
	return append(out, dlacPayload...)
}

// buildGraphicalOverlayAPDU builds a record_format=8 APDU payload with 1 record.
func buildGraphicalOverlayAPDU(loc string, reportNum, reportYear uint16, applicability, dtFmt, geomOpt uint8, dates []byte, vertexCount int, geomPayload []byte) []byte {
	hdr := make([]byte, 6)
	hdr[0] = 0x82 // record_format=8, version=2
	hdr[1] = 0x10 // record_count=1
	if loc != "" {
		encLoc := dlacEncode(loc)
		for i := 0; i < 3 && i < len(encLoc); i++ {
			hdr[2+i] = encLoc[i]
		}
		hdr[5] = 0x00
	} else {
		hdr[5] = 0xFF
	}

	body := make([]byte, 0, 64)
	body = append(body, 0x00, 0x00) // numeric object_label (2 bytes)
	body = append(body, 0x00, 0xEF) // element/qualifier=0/param=0, type/status
	body = append(body, ((applicability&0x03)<<6)|((dtFmt&0x03)<<4)|(geomOpt&0x0F))
	body = append(body, byte((vertexCount-1)&0x3F))
	body = append(body, dates...)
	body = append(body, geomPayload...)

	recLen := uint16(5 + len(body))
	recHdr := make([]byte, 5)
	recHdr[0] = byte(recLen >> 2)
	recHdr[1] = byte(((recLen & 0x03) << 6) | ((reportNum >> 8) & 0x3F))
	recHdr[2] = byte(reportNum & 0xFF)
	recHdr[3] = byte((reportYear & 0x7F) << 1)
	recHdr[4] = 0x00 // overlay_id=1, object_label_flag=0 (numeric)

	out := append(hdr, recHdr...)
	out = append(out, body...)
	return out
}

func TestParseSegHeaderStandaloneAndRaw(t *testing.T) {
	// Standalone 5-byte header.
	buf := make([]byte, 8)
	packBits(buf, 0, 10, 513)
	packBits(buf, 10, 9, 4)
	packBits(buf, 19, 9, 2)
	copy(buf[5:], []byte("ABC"))

	hdr, payload, ok := ParseSegHeader(buf)
	if !ok {
		t.Fatalf("ParseSegHeader failed unexpectedly")
	}
	if hdr.ProductFileID != 513 || hdr.ProductFileLength != 4 || hdr.APDUNumber != 2 {
		t.Fatalf("ParseSegHeader got %+v, want ID=513 Len=4 Num=2", hdr)
	}
	if string(payload) != "ABC" {
		t.Fatalf("payload got %q, want ABC", string(payload))
	}

	// Short/invalid standalone headers.
	if _, _, ok := ParseSegHeader(buf[:4]); ok {
		t.Errorf("expected short 4-byte buffer to fail")
	}
	invalid := make([]byte, 5) // fileLen=0, apduNum=0
	if _, _, ok := ParseSegHeader(invalid); ok {
		t.Errorf("expected zero fileLen/apduNum to fail")
	}
	packBits(invalid, 10, 9, 2)
	packBits(invalid, 19, 9, 3) // apduNum > fileLen
	if _, _, ok := ParseSegHeader(invalid); ok {
		t.Errorf("expected apduNum > fileLen to fail")
	}

	// All 4 t_opt values for ParseSegHeaderFromRaw.
	for _, tOpt := range []uint32{0, 1, 2, 3} {
		want := SegHeader{ProductFileID: 777, ProductFileLength: 5, APDUNumber: 3}
		raw := buildSegmentedInfoFrame(11, tOpt, want, []byte("PAYLOAD"))
		got, pay, ok := ParseSegHeaderFromRaw(raw, tOpt)
		if !ok {
			t.Fatalf("tOpt=%d: ParseSegHeaderFromRaw failed", tOpt)
		}
		if got != want {
			t.Errorf("tOpt=%d: got %+v, want %+v", tOpt, got, want)
		}
		if string(pay) != "PAYLOAD" {
			t.Errorf("tOpt=%d: payload got %q, want PAYLOAD", tOpt, string(pay))
		}
		// Truncated raw must fail cleanly.
		if _, _, ok := ParseSegHeaderFromRaw(raw[:5], tOpt); ok {
			t.Errorf("tOpt=%d: expected truncated frame to fail", tOpt)
		}
	}
}

func TestFISBReassemblerTwoAndThreePart(t *testing.T) {
	baseTime := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	t.Run("2-part in-order", func(t *testing.T) {
		r := NewFISBReassembler(15 * time.Minute)
		if got, done := r.AddSegment(413, SegHeader{ProductFileID: 10, ProductFileLength: 2, APDUNumber: 1}, []byte("PART1-"), baseTime); done || got != nil {
			t.Fatalf("seg 1 should not complete: done=%v got=%q", done, got)
		}
		if r.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", r.Len())
		}
		got, done := r.AddSegment(413, SegHeader{ProductFileID: 10, ProductFileLength: 2, APDUNumber: 2}, []byte("PART2"), baseTime.Add(time.Second))
		if !done {
			t.Fatalf("seg 2 should complete")
		}
		if string(got) != "PART1-PART2" {
			t.Fatalf("assembled = %q, want PART1-PART2", string(got))
		}
		if r.Len() != 0 {
			t.Fatalf("Len() after completion = %d, want 0", r.Len())
		}
	})

	t.Run("3-part out-of-order with duplicate segment", func(t *testing.T) {
		r := NewFISBReassembler(15 * time.Minute)
		// Deliver segment 3, then 1, then duplicate 3, then 2.
		if _, done := r.AddSegment(11, SegHeader{ProductFileID: 42, ProductFileLength: 3, APDUNumber: 3}, []byte("[THREE]"), baseTime); done {
			t.Fatalf("seg 3 alone should not complete")
		}
		if _, done := r.AddSegment(11, SegHeader{ProductFileID: 42, ProductFileLength: 3, APDUNumber: 1}, []byte("[ONE]"), baseTime.Add(time.Second)); done {
			t.Fatalf("segs 3+1 should not complete")
		}
		// Duplicate segment 3 must be idempotent and not corrupt or complete early.
		if _, done := r.AddSegment(11, SegHeader{ProductFileID: 42, ProductFileLength: 3, APDUNumber: 3}, []byte("[DUP]"), baseTime.Add(2*time.Second)); done {
			t.Fatalf("duplicate seg 3 should not complete")
		}
		got, done := r.AddSegment(11, SegHeader{ProductFileID: 42, ProductFileLength: 3, APDUNumber: 2}, []byte("[TWO]"), baseTime.Add(3*time.Second))
		if !done {
			t.Fatalf("final missing seg 2 should complete")
		}
		if string(got) != "[ONE][TWO][THREE]" {
			t.Fatalf("assembled = %q, want [ONE][TWO][THREE]", string(got))
		}
	})

	t.Run("TTL eviction via Prune and lazy eviction", func(t *testing.T) {
		r := NewFISBReassembler(10 * time.Minute)
		_, done := r.AddSegment(8, SegHeader{ProductFileID: 100, ProductFileLength: 2, APDUNumber: 1}, []byte("STALE"), baseTime)
		if done || r.Len() != 1 {
			t.Fatalf("expected 1 buffered file, got done=%v len=%d", done, r.Len())
		}
		// Before TTL -> still buffered.
		r.Prune(baseTime.Add(9*time.Minute + 59*time.Second))
		if r.Len() != 1 {
			t.Fatalf("expected file retained before TTL, len=%d", r.Len())
		}
		// At TTL -> evicted.
		r.Prune(baseTime.Add(10 * time.Minute))
		if r.Len() != 0 {
			t.Fatalf("expected file evicted at TTL, len=%d", r.Len())
		}
		// Arriving segment 2 after eviction should NOT complete with evicted segment 1.
		if _, done := r.AddSegment(8, SegHeader{ProductFileID: 100, ProductFileLength: 2, APDUNumber: 2}, []byte("LATE"), baseTime.Add(11*time.Minute)); done {
			t.Fatalf("segment 2 after TTL eviction should not complete")
		}
	})

	t.Run("DoS protection: oversized payload, max buffered files, malformed headers", func(t *testing.T) {
		r := NewFISBReassembler(15 * time.Minute)

		// Malformed headers must be rejected cleanly.
		for _, bad := range []SegHeader{
			{ProductFileID: 1, ProductFileLength: 0, APDUNumber: 1},
			{ProductFileID: 1, ProductFileLength: 2, APDUNumber: 0},
			{ProductFileID: 1, ProductFileLength: 2, APDUNumber: 3},
			{ProductFileID: 1, ProductFileLength: 600, APDUNumber: 1},
		} {
			if _, done := r.AddSegment(8, bad, []byte("x"), baseTime); done {
				t.Errorf("expected bad header %+v rejected", bad)
			}
		}
		if _, done := r.AddSegment(8, SegHeader{ProductFileID: 1, ProductFileLength: 1, APDUNumber: 1}, nil, baseTime); done {
			t.Errorf("expected empty payload rejected")
		}

		// Oversized reassembly > 64 KiB must be rejected and evicted.
		chunk := bytes.Repeat([]byte("A"), 40*1024) // 40 KB
		if _, done := r.AddSegment(8, SegHeader{ProductFileID: 9, ProductFileLength: 2, APDUNumber: 1}, chunk, baseTime); done {
			t.Fatalf("first 40KB chunk should not complete")
		}
		if _, done := r.AddSegment(8, SegHeader{ProductFileID: 9, ProductFileLength: 2, APDUNumber: 2}, chunk, baseTime.Add(time.Second)); done {
			t.Fatalf("80KB total must be rejected (> 64KB cap)")
		}
		if r.Len() != 0 {
			t.Fatalf("oversized file should be purged from buffer, len=%d", r.Len())
		}

		// Max concurrent buffered files cap (256) must never be exceeded.
		for i := 0; i < DefaultFISBMaxBufferedFiles+50; i++ {
			r.AddSegment(8, SegHeader{ProductFileID: uint16(i % 1024), ProductFileLength: 2, APDUNumber: 1}, []byte("seg"), baseTime.Add(time.Duration(i)*time.Millisecond))
		}
		if r.Len() > DefaultFISBMaxBufferedFiles {
			t.Fatalf("buffered files = %d, exceeded cap %d", r.Len(), DefaultFISBMaxBufferedFiles)
		}
	})
}

func TestFISBReassemblerConcurrentRace(t *testing.T) {
	r := NewFISBReassembler(5 * time.Minute)
	now := time.Now().UTC()

	var wg sync.WaitGroup
	const workers = 16
	const filesPerWorker = 20

	completedCount := int64(0)
	var countMu sync.Mutex

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < filesPerWorker; i++ {
				fid := uint16((workerID*filesPerWorker + i) % 1024)
				p1 := []byte(fmt.Sprintf("W%d-F%d-P1|", workerID, i))
				p2 := []byte(fmt.Sprintf("W%d-F%d-P2|", workerID, i))
				p3 := []byte(fmt.Sprintf("W%d-F%d-P3", workerID, i))
				// Interleave segments out-of-order.
				if _, ok := r.AddSegment(uint16(11+workerID%3), SegHeader{ProductFileID: fid, ProductFileLength: 3, APDUNumber: 2}, p2, now); ok {
					countMu.Lock()
					completedCount++
					countMu.Unlock()
				}
				if i%5 == 0 {
					r.Prune(now.Add(time.Minute))
					_ = r.Len()
				}
				if _, ok := r.AddSegment(uint16(11+workerID%3), SegHeader{ProductFileID: fid, ProductFileLength: 3, APDUNumber: 3}, p3, now); ok {
					countMu.Lock()
					completedCount++
					countMu.Unlock()
				}
				if out, ok := r.AddSegment(uint16(11+workerID%3), SegHeader{ProductFileID: fid, ProductFileLength: 3, APDUNumber: 1}, p1, now); ok {
					want := string(p1) + string(p2) + string(p3)
					if string(out) != want {
						t.Errorf("corrupted reassembly: got %q, want %q", string(out), want)
					}
					countMu.Lock()
					completedCount++
					countMu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	if completedCount == 0 {
		t.Fatalf("expected concurrent reassemblies to complete")
	}
}

func TestDecodeUplinkWithReassemblerMultiSegmentEndToEnd(t *testing.T) {
	r := NewFISBReassembler(15 * time.Minute)

	// Build a Product 8 TFR NOTAM text APDU and split it into 3 segments (repeating 6-byte APDU header like real FAA broadcasts).
	fullAPDU := buildTextRecordAPDU("KORD", 5711, 26, "NOTAM-TFR KORD 5/5711 TEMPORARY FLIGHT RESTRICTIONS WITHIN 30NM RADIUS SFC TO FL180 EFFECTIVE IMMEDIATELY UNTIL FURTHER NOTICE")
	hdr6 := fullAPDU[:6]
	body := fullAPDU[6:]
	cut1 := len(body) / 3
	cut2 := 2 * len(body) / 3

	seg1 := append(append([]byte(nil), hdr6...), body[:cut1]...)
	seg2 := append(append([]byte(nil), hdr6...), body[cut1:cut2]...)
	seg3 := append(append([]byte(nil), hdr6...), body[cut2:]...)

	// Deliver out-of-order: seg 2, seg 3, seg 1.
	u2 := buildUplinkMsg(buildSegmentedInfoFrame(8, 2, SegHeader{ProductFileID: 301, ProductFileLength: 3, APDUNumber: 2}, seg2))
	u3 := buildUplinkMsg(buildSegmentedInfoFrame(8, 2, SegHeader{ProductFileID: 301, ProductFileLength: 3, APDUNumber: 3}, seg3))
	u1 := buildUplinkMsg(buildSegmentedInfoFrame(8, 2, SegHeader{ProductFileID: 301, ProductFileLength: 3, APDUNumber: 1}, seg1))

	if err := u2.DecodeUplinkWithReassembler(r); err != nil {
		t.Fatalf("u2 decode error: %v", err)
	}
	if prods, _ := u2.GetGraphicalProducts(); len(prods) != 0 {
		t.Fatalf("incomplete seg 2 should yield 0 products, got %d", len(prods))
	}

	if err := u3.DecodeUplinkWithReassembler(r); err != nil {
		t.Fatalf("u3 decode error: %v", err)
	}
	if prods, _ := u3.GetGraphicalProducts(); len(prods) != 0 {
		t.Fatalf("incomplete seg 3 should yield 0 products, got %d", len(prods))
	}

	if err := u1.DecodeUplinkWithReassembler(r); err != nil {
		t.Fatalf("u1 decode error: %v", err)
	}
	prods, err := u1.GetGraphicalProducts()
	if err != nil {
		t.Fatalf("GetGraphicalProducts error: %v", err)
	}
	if len(prods) != 1 {
		t.Fatalf("expected 1 reassembled GraphicalProduct, got %d", len(prods))
	}
	gp := prods[0]
	if gp.ProductID != 8 || gp.ProductName != "TFR/NOTAM" {
		t.Errorf("got ProductID=%d Name=%q, want 8 TFR/NOTAM", gp.ProductID, gp.ProductName)
	}
	if gp.ReportNumber != 5711 || gp.Location != "KORD" {
		t.Errorf("got ReportNumber=%d Location=%q, want 5711 KORD", gp.ReportNumber, gp.Location)
	}
	if !strings.Contains(gp.Text, "TEMPORARY FLIGHT RESTRICTIONS") || !strings.Contains(gp.Text, "UNTIL FURTHER NOTICE") {
		t.Errorf("reassembled text incomplete: %q", gp.Text)
	}
	if !u1.Frames[0].Reassembled {
		t.Errorf("expected frame.Reassembled == true")
	}
}

func TestGraphicalProductDecodingPolygonsCirclesPointsText(t *testing.T) {
	refTime := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)

	t.Run("G-AIRMET Product 14 3D Polygon with top+bottom rings and GeoJSON [lon, lat]", func(t *testing.T) {
		// 3D prism polygon: 4 vertices at 24,000 ft + same 4 vertices at 10,000 ft.
		pts := [][2]float64{
			{41.0, -85.0},
			{40.0, -86.5},
			{39.5, -84.0},
			{41.0, -85.0},
		}
		var geom []byte
		for _, p := range pts {
			geom = append(geom, encode3DVertex(p[0], p[1], 24000)...)
		}
		for _, p := range pts {
			geom = append(geom, encode3DVertex(p[0], p[1], 10000)...)
		}
		// Start: Sep 06 14:00, End: Sep 06 20:00 (date_time_format=1: Mo,D,H,M)
		dates := []byte{9, 6, 14, 0, 9, 6, 20, 0}
		apdu := buildGraphicalOverlayAPDU("ZAU", 1234, 26, 3, 1, 3, dates, 8, geom)

		f := &UATFrame{Product_id: 14, FISB_data: apdu, FISB_length: uint32(len(apdu))}
		f.decodeAirmetAt(refTime)

		if len(f.GraphicalProducts) != 1 {
			t.Fatalf("expected 1 GraphicalProduct, got %d", len(f.GraphicalProducts))
		}
		gp := f.GraphicalProducts[0]
		if gp.ProductID != 14 || gp.ProductName != "G-AIRMET" {
			t.Errorf("got ID=%d Name=%q, want 14 G-AIRMET", gp.ProductID, gp.ProductName)
		}
		if gp.GeometryType != "polygon" {
			t.Errorf("GeometryType = %q, want polygon", gp.GeometryType)
		}
		// Top+bottom identical rings collapsed to 4 2D vertices.
		if len(gp.Coordinates) != 4 {
			t.Fatalf("len(Coordinates) = %d, want 4", len(gp.Coordinates))
		}
		// Verify GeoJSON [lon, lat] order!
		if math.Abs(gp.Coordinates[0][0]-(-85.0)) > 0.01 || math.Abs(gp.Coordinates[0][1]-41.0) > 0.01 {
			t.Errorf("coord[0] = %v, want [-85.0, 41.0] (GeoJSON [lon, lat])", gp.Coordinates[0])
		}
		if gp.AltLowFt != 10000 || gp.AltHighFt != 24000 {
			t.Errorf("Alt = %d..%d, want 10000..24000", gp.AltLowFt, gp.AltHighFt)
		}
		wantStart := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
		wantEnd := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
		if !gp.ValidStart.Equal(wantStart) || !gp.ValidEnd.Equal(wantEnd) {
			t.Errorf("valid times got %v..%v, want %v..%v", gp.ValidStart, gp.ValidEnd, wantStart, wantEnd)
		}
		if gp.Key == "" {
			t.Errorf("expected non-empty dedup Key")
		}

		// Verify clean JSON serialization round-trip.
		encoded, err := json.Marshal(gp)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		var roundTrip GraphicalProduct
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		if roundTrip.ProductName != gp.ProductName || roundTrip.AltHighFt != gp.AltHighFt || len(roundTrip.Coordinates) != 4 {
			t.Errorf("JSON round-trip mismatch: %+v", roundTrip)
		}
	})

	t.Run("TFR Product 8 Circular Prism MSL", func(t *testing.T) {
		prism := encodeCircularPrism(42.55, -83.02, 0, 18000, 30.0)
		dates := []byte{23, 30, 1, 30} // format 3 (H,M) wrapping midnight
		apdu := buildGraphicalOverlayAPDU("KDTW", 5960, 26, 3, 3, 7, dates, 1, prism)

		f := &UATFrame{Product_id: 8, FISB_data: apdu, FISB_length: uint32(len(apdu))}
		f.decodeAirmetAt(refTime)

		if len(f.GraphicalProducts) != 1 {
			t.Fatalf("expected 1 GraphicalProduct, got %d", len(f.GraphicalProducts))
		}
		gp := f.GraphicalProducts[0]
		if gp.GeometryType != "circle" {
			t.Errorf("GeometryType = %q, want circle", gp.GeometryType)
		}
		if len(gp.Coordinates) != 1 || math.Abs(gp.Coordinates[0][0]-(-83.02)) > 0.01 || math.Abs(gp.Coordinates[0][1]-42.55) > 0.01 {
			t.Errorf("circle center = %v, want [-83.02, 42.55]", gp.Coordinates)
		}
		if math.Abs(gp.RadiusNM-30.0) > 0.25 {
			t.Errorf("RadiusNM = %v, want 30.0", gp.RadiusNM)
		}
		if gp.AltLowFt != 0 || gp.AltHighFt != 18000 {
			t.Errorf("Alt = %d..%d, want 0..18000", gp.AltLowFt, gp.AltHighFt)
		}
		if !gp.ValidEnd.After(gp.ValidStart) {
			t.Errorf("midnight wrap failed: start=%v end=%v", gp.ValidStart, gp.ValidEnd)
		}
	})

	t.Run("Convective SIGMET Product 12, SUA 13, CWA 15, Turbulence 16, Icing 17, Lightning 20, Cloud Tops 25/26", func(t *testing.T) {
		cases := []struct {
			pid      uint16
			text     string
			wantName string
		}{
			{12, "CONVECTIVE SIGMET 42C VALID UNTIL 1955Z", "Convective SIGMET"},
			{12, "SIGMET NOVEMBER 2 VALID UNTIL 2100Z", "SIGMET"},
			{13, "SUA R-2508 ACTIVE SFC-FL600", "SUA"},
			{15, "CWA ZOB 101 VALID UNTIL 1800Z", "Center Weather Advisory"},
			{16, "MOD-SEV TURB FL240-FL380", "Turbulence"},
			{17, "MOD RIME ICING 080-160", "Icing"},
			{20, "LIGHTNING STRIKES DETECTED", "Lightning"},
			{25, "CLOUD TOPS FL350", "Cloud Tops"},
			{26, "CLOUD TOPS FL410", "Cloud Tops"},
		}
		for _, tc := range cases {
			apdu := buildTextRecordAPDU("KDEN", 100, 26, tc.text)
			f := &UATFrame{Product_id: uint32(tc.pid), FISB_data: apdu, FISB_length: uint32(len(apdu))}
			f.decodeAirmetAt(refTime)
			if len(f.GraphicalProducts) != 1 {
				t.Fatalf("pid=%d: expected 1 product, got %d", tc.pid, len(f.GraphicalProducts))
			}
			if f.GraphicalProducts[0].ProductName != tc.wantName {
				t.Errorf("pid=%d: ProductName=%q, want %q", tc.pid, f.GraphicalProducts[0].ProductName, tc.wantName)
			}
			if f.GraphicalProducts[0].GeometryType != "text" {
				t.Errorf("pid=%d: GeometryType=%q, want text", tc.pid, f.GraphicalProducts[0].GeometryType)
			}
		}
	})

	t.Run("2D Polygon (geom 1) and 2D Point (geom 11)", func(t *testing.T) {
		var poly2d []byte
		poly2d = append(poly2d, encode2DVertex(38.0, -104.0)...)
		poly2d = append(poly2d, encode2DVertex(39.0, -105.0)...)
		poly2d = append(poly2d, encode2DVertex(38.5, -103.5)...)
		apdu := buildGraphicalOverlayAPDU("ZDV", 200, 26, 0, 0, 1, nil, 3, poly2d)
		f := &UATFrame{Product_id: 16, FISB_data: apdu, FISB_length: uint32(len(apdu))}
		f.decodeAirmetAt(refTime)
		if len(f.GraphicalProducts) != 1 || f.GraphicalProducts[0].GeometryType != "polygon" || len(f.GraphicalProducts[0].Coordinates) != 3 {
			t.Fatalf("2D polygon decode failed: %+v", f.GraphicalProducts)
		}

		pt2d := encode2DVertex(35.2, -97.4)
		apduPt := buildGraphicalOverlayAPDU("KOKC", 201, 26, 0, 0, 11, nil, 1, pt2d)
		fPt := &UATFrame{Product_id: 20, FISB_data: apduPt, FISB_length: uint32(len(apduPt))}
		fPt.decodeAirmetAt(refTime)
		if len(fPt.GraphicalProducts) != 1 || fPt.GraphicalProducts[0].GeometryType != "point" || fPt.GraphicalProducts[0].ProductName != "Lightning" {
			t.Fatalf("2D point decode failed: %+v", fPt.GraphicalProducts)
		}
		if math.Abs(fPt.GraphicalProducts[0].Coordinates[0][0]-(-97.4)) > 0.01 || math.Abs(fPt.GraphicalProducts[0].Coordinates[0][1]-35.2) > 0.01 {
			t.Errorf("point coord = %v, want [-97.4, 35.2]", fPt.GraphicalProducts[0].Coordinates[0])
		}
	})

	t.Run("Product 413 text-extracted PIREP and Winds Aloft", func(t *testing.T) {
		text := "PIREP KORD 1520Z UA /OV ORD/TM 1520/FL120/TP C172/TB MOD\x1eWINDS KORD 061200Z 3000 2715 6000 2820+05\x1eMETAR KORD 061551Z 27010KT 10SM CLR\x03"
		rawPayload := dlacEncode(text)
		// Unsegmented Product 413 frame (t_opt=0, 4-byte header).
		b0 := byte((413 >> 6) & 0x1f)
		b1 := byte((413 & 0x3f) << 2)
		raw := append([]byte{b0, b1, 0x00, 0x00}, rawPayload...)

		u := buildUplinkMsg(raw)
		prods, err := u.GetGraphicalProducts()
		if err != nil {
			t.Fatalf("GetGraphicalProducts error: %v", err)
		}
		if len(prods) != 2 {
			t.Fatalf("expected 2 products (PIREP + Winds Aloft), got %d: %+v", len(prods), prods)
		}
		if prods[0].ProductName != "PIREP" || prods[0].Location != "KORD" || prods[0].AltLowFt != 12000 {
			t.Errorf("unexpected PIREP product: %+v", prods[0])
		}
		if prods[1].ProductName != "Winds Aloft" || prods[1].Location != "KORD" {
			t.Errorf("unexpected Winds Aloft product: %+v", prods[1])
		}
	})

	t.Run("Real multi-record 4-prism FAA TFR APDU", func(t *testing.T) {
		// Real captured 4-record Circular Prism MSL TFR from cyoung-09062015-noproblem-stratux-uat.log line 4523.
		rawHex := "8240000000ff0857c80a00000080efd7400909110f0909151ec50087914c5008791400914aa5000857c80a02000080efd7400909110f0909121ec519c7935c519c793500906432000857c80a04000080efd7400909120f09091500c4f9478e8c4f9478e800906e37000857c80a06000080efd7400909140f0909151ec519c7935c519c79350090643200"
		apdu, err := hex.DecodeString(rawHex)
		if err != nil {
			t.Fatalf("hex decode failed: %v", err)
		}
		f := &UATFrame{Product_id: 8, FISB_data: apdu, FISB_length: uint32(len(apdu))}
		f.decodeAirmetAt(refTime)
		if len(f.GraphicalProducts) != 4 {
			t.Fatalf("expected 4 circular prism products, got %d", len(f.GraphicalProducts))
		}
		for i, gp := range f.GraphicalProducts {
			if gp.GeometryType != "circle" || gp.AltHighFt != 18000 || gp.RadiusNM <= 0 {
				t.Errorf("prism[%d] unexpected: %+v", i, gp)
			}
		}
		// First prism is 33 NM radius near Detroit (42.55N, -83.02W).
		if math.Abs(f.GraphicalProducts[0].RadiusNM-33.0) > 0.1 {
			t.Errorf("prism[0] RadiusNM = %v, want 33.0", f.GraphicalProducts[0].RadiusNM)
		}
	})
}

func TestSampleDataGraphicalAndSegmentedReassembly(t *testing.T) {
	t.Run("example.dump978 graphical products", func(t *testing.T) {
		f, err := os.Open("../test-data/example.dump978")
		if err != nil {
			t.Skipf("sample data not available: %v", err)
		}
		defer f.Close()

		r := NewFISBReassembler(15 * time.Minute)
		scanner := bufio.NewScanner(f)
		byGeom := make(map[string]int)
		byProd := make(map[string]int)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "+") {
				continue
			}
			u, err := New(line)
			if err != nil {
				continue
			}
			if err := u.DecodeUplinkWithReassembler(r); err != nil {
				t.Fatalf("DecodeUplinkWithReassembler error: %v", err)
			}
			prods, err := u.GetGraphicalProducts()
			if err != nil {
				t.Fatalf("GetGraphicalProducts error: %v", err)
			}
			for _, p := range prods {
				byGeom[p.GeometryType]++
				byProd[p.ProductName]++
				if p.Key == "" {
					t.Errorf("empty Key for product %+v", p)
				}
			}
		}
		if byGeom["polygon"] == 0 || byGeom["point"] == 0 || byGeom["text"] == 0 {
			t.Errorf("expected polygons, points, and text products from example.dump978, got %v", byGeom)
		}
		if byProd["AIRMET"] == 0 || byProd["SIGMET"]+byProd["Convective SIGMET"] == 0 || byProd["TFR/NOTAM"] == 0 || byProd["SUA"] == 0 {
			t.Errorf("expected AIRMET/SIGMET/TFR/SUA from example.dump978, got %v", byProd)
		}
	})

	t.Run("cyoung real multi-segment TFR reassembly", func(t *testing.T) {
		f, err := os.Open("../test-data/cyoung-09062015-noproblem-stratux-uat.log")
		if err != nil {
			t.Skipf("cyoung sample log not available: %v", err)
		}
		defer f.Close()

		r := NewFISBReassembler(15 * time.Minute)
		scanner := bufio.NewScanner(f)
		reassembledCount := 0
		circleCount := 0
		for scanner.Scan() {
			line := scanner.Text()
			idx := strings.IndexByte(line, '+')
			if idx < 0 {
				continue
			}
			u, err := New(line[idx:])
			if err != nil {
				continue
			}
			if err := u.DecodeUplinkWithReassembler(r); err != nil {
				continue
			}
			for _, fr := range u.Frames {
				if fr.Reassembled && len(fr.GraphicalProducts) > 0 {
					reassembledCount++
				}
				for _, gp := range fr.GraphicalProducts {
					if gp.GeometryType == "circle" {
						circleCount++
					}
				}
			}
		}
		if reassembledCount == 0 {
			t.Errorf("expected real multi-segment FAA NOTAM/TFRs to reassemble in cyoung log, got 0")
		}
		if circleCount == 0 {
			t.Errorf("expected circular prism TFRs in cyoung log, got 0")
		}
	})
}

func TestGraphicalAndReassemblyFuzzNoPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	r := NewFISBReassembler(15 * time.Minute)
	now := time.Now().UTC()

	// Fuzz decodeAirmet across all record formats, product IDs, and truncated lengths.
	for i := 0; i < 500; i++ {
		n := rng.Intn(128)
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = byte(rng.Intn(256))
		}
		pid := []uint32{8, 11, 12, 13, 14, 15, 16, 17, 20, 25, 26, 413}[i%12]
		f := &UATFrame{
			Product_id:   pid,
			Raw_data:     buf,
			frame_length: uint32(len(buf)),
			FISB_data:    buf,
			FISB_length:  uint32(len(buf)),
			Frame_type:   0,
		}
		f.decodeAirmetAt(now)
		f.decodeInfoFrameWithReassembler(r, now)
		_, _, _ = ParseSegHeader(buf)
		_, _, _ = ParseSegHeaderFromRaw(buf, uint32(i%5))
	}

	// Fuzz full UATMsg DecodeUplinkWithReassembler + GetGraphicalProducts.
	for i := 0; i < 100; i++ {
		raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
		for j := range raw {
			raw[j] = byte(rng.Intn(256))
		}
		raw[6] |= 0x20
		u := &UATMsg{msg: raw}
		_ = u.DecodeUplinkWithReassembler(r)
		_, _ = u.GetGraphicalProducts()
		_, _ = u.GetTextReports()
	}
}
