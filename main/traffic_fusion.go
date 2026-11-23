/*
	Copyright (c) 2025 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	traffic_fusion.go: Cross-source traffic fusion, source confidence tiers,
	OGN DDB FLARM-to-ICAO aliasing, 3D spatial/velocity deduplication, and
	baro/GNSS altitude arbitration.
*/

package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/common"
)

const (
	// Additional traffic source bitmasks (non-colliding with traffic.go: 1, 2, 4, 8 and remoteid.go: 16).
	TRAFFIC_SOURCE_COT   = 32
	TRAFFIC_SOURCE_FLARM = 64

	// Source confidence/priority tiers.
	TrafficPriorityADSB         = 100
	TrafficPriorityOGNFLARM     = 95
	TrafficPriorityRemoteID     = 85
	TrafficPriorityADSRTISB     = 75
	TrafficPriorityAIS          = 70
	TrafficPriorityCoT          = 50
	TrafficPriorityModeS        = 40
	TrafficPriorityAPRS         = 30
	TrafficPriorityExtrapolated = 10

	// 3D spatial & kinematic correlation fallback thresholds.
	fusionFreshWindowSec     = 5.0
	fusionMaxHorizMeters     = 150.0
	fusionMaxVertFeet        = 300.0
	fusionMaxTrackDeltaDeg   = 25.0
	fusionMaxSpeedDeltaKt    = 15.0
	fusionPositionProtectSec = 6.0
	fusionBaroGNSSWindowSec  = 10.0
)

// TrafficSourcePriority returns the confidence/priority tier for a non-APRS traffic source.
func TrafficSourcePriority(source int, targetType uint8, extrapolated bool) int {
	return TrafficSourcePriorityWithAPRS(source, targetType, extrapolated, false)
}

// TrafficSourcePriorityWithAPRS returns the confidence/priority tier for a traffic source,
// explicitly accounting for internet-relayed APRS feeds.
func TrafficSourcePriorityWithAPRS(source int, targetType uint8, extrapolated bool, isAPRS bool) int {
	if extrapolated {
		return TrafficPriorityExtrapolated
	}
	if isAPRS {
		return TrafficPriorityAPRS
	}

	// Check dedicated non-ADS-B sources before TargetType because OGN/FLARM/CoT/AIS
	// often leave TargetType at its zero value (which collides numerically with TARGET_TYPE_MODE_S).
	switch source {
	case TRAFFIC_SOURCE_OGN, TRAFFIC_SOURCE_FLARM:
		return TrafficPriorityOGNFLARM
	case TRAFFIC_SOURCE_REMOTEID:
		return TrafficPriorityRemoteID
	case TRAFFIC_SOURCE_AIS:
		return TrafficPriorityAIS
	case TRAFFIC_SOURCE_COT:
		return TrafficPriorityCoT
	}

	switch targetType {
	case TARGET_TYPE_ADSB:
		return TrafficPriorityADSB
	case TARGET_TYPE_REMOTEID:
		return TrafficPriorityRemoteID
	case TARGET_TYPE_ADSR, TARGET_TYPE_TISB, TARGET_TYPE_TISB_S:
		return TrafficPriorityADSRTISB
	case TARGET_TYPE_AIS:
		return TrafficPriorityAIS
	case TARGET_TYPE_MODE_S:
		return TrafficPriorityModeS
	default:
		if source == TRAFFIC_SOURCE_1090ES || source == TRAFFIC_SOURCE_UAT {
			return TrafficPriorityModeS
		}
		return TrafficPriorityExtrapolated
	}
}

// TrafficFusionIndex maintains cross-source identity aliases (e.g. OGN/FLARM ID -> ICAO 24-bit)
// and resolves canonical traffic map keys across RF/network bands.
type TrafficFusionIndex struct {
	mu          sync.RWMutex
	flarmToICAO map[uint32]uint32
	icaoToFlarm map[uint32]uint32
}

// globalTrafficFusion is the default shared fusion index for traffic ingestion paths.
var globalTrafficFusion = NewTrafficFusionIndex()

// NewTrafficFusionIndex creates an empty thread-safe TrafficFusionIndex.
func NewTrafficFusionIndex() *TrafficFusionIndex {
	return &TrafficFusionIndex{
		flarmToICAO: make(map[uint32]uint32),
		icaoToFlarm: make(map[uint32]uint32),
	}
}

// RegisterDDBAlias registers a 24-bit FLARM/OGN device ID as an alias for a 24-bit ICAO address.
func (idx *TrafficFusionIndex) RegisterDDBAlias(flarmID, icaoAddr uint32) {
	if idx == nil {
		return
	}
	flarm24 := flarmID & 0x00FFFFFF
	icao24 := icaoAddr & 0x00FFFFFF
	if flarm24 == 0 || icao24 == 0 {
		return
	}
	idx.mu.Lock()
	idx.flarmToICAO[flarm24] = icao24
	idx.icaoToFlarm[icao24] = flarm24
	idx.mu.Unlock()
}

// LookupICAO returns the mapped 24-bit ICAO address for a FLARM/OGN device ID, if known.
func (idx *TrafficFusionIndex) LookupICAO(flarmID uint32) (uint32, bool) {
	if idx == nil {
		return 0, false
	}
	idx.mu.RLock()
	icao, ok := idx.flarmToICAO[flarmID&0x00FFFFFF]
	idx.mu.RUnlock()
	return icao, ok
}

type fusionDDBDevice struct {
	DeviceType   string `json:"device_type"`
	DeviceID     string `json:"device_id"`
	Registration string `json:"registration"`
}

type fusionDDBFile struct {
	Devices []fusionDDBDevice `json:"devices"`
}

// LoadDDBAliasesFromJSON parses an OGN ddb.json payload and links FLARM ("F") / OGN ("O")
// device IDs to ICAO ("I") addresses that share the same non-empty aircraft registration.
// Returns the number of aliases registered.
func (idx *TrafficFusionIndex) LoadDDBAliasesFromJSON(data []byte) int {
	if idx == nil || len(data) == 0 {
		return 0
	}
	var ddb fusionDDBFile
	if err := json.Unmarshal(data, &ddb); err != nil {
		return 0
	}

	regToICAO := make(map[string]uint32)
	type nonICAOEntry struct {
		reg string
		id  uint32
	}
	var nonICAOs []nonICAOEntry

	for _, dev := range ddb.Devices {
		reg := strings.ToUpper(strings.TrimSpace(dev.Registration))
		if reg == "" {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(dev.DeviceID), 16, 32)
		if err != nil || parsed == 0 {
			continue
		}
		id24 := uint32(parsed) & 0x00FFFFFF
		devType := strings.ToUpper(strings.TrimSpace(dev.DeviceType))
		switch devType {
		case "I":
			regToICAO[reg] = id24
		case "F", "O":
			nonICAOs = append(nonICAOs, nonICAOEntry{reg: reg, id: id24})
		}
	}

	count := 0
	for _, entry := range nonICAOs {
		if icao, ok := regToICAO[entry.reg]; ok && icao != entry.id {
			idx.RegisterDDBAlias(entry.id, icao)
			count++
		}
	}
	return count
}

// ResolveCanonicalKey resolves an incoming target onto an existing key in `existing` if possible:
//  1. Direct exact key match (`uint32(addrType)<<24 | addr` or bare 24-bit `addr`).
//  2. OGN DDB cross-link (FLARM/OGN ID -> known ICAO key already present in `existing`).
//  3. 3D spatial + velocity correlation fallback against fresh (< 5s) existing targets.
//
// If no existing target matches, it returns `(uint32(addrType)<<24 | (addr&0x00FFFFFF), false)`.
func (idx *TrafficFusionIndex) ResolveCanonicalKey(
	addr uint32,
	addrType uint8,
	lat, lng float32,
	alt int32,
	track float32,
	posValid bool,
	existing map[uint32]TrafficInfo,
) (key uint32, matchedExisting bool) {
	return idx.ResolveCanonicalKeyWithContext(addr, addrType, 0, lat, lng, alt, track, 0, false, posValid, time.Time{}, existing)
}

// ResolveCanonicalKeyWithContext extends ResolveCanonicalKey with source, speed, and explicit timestamp
// so callers can enforce cross-source-only spatial matching and speed-delta checks.
func (idx *TrafficFusionIndex) ResolveCanonicalKeyWithContext(
	addr uint32,
	addrType uint8,
	incomingSource int,
	lat, lng float32,
	alt int32,
	track float32,
	speed uint16,
	speedValid bool,
	posValid bool,
	now time.Time,
	existing map[uint32]TrafficInfo,
) (key uint32, matchedExisting bool) {
	addr24 := addr & 0x00FFFFFF
	defaultKey := uint32(addrType)<<24 | addr24

	if len(existing) == 0 {
		return defaultKey, false
	}

	// Step 1: Direct exact key match (typed key, or bare ICAO key).
	if _, ok := existing[defaultKey]; ok {
		return defaultKey, true
	}
	if addr24 != defaultKey {
		if _, ok := existing[addr24]; ok {
			return addr24, true
		}
	}

	// Step 2: OGN DDB cross-link (FLARM/OGN device ID -> existing ICAO key).
	if idx != nil {
		if icao, ok := idx.LookupICAO(addr24); ok {
			if _, exists := existing[icao]; exists {
				return icao, true
			}
		}
	}

	// Step 3: 3D spatial + velocity correlation fallback.
	lat64 := float64(lat)
	lng64 := float64(lng)
	if !posValid || math.IsNaN(lat64) || math.IsNaN(lng64) || math.IsInf(lat64, 0) || math.IsInf(lng64, 0) || (lat == 0 && lng == 0) {
		return defaultKey, false
	}

	var bestKey uint32
	bestDist := math.MaxFloat64
	found := false

	for k, ti := range existing {
		if !ti.Position_valid || ti.ExtrapolatedPosition {
			continue
		}
		// Spatial correlation only fuses targets from a DIFFERENT source so real formation
		// flights on the same band are never collapsed together.
		if incomingSource != 0 && int(ti.Last_source) == incomingSource {
			continue
		}

		age := trafficAgeSeconds(ti, now)
		if age < -2.0 || age >= fusionFreshWindowSec {
			continue
		}

		if ti.Lat == 0 && ti.Lng == 0 {
			continue
		}

		// Vertical proximity check (<= 300 ft).
		if math.Abs(float64(alt)-float64(ti.Alt)) > fusionMaxVertFeet {
			continue
		}

		// Track alignment check (<= 25 deg wrap-around).
		if angularDeltaDeg(float64(track), float64(ti.Track)) > fusionMaxTrackDeltaDeg {
			continue
		}

		// Speed alignment check (<= 15 kt) when both speeds are valid.
		if speedValid && ti.Speed_valid {
			if math.Abs(float64(speed)-float64(ti.Speed)) > fusionMaxSpeedDeltaKt {
				continue
			}
		}

		// Horizontal proximity check (<= 150 m).
		dist, _, _, _ := common.DistRect(lat64, lng64, float64(ti.Lat), float64(ti.Lng))
		if math.IsNaN(dist) || dist > fusionMaxHorizMeters {
			continue
		}

		if !found || dist < bestDist || (dist == bestDist && k < bestKey) {
			bestDist = dist
			bestKey = k
			found = true
		}
	}

	if found {
		return bestKey, true
	}
	return defaultKey, false
}

// ShouldAcceptPositionUpdate decides whether an incoming position report should overwrite
// an existing target's position. It rejects lower-priority updates (e.g. internet APRS,
// TIS-B/ADS-R, CoT) while the existing target has a fresh (< 6s) higher-priority direct RF fix.
func ShouldAcceptPositionUpdate(
	existing TrafficInfo,
	incomingSource int,
	incomingTargetType uint8,
	incomingIsAPRS bool,
	now time.Time,
) bool {
	if !existing.Position_valid || existing.ExtrapolatedPosition {
		return true
	}

	existingPrio := TrafficSourcePriority(int(existing.Last_source), existing.TargetType, existing.ExtrapolatedPosition)
	incomingPrio := TrafficSourcePriorityWithAPRS(incomingSource, incomingTargetType, false, incomingIsAPRS)

	if incomingPrio >= existingPrio {
		return true
	}

	existingAge := trafficAgeSeconds(existing, now)
	// Reject lower-priority updates while higher-priority RF fix is fresh (< 6s).
	if existingAge > -5.0 && existingAge < fusionPositionProtectSec {
		return false
	}
	return true
}

// MergeAltitude arbitrates barometric vs. GNSS altitude updates for a fused target.
// It prevents baro/GNSS ping-pong across bands by preferring fresh barometric pressure
// altitude (for GDL90/ATC consistency) while recording GnssDiffFromBaroAlt whenever
// both barometric and GNSS altitudes are observed within 10 seconds.
func MergeAltitude(existing *TrafficInfo, incomingAlt int32, incomingIsGNSS bool, incomingSource int, now time.Time) {
	if existing == nil {
		return
	}
	if now.IsZero() {
		now = currentFusionTime()
	}

	hasExisting := !existing.Last_alt.IsZero() || existing.Alt != 0
	altAge := math.Inf(1)
	if !existing.Last_alt.IsZero() {
		altAge = timeDeltaSeconds(now, existing.Last_alt)
	}

	if !incomingIsGNSS {
		// Incoming barometric pressure altitude.
		if hasExisting && existing.AltIsGNSS && altAge >= -2.0 && altAge <= fusionBaroGNSSWindowSec {
			existing.GnssDiffFromBaroAlt = existing.Alt - incomingAlt
			existing.Last_GnssDiff = now
			existing.Last_GnssDiffAlt = incomingAlt
		}
		existing.Alt = incomingAlt
		existing.AltIsGNSS = false
		existing.Last_alt = now
		existing.AgeLastAlt = 0
		return
	}

	// Incoming GNSS altitude.
	if hasExisting && !existing.AltIsGNSS && altAge >= -2.0 && altAge <= fusionBaroGNSSWindowSec {
		// Fresh baro altitude exists: preserve baro Alt and record GNSS-minus-baro offset.
		existing.GnssDiffFromBaroAlt = incomingAlt - existing.Alt
		existing.Last_GnssDiff = now
		existing.Last_GnssDiffAlt = existing.Alt
		return
	}

	// No fresh baro altitude available; accept GNSS altitude.
	existing.Alt = incomingAlt
	existing.AltIsGNSS = true
	existing.Last_alt = now
	existing.AgeLastAlt = 0
}

// angularDeltaDeg returns the shortest unsigned angular difference in [0, 180] degrees.
func angularDeltaDeg(a, b float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return 180.0
	}
	d := math.Mod(math.Abs(a-b), 360.0)
	if d > 180.0 {
		d = 360.0 - d
	}
	return d
}

func currentFusionTime() time.Time {
	if stratuxClock != nil {
		return stratuxClock.GetTime()
	}
	return time.Now()
}

// timeDeltaSeconds returns now - ref in seconds when both timestamps share a clock domain
// (wall clock vs. stratuxClock monotonic epoch); otherwise returns +Inf.
func timeDeltaSeconds(now, ref time.Time) float64 {
	if now.IsZero() || ref.IsZero() {
		return math.Inf(1)
	}
	// Guard against mixing wall-clock (year >= 2000) with stratuxClock monotonic epoch (year 0001).
	nowWall := now.Year() >= 2000
	refWall := ref.Year() >= 2000
	if nowWall != refWall {
		return math.Inf(1)
	}
	return now.Sub(ref).Seconds()
}

func trafficAgeSeconds(ti TrafficInfo, now time.Time) float64 {
	if ti.Last_seen.IsZero() {
		return ti.Age
	}
	if !now.IsZero() {
		if d := timeDeltaSeconds(now, ti.Last_seen); !math.IsInf(d, 0) {
			return d
		}
	}
	if stratuxClock != nil && ti.Last_seen.Year() < 100 {
		return stratuxClock.Since(ti.Last_seen).Seconds()
	}
	if ti.Last_seen.Year() >= 2000 {
		return time.Since(ti.Last_seen).Seconds()
	}
	return ti.Age
}
