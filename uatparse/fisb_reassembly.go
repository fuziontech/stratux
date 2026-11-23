package uatparse

import (
	"bytes"
	"sync"
	"time"
)

const (
	// DefaultFISBReassemblyTTL is the default time-to-live for incomplete segmented APDUs.
	DefaultFISBReassemblyTTL = 15 * time.Minute
	// DefaultFISBMaxBufferedFiles bounds concurrent incomplete segmented files to prevent memory exhaustion.
	DefaultFISBMaxBufferedFiles = 256
	// DefaultFISBMaxReassembledLen bounds the maximum reassembled APDU size (64 KiB) to prevent DoS.
	DefaultFISBMaxReassembledLen = 64 * 1024

	// maxFISBSegmentCount is the maximum 9-bit Product File Length value in RTCA DO-267A / DO-358.
	maxFISBSegmentCount = 511
)

// SegHeader represents the RTCA DO-267A / DO-358 FIS-B APDU Segmentation Block.
type SegHeader struct {
	ProductFileID     uint16 // 10-bit product file identifier (0..1023)
	ProductFileLength uint16 // 9-bit total number of linked APDUs (1..511)
	APDUNumber        uint16 // 9-bit 1-based APDU sequence number (1..ProductFileLength)
}

type fisbFileKey struct {
	productID     uint16
	fileID        uint16
	totalSegments uint16
}

type fisbFileState struct {
	segments   map[uint16][]byte
	totalBytes int
	firstSeen  time.Time
	lastSeen   time.Time
}

// FISBReassembler reassembles multi-segment RTCA DO-267A / DO-358 FIS-B APDUs.
// It is safe for concurrent use by multiple goroutines.
type FISBReassembler struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxFiles int
	maxSize  int
	files    map[fisbFileKey]*fisbFileState
}

var defaultFISBReassembler = NewFISBReassembler(DefaultFISBReassemblyTTL)

// DefaultFISBReassembler returns the package-level default FIS-B APDU reassembler.
func DefaultFISBReassembler() *FISBReassembler {
	return defaultFISBReassembler
}

// NewFISBReassembler creates a new thread-safe FIS-B APDU reassembler with the given TTL.
// If ttl <= 0, DefaultFISBReassemblyTTL (15 minutes) is used.
func NewFISBReassembler(ttl time.Duration) *FISBReassembler {
	if ttl <= 0 {
		ttl = DefaultFISBReassemblyTTL
	}
	return &FISBReassembler{
		ttl:      ttl,
		maxFiles: DefaultFISBMaxBufferedFiles,
		maxSize:  DefaultFISBMaxReassembledLen,
		files:    make(map[fisbFileKey]*fisbFileState),
	}
}

// extractBits reads bitLen bits (1..32) starting at big-endian bitOffset in data.
func extractBits(data []byte, bitOffset, bitLen int) (uint32, bool) {
	if bitOffset < 0 || bitLen <= 0 || bitLen > 32 {
		return 0, false
	}
	if bitOffset+bitLen > len(data)*8 {
		return 0, false
	}
	var val uint32
	for i := 0; i < bitLen; i++ {
		bitPos := bitOffset + i
		b := data[bitPos/8]
		shift := uint(7 - (bitPos % 8))
		val = (val << 1) | uint32((b>>shift)&0x01)
	}
	return val, true
}

// ParseSegHeader parses a standalone 5-byte (40-bit) FIS-B segmentation header
// (ProductFileID: 10 bits, ProductFileLength: 9 bits, APDUNumber: 9 bits, 12-bit pad)
// and returns the decoded header plus remaining payload bytes.
func ParseSegHeader(data []byte) (SegHeader, []byte, bool) {
	if len(data) < 5 {
		return SegHeader{}, nil, false
	}
	fileID, ok1 := extractBits(data, 0, 10)
	fileLen, ok2 := extractBits(data, 10, 9)
	apduNum, ok3 := extractBits(data, 19, 9)
	if !ok1 || !ok2 || !ok3 {
		return SegHeader{}, nil, false
	}
	if fileLen == 0 || apduNum == 0 || apduNum > fileLen {
		return SegHeader{}, nil, false
	}
	return SegHeader{
		ProductFileID:     uint16(fileID),
		ProductFileLength: uint16(fileLen),
		APDUNumber:        uint16(apduNum),
	}, data[5:], true
}

// ParseSegHeaderFromRaw parses the RTCA DO-267A / DO-358 bit-packed 28-bit segmentation
// block directly from an info frame's Raw_data according to its 2-bit time format (tOpt).
//
// Header bit layout:
//   - 17 bits fixed prefix: A(1) G(1) P(1) ProductID(11) S(1) T_opt(2)
//   - Time field:
//     tOpt=0 (H,M):       11 bits -> ends at bit 28 -> +28 seg bits = 56 bits -> 7 bytes
//     tOpt=1 (H,M,S):     17 bits -> ends at bit 34 -> +28 seg bits = 62 bits -> 8 bytes
//     tOpt=2 (Mo,D,H,M):  20 bits -> ends at bit 37 -> +28 seg bits = 65 bits -> 9 bytes
//     tOpt=3 (Mo,D,H,M,S):26 bits -> ends at bit 43 -> +28 seg bits = 71 bits -> 9 bytes
//   - Segmentation block (28 bits at timeEndBit):
//     ProductFileID (10 bits) | ProductFileLength (9 bits) | APDUNumber (9 bits)
//   - Zero-padded to next octet boundary before payload starts.
func ParseSegHeaderFromRaw(raw []byte, tOpt uint32) (SegHeader, []byte, bool) {
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
	default:
		return SegHeader{}, nil, false
	}

	payloadStart := (timeEndBit + 28 + 7) / 8
	if len(raw) < payloadStart {
		return SegHeader{}, nil, false
	}

	fileID, ok1 := extractBits(raw, timeEndBit, 10)
	fileLen, ok2 := extractBits(raw, timeEndBit+10, 9)
	apduNum, ok3 := extractBits(raw, timeEndBit+19, 9)
	if !ok1 || !ok2 || !ok3 {
		return SegHeader{}, nil, false
	}
	if fileLen == 0 || apduNum == 0 || apduNum > fileLen {
		return SegHeader{}, nil, false
	}

	return SegHeader{
		ProductFileID:     uint16(fileID),
		ProductFileLength: uint16(fileLen),
		APDUNumber:        uint16(apduNum),
	}, raw[payloadStart:], true
}

// isAerodromeAirspaceProduct reports whether productID uses the 6-byte FAA Aerodrome/Airspace
// APDU record header that FAA ground stations repeat at the start of every linked APDU segment.
func isAerodromeAirspaceProduct(productID uint16) bool {
	switch productID {
	case 8, 11, 12, 13, 14, 15, 16, 17:
		return true
	default:
		return false
	}
}

// hasRepeatedAPDUHeader reports whether segments 1..totalSegments all share an identical
// valid 6-byte Aerodrome/Airspace APDU payload header.
func hasRepeatedAPDUHeader(segments map[uint16][]byte, totalSegments uint16) bool {
	if totalSegments < 2 {
		return false
	}
	first := segments[1]
	if len(first) < 6 {
		return false
	}
	recFmt := (first[0] & 0xF0) >> 4
	recCnt := (first[1] & 0xF0) >> 4
	reserved := first[1] & 0x0F
	if recCnt == 0 || reserved != 0 {
		return false
	}
	switch recFmt {
	case 1, 2, 3, 4, 8:
	default:
		return false
	}
	hdr := first[:6]
	for i := uint16(2); i <= totalSegments; i++ {
		seg := segments[i]
		if len(seg) < 6 || !bytes.Equal(seg[:6], hdr) {
			return false
		}
	}
	return true
}

// AddSegment buffers a single APDU segment and returns the complete reassembled payload
// once all segments (1..header.ProductFileLength) have arrived.
func (r *FISBReassembler) AddSegment(productID uint16, header SegHeader, payload []byte, now time.Time) (assembled []byte, complete bool) {
	if r == nil {
		return nil, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if header.ProductFileLength == 0 || header.ProductFileLength > maxFISBSegmentCount {
		return nil, false
	}
	if header.APDUNumber == 0 || header.APDUNumber > header.ProductFileLength {
		return nil, false
	}
	if len(payload) == 0 || len(payload) > r.maxSize {
		return nil, false
	}

	// Single-segment APDU completes immediately without touching state.
	if header.ProductFileLength == 1 {
		out := make([]byte, len(payload))
		copy(out, payload)
		return out, true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneLocked(now)

	key := fisbFileKey{
		productID:     productID,
		fileID:        header.ProductFileID,
		totalSegments: header.ProductFileLength,
	}

	state, exists := r.files[key]
	if !exists {
		if len(r.files) >= r.maxFiles {
			r.evictOldestLocked()
		}
		state = &fisbFileState{
			segments:  make(map[uint16][]byte),
			firstSeen: now,
			lastSeen:  now,
		}
		r.files[key] = state
	}

	if _, dup := state.segments[header.APDUNumber]; dup {
		state.lastSeen = now
		return nil, false
	}

	if state.totalBytes+len(payload) > r.maxSize {
		// Reject oversized/malicious file and drop its buffered state.
		delete(r.files, key)
		return nil, false
	}

	segCopy := make([]byte, len(payload))
	copy(segCopy, payload)
	state.segments[header.APDUNumber] = segCopy
	state.totalBytes += len(segCopy)
	state.lastSeen = now

	if len(state.segments) < int(header.ProductFileLength) {
		return nil, false
	}

	// All segments present: reassemble in order 1..ProductFileLength.
	stripRepeatedHdr := isAerodromeAirspaceProduct(productID) && hasRepeatedAPDUHeader(state.segments, header.ProductFileLength)

	outCap := state.totalBytes
	if stripRepeatedHdr {
		outCap -= 6 * (int(header.ProductFileLength) - 1)
		if outCap < 0 {
			outCap = 0
		}
	}
	if outCap > r.maxSize {
		delete(r.files, key)
		return nil, false
	}

	out := make([]byte, 0, outCap)
	for i := uint16(1); i <= header.ProductFileLength; i++ {
		seg := state.segments[i]
		if stripRepeatedHdr && i > 1 && len(seg) >= 6 {
			out = append(out, seg[6:]...)
		} else {
			out = append(out, seg...)
		}
	}

	delete(r.files, key)
	return out, true
}

// Prune removes all incomplete segmented files whose last update is older than the reassembler TTL.
func (r *FISBReassembler) Prune(now time.Time) {
	if r == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r.mu.Lock()
	r.pruneLocked(now)
	r.mu.Unlock()
}

func (r *FISBReassembler) pruneLocked(now time.Time) {
	for k, st := range r.files {
		if now.Sub(st.lastSeen) >= r.ttl {
			delete(r.files, k)
		}
	}
}

func (r *FISBReassembler) evictOldestLocked() {
	var oldestKey fisbFileKey
	var oldestTime time.Time
	first := true
	for k, st := range r.files {
		if first || st.lastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = st.lastSeen
			first = false
		}
	}
	if !first {
		delete(r.files, oldestKey)
	}
}

// Len returns the number of currently buffered incomplete segmented files.
func (r *FISBReassembler) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	n := len(r.files)
	r.mu.Unlock()
	return n
}

// Reset clears all buffered incomplete segmented files.
func (r *FISBReassembler) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.files = make(map[fisbFileKey]*fisbFileState)
	r.mu.Unlock()
}
