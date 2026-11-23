package uatparse

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	UPLINK_BLOCK_DATA_BITS  = 576
	UPLINK_BLOCK_BITS       = (UPLINK_BLOCK_DATA_BITS + 160)
	UPLINK_BLOCK_DATA_BYTES = (UPLINK_BLOCK_DATA_BITS / 8)
	UPLINK_BLOCK_BYTES      = (UPLINK_BLOCK_BITS / 8)

	UPLINK_FRAME_BLOCKS     = 6
	UPLINK_FRAME_DATA_BITS  = (UPLINK_FRAME_BLOCKS * UPLINK_BLOCK_DATA_BITS)
	UPLINK_FRAME_BITS       = (UPLINK_FRAME_BLOCKS * UPLINK_BLOCK_BITS)
	UPLINK_FRAME_DATA_BYTES = (UPLINK_FRAME_DATA_BITS / 8)
	UPLINK_FRAME_BYTES      = (UPLINK_FRAME_BITS / 8)

	// assume 6 byte frames: 2 header bytes, 4 byte payload
	// (TIS-B heartbeat with one address, or empty FIS-B APDU)
	UPLINK_MAX_INFO_FRAMES = (424 / 6)

	dlac_alpha = "\x03ABCDEFGHIJKLMNOPQRSTUVWXYZ\x1A\t\x1E\n| !\"#$%&'()*+,-./0123456789:;<=>?"

	maxDLACDecodedLen = 8192
)

type UATFrame struct {
	Raw_data     []byte
	FISB_data    []byte
	FISB_month   uint32
	FISB_day     uint32
	FISB_hours   uint32
	FISB_minutes uint32
	FISB_seconds uint32

	FISB_length uint32

	frame_length uint32
	Frame_type   uint32

	Product_id uint32
	// Text data, if applicable.
	Text_data []string

	// Flags.
	a_f   bool
	g_f   bool
	p_f   bool
	s_f   bool
	t_opt uint32

	// Segmentation metadata (non-nil when s_f is true and header parsed cleanly).
	SegHeader   *SegHeader
	Reassembled bool

	// For AIRMET/NOTAM.
	//FIXME: Temporary.
	Points             []GeoPoint
	ReportNumber       uint16
	ReportYear         uint16
	LocationIdentifier string
	RecordFormat       uint8
	ReportStart        string
	ReportEnd          string

	// Structured graphical FIS-B products decoded from this frame.
	GraphicalProducts []GraphicalProduct

	// For NEXRAD.
	NEXRAD []NEXRADBlock
}

type UATMsg struct {
	// Metadata from demodulation.
	RS_Err         int
	SignalStrength int
	msg            []byte
	decoded        bool
	// Station location for uplink frames, aircraft position for downlink frames.
	Lat    float64
	Lon    float64
	Frames []*UATFrame
}

func dlac_decode(data []byte, data_len uint32) string {
	if int(data_len) > len(data) {
		data_len = uint32(len(data))
	}
	step := 0
	tab := false
	var sb strings.Builder
	for i := uint32(0); i < data_len && sb.Len() < maxDLACDecodedLen; i++ {
		var ch uint32
		switch step {
		case 0:
			ch = uint32(data[i+0]) >> 2
		case 1:
			ch = ((uint32(data[i-1]) & 0x03) << 4) | (uint32(data[i+0]) >> 4)
		case 2:
			ch = ((uint32(data[i-1]) & 0x0f) << 2) | (uint32(data[i+0]) >> 6)
			i = i - 1
		case 3:
			ch = uint32(data[i+0]) & 0x3f
		}
		if tab {
			for ch > 0 && sb.Len() < maxDLACDecodedLen {
				sb.WriteByte(' ')
				ch--
			}
			tab = false
		} else if ch == 28 { // tab
			tab = true
		} else {
			sb.WriteByte(dlac_alpha[ch])
		}
		step = (step + 1) % 4
	}
	return sb.String()
}

// Decodes the time format and aligns 'FISB_data' accordingly.
// TODO: Make a new "FISB Time" structure that also encodes the type of timestamp received.
// TODO: pass up error.
func (f *UATFrame) decodeTimeFormat() {
	if len(f.Raw_data) < 3 {
		return // Can't determine time format.
	}

	t_opt := ((uint32(f.Raw_data[1]) & 0x01) << 1) | (uint32(f.Raw_data[2]) >> 7)

	var fisb_data []byte
	switch t_opt {
	case 0: // Hours, Minutes.
		if f.frame_length < 4 || len(f.Raw_data) < 4 {
			return
		}
		f.FISB_hours = (uint32(f.Raw_data[2]) & 0x7c) >> 2
		f.FISB_minutes = ((uint32(f.Raw_data[2]) & 0x03) << 4) | (uint32(f.Raw_data[3]) >> 4)
		f.FISB_length = f.frame_length - 4
		fisb_data = f.Raw_data[4:]
	case 1: // Hours, Minutes, Seconds.
		if f.frame_length < 5 || len(f.Raw_data) < 5 {
			return
		}
		f.FISB_hours = (uint32(f.Raw_data[2]) & 0x7c) >> 2
		f.FISB_minutes = ((uint32(f.Raw_data[2]) & 0x03) << 4) | (uint32(f.Raw_data[3]) >> 4)
		f.FISB_seconds = ((uint32(f.Raw_data[3]) & 0x0f) << 2) | (uint32(f.Raw_data[4]) >> 6)
		f.FISB_length = f.frame_length - 5
		fisb_data = f.Raw_data[5:]
	case 2: // Month, Day, Hours, Minutes.
		if f.frame_length < 5 || len(f.Raw_data) < 5 {
			return
		}
		f.FISB_month = (uint32(f.Raw_data[2]) & 0x78) >> 3
		f.FISB_day = ((uint32(f.Raw_data[2]) & 0x07) << 2) | (uint32(f.Raw_data[3]) >> 6)
		f.FISB_hours = (uint32(f.Raw_data[3]) & 0x3e) >> 1
		f.FISB_minutes = ((uint32(f.Raw_data[3]) & 0x01) << 5) | (uint32(f.Raw_data[4]) >> 3)
		f.FISB_length = f.frame_length - 5
		fisb_data = f.Raw_data[5:]
	case 3: // Month, Day, Hours, Minutes, Seconds.
		if f.frame_length < 6 || len(f.Raw_data) < 6 {
			return
		}
		f.FISB_month = (uint32(f.Raw_data[2]) & 0x78) >> 3
		f.FISB_day = ((uint32(f.Raw_data[2]) & 0x07) << 2) | (uint32(f.Raw_data[3]) >> 6)
		f.FISB_hours = (uint32(f.Raw_data[3]) & 0x3e) >> 1
		f.FISB_minutes = ((uint32(f.Raw_data[3]) & 0x01) << 5) | (uint32(f.Raw_data[4]) >> 3)
		f.FISB_seconds = ((uint32(f.Raw_data[4]) & 0x07) << 3) | (uint32(f.Raw_data[5]) >> 5)
		f.FISB_length = f.frame_length - 6
		fisb_data = f.Raw_data[6:]
	default:
		return // Should never reach this.
	}

	f.FISB_data = fisb_data
	f.t_opt = t_opt

	f.a_f = (f.Raw_data[0] & 0x80) != 0
	f.g_f = (f.Raw_data[0] & 0x40) != 0
	f.p_f = (f.Raw_data[0] & 0x20) != 0
	f.s_f = (f.Raw_data[1] & 0x02) != 0
}

// Format newlines.
func formatDLACData(p string) []string {
	ret := make([]string, 0)
	for {
		pos := strings.Index(p, "\x1E")
		if pos == -1 {
			pos = strings.Index(p, "\x03")
			if pos == -1 {
				ret = append(ret, p)
				break
			}
		}
		ret = append(ret, p[:pos])
		p = p[pos+1:]
	}
	return ret
}

// Whole frame contents is DLAC encoded text.
func (f *UATFrame) decodeTextFrame() {
	if len(f.FISB_data) < int(f.FISB_length) {
		return
	}

	p := dlac_decode(f.FISB_data, f.FISB_length)

	f.Text_data = formatDLACData(p)
}

func (f *UATFrame) decodeInfoFrame() {
	f.decodeInfoFrameWithReassembler(defaultFISBReassembler, time.Now().UTC())
}

func (f *UATFrame) decodeInfoFrameWithReassembler(r *FISBReassembler, now time.Time) {
	if len(f.Raw_data) < 2 {
		return // Can't determine Product_id.
	}

	f.Product_id = ((uint32(f.Raw_data[0]) & 0x1f) << 6) | (uint32(f.Raw_data[1]) >> 2)

	if f.Frame_type != 0 {
		return // Not FIS-B.
	}

	f.decodeTimeFormat()

	if f.s_f {
		raw := f.Raw_data
		if f.frame_length > 0 && int(f.frame_length) < len(raw) {
			raw = raw[:f.frame_length]
		}
		hdr, payload, ok := ParseSegHeaderFromRaw(raw, f.t_opt)
		if !ok {
			return
		}
		f.SegHeader = &hdr
		if r == nil {
			r = defaultFISBReassembler
		}
		assembled, complete := r.AddSegment(uint16(f.Product_id), hdr, payload, now)
		if !complete {
			f.FISB_data = payload
			f.FISB_length = uint32(len(payload))
			return
		}
		f.FISB_data = assembled
		f.FISB_length = uint32(len(assembled))
		f.Reassembled = true
	}

	switch f.Product_id {
	case 413:
		f.decodeTextFrame()
		f.extractTextGraphicalProducts(now)
	case 8, 11, 12, 13, 14, 15, 16, 17, 20, 25, 26:
		f.decodeAirmetAt(now)
	case 63, 64:
		f.decodeNexradFrame()
	default:
		fmt.Fprintf(io.Discard, "don't know what to do with product id: %d\n", f.Product_id)
	}
}

func (u *UATMsg) DecodeUplink() error {
	return u.DecodeUplinkWithReassembler(defaultFISBReassembler)
}

func (u *UATMsg) DecodeUplinkWithReassembler(r *FISBReassembler) error {
	//	position_valid := (uint32(frame[5]) & 0x01) != 0
	frame := u.msg

	if len(frame) < UPLINK_FRAME_DATA_BYTES {
		return fmt.Errorf("DecodeUplink: short read (%d).", len(frame))
	}

	raw_lat := (uint32(frame[0]) << 15) | (uint32(frame[1]) << 7) | (uint32(frame[2]) >> 1)

	raw_lon := ((uint32(frame[2]) & 0x01) << 23) | (uint32(frame[3]) << 15) | (uint32(frame[4]) << 7) | (uint32(frame[5]) >> 1)
	lat := float64(raw_lat) * 360.0 / 16777216.0
	lon := float64(raw_lon) * 360.0 / 16777216.0

	if lat > 90 {
		lat = lat - 180
	}
	if lon > 180 {
		lon = lon - 360
	}

	u.Lat = lat
	u.Lon = lon

	//	utc_coupled := (uint32(frame[6]) & 0x80) != 0
	app_data_valid := (uint32(frame[6]) & 0x20) != 0
	//	slot_id := uint32(frame[6]) & 0x1f
	//	tisb_site_id := uint32(frame[7]) >> 4

	//	logger.Printf("position_valid=%t, %.04f, %.04f, %t, %t, %d, %d\n", position_valid, lat, lon, utc_coupled, app_data_valid, slot_id, tisb_site_id)

	if !app_data_valid {
		u.decoded = true
		return nil // Not sure when this even happens?
	}

	now := time.Now().UTC()
	app_data := frame[8:432]
	num_info_frames := 0
	pos := 0
	total_len := len(app_data)
	for (num_info_frames < UPLINK_MAX_INFO_FRAMES) && (pos+2 <= total_len) {
		data := app_data[pos:]
		frame_length := (uint32(data[0]) << 1) | (uint32(data[1]) >> 7)
		frame_type := uint32(data[1]) & 0x0f
		if pos+int(frame_length)+2 > total_len {
			break // Overrun?
		}

		if frame_length == 0 { // Empty frame. Quit here.
			break
		}

		pos = pos + 2

		data = data[2 : frame_length+2]

		thisFrame := new(UATFrame)
		thisFrame.Raw_data = data
		thisFrame.frame_length = frame_length
		thisFrame.Frame_type = frame_type

		thisFrame.decodeInfoFrameWithReassembler(r, now)

		// Save the decoded frame.
		u.Frames = append(u.Frames, thisFrame)

		pos = pos + int(frame_length)
		num_info_frames++
	}

	u.decoded = true
	return nil
}

/*
	Aggregate all of the text rates across the frames in the message and return as an array.
*/

func (u *UATMsg) GetTextReports() ([]string, error) {
	ret := make([]string, 0)
	if !u.decoded {
		err := u.DecodeUplink()
		if err != nil {
			return ret, err
		}
	}

	for _, f := range u.Frames {
		for _, m := range f.Text_data {
			if len(m) > 0 {
				ret = append(ret, m)
			}
		}
	}

	return ret, nil
}

/*
	Parse out the message from the "dump978" output format.
*/

func New(buf string) (*UATMsg, error) {
	ret := new(UATMsg)

	buf = strings.Trim(buf, "\r\n") // Remove newlines.
	x := strings.Split(buf, ";")    // We want to discard everything before the first ';'.

	if len(x) < 2 {
		return ret, errors.New(fmt.Sprintf("New UATMsg: Invalid format (%s).", buf))
	}

	/*
		Parse _;rs=?;ss=? - if available.
			RS_Err         int
			SignalStrength int
	*/
	ret.SignalStrength = -1
	ret.RS_Err = -1
	for _, f := range x[1:] {
		x2 := strings.Split(f, "=")
		if len(x2) != 2 {
			continue
		}
		i, err := strconv.Atoi(x2[1])
		if err != nil {
			continue
		}
		if x2[0] == "ss" {
			ret.SignalStrength = i
		} else if x2[0] == "rs" {
			ret.RS_Err = i
		}
	}
	s := x[0]
	if len(s) <= 1 || s[0] != '+' { // Only want + ("Uplink") messages currently. - (Downlink) or messages that start with other are discarded.
		return ret, errors.New("New UATMsg: expecting uplink frame.")
	}

	s = s[1:] // Remove the preceding '+' character.
	if len(s)%2 != 0 {
		return ret, fmt.Errorf("New UATMsg: odd hex length (%d).", len(s))
	}

	msglen := len(s) / 2
	if msglen > UPLINK_FRAME_DATA_BYTES {
		return ret, fmt.Errorf("New UATMsg: oversized uplink (%d bytes).", msglen)
	}
	if msglen < UPLINK_FRAME_DATA_BYTES {
		difference := UPLINK_FRAME_DATA_BYTES - msglen
		s = s + strings.Repeat("00", difference)
	}

	// Convert the hex string into a byte array.
	frame := make([]byte, UPLINK_FRAME_DATA_BYTES)
	if _, err := hex.Decode(frame, []byte(s)); err != nil {
		return ret, fmt.Errorf("New UATMsg: invalid hex: %w", err)
	}
	ret.msg = frame

	return ret, nil
}
