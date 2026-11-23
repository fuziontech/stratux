/*
	Copyright (c) 2025 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	remoteid.go: ASTM F3411-22a / OpenDroneID binary decoder, active UAS state manager,
	TrafficInfo conversion, and passive BLE 0xFFFA scanner.
*/

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/common"
	"tinygo.org/x/bluetooth"
)

const (
	// Non-colliding traffic source & target type constants (traffic.go uses 1,2,4,8 and 0..5).
	TRAFFIC_SOURCE_REMOTEID = 16
	TARGET_TYPE_REMOTEID    = 6

	// ASTM F3411-22a / OpenDroneID wire constants.
	ODIDMessageSize       = 25
	ODIDMaxPackMessages   = 10
	ODIDBLEServiceUUID    = uint16(0xFFFA)
	ODIDBLEAppCode        = byte(0x0D)
	ODIDAltInvalidRaw     = uint16(0) // -1000.0 m sentinel
	ODIDSpeedInvalidRaw   = byte(255)
	ODIDVertSpeedInvalid  = int8(63)
	ODIDDefaultExpiry     = 30 * time.Second
	odidMetersToFeet      = 3.280839895013123
	odidMpsToKnots        = 1.9438444924406048
	odidMpsToFpm          = 196.85039370078738
	odidEmitterCategoryUA = uint8(14) // GDL90 UAV emitter category

	ODIDMsgTypeBasicID     uint8 = 0x0
	ODIDMsgTypeLocation    uint8 = 0x1
	ODIDMsgTypeAuth        uint8 = 0x2
	ODIDMsgTypeSelfID      uint8 = 0x3
	ODIDMsgTypeSystem      uint8 = 0x4
	ODIDMsgTypeOperatorID  uint8 = 0x5
	ODIDMsgTypeMessagePack uint8 = 0xF
)

// RemoteIDTarget represents decoded ASTM F3411 / OpenDroneID telemetry for a UAS.
type RemoteIDTarget struct {
	UASID       string
	IDType      uint8
	UAType      uint8
	Lat         float64
	Lon         float64
	AltBaroFt   float64
	AltGeodFt   float64
	HeightAGLFt float64
	SpeedKt     float64
	TrackDeg    float64
	VvelFpm     float64
	OpStatus    uint8
	OperatorLat float64
	OperatorLon float64
	OperatorID  string
	LastSeen    time.Time

	HasBasicID    bool
	HasLocation   bool
	HasBaroAlt    bool
	HasGeodAlt    bool
	HasHeightAGL  bool
	HasSystem     bool
	HasOperatorID bool
}

// ParseOpenDroneIDBLEAdv parses BLE Service Data received under 16-bit Service UUID 0xFFFA.
// Wire format: [AppCode=0x0D][MsgCounter][ODID payload (25B message or 0xF Message Pack)].
func ParseOpenDroneIDBLEAdv(serviceData []byte) (*RemoteIDTarget, error) {
	if len(serviceData) < 2+ODIDMessageSize {
		return nil, fmt.Errorf("remoteid: BLE service data too short (%d bytes)", len(serviceData))
	}
	if serviceData[0] != ODIDBLEAppCode {
		return nil, fmt.Errorf("remoteid: unexpected BLE app code 0x%02X (want 0x%02X)", serviceData[0], ODIDBLEAppCode)
	}
	return ParseOpenDroneIDFrame(serviceData[2:])
}

// ParseOpenDroneIDFrame decodes either a single 25-byte ASTM F3411-22a message
// (Basic ID 0x0, Location/Vector 0x1, System 0x4, Operator ID 0x5) or a Message Pack (0xF).
func ParseOpenDroneIDFrame(payload []byte) (*RemoteIDTarget, error) {
	if len(payload) < 1 {
		return nil, errors.New("remoteid: empty payload")
	}

	msgType := (payload[0] >> 4) & 0x0F
	if msgType == ODIDMsgTypeMessagePack {
		return parseOpenDroneIDMessagePack(payload)
	}
	if len(payload) < ODIDMessageSize {
		return nil, fmt.Errorf("remoteid: single message too short (%d < %d bytes)", len(payload), ODIDMessageSize)
	}

	target := &RemoteIDTarget{}
	if err := decodeOpenDroneIDSingleMessage(payload[:ODIDMessageSize], target); err != nil {
		return nil, err
	}
	return target, nil
}

func parseOpenDroneIDMessagePack(payload []byte) (*RemoteIDTarget, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("remoteid: message pack header too short (%d bytes)", len(payload))
	}
	msgSize := int(payload[1])
	msgCount := int(payload[2])
	if msgSize != ODIDMessageSize {
		return nil, fmt.Errorf("remoteid: invalid message pack item size %d (want %d)", msgSize, ODIDMessageSize)
	}
	if msgCount <= 0 || msgCount > ODIDMaxPackMessages {
		return nil, fmt.Errorf("remoteid: invalid message pack count %d", msgCount)
	}
	requiredLen := 3 + msgCount*msgSize
	if requiredLen > len(payload) {
		return nil, fmt.Errorf("remoteid: truncated message pack (need %d bytes, got %d)", requiredLen, len(payload))
	}

	target := &RemoteIDTarget{}
	decodedAny := false
	for i := 0; i < msgCount; i++ {
		off := 3 + i*msgSize
		sub := payload[off : off+msgSize]
		subType := (sub[0] >> 4) & 0x0F
		if subType == ODIDMsgTypeMessagePack {
			return nil, errors.New("remoteid: nested message pack not permitted")
		}
		if err := decodeOpenDroneIDSingleMessage(sub, target); err == nil {
			decodedAny = true
		}
	}
	if !decodedAny {
		return nil, errors.New("remoteid: message pack contained no supported messages")
	}
	return target, nil
}

func decodeOpenDroneIDSingleMessage(msg []byte, dst *RemoteIDTarget) error {
	if len(msg) < ODIDMessageSize {
		return fmt.Errorf("remoteid: message too short (%d bytes)", len(msg))
	}
	msgType := (msg[0] >> 4) & 0x0F
	switch msgType {
	case ODIDMsgTypeBasicID:
		dst.IDType = (msg[1] >> 4) & 0x0F
		dst.UAType = msg[1] & 0x0F
		dst.UASID = decodeODIDString(msg[2:22], dst.IDType == 3 || dst.IDType == 4)
		dst.HasBasicID = true
		return nil

	case ODIDMsgTypeLocation:
		flags := msg[1]
		dst.OpStatus = (flags >> 4) & 0x0F
		ewDir := (flags >> 1) & 0x01
		speedMult := flags & 0x01

		// Track direction (0..359 deg; raw > 180 or ew=1 && raw >= 180 denotes invalid/361).
		rawDir := msg[2]
		if ewDir == 0 && rawDir <= 180 {
			if rawDir == 180 {
				dst.TrackDeg = 180
			} else {
				dst.TrackDeg = float64(rawDir)
			}
		} else if ewDir == 1 && rawDir < 180 {
			dst.TrackDeg = float64(rawDir) + 180.0
		} else {
			dst.TrackDeg = 0
		}

		// Horizontal speed (ASTM F3411-22a Table 4).
		rawSpeed := msg[3]
		if rawSpeed != ODIDSpeedInvalidRaw {
			var speedMps float64
			if speedMult == 0 {
				speedMps = float64(rawSpeed) * 0.25
			} else {
				speedMps = float64(rawSpeed)*0.75 + 255.0*0.25
			}
			dst.SpeedKt = speedMps * odidMpsToKnots
		}

		// Vertical speed (int8 * 0.5 m/s; 63 = invalid).
		rawVvel := int8(msg[4])
		if rawVvel != ODIDVertSpeedInvalid {
			dst.VvelFpm = float64(rawVvel) * 0.5 * odidMpsToFpm
		}

		// Latitude / Longitude (int32 LE * 1e-7 deg).
		rawLat := int32(binary.LittleEndian.Uint32(msg[5:9]))
		rawLon := int32(binary.LittleEndian.Uint32(msg[9:13]))
		lat := float64(rawLat) * 1e-7
		lon := float64(rawLon) * 1e-7
		if lat >= -90.0 && lat <= 90.0 && lon >= -180.0 && lon <= 180.0 && !(rawLat == 0 && rawLon == 0) {
			dst.Lat = lat
			dst.Lon = lon
			dst.HasLocation = true
		}

		// Altitudes: uint16 LE, encoded as (alt_m + 1000) * 2; 0 == invalid (-1000 m).
		if rawBaro := binary.LittleEndian.Uint16(msg[13:15]); rawBaro != ODIDAltInvalidRaw {
			dst.AltBaroFt = (float64(rawBaro)*0.5 - 1000.0) * odidMetersToFeet
			dst.HasBaroAlt = true
		}
		if rawGeod := binary.LittleEndian.Uint16(msg[15:17]); rawGeod != ODIDAltInvalidRaw {
			dst.AltGeodFt = (float64(rawGeod)*0.5 - 1000.0) * odidMetersToFeet
			dst.HasGeodAlt = true
		}
		if rawHeight := binary.LittleEndian.Uint16(msg[17:19]); rawHeight != ODIDAltInvalidRaw {
			dst.HeightAGLFt = (float64(rawHeight)*0.5 - 1000.0) * odidMetersToFeet
			dst.HasHeightAGL = true
		}
		return nil

	case ODIDMsgTypeSystem:
		rawOpLat := int32(binary.LittleEndian.Uint32(msg[2:6]))
		rawOpLon := int32(binary.LittleEndian.Uint32(msg[6:10]))
		opLat := float64(rawOpLat) * 1e-7
		opLon := float64(rawOpLon) * 1e-7
		if opLat >= -90.0 && opLat <= 90.0 && opLon >= -180.0 && opLon <= 180.0 && !(rawOpLat == 0 && rawOpLon == 0) {
			dst.OperatorLat = opLat
			dst.OperatorLon = opLon
		}
		dst.HasSystem = true
		return nil

	case ODIDMsgTypeOperatorID:
		dst.OperatorID = decodeODIDString(msg[2:22], false)
		dst.HasOperatorID = true
		return nil

	case ODIDMsgTypeAuth, ODIDMsgTypeSelfID:
		// Valid ASTM F3411 message types that carry supplementary info not needed for traffic fusion.
		return nil

	default:
		return fmt.Errorf("remoteid: unsupported ODID message type 0x%X", msgType)
	}
}

func decodeODIDString(raw []byte, allowBinaryHex bool) string {
	// Trim trailing NUL bytes first.
	end := len(raw)
	for end > 0 && raw[end-1] == 0 {
		end--
	}
	trimmed := raw[:end]
	if len(trimmed) == 0 {
		return ""
	}

	allPrintable := true
	for _, b := range trimmed {
		if b < 0x20 || b >= 0x7f {
			allPrintable = false
			break
		}
	}
	if allPrintable {
		return strings.TrimSpace(string(trimmed))
	}
	if allowBinaryHex {
		return strings.ToUpper(hex.EncodeToString(trimmed))
	}

	var b strings.Builder
	b.Grow(len(trimmed))
	for _, c := range trimmed {
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// RemoteIDState manages active Remote ID UAS targets, merging partial Basic ID,
// Location/Vector, System, and Operator ID advertisements and expiring stale entries.
type RemoteIDState struct {
	mu      sync.Mutex
	expiry  time.Duration
	targets map[string]*RemoteIDTarget
}

// globalRemoteIDState is the shared Remote ID target state manager.
var globalRemoteIDState = NewRemoteIDState()

// NewRemoteIDState creates a new thread-safe RemoteIDState with a 30-second expiry window.
func NewRemoteIDState() *RemoteIDState {
	return &RemoteIDState{
		expiry:  ODIDDefaultExpiry,
		targets: make(map[string]*RemoteIDTarget),
	}
}

// Update merges an incoming partial or complete RemoteIDTarget into state keyed by sourceKey
// (typically the BLE MAC address, or UAS ID if sourceKey is empty) and returns a copy.
func (s *RemoteIDState) Update(sourceKey string, incoming *RemoteIDTarget, now time.Time) *RemoteIDTarget {
	if s == nil || incoming == nil {
		return nil
	}
	key := strings.TrimSpace(sourceKey)
	if key == "" {
		key = incoming.UASID
	}
	if key == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cur, exists := s.targets[key]
	if !exists {
		cur = &RemoteIDTarget{}
		s.targets[key] = cur
	}

	if incoming.HasBasicID {
		if incoming.UASID != "" {
			cur.UASID = incoming.UASID
		}
		cur.IDType = incoming.IDType
		cur.UAType = incoming.UAType
		cur.HasBasicID = true
	}
	if incoming.HasLocation {
		cur.Lat = incoming.Lat
		cur.Lon = incoming.Lon
		cur.SpeedKt = incoming.SpeedKt
		cur.TrackDeg = incoming.TrackDeg
		cur.VvelFpm = incoming.VvelFpm
		cur.OpStatus = incoming.OpStatus
		cur.HasLocation = true
	}
	if incoming.HasBaroAlt {
		cur.AltBaroFt = incoming.AltBaroFt
		cur.HasBaroAlt = true
	}
	if incoming.HasGeodAlt {
		cur.AltGeodFt = incoming.AltGeodFt
		cur.HasGeodAlt = true
	}
	if incoming.HasHeightAGL {
		cur.HeightAGLFt = incoming.HeightAGLFt
		cur.HasHeightAGL = true
	}
	if incoming.HasSystem {
		if incoming.OperatorLat != 0 || incoming.OperatorLon != 0 {
			cur.OperatorLat = incoming.OperatorLat
			cur.OperatorLon = incoming.OperatorLon
		}
		cur.HasSystem = true
	}
	if incoming.HasOperatorID {
		if incoming.OperatorID != "" {
			cur.OperatorID = incoming.OperatorID
		}
		cur.HasOperatorID = true
	}
	cur.LastSeen = now

	cp := *cur
	return &cp
}

// Expire deletes Remote ID targets whose LastSeen exceeds the expiry duration (30s).
func (s *RemoteIDState) Expire(now time.Time) []string {
	if s == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []string
	for k, t := range s.targets {
		if now.Sub(t.LastSeen) > s.expiry {
			delete(s.targets, k)
			expired = append(expired, k)
		}
	}
	return expired
}

// Snapshot returns a slice copy of all currently active RemoteIDTarget entries.
func (s *RemoteIDState) Snapshot() []RemoteIDTarget {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]RemoteIDTarget, 0, len(s.targets))
	for _, t := range s.targets {
		out = append(out, *t)
	}
	return out
}

// ToTrafficInfo converts a merged RemoteIDTarget with valid location into a Stratux TrafficInfo
// and deterministic non-ICAO traffic map key.
func (s *RemoteIDState) ToTrafficInfo(sourceKey string, target *RemoteIDTarget, now time.Time) (uint32, TrafficInfo, bool) {
	if target == nil || !target.HasLocation {
		return 0, TrafficInfo{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	idSeed := target.UASID
	if idSeed == "" {
		idSeed = sourceKey
	}
	addr24 := remoteIDHash24(idSeed)
	key := uint32(1)<<24 | addr24

	clockNow := now
	if stratuxClock != nil {
		clockNow = stratuxClock.GetTime()
	}

	var altFt int32
	altIsGNSS := true
	switch {
	case target.HasBaroAlt:
		altFt = int32(math.Round(target.AltBaroFt))
		altIsGNSS = false
	case target.HasGeodAlt:
		altFt = int32(math.Round(target.AltGeodFt))
		altIsGNSS = true
	case target.HasHeightAGL:
		altFt = int32(math.Round(target.HeightAGLFt))
		altIsGNSS = true
	}

	speedKt := target.SpeedKt
	if speedKt < 0 || math.IsNaN(speedKt) || math.IsInf(speedKt, 0) {
		speedKt = 0
	}
	if speedKt > math.MaxUint16 {
		speedKt = math.MaxUint16
	}

	var priorityStatus uint8
	if target.OpStatus == 3 { // Emergency
		priorityStatus = 1
	}

	ti := TrafficInfo{
		Icao_addr:            addr24,
		Addr_type:            1, // Non-ICAO self-assigned
		TargetType:           TARGET_TYPE_REMOTEID,
		Last_source:          TRAFFIC_SOURCE_REMOTEID,
		Emitter_category:     odidEmitterCategoryUA,
		Tail:                 formatRemoteIDTail(target.UASID, sourceKey),
		Reg:                  sanitizeRemoteIDText(target.OperatorID, 20),
		OnGround:             target.OpStatus == 1,
		Position_valid:       true,
		Lat:                  float32(target.Lat),
		Lng:                  float32(target.Lon),
		Alt:                  altFt,
		AltIsGNSS:            altIsGNSS,
		Track:                float32(target.TrackDeg),
		Speed:                uint16(math.Round(speedKt)),
		Speed_valid:          true,
		Vvel:                 common.RoundToInt16(target.VvelFpm),
		PriorityStatus:       priorityStatus,
		Timestamp:            now.UTC(),
		Last_seen:            clockNow,
		Last_alt:             clockNow,
		Last_speed:           clockNow,
		ExtrapolatedPosition: false,
	}

	return key, ti, true
}

// IngestAndPublish merges an incoming Remote ID target into state and, when a valid position
// is present, publishes it into the global `traffic` map under `trafficMutex`.
func (s *RemoteIDState) IngestAndPublish(sourceKey string, incoming *RemoteIDTarget, rssi int16, now time.Time) (uint32, TrafficInfo, bool) {
	merged := s.Update(sourceKey, incoming, now)
	key, ti, ok := s.ToTrafficInfo(sourceKey, merged, now)
	if !ok {
		return 0, TrafficInfo{}, false
	}
	if rssi != 0 {
		ti.SignalLevel = float64(rssi)
	}

	if trafficMutex != nil && traffic != nil {
		trafficMutex.Lock()
		if existing, exists := traffic[key]; exists {
			ti.ReceivedMsgs = existing.ReceivedMsgs
		}
		postProcessTraffic(&ti)
		traffic[key] = ti
		markTrafficSeen(key)
		trafficMutex.Unlock()
		registerTrafficUpdate(ti)
	}

	return key, ti, true
}

// StartRemoteIDScanner starts a passive BLE scan filtering ASTM F3411 / OpenDroneID
// advertisements (Service UUID 0xFFFA) and feeds decoded UAS targets into global traffic.
// If no Bluetooth adapter is available or enabling fails, it logs gracefully and returns.
func StartRemoteIDScanner(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	adapter := bluetooth.DefaultAdapter
	if adapter == nil {
		log.Printf("Remote ID BLE scanner disabled: no default Bluetooth adapter")
		return
	}
	if err := adapter.Enable(); err != nil {
		log.Printf("Remote ID BLE scanner unavailable: %v", err)
		return
	}

	odidUUID := bluetooth.New16BitUUID(ODIDBLEServiceUUID)
	log.Printf("Remote ID BLE passive scanner started (UUID 0x%04X)", ODIDBLEServiceUUID)

	done := make(chan struct{})
	defer close(done)

	// Periodic expiry of stale Remote ID entries.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				globalRemoteIDState.Expire(time.Now().UTC())
			}
		}
	}()

	// Stop BLE scan cleanly when ctx is cancelled.
	go func() {
		select {
		case <-ctx.Done():
			_ = adapter.StopScan()
		case <-done:
		}
	}()

	err := adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
		for _, elem := range result.ServiceData() {
			if elem.UUID != odidUUID {
				continue
			}
			// Copy slice because ScanResult payload buffers may be reused after callback returns.
			data := make([]byte, len(elem.Data))
			copy(data, elem.Data)

			parsed, parseErr := ParseOpenDroneIDBLEAdv(data)
			if parseErr != nil {
				continue
			}
			globalRemoteIDState.IngestAndPublish(result.Address.String(), parsed, result.RSSI, time.Now().UTC())
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("Remote ID BLE scanner stopped with error: %v", err)
	}
}

func remoteIDHash24(seed string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	addr := h.Sum32() & 0x00FFFFFF
	if addr == 0 {
		addr = 1
	}
	return addr
}

func formatRemoteIDTail(uasID, fallbackKey string) string {
	clean := sanitizeRemoteIDText(uasID, 8)
	if clean != "" {
		return clean
	}
	clean = sanitizeRemoteIDText(strings.ReplaceAll(fallbackKey, ":", ""), 8)
	if clean != "" {
		return clean
	}
	return "UAS"
}

func sanitizeRemoteIDText(s string, maxLen int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxLen; i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f || c == ',' || c == '*' || c == '$' {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}
