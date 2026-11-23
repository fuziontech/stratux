package uatparse

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func dlacEncode(s string) []byte {
	var vals []uint8
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(dlac_alpha, s[i])
		if idx < 0 {
			idx = strings.IndexByte(dlac_alpha, ' ')
		}
		vals = append(vals, uint8(idx))
	}
	var out []byte
	for i := 0; i < len(vals); i += 4 {
		var v [4]uint8
		n := len(vals) - i
		if n > 4 {
			n = 4
		}
		for j := 0; j < n; j++ {
			v[j] = vals[i+j]
		}
		out = append(out, (v[0]<<2)|(v[1]>>4))
		if n > 1 {
			out = append(out, ((v[1]&0x0f)<<4)|(v[2]>>2))
		}
		if n > 2 {
			out = append(out, ((v[2]&0x03)<<6)|v[3])
		}
	}
	return out
}

func TestNewValidation(t *testing.T) {
	validHex := strings.Repeat("00", UPLINK_FRAME_DATA_BYTES)

	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantRS  int
		wantSS  int
	}{
		{name: "empty", input: "", wantErr: true},
		{name: "missing semicolon", input: "+" + validHex, wantErr: true},
		{name: "empty before semicolon", input: ";rs=1;", wantErr: true},
		{name: "downlink frame rejected", input: "-" + strings.Repeat("aa", 18) + ";rs=0;ss=100;", wantErr: true},
		{name: "plus only", input: "+;rs=0;", wantErr: true},
		{name: "odd hex length", input: "+123;rs=0;", wantErr: true},
		{name: "oversized uplink", input: "+" + strings.Repeat("00", UPLINK_FRAME_DATA_BYTES+1) + ";rs=0;", wantErr: true},
		{name: "invalid hex chars", input: "+zzzz;rs=0;", wantErr: true},
		{name: "short uplink zero-padded", input: "+3cc0;rs=3;ss=420;", wantErr: false, wantRS: 3, wantSS: 420},
		{name: "full uplink", input: "+" + validHex + ";rs=5;ss=300;\r\n", wantErr: false, wantRS: 5, wantSS: 300},
		{name: "full uplink no metadata", input: "+" + validHex + ";", wantErr: false, wantRS: -1, wantSS: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := New(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("New(%q) succeeded unexpectedly", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) unexpected error: %v", tc.input, err)
			}
			if len(msg.msg) != UPLINK_FRAME_DATA_BYTES {
				t.Fatalf("len(msg.msg) = %d, want %d", len(msg.msg), UPLINK_FRAME_DATA_BYTES)
			}
			if msg.RS_Err != tc.wantRS {
				t.Errorf("RS_Err = %d, want %d", msg.RS_Err, tc.wantRS)
			}
			if msg.SignalStrength != tc.wantSS {
				t.Errorf("SignalStrength = %d, want %d", msg.SignalStrength, tc.wantSS)
			}
		})
	}
}

func TestDecodeUplinkBoundsAndFlags(t *testing.T) {
	t.Run("frame_length equals total_len does not panic", func(t *testing.T) {
		raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
		raw[6] = 0x20 // app_data_valid = true
		// frame_length = 424 (== total_len), encoded in 9 bits: data[0]<<1 | data[1]>>7
		raw[8] = byte(424 >> 1)
		raw[9] = byte((424 & 1) << 7)

		u := &UATMsg{msg: raw}
		if err := u.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink error: %v", err)
		}
		if len(u.Frames) != 0 {
			t.Fatalf("expected 0 frames for overrun length, got %d", len(u.Frames))
		}
		if !u.decoded {
			t.Errorf("expected u.decoded == true")
		}
	})

	t.Run("frame_length total_len-1 does not panic", func(t *testing.T) {
		raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
		raw[6] = 0x20
		// frame_length = 423 -> pos(0)+423+2 = 425 > 424
		raw[8] = byte(423 >> 1)
		raw[9] = byte((423 & 1) << 7)

		u := &UATMsg{msg: raw}
		if err := u.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink error: %v", err)
		}
		if len(u.Frames) != 0 {
			t.Fatalf("expected 0 frames, got %d", len(u.Frames))
		}
	})

	t.Run("app_data_valid false sets decoded true", func(t *testing.T) {
		raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
		raw[6] = 0x00 // app_data_valid = false
		u := &UATMsg{msg: raw}
		if err := u.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink error: %v", err)
		}
		if !u.decoded {
			t.Fatalf("expected u.decoded == true when app_data_valid is false")
		}
		reports, err := u.GetTextReports()
		if err != nil || len(reports) != 0 {
			t.Fatalf("GetTextReports() = (%v, %v), want empty, nil", reports, err)
		}
	})

	t.Run("num_info_frames cap enforced", func(t *testing.T) {
		raw := make([]byte, UPLINK_FRAME_DATA_BYTES)
		raw[6] = 0x20
		// Pack 100 3-byte frames (2-byte header + 1-byte payload, frame_length = 1, type = 15)
		pos := 8
		for i := 0; i < 100 && pos+3 <= len(raw); i++ {
			raw[pos] = 0x00
			raw[pos+1] = 0x80 | 0x0f // frame_length=1, frame_type=15
			raw[pos+2] = 0xaa
			pos += 3
		}
		u := &UATMsg{msg: raw}
		if err := u.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink error: %v", err)
		}
		if len(u.Frames) != UPLINK_MAX_INFO_FRAMES {
			t.Fatalf("len(u.Frames) = %d, want %d", len(u.Frames), UPLINK_MAX_INFO_FRAMES)
		}
	})
}

func TestDLACDecodeTabExpansionCap(t *testing.T) {
	// 28 = tab, 63 = max 6-bit repeat count.
	// Encode repeating (28, 63) pairs into 418 bytes (~278 pairs -> 17514 spaces uncapped).
	var payload []byte
	for len(payload) < 418 {
		// 4 6-bit chars: 28, 63, 28, 63 -> 3 bytes
		c0, c1, c2, c3 := byte(28), byte(63), byte(28), byte(63)
		payload = append(payload,
			(c0<<2)|(c1>>4),
			((c1&0x0f)<<4)|(c2>>2),
			((c2&0x03)<<6)|c3,
		)
	}
	decoded := dlac_decode(payload, uint32(len(payload)))
	if len(decoded) != maxDLACDecodedLen {
		t.Fatalf("len(decoded) = %d, want cap %d", len(decoded), maxDLACDecodedLen)
	}

	// Oversized data_len > len(data) must not panic.
	short := dlacEncode("METAR KORD")
	got := dlac_decode(short, uint32(len(short)+100))
	if !strings.HasPrefix(got, "METAR KORD") {
		t.Fatalf("dlac_decode got %q, want prefix %q", got, "METAR KORD")
	}
}

func TestDecodeTimeFormatSeconds6BitAndFlags(t *testing.T) {
	for _, sec := range []uint32{0, 1, 31, 32, 45, 59} {
		month := uint32(11)
		day := uint32(23)
		hours := uint32(15)
		minutes := uint32(42)

		// Byte 0: A=1 (0x80), G=0, P=1 (0x20), product_id=413 (top 5 bits: 413>>6 = 6)
		b0 := byte(0x80 | 0x20 | ((413 >> 6) & 0x1f))
		// Byte 1: product_id bottom 6 bits << 2 | S=0 | t_opt bit 1 = 1
		b1 := byte(((413 & 0x3f) << 2) | 0x01)
		// Byte 2: t_opt bit 0 = 1 (0x80) | month(4 bits)<<3 | day top 3 bits
		b2 := byte(0x80 | ((month & 0x0f) << 3) | ((day >> 2) & 0x07))
		// Byte 3: day bottom 2 bits << 6 | hours(5 bits)<<1 | minutes top 1 bit
		b3 := byte(((day & 0x03) << 6) | ((hours & 0x1f) << 1) | ((minutes >> 5) & 0x01))
		// Byte 4: minutes bottom 5 bits << 3 | seconds top 3 bits
		b4 := byte(((minutes & 0x1f) << 3) | ((sec >> 3) & 0x07))
		// Byte 5: seconds bottom 3 bits << 5
		b5 := byte((sec & 0x07) << 5)

		raw := []byte{b0, b1, b2, b3, b4, b5, 0x00}
		f := &UATFrame{
			Raw_data:     raw,
			frame_length: uint32(len(raw)),
			Frame_type:   0,
		}
		f.decodeTimeFormat()

		if f.FISB_month != month || f.FISB_day != day || f.FISB_hours != hours || f.FISB_minutes != minutes || f.FISB_seconds != sec {
			t.Errorf("sec=%d: got %02d-%02d %02d:%02d:%02d, want %02d-%02d %02d:%02d:%02d",
				sec, f.FISB_month, f.FISB_day, f.FISB_hours, f.FISB_minutes, f.FISB_seconds,
				month, day, hours, minutes, sec)
		}
		if !f.a_f || f.g_f || !f.p_f || f.s_f {
			t.Errorf("flags got a=%v g=%v p=%v s=%v, want true false true false", f.a_f, f.g_f, f.p_f, f.s_f)
		}
	}
}

func TestDecodeInfoFrameSegmented(t *testing.T) {
	// Product 413, t_opt=0 (28-bit time end -> +28-bit seg header = 56 bits = 7 bytes total),
	// s_f=1, ProductFileID=17, ProductFileLength=1, APDUNumber=1 + DLAC "HELLO"
	b0 := byte((413 >> 6) & 0x1f)
	b1 := byte(((413 & 0x3f) << 2) | 0x02) // s_f = 1, t_opt bit1 = 0
	b2 := byte(0x00)                       // t_opt bit0 = 0, hours=0, min=0
	// Bit 28..37 = fileID(10) = 17 (0b0000_010001)
	// Bit 38..46 = fileLen(9) = 1  (0b000000001)
	// Bit 47..55 = apduNum(9) = 1  (0b000000001)
	b3 := byte(0x00)             // min low 4 bits = 0, fileID[9:6] = 0000
	b4 := byte((17 << 2) | 0x00) // fileID[5:0]=010001, fileLen[8:7]=00
	b5 := byte(0x02)             // fileLen[6:0]=0000001 (bits 7..1), apduNum[8]=0 (bit 0)
	b6 := byte(0x01)             // apduNum[7:0]=00000001
	dlacText := dlacEncode("HELLO")

	raw := append([]byte{b0, b1, b2, b3, b4, b5, b6}, dlacText...)

	f := &UATFrame{
		Raw_data:     raw,
		frame_length: uint32(len(raw)),
		Frame_type:   0,
	}
	f.decodeInfoFrame()

	if !f.s_f {
		t.Fatalf("expected s_f == true")
	}
	if f.SegHeader == nil || f.SegHeader.ProductFileID != 17 || f.SegHeader.ProductFileLength != 1 || f.SegHeader.APDUNumber != 1 {
		t.Fatalf("unexpected SegHeader: %+v", f.SegHeader)
	}
	if !f.Reassembled {
		t.Fatalf("expected Reassembled == true")
	}
	if len(f.Text_data) == 0 || !strings.HasPrefix(f.Text_data[0], "HELLO") {
		t.Fatalf("segmented text got %v, want prefix HELLO", f.Text_data)
	}

	// Truncated segmented frame (< 7 bytes for t_opt=0) must not panic.
	trunc := []byte{b0, b1, b2, b3, 0x11, 0x22}
	fTrunc := &UATFrame{
		Raw_data:     trunc,
		frame_length: uint32(len(trunc)),
		Frame_type:   0,
	}
	fTrunc.decodeInfoFrame()
	if len(fTrunc.Text_data) != 0 {
		t.Errorf("expected no text for truncated segmented frame, got %v", fTrunc.Text_data)
	}
}

func TestDecodeAirmetBoundsAndDates(t *testing.T) {
	// Test record_format=2 with record_length < 5 (previously underflowed uint16).
	fShort := &UATFrame{
		FISB_data: []byte{0x20, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00},
	}
	fShort.decodeAirmet() // Must not panic.

	// Test record_format=8 with truncated inputs of all sizes 0..25.
	full := make([]byte, 40)
	full[0] = 0x80 // record_format = 8
	full[1] = 0x10 // record_count = 1
	for n := 0; n <= 25; n++ {
		f := &UATFrame{FISB_data: full[:n]}
		f.decodeAirmet() // Must not panic.
	}

	// Test record_format=8 with date_time_format=3 (2 bytes: HH:MM), applicability=3 (start+end),
	// geometry=9 (6-byte 3D point), object_label_flag=0 (high byte non-zero).
	buf := []byte{
		0x80, 0x10, 0x00, 0x00, 0x00, 0x00, // 6-byte APDU header
		0x00, 0x01, 0x02, 0x24, 0x00, // record_length, report_number, report_year, overlay_id, label_flag=0
		0x12, 0x34, // numeric object_label (2 bytes)
		0x00, 0x00, // element/qualifier=0/param=0, object_type/status
		0xf9,   // applicability=3 (0xc0), date_time_format=3 (0x30), geometry=9 (0x09)
		0x00,   // operator=0, vertices_count=1
		12, 30, // start time 12:30 (2 bytes for format 3)
		14, 45, // end time 14:45 (2 bytes for format 3)
		0x00, 0x00, 0x00, 0x00, 0x00, 0x0a, // 6-byte 3D point (alt=10*100=1000)
	}
	f8 := &UATFrame{FISB_data: buf}
	f8.decodeAirmet()
	if f8.ReportStart != "12:30" || f8.ReportEnd != "14:45" {
		t.Errorf("dates got start=%q end=%q, want 12:30 / 14:45", f8.ReportStart, f8.ReportEnd)
	}
	if len(f8.Points) != 1 || f8.Points[0].Alt != 1000 {
		t.Errorf("points got %+v, want 1 point with Alt=1000", f8.Points)
	}
}

func TestSampleDataExampleDump978(t *testing.T) {
	f, err := os.Open("../test-data/example.dump978")
	if err != nil {
		t.Skipf("sample data not available: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	uplinks := 0
	textReports := 0
	nexradBlocks := 0
	for scanner.Scan() && uplinks < 250 {
		line := scanner.Text()
		if !strings.HasPrefix(line, "+") {
			continue
		}
		u, err := New(line)
		if err != nil {
			t.Fatalf("New failed on sample line %d: %v", uplinks, err)
		}
		if err := u.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink failed on sample line %d: %v", uplinks, err)
		}
		reports, err := u.GetTextReports()
		if err != nil {
			t.Fatalf("GetTextReports failed: %v", err)
		}
		textReports += len(reports)
		for _, fr := range u.Frames {
			nexradBlocks += len(fr.NEXRAD)
		}
		uplinks++
	}
	if uplinks == 0 {
		t.Fatalf("no uplink frames read from sample data")
	}
	if textReports == 0 && nexradBlocks == 0 {
		t.Errorf("decoded %d uplinks but got 0 text reports and 0 NEXRAD blocks", uplinks)
	}
	// Sanity-check hex helper is used/valid.
	_ = hex.EncodedLen(1)
}
