/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	traffic.go: Target management, UAT downlink message processing, 1090ES source input, GDL90 traffic reports.
*/

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/common"
)

//-0b2b48fe3aef1f88621a0856110a31c01105c4e6c4e6c40a9a820300000000000000;rs=7;

/*

HDR:
 MDB Type:          1
 Address:           2B48FE (TIS-B track file address)
SV:
 NIC:               6
 Latitude:          +41.4380
 Longitude:         -84.1056
 Altitude:          2300 ft (barometric)
 N/S velocity:      -65 kt
 E/W velocity:      -98 kt
 Track:             236
 Speed:             117 kt
 Vertical rate:     0 ft/min (from barometric altitude)
 UTC coupling:      no
 TIS-B site ID:     1
MS:
 Emitter category:  No information
 Callsign:          unavailable
 Emergency status:  No emergency
 UAT version:       2
 SIL:               2
 Transmit MSO:      38
 NACp:              8
 NACv:              1
 NICbaro:           0
 Capabilities:
 Active modes:
 Target track type: true heading
AUXSV:
 Sec. altitude:     unavailable

*/

const (
	TRAFFIC_SOURCE_1090ES = 1
	TRAFFIC_SOURCE_UAT    = 2
	TRAFFIC_SOURCE_OGN    = 4
	TRAFFIC_SOURCE_AIS    = 8
	TARGET_TYPE_MODE_S    = 0
	TARGET_TYPE_ADSB      = 1
	TARGET_TYPE_ADSR      = 2
	// Assign next type to UAT messages with address qualifier == 2
	// (code corresponds to any UAT GBT targets with Mode S addresses.
	// These will be displayed with the airplane icon on the traffic UI page.
	// If we see a proper emitter category and NIC > 7, they'll be reassigned to TYPE_ADSR.
	TARGET_TYPE_TISB_S = 3
	TARGET_TYPE_TISB   = 4
	TARGET_TYPE_AIS    = 5
)

type TrafficInfo struct {
	Icao_addr           uint32
	Reg                 string    // Registration. Calculated from Icao_addr for civil aircraft of US registry.
	Tail                string    // Callsign. Transmitted by aircraft.
	Emitter_category    uint8     // Formatted using GDL90 standard, e.g. in a Mode ES report, A7 becomes 0x07, B0 becomes 0x08, etc.
	SurfaceVehicleType	uint16    // Type of service vehicle (when Emitter_category==18) 0..255 is reserved for AIS vessels
	OnGround            bool      // Air-ground status. On-ground is "true".
	Addr_type           uint8     // UAT address qualifier. Used by GDL90 format, so translations for ES TIS-B/ADS-R are needed.
	TargetType          uint8     // types decribed in const above
	SignalLevel         float64   // Signal level, dB RSSI.
	SignalLevelHist     []float64 // last 8 values. For 1090ES we store the last 8 values here. SignalLevel will then become the maximum of these to get more stable data with antenna diversity
	Squawk              int       // Squawk code
	Position_valid      bool      //TODO: set when position report received. Unset after n seconds?
	Lat                 float32   // decimal common.Degrees, north positive
	Lng                 float32   // decimal common.Degrees, east positive
	Alt                 int32     // Pressure altitude, feet
	GnssDiffFromBaroAlt int32     // GNSS altitude above WGS84 datum. Reported in TC 20-22 messages
	AltIsGNSS           bool      // Pressure alt = 0; GNSS alt = 1
	NIC                 int       // Navigation Integrity Category.
	NACp                int       // Navigation Accuracy Category for Position.
	Track               float32   // common.Degrees true
	TurnRate            float32   // Turn rate in deg/sec (negative = turning left, positive = right)
	Speed               uint16    // knots
	Speed_valid         bool      // set when speed report received.
	Vvel                int16     // feet per minute
	Timestamp           time.Time // timestamp of traffic message, UTC
	PriorityStatus      uint8     // Emergency or priority code as defined in GDL90 spec, DO-260B (Type 28 msg) and DO-282B

	// Parameters starting at 'Age' are calculated from last message receipt on each call of sendTrafficUpdates().
	// Mode S transmits position and track in separate messages, and altitude can also be
	// received from interrogations.
	Age                  float64   // Age of last valid position fix or last Mode-S transmission, seconds ago.
	AgeLastAlt           float64   // Age of last altitude message, seconds ago.
	Last_seen            time.Time // Time of last position update (stratuxClock) or Mode-S transmission. Used for timing out expired data.
	Last_alt             time.Time // Time of last altitude update (stratuxClock).
	Last_GnssDiff        time.Time // Time of last GnssDiffFromBaroAlt update (stratuxClock).
	Last_GnssDiffAlt     int32     // Altitude at last GnssDiffFromBaroAlt update.
	Last_speed           time.Time // Time of last velocity and track update (stratuxClock).
	Last_source          uint8     // Last frequency on which this target was received.
	ExtrapolatedPosition bool      //TODO: True if Stratux is "coasting" the target from last known position.
	Last_extrapolation   time.Time
	AgeExtrapolation     float64
	Lat_fix              float32   // Last real, non-extrapolated latitude
	Lng_fix              float32   // Last real, non-extrapolated longitude
	Alt_fix              int32     // Last real, non-extrapolated altitude

	BearingDist_valid    bool      // set when bearing and distance information is valid
	Bearing              float64   // Bearing in common.Degrees true to traffic from ownship, if it can be calculated. Units: common.Degrees.
	Distance             float64   // Distance to traffic from ownship, if it can be calculated. Units: meters.
	DistanceEstimated    float64   // Estimated distance of the target if real distance can't be calculated, Estimated from signal strength with exponential smoothing.
	DistanceEstimatedLastTs time.Time // Used to compute moving average
	ReceivedMsgs         uint64    // Number of messages received by this aircraft
	IsStratux            bool      // Target is equipped with a Stratux that transmits via OGN tracker
	AlertLevel           uint8     // 0=None, 1=Advisory, 2=Warning, 3=Urgent (3D CPA)
	TimeToCPA            float64   // seconds to closest point of approach
	HorizMissDist        float64   // predicted horizontal miss distance (meters)
	VertMissDist         float64   // predicted vertical miss distance (feet)
	Callout              string    // spoken/UI CPA callout phrase (debounced)
	SourceConfidence     int       // fusion source confidence score (0-100)
	FusedSources         []string  // list of fused source names (e.g. ["1090ES", "OGN"])
	//FIXME: Rename variables for consistency, especially "Last_".
}

type dump1090Data struct {
	Icao_addr           uint32
	DF                  int     // Mode S downlink format.
	CA                  int     // Lowest 3 bits of first byte of Mode S message (DF11 and DF17 capability; DF18 control field, zero for all other DF types)
	TypeCode            int     // Mode S type code
	SubtypeCode         int     // Mode S subtype code
	SBS_MsgType         int     // type of SBS message (used in "old" 1090 parsing)
	SignalLevel         float64 // Decimal RSSI (0-1 nominal) as reported by dump1090-mutability. Convert to dB RSSI before setting in TrafficInfo.
	Tail                *string
	Squawk              *int // 12-bit squawk code in octal format
	Emitter_category    *int
	OnGround            *bool
	Lat                 *float32
	Lng                 *float32
	Position_valid      bool
	NACp                *int
	Alt                 *int
	AltIsGNSS           bool   //
	GnssDiffFromBaroAlt *int16 // GNSS height above baro altitude in feet; valid range is -3125 to 3125. +/- 3138 indicates larger difference.
	Vvel                *int16
	Speed_valid         bool
	Speed               *uint16
	Track               *uint16
	Timestamp           time.Time // time traffic last seen, UTC
}

type esmsg struct {
	TimeReceived time.Time
	Data         string
}

var traffic map[uint32]TrafficInfo
var trafficMutex *sync.Mutex
var seenTraffic map[uint32]bool // Historical list of all ICAO addresses seen.

var OwnshipTrafficInfo TrafficInfo

const maxFusedSources = 8

var (
	cpaDebouncer          = NewCalloutDebouncer(15 * time.Second)
	cpaLastPrune          time.Time
	highestCPAThreatMu    sync.RWMutex
	highestCPAThreat      CPAResult
	highestCPAThreatKey   uint32
	highestCPAThreatValid bool
)

func addFusedSource(ti *TrafficInfo, name string) {
	if ti == nil || name == "" {
		return
	}
	for _, existing := range ti.FusedSources {
		if existing == name {
			return
		}
	}
	if len(ti.FusedSources) >= maxFusedSources {
		return
	}
	ti.FusedSources = append(ti.FusedSources, name)
}

func setHighestCPAThreat(key uint32, res CPAResult, valid bool) {
	highestCPAThreatMu.Lock()
	highestCPAThreatKey = key
	highestCPAThreat = res
	highestCPAThreatValid = valid
	highestCPAThreatMu.Unlock()
}

func getHighestCPAThreat() (CPAResult, bool) {
	highestCPAThreatMu.RLock()
	res := highestCPAThreat
	valid := highestCPAThreatValid
	highestCPAThreatMu.RUnlock()
	return res, valid
}

func convertFeetToMeters(feet float32) float32 {
	return feet * 0.3048
}

func convertMetersToFeet(meters float32) float32 {
	return meters / 0.3048
}

func cleanupOldEntries() {
	for key, ti := range traffic {
		
		if ti.Last_source != TRAFFIC_SOURCE_AIS && stratuxClock.Since(ti.Last_seen).Seconds() > 60 { // keep it in the database for up to 60 seconds, so we don't lose tail number, etc...
			delete(traffic, key)
		}

		if ti.Last_source == TRAFFIC_SOURCE_AIS && stratuxClock.Since(ti.Last_seen).Seconds() > 60*15 { // keep it in the database for up to 15 minutes, so we don't lose tail number, etc...
			delete(traffic, key)
		}
	}
}


const maxSeenTrafficEntries = 50000

// markTrafficSeen records an ICAO address in seenTraffic while keeping the map bounded.
// Caller must hold trafficMutex.
func markTrafficSeen(icao uint32) {
	if seenTraffic == nil {
		return
	}
	if _, exists := seenTraffic[icao]; exists {
		return
	}
	if len(seenTraffic) >= maxSeenTrafficEntries {
		// Prune entries that are no longer actively tracked to bound memory growth.
		for k := range seenTraffic {
			if _, active := traffic[k]; !active {
				delete(seenTraffic, k)
				if len(seenTraffic) < maxSeenTrafficEntries/2 {
					break
				}
			}
		}
		if len(seenTraffic) >= maxSeenTrafficEntries {
			return
		}
	}
	seenTraffic[icao] = true
}

func removeTarget(id uint32) {
	var removed TrafficInfo
	var found bool
	trafficMutex.Lock()
	if val, ok := traffic[id]; ok {
		// Make sure the web interface times it out..
		val.Age = 60
		val.Position_valid = false
		removed = val
		found = true
		delete(traffic, id)
	}
	trafficMutex.Unlock()
	if found {
		registerTrafficUpdate(removed)
	}
}

// Checks if the given TrafficInfo is our ownship. As the user can specify multiple ownship
// hex codes, this is able to smartly identify if it really is our ownship.
// If the ti is very close and at same altitude, it is considered to be us
// If it has no position information, we will not take it as ownship, but ignore its data (no mode-s detection for everything that is configured as ownship)
func isOwnshipTrafficInfo(ti TrafficInfo) (isOwnshipInfo bool, shouldIgnore bool) {
	globalStatusMutex.RLock()
	gpsDetectedType := globalStatus.GPS_detected_type
	ognPrevRandomAddr := globalStatus.OGNPrevRandomAddr
	globalStatusMutex.RUnlock()

	globalSettingsMutex.RLock()
	ognAddr := globalSettings.OGNAddr
	ownshipModeS := globalSettings.OwnshipModeS
	debugEnabled := globalSettings.DEBUG
	globalSettingsMutex.RUnlock()

	// First, check if this is our own OGN tracker
	if (gpsDetectedType & 0x0f) == GPS_TYPE_OGNTRACKER {
		ognTrackerCodeInt, err1 := strconv.ParseUint(strings.TrimSpace(ognAddr), 16, 32)
		prevTrackerCodeInt, err2 := strconv.ParseUint(strings.TrimSpace(ognPrevRandomAddr), 16, 32)
		if (err1 == nil && ognTrackerCodeInt != 0 && uint32(ognTrackerCodeInt) == ti.Icao_addr) ||
			(err2 == nil && prevTrackerCodeInt != 0 && uint32(prevTrackerCodeInt) == ti.Icao_addr) {
			isOwnshipInfo = !isGPSValid() // only use OGN tracker as ownship position if we are not equipped with a GPS..
			shouldIgnore = true
			return
		}
	}

	codes := strings.Split(ownshipModeS, ",")
	shouldIgnore = false

	for _, ownCode := range codes {
		ownCodeInt, err := strconv.ParseUint(strings.TrimSpace(ownCode), 16, 32)
		if err != nil || ownCodeInt == 0 {
			continue
		}
		if uint32(ownCodeInt) == ti.Icao_addr {
			if !ti.Position_valid {
				// Can't verify the ownship, ignore it for bearingless display
				shouldIgnore = true
				continue
			}

			gpsValid := isGPSValid()
			mySituation.muGPS.Lock()
			gpsLastTime := mySituation.GPSLastGPSTimeStratuxTime
			gpsGroundSpeed := mySituation.GPSGroundSpeed
			gpsLat := mySituation.GPSLatitude
			gpsLng := mySituation.GPSLongitude
			gpsHAE := mySituation.GPSHeightAboveEllipsoid
			gpsHAcc := mySituation.GPSHorizontalAccuracy
			mySituation.muGPS.Unlock()

			mySituation.muBaro.Lock()
			baroAlt := mySituation.BaroPressureAltitude
			mySituation.muBaro.Unlock()

			// User might have specified the address of another airplane that he regularly flys.
			// If this airplane is currently in the air and we receive it, it gets priority over our ownship information.
			// This is a sanity check to filter out such cases - only accept the ownship data if
			// it somewhat matches our real data
			// because of second-resolution in flarm we assume worst case of +1 second
			timeDiff := math.Abs(ti.Age-stratuxClock.Since(gpsLastTime).Seconds()) + 1
			speed := gpsGroundSpeed
			if ti.Speed_valid {
				speed = math.Max(float64(ti.Speed), gpsGroundSpeed)
			}
			trafficDist := 0.0
			if gpsValid {
				trafficDist, _, _, _ = common.DistRect(float64(gpsLat), float64(gpsLng), float64(ti.Lat), float64(ti.Lng))
			}
			altDiff := 99999.0
			if ti.AltIsGNSS && gpsValid && ti.Alt != 0 {
				altDiff = math.Abs(float64(gpsHAE) - float64(ti.Alt))
			} else if isTempPressValid() && ti.Alt != 0 {
				altDiff = math.Abs(float64(baroAlt - float32(ti.Alt)))
			} else {
				// Cant verify relative altitude.. ignore it but don't use
				shouldIgnore = true
				continue
			}

			// Check if the distance to the ti is plausible
			maxDistMetersIgnore := (timeDiff*speed*0.514444 + float64(gpsHAcc) + 50) * 3
			if trafficDist > maxDistMetersIgnore {
				log.Printf("Skipping ownship %s because it's too far away (%fm, speed=%f, max=%f)", ownCode, trafficDist, speed, maxDistMetersIgnore)
				continue
			}

			// If we have a pressure sensor, and the pressure altitude of traffic and ownship is too big, skip...
			if altDiff > 500 {
				log.Printf("Skipping ownship %s because the altitude is off (%f ft)", ownCode, altDiff)
				continue
			}

			// To really use the information from the ownship traffic info, we have much more
			// strict requirements. At most 5s old and must be much closer
			maxDistMetersOwnship := (timeDiff*speed*0.514444 + float64(gpsHAcc) + 20) * 1.4
			if (!gpsValid || (ti.Age <= 5 && trafficDist < maxDistMetersOwnship)) && !ti.AltIsGNSS {
				isOwnshipInfo = true
			}
			if debugEnabled {
				log.Printf("Using ownship %s. MaxDistIgnore: %f, maxDistOwnShip: %f, dist: %f, altDiff: %f, speed: %f, timeDiffS: %f, useForInfo: %t",
					ownCode, maxDistMetersIgnore, maxDistMetersOwnship, trafficDist, altDiff, speed, timeDiff, isOwnshipInfo)
			}
			shouldIgnore = true
			return
		}
	}
	isOwnshipInfo = false
	return
}

func sendTrafficUpdates() {
	var trafficBroadcasts []TrafficInfo
	var radarBroadcasts []TrafficInfo

	globalSettingsMutex.RLock()
	debugEnabled := globalSettings.DEBUG
	radarLimits := globalSettings.RadarLimits
	radarRange := globalSettings.RadarRange
	estimateBearinglessDist := globalSettings.EstimateBearinglessDist
	cpaAlertsEnabled := globalSettings.CPA_Alerts_Enabled
	globalSettingsMutex.RUnlock()

	var currAlt, baroVvel float32
	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
		currAlt = mySituation.BaroPressureAltitude
		baroVvel = mySituation.BaroVerticalSpeed
		mySituation.muBaro.Unlock()
	} else {
		currAlt = mySituation.BaroPressureAltitude
		baroVvel = mySituation.BaroVerticalSpeed
	}

	var gpsLat, gpsLng, gpsAltMSL, gpsCourse, gpsVvelFps float32
	var gpsSpeedKt float64
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsAltMSL = mySituation.GPSAltitudeMSL
		gpsCourse = mySituation.GPSTrueCourse
		gpsSpeedKt = mySituation.GPSGroundSpeed
		gpsVvelFps = mySituation.GPSVerticalSpeed
		mySituation.muGPS.Unlock()
	} else {
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsAltMSL = mySituation.GPSAltitudeMSL
		gpsCourse = mySituation.GPSTrueCourse
		gpsSpeedKt = mySituation.GPSGroundSpeed
		gpsVvelFps = mySituation.GPSVerticalSpeed
	}

	gpsValid := isGPSValid()
	baroValid := isTempPressValid()
	if currAlt == 99999 { // no valid BaroAlt, take GPS instead, better than nothing
		currAlt = gpsAltMSL
	}
	ownAltFt := float64(currAlt)
	if !baroValid {
		ownAltFt = float64(gpsAltMSL)
	}
	ownVvelFpm := float64(gpsVvelFps) * 60.0
	if baroValid {
		ownVvelFpm = float64(baroVvel)
	}
	ownTrackDeg := float64(gpsCourse)
	ownSpeedKt := gpsSpeedKt

	var uatTracking uint16
	var esTracking uint16

	func() {
		trafficMutex.Lock()
		defer trafficMutex.Unlock()
		cleanupOldEntries()

		// Summarize number of UAT and 1090ES traffic targets for reports that follow.
		for _, traf := range traffic {
			switch traf.Last_source {
			case TRAFFIC_SOURCE_1090ES:
				esTracking++
			case TRAFFIC_SOURCE_UAT:
				uatTracking++
			}
		}

		var bestEstimate TrafficInfo
		var highestAlarmLevel uint8
		var highestAlarmTraffic TrafficInfo

		nowMono := stratuxClock.GetTime()
		nowSec := nowMono.Second()
		cpaCfg := DefaultCPAConfig()
		ownOnGround := OwnshipTrafficInfo.Position_valid && OwnshipTrafficInfo.OnGround
		var cpaResults map[uint32]CPAResult
		if cpaAlertsEnabled && gpsValid {
			cpaResults = make(map[uint32]CPAResult)
		}

		if debugEnabled && (nowSec%15) == 0 {
			log.Printf("List of all aircraft being tracked:\n")
			log.Printf("==================================================================\n")
		}
		for key, ti := range traffic { // ForeFlight 7.5 chokes at ~1000-2000 messages depending on iDevice RAM. Practical limit likely around ~500 aircraft without filtering.
			if gpsValid && ti.Position_valid {
				dist, bearing := common.Distance(float64(gpsLat), float64(gpsLng), float64(ti.Lat), float64(ti.Lng))
				ti.Distance = dist
				ti.Bearing = bearing
				ti.BearingDist_valid = true
			} else {
				ti.Distance = 0
				ti.Bearing = 0
				ti.BearingDist_valid = false
			}
			ti.Age = stratuxClock.Since(ti.Last_seen).Seconds()
			ti.AgeExtrapolation = stratuxClock.Since(ti.Last_extrapolation).Seconds()
			ti.AgeLastAlt = stratuxClock.Since(ti.Last_alt).Seconds()

			// Keep non-extrapolated traffic for 6 seconds, but extrapolate for 20
			isCurrent := (ti.ExtrapolatedPosition && ti.AgeExtrapolation < 2 && ti.Age < 25) || (!ti.ExtrapolatedPosition && ti.Age < 6)

			isOwnshipTi, shouldIgnore := isOwnshipTrafficInfo(ti)

			if cpaAlertsEnabled && gpsValid && !isOwnshipTi && !shouldIgnore && ti.Position_valid && isCurrent {
				res := ComputeCPA(
					float64(gpsLat), float64(gpsLng), ownAltFt,
					ownTrackDeg, ownSpeedKt, ownVvelFpm,
					ownOnGround, ti, cpaCfg,
				)
				if res.Valid {
					ti.AlertLevel = res.AlertLevel
					ti.TimeToCPA = res.TimeToCPASec
					ti.HorizMissDist = res.HorizMissMeters
					ti.VertMissDist = res.VertMissFeet
					if res.AlertLevel == CPAAlertNone {
						ti.Callout = ""
					} else if cpaDebouncer.ShouldAnnounce(key, res, nowMono) {
						ti.Callout = res.Callout
					}
					cpaResults[key] = res
				} else {
					ti.AlertLevel = CPAAlertNone
					ti.TimeToCPA = 0
					ti.HorizMissDist = 0
					ti.VertMissDist = 0
					ti.Callout = ""
				}
			} else {
				ti.AlertLevel = CPAAlertNone
				ti.TimeToCPA = 0
				ti.HorizMissDist = 0
				ti.VertMissDist = 0
				ti.Callout = ""
			}

			// As bearingless targets, we show the closest recent estimated traffic that is between +-2000ft
			if !shouldIgnore && !ti.Position_valid && ti.Age < 6 && ti.DistanceEstimated > 0 &&
				(bestEstimate.DistanceEstimated == 0 || ti.DistanceEstimated < bestEstimate.DistanceEstimated) {
				if ti.Alt != 0 && math.Abs(float64(ti.Alt)-float64(currAlt)) < 2000 {
					bestEstimate = ti
				}
			}

			// DEBUG: Print the list of all tracked targets (with data) to the log every 15 seconds if "DEBUG" option is enabled
			if debugEnabled && (nowSec%15) == 0 {
				s_out, err := json.Marshal(ti)
				if err != nil {
					log.Printf("Error generating output: %s\n", err.Error())
				} else {
					log.Printf("%X => %s\n", ti.Icao_addr, string(s_out))
				}
			}
			traffic[key] = ti // write the updated ti back to the map
			if ti.Age > 2 {   // if nothing polls an inactive ti, it won't push to the webUI, and its Age won't update.
				trafficBroadcasts = append(trafficBroadcasts, ti)
			}
			if !shouldIgnore && isCurrent {
				if float32(ti.Alt) <= currAlt+float32(radarLimits)*1.3 && //take 30% more to see moving outs
					float32(ti.Alt) >= currAlt-float32(radarLimits)*1.3 && // altitude lower than upper boundary
					(!ti.Position_valid || ti.Distance < float64(radarRange)*1852.0*1.3) { //allow more so that aircraft moves out
					radarBroadcasts = append(radarBroadcasts, ti)
				}
			}
			if ti.Position_valid && isCurrent { // ... but don't pass stale data to the EFB.
				//TODO: Coast old traffic? Need to determine how FF, WingX, etc deal with stale targets.
				logTraffic(ti) // only add to the SQLite log if it's not stale

				if isOwnshipTi {
					if debugEnabled {
						log.Printf("Ownship target detected for code %X\n", ti.Icao_addr)
					}
					OwnshipTrafficInfo = ti
				} else if !shouldIgnore {
					priority := computeTrafficPriority(&ti)
					sendGDL90(makeTrafficReportMsg(ti), time.Second, priority)
					thisMsgFLARM, validFLARM, alarmLevel := makeFlarmPFLAAString(ti)
					if alarmLevel > highestAlarmLevel {
						highestAlarmLevel = alarmLevel
						highestAlarmTraffic = ti
					}

					var trafficCallsign string
					if len(ti.Tail) > 0 {
						trafficCallsign = ti.Tail
					} else {
						trafficCallsign = fmt.Sprintf("%X_%d", ti.Icao_addr, ti.Squawk)
					}

					// send traffic message to X-Plane
					sendXPlane(createXPlaneTrafficMsg(ti.Icao_addr, ti.Lat, ti.Lng, ti.Alt, uint32(ti.Speed), int32(ti.Vvel), ti.OnGround, uint32(ti.Track), trafficCallsign), time.Second, priority)
					if validFLARM {
						sendNetFLARM(thisMsgFLARM, time.Second, priority)
					}
				}
			}
		}

		if cpaAlertsEnabled && gpsValid {
			bestKey, bestRes, hasThreat := SelectHighestThreat(cpaResults)
			setHighestCPAThreat(bestKey, bestRes, hasThreat)
			if hasThreat {
				if threatTi, ok := traffic[bestKey]; ok {
					highestAlarmTraffic = threatTi
				}
			}
			if cpaLastPrune.IsZero() || nowMono.Sub(cpaLastPrune) >= 60*time.Second {
				cpaDebouncer.Prune(nowMono, 2*time.Minute)
				cpaLastPrune = nowMono
			}
		} else {
			setHighestCPAThreat(0, CPAResult{}, false)
		}

		// Also send the nearest best bearingless
		if bestEstimate.DistanceEstimated > 0 && bestEstimate.DistanceEstimated < 15000 {
			if gpsValid {
				if estimateBearinglessDist {
					fakeTargets := calculateModeSFakeTargets(bestEstimate)
					fakeMsg := make([]byte, 0)
					for _, ti := range fakeTargets {
						fakeMsg = append(fakeMsg, makeTrafficReportMsg(ti)...)
					}
					prio := computeTrafficPriority(&fakeTargets[0])
					sendGDL90(fakeMsg, time.Second, prio)
				}
				prio := computeTrafficPriority(&bestEstimate)
				msg, valid, _ := makeFlarmPFLAAString(bestEstimate)
				if valid {
					sendNetFLARM(msg, time.Second, prio)
				}
			}
		}

		msgPFLAU := makeFlarmPFLAUString(highestAlarmTraffic)
		sendNetFLARM(msgPFLAU, time.Second, 0)
	}()

	globalStatusMutex.Lock()
	globalStatus.UAT_traffic_targets_tracking = uatTracking
	globalStatus.ES_traffic_targets_tracking = esTracking
	globalStatusMutex.Unlock()

	for _, ti := range trafficBroadcasts {
		trafficUpdate.SendJSON(ti)
	}
	for _, ti := range radarBroadcasts {
		radarUpdate.SendJSON(ti)
	}
}

func computeTrafficPriority(ti *TrafficInfo) int32 {
	if !ti.BearingDist_valid || ti.Alt == 0 {
		return 9999999
	}
	var myAlt float32
	if isTempPressValid() {
		myAlt = mySituation.BaroPressureAltitude
	} else {
		myAlt = mySituation.GPSAltitudeMSL
	}
	altDiff := math.Abs(float64(myAlt) - float64(ti.Alt))
	// assumes 333ft vertical difference has same priority 1000m horizontal
	// This will usually produce priorities ranging from around 0-10
	return int32((altDiff / 3.33 + ti.Distance) / 10000.0)
}

// Used to tune to our radios. We compare our estimate to real values for ADS-B Traffic.
// If we tend to estimate too high, we reduce this value, otherwise we increase it.
// We also try to correct for different transponder transmit power, by assuming that aircraft that fly high are bigger aircraft
// and have a stronger transponder. Low aircraft are small aircraft with weak transmission power.
// This is only a wild guess, but seems to help a bit. To do so, we use different estimatedDistFactors for different
// altitude buckets: <5000ft, 5000-10000ft, >10000ft
var estimatedDistFactors [3]float64 = [3]float64{2500.0, 2800.0, 3000.0}

func estimateDistance(ti *TrafficInfo) {
	if ti.Last_source != TRAFFIC_SOURCE_1090ES {
		return
	}
	if ti.SignalLevel <= -100 || math.IsNaN(ti.SignalLevel) || math.IsInf(ti.SignalLevel, 0) {
		return
	}
	altClass := int32(math.Max(0.0, math.Min(float64(ti.Alt/5000), 2.0)))
	dist := math.Pow(2.0, -ti.SignalLevel/6.0) * estimatedDistFactors[altClass] // distance approx. in meters, 6dB for double distance
	if math.IsNaN(dist) || math.IsInf(dist, 0) || dist <= 0 {
		return
	}

	lambda := 0.2
	now := stratuxClock.GetTime()
	if ti.DistanceEstimatedLastTs.IsZero() {
		ti.DistanceEstimatedLastTs = now
		ti.DistanceEstimated = dist
	} else {
		timeDiff := now.Sub(ti.DistanceEstimatedLastTs).Seconds() * 1000
		ti.DistanceEstimatedLastTs = now
		if timeDiff < 0.0 {
			return
		}
		expon := math.Exp(-timeDiff / 100 * lambda)
		if math.IsNaN(ti.DistanceEstimated) || math.IsInf(ti.DistanceEstimated, 0) || ti.DistanceEstimated <= 0 {
			ti.DistanceEstimated = dist
		} else {
			ti.DistanceEstimated = ti.DistanceEstimated*expon + dist*(1-expon)
		}
	}

	if ti.DistanceEstimated <= 0 || math.IsNaN(ti.DistanceEstimated) || math.IsInf(ti.DistanceEstimated, 0) {
		return
	}

	// Only learn from 1090ES ADS-B targets
	// We ignore targets that are too far away (a lot of signal strength fluctuation), too close (non-reception cone or ownship)
	// and of course extrapolated targets and invalid signal levels
	if ti.BearingDist_valid && !math.IsNaN(ti.Distance) && !math.IsInf(ti.Distance, 0) &&
		ti.Distance < 50000 && ti.Distance > 1500 && ti.Last_source == TRAFFIC_SOURCE_1090ES &&
		ti.TargetType == TARGET_TYPE_ADSB && ti.SignalLevel > -30 && ti.SignalLevel < 0 && !ti.ExtrapolatedPosition {
		var errorFactor float64
		if ti.DistanceEstimated > ti.Distance {
			errorFactor = -(ti.DistanceEstimated / ti.Distance)
		} else {
			errorFactor = ti.Distance / ti.DistanceEstimated
		}
		if math.IsNaN(errorFactor) || math.IsInf(errorFactor, 0) {
			return
		}
		if errorFactor > 50.0 {
			errorFactor = 50.0
		} else if errorFactor < -50.0 {
			errorFactor = -50.0
		}
		estimatedDistFactors[altClass] += errorFactor
		if estimatedDistFactors[altClass] < 1.0 || math.IsNaN(estimatedDistFactors[altClass]) {
			estimatedDistFactors[altClass] = 1.0
		} else if estimatedDistFactors[altClass] > 100000.0 || math.IsInf(estimatedDistFactors[altClass], 0) {
			estimatedDistFactors[altClass] = 100000.0
		}
	}
}

// calculates coordinates of a point defined by a location, a bearing, and a distance, thanks to 0x74-0x62
func calcLocationForBearingDistance(lat1, lon1, bearingDeg, distanceNm float64) (lat2, lon2 float64) {
	lat1Rad := common.Radians(lat1)
	lon1Rad := common.Radians(lon1)
	bearingRad := common.Radians(bearingDeg)
	distanceRad := distanceNm / (180 * 60 / math.Pi)

	sinLat2 := math.Sin(lat1Rad)*math.Cos(distanceRad) + math.Cos(lat1Rad)*math.Sin(distanceRad)*math.Cos(bearingRad)
	if sinLat2 > 1.0 {
		sinLat2 = 1.0
	} else if sinLat2 < -1.0 {
		sinLat2 = -1.0
	}
	lat2Rad := math.Asin(sinLat2)
	distanceLon := -math.Atan2(math.Sin(bearingRad)*math.Sin(distanceRad)*math.Cos(lat1Rad), math.Cos(distanceRad)-math.Sin(lat1Rad)*math.Sin(lat2Rad))
	lon2Rad := math.Mod(lon1Rad-distanceLon+math.Pi, 2.0*math.Pi)
	if lon2Rad < 0 {
		lon2Rad += 2.0 * math.Pi
	}
	lon2Rad -= math.Pi

	lat2 = common.Degrees(lat2Rad)
	lon2 = common.Degrees(lon2Rad)

	return
}

func calculateModeSFakeTargets(bearinglessTi TrafficInfo) []TrafficInfo {
	result := make([]TrafficInfo, 8)
	for i := 0; i < 8; i++ {
		ti := bearinglessTi
		lat, lon := calcLocationForBearingDistance(float64(mySituation.GPSLatitude), float64(mySituation.GPSLongitude), float64(i * 45), bearinglessTi.DistanceEstimated / 1852.0)
		ti.Lat = float32(lat)
		ti.Lng = float32(lon)
		ti.Icao_addr = uint32(i) // So that the EFB shows it as a different aircraft
		ti.Speed = 0
		ti.Speed_valid = true
		ti.Tail = "MODE S"
		result[i] = ti
	}
	return result
}

func postProcessTraffic(ti *TrafficInfo) {
	ti.ReceivedMsgs += 1
	estimateDistance(ti)
}

// Send update to attached JSON client.
func registerTrafficUpdate(ti TrafficInfo) {
	//logTraffic(ti) // moved to sendTrafficUpdates() to reduce SQLite log size
	/*
		if !ti.Position_valid { // Don't send unless a valid position exists.
			return
		}
	*/ // Send all traffic to the websocket and let JS sort it out. This will provide user indication of why they see 1000 ES messages and no traffic.
	trafficUpdate.SendJSON(ti)
}

func isTrafficAlertable(ti TrafficInfo) bool {
	// Set alert bit if possible and traffic is within some threshold
	// TODO: Could be more intelligent, taking into account headings etc.
	if !ti.BearingDist_valid {
		// If not able to calculate the distance to the target, let the alert bit be set always.
		return true
	}
	if ti.BearingDist_valid &&
		ti.Distance < 3704 { // 3704 meters, 2 nm.
		return true
	}

	return false
}

func makeTrafficReportMsg(ti TrafficInfo) []byte {
	msg := make([]byte, 28)
	// See p.16.
	msg[0] = 0x14 // Message type "Traffic Report".

	// Address type (lower nibble); upper nibble is Traffic Alert status per GDL90 ICD pg. 18.
	msg[1] = ti.Addr_type & 0x0F

	globalSettingsMutex.RLock()
	cpaAlertsEnabled := globalSettings.CPA_Alerts_Enabled
	globalSettingsMutex.RUnlock()

	// Set alert if needed
	if cpaAlertsEnabled {
		if ti.AlertLevel >= CPAAlertWarning {
			// Set the alert bit. See pg. 18 of GDL90 ICD.
			msg[1] |= 0x10
		}
	} else if isTrafficAlertable(ti) {
		// Set the alert bit.  See pg. 18 of GDL90 ICD
		msg[1] |= 0x10
	}

	// ICAO Address.
	msg[2] = byte((ti.Icao_addr & 0x00FF0000) >> 16)
	msg[3] = byte((ti.Icao_addr & 0x0000FF00) >> 8)
	msg[4] = byte((ti.Icao_addr & 0x000000FF))

	lat := float32(ti.Lat)
	tmp := makeLatLng(lat)

	msg[5] = tmp[0] // Latitude.
	msg[6] = tmp[1] // Latitude.
	msg[7] = tmp[2] // Latitude.

	lng := float32(ti.Lng)
	tmp = makeLatLng(lng)

	msg[8] = tmp[0]  // Longitude.
	msg[9] = tmp[1]  // Longitude.
	msg[10] = tmp[2] // Longitude.

	// Altitude: OK
	// GDL 90 Data Interface Specification examples:
	// where 1,000 foot offset and 25 foot resolution (1,000 / 25 = 40)
	//    -1,000 feet               0x000
	//    0 feet                    0x028
	//    +1000 feet                0x050
	//    +101,350 feet             0xFFE
	//    Invalid or unavailable    0xFFF
	//
	// Algo example at: https://play.golang.org/p/VXCckSdsvT
	//
	// GDL90 expects barometric altitude in traffic reports
	var baroAlt int32
	if ti.AltIsGNSS && isTempPressValid() && isGPSValid() {
		mySituation.muGPS.Lock()
		geoidSep := mySituation.GPSGeoidSep
		gpsAltMSL := mySituation.GPSAltitudeMSL
		mySituation.muGPS.Unlock()
		mySituation.muBaro.Lock()
		ownBaroAlt := mySituation.BaroPressureAltitude
		mySituation.muBaro.Unlock()

		// Convert from GPS geoid height to barometric altitude
		baroAlt = ti.Alt - int32(geoidSep)
		baroAlt = baroAlt - int32(gpsAltMSL) + int32(ownBaroAlt)
	} else {
		baroAlt = ti.Alt
	}
	var encodedAlt int16
	if baroAlt < -1000 || baroAlt > 101350 {
		encodedAlt = 0x0FFF
	} else {
		// output guaranteed to be between 0x0000 and 0x0FFE
		encodedAlt = int16((baroAlt / 25) + 40)
	}
	msg[11] = byte((encodedAlt & 0xFF0) >> 4) // Altitude.
	msg[12] = byte((encodedAlt & 0x00F) << 4)

	// "m" field. Lower four bits define indicator bits:
	// - - 0 0   "tt" (msg[17]) is not valid
	// - - 0 1   "tt" is true track
	// - - 1 0   "tt" is magnetic heading
	// - - 1 1   "tt" is true heading
	// - 0 - -   Report is updated (current data)
	// - 1 - -   Report is extrapolated
	// 0 - - -   On ground
	// 1 - - -   Airborne

	// Define tt type / validity
	if ti.Speed_valid {
		msg[12] = msg[12] | 0x01 // assume true track
	}

	if ti.ExtrapolatedPosition {
		msg[12] = msg[12] | 0x04
	}

	if !ti.OnGround {
		msg[12] = msg[12] | 0x08 // Airborne.
	}

	// Position containment / navigational accuracy
	msg[13] = ((byte(ti.NIC) << 4) & 0xF0) | (byte(ti.NACp) & 0x0F)

	// Horizontal velocity (speed): 0x000..0xFFE valid, 0xFFF invalid.
	speed := uint16(0xFFF)
	if ti.Speed_valid {
		speed = ti.Speed
		if speed > 0xFFE {
			speed = 0xFFE
		}
	}
	msg[14] = byte((speed & 0x0FF0) >> 4)
	msg[15] = byte((speed & 0x000F) << 4)

	// Vertical velocity (64fpm resolution, signed 12-bit, 0x800 reserved for unavailable).
	vvel := ti.Vvel / 64
	if vvel > 511 {
		vvel = 511
	} else if vvel < -511 {
		vvel = -511
	}
	msg[15] = msg[15] | byte((vvel&0x0F00)>>8)
	msg[16] = byte(vvel & 0x00FF)

	// Track: normalize and round to nearest 8-bit bin.
	tempTrack := normalizeAngle360(float64(ti.Track) + float64(TRACK_RESOLUTION)/2.0)
	trk := uint8(int(tempTrack/float64(TRACK_RESOLUTION)) & 0xFF) // Resolution is ~1.4 common.Degrees.
	msg[17] = byte(trk)

	emitterCat := ti.Emitter_category
	if emitterCat == 0 && ti.TargetType == TARGET_TYPE_REMOTEID {
		emitterCat = 14 // GDL90 UAV emitter category
	}
	msg[18] = emitterCat

	// msg[19] to msg[26] are "call sign" (tail), space-padded per GDL90 ICD.
	for i := 19; i < 27; i++ {
		msg[i] = 0x20
	}
	for i := 0; i < len(ti.Tail) && i < 8; i++ {
		c := byte(ti.Tail[i])
		if c != 0x20 && !((c >= 48) && (c <= 57)) && !((c >= 65) && (c <= 90)) && c != 'e' && c != 'u' && c != 'a' && c != 'r' && c != 't' { // See p.24, FAA ref.
			c = byte(0x20)
		}
		msg[19+i] = c
	}

	//msg[27] is priority / emergency status per GDL90 spec (DO260B and DO282B are same codes)
	msg[27] = ti.PriorityStatus << 4

	return prepareMessage(msg)
}

// parseDownlinkReport decodes a UAT downlink message to extract identity, state vector, and mode status data.
// Decoded data is used to update a TrafficInfo object, keyed to the 24-bit ICAO code contained in the
// downlink message.
// Inputs are a checksum-verified hex string corresponding to the 18 or 34-byte UAT
// message, and an int representing UAT signal amplitude (0-1000).
func parseDownlinkReport(s string, signalLevel int) {
	if len(s) <= 1 {
		return
	}
	s = s[1:]
	if len(s)%2 != 0 {
		return
	}
	frameLen := len(s) / 2
	if frameLen < 18 {
		return
	}
	frame := make([]byte, frameLen)
	if _, err := hex.Decode(frame, []byte(s)); err != nil {
		return
	}

	// Extract header
	msg_type := (uint8(frame[0]) >> 3) & 0x1f
	addr_type := uint8(frame[0]) & 0x07
	icao_addr := (uint32(frame[1]) << 16) | (uint32(frame[2]) << 8) | uint32(frame[3])

	globalSettingsMutex.RLock()
	debugEnabled := globalSettings.DEBUG
	displayTrafficSource := globalSettings.DisplayTrafficSource
	globalSettingsMutex.RUnlock()

	var (
		uat_version       byte // sent as part of MS element, byte 24
		hasMS             bool
		msEmitterCategory uint8
		hasTail           bool
		msTail            string
		hasSquawk         bool
		msSquawk          int
		msNACp            int
		msPriorityStatus  uint8
	)

	// Extract parameters from Mode Status elements, if available (requires 34-byte long frame).
	if (msg_type == 1 || msg_type == 3) && len(frame) >= 34 {
		hasMS = true
		uat_version = (frame[23] >> 2) & 0x07

		v := (uint16(frame[17]) << 8) | (uint16(frame[18]))
		msEmitterCategory = uint8((v / 1600) % 40)

		csid := (frame[26] >> 1) & 0x01
		if csid == 1 {
			base40_alphabet := string("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ  ..")
			tail := ""

			v := (uint16(frame[17]) << 8) | uint16(frame[18])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			v = (uint16(frame[19]) << 8) | uint16(frame[20])
			tail += string(base40_alphabet[(v/1600)%40])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			v = (uint16(frame[21]) << 8) | uint16(frame[22])
			tail += string(base40_alphabet[(v/1600)%40])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			msTail = strings.Trim(tail, " ")
			hasTail = true
		} else if uat_version >= 2 {
			v := (uint16(frame[17]) << 8) | uint16(frame[18])
			squawk_a := (v / 40) % 40
			squawk_b := v % 40
			v = (uint16(frame[19]) << 8) | uint16(frame[20])
			squawk_c := (v / 1600) % 40
			squawk_d := (v / 40) % 40
			squawk := 1000*squawk_a + 100*squawk_b + 10*squawk_c + squawk_d
			msSquawk = int(squawk)
			hasSquawk = true
		}

		msNACp = int((frame[25] >> 4) & 0x0F)
		msPriorityStatus = (frame[23] >> 5) & 0x07

		if debugEnabled {
			var status_sil byte
			var status_sda byte
			var status_nacv byte
			var capability_uat_in bool
			var capability_1090_in bool

			status_sil = frame[23] & 0x03
			status_nacv = (frame[25] >> 1) & 0x07

			if uat_version == 2 {
				status_sda = frame[24] & 0x03
				capability_uat_in = (frame[26] >> 7) != 0
				capability_1090_in = ((frame[26] >> 6) & 0x01) != 0
			}

			log.Printf("Supplemental UAT Mode Status for %06X: Version = %d; SIL = %d; SDA = %d; NACv = %d; 978 In = %v; 1090 In = %v\n", icao_addr, uat_version, status_sil, status_sda, status_nacv, capability_uat_in, capability_1090_in)
		}
	}

	nic := int(frame[11] & 0x0F)

	var power float64
	if signalLevel > 0 {
		power = 20 * (math.Log10(float64(signalLevel) / 1000)) // reported amplitude is 0-1000. Normalize to max = 1 and do amplitude dB calculation (20 dB per decade)
	} else {
		power = -999
	}

	raw_lat := (uint32(frame[4]) << 15) | (uint32(frame[5]) << 7) | (uint32(frame[6]) >> 1)
	raw_lon := ((uint32(frame[6]) & 0x01) << 23) | (uint32(frame[7]) << 15) | (uint32(frame[8]) << 7) | (uint32(frame[9]) >> 1)

	lat := float32(0.0)
	lng := float32(0.0)
	position_valid := false
	if (raw_lat != 0) && (raw_lon != 0) {
		position_valid = true
		lat = float32(raw_lat) * 360.0 / 16777216.0
		if lat > 90 {
			lat = lat - 180
		}
		lng = float32(raw_lon) * 360.0 / 16777216.0
		if lng > 180 {
			lng = lng - 180
		}
	}

	raw_alt := (int32(frame[10]) << 4) | ((int32(frame[11]) & 0xf0) >> 4)
	alt_geo := false // Default case (i.e. 'false') is barometric
	alt := int32(0)
	if raw_alt != 0 {
		alt_geo = (uint8(frame[9]) & 1) != 0
		alt = ((raw_alt - 1) * 25) - 1000
	}

	airground_state := (uint8(frame[12]) >> 6) & 0x03

	ns_vel := int32(0)
	ew_vel := int32(0)
	track := float32(0)
	speed_valid := false
	speed := uint16(0)
	vvel := int16(0)
	hasOnGround := false
	onGround := false
	if airground_state == 0 || airground_state == 1 { // Subsonic. Supersonic.
		hasOnGround = true
		onGround = false
		ns_vel_valid := false
		ew_vel_valid := false
		raw_ns := ((int16(frame[12]) & 0x1f) << 6) | ((int16(frame[13]) & 0xfc) >> 2)
		if (raw_ns & 0x3ff) != 0 {
			ns_vel_valid = true
			ns_vel = int32((raw_ns & 0x3ff) - 1)
			if (raw_ns & 0x400) != 0 {
				ns_vel = 0 - ns_vel
			}
			if airground_state == 1 {
				ns_vel = ns_vel * 4
			}
		}
		raw_ew := ((int16(frame[13]) & 0x03) << 9) | (int16(frame[14]) << 1) | ((int16(frame[15] & 0x80)) >> 7)
		if (raw_ew & 0x3ff) != 0 {
			ew_vel_valid = true
			ew_vel = int32((raw_ew & 0x3ff) - 1)
			if (raw_ew & 0x400) != 0 {
				ew_vel = 0 - ew_vel
			}
			if airground_state == 1 {
				ew_vel = ew_vel * 4
			}
		}
		if ns_vel_valid && ew_vel_valid {
			if ns_vel != 0 || ew_vel != 0 {
				track = float32((360 + 90 - (int16(math.Atan2(float64(ns_vel), float64(ew_vel)) * 180 / math.Pi))) % 360)
			}
			speed_valid = true
			speed = uint16(math.Sqrt(float64((ns_vel * ns_vel) + (ew_vel * ew_vel))))
		}

		raw_vvel := ((int16(frame[15]) & 0x7f) << 4) | ((int16(frame[16]) & 0xf0) >> 4)
		if (raw_vvel & 0x1ff) != 0 {
			vvel = ((raw_vvel & 0x1ff) - 1) * 64
			if (raw_vvel & 0x200) != 0 {
				vvel = 0 - vvel
			}
		}
	} else if airground_state == 2 { // Ground vehicle.
		hasOnGround = true
		onGround = true
		raw_gs := ((uint16(frame[12]) & 0x1f) << 6) | ((uint16(frame[13]) & 0xfc) >> 2)
		if raw_gs != 0 {
			speed_valid = true
			speed = (raw_gs & 0x3ff) - 1
		}
		raw_track := ((uint16(frame[13]) & 0x03) << 9) | (uint16(frame[14]) << 1) | ((uint16(frame[15]) & 0x80) >> 7)
		track = float32((raw_track & 0x1ff) * 360 / 512)
	}

	var ti TrafficInfo

	func() {
		trafficMutex.Lock()
		defer trafficMutex.Unlock()

		nowMono := stratuxClock.GetTime()

		// Resolve canonical key via traffic fusion (exact ICAO, DDB alias, or 3D spatial/velocity correlation).
		key, matchedExisting := globalTrafficFusion.ResolveCanonicalKeyWithContext(
			icao_addr, 0, TRAFFIC_SOURCE_UAT,
			lat, lng, alt, track, speed, speed_valid, position_valid, nowMono, traffic,
		)
		if matchedExisting {
			ti = traffic[key]
		} else {
			ti.Last_seen = nowMono // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
			ti.Icao_addr = icao_addr
			ti.ExtrapolatedPosition = false

			thisReg, validReg := icao2reg(icao_addr)
			if validReg {
				ti.Reg = thisReg
				ti.Tail = thisReg
			}
		}

		effEmitterCat := ti.Emitter_category
		if hasMS {
			effEmitterCat = msEmitterCategory
		}
		incomingTargetType := ti.TargetType
		if addr_type == 0 {
			incomingTargetType = TARGET_TYPE_ADSB
		} else if addr_type == 3 {
			incomingTargetType = TARGET_TYPE_TISB
		} else if addr_type == 6 {
			incomingTargetType = TARGET_TYPE_ADSR
		} else if addr_type == 2 {
			incomingTargetType = TARGET_TYPE_TISB_S
			if (nic >= 7) && (effEmitterCat > 0) { // If NIC is sufficiently high and emitter type is transmitted, we'll assume it's ADS-R.
				incomingTargetType = TARGET_TYPE_ADSR
			}
		}

		acceptPos := !matchedExisting || ShouldAcceptPositionUpdate(ti, TRAFFIC_SOURCE_UAT, incomingTargetType, false, nowMono)

		if hasMS {
			if msEmitterCategory > 0 || ti.Emitter_category == 0 {
				ti.Emitter_category = msEmitterCategory
			}
			if hasTail && (acceptPos || len(ti.Tail) == 0) {
				ti.Tail = msTail
			}
			if hasSquawk {
				ti.Squawk = msSquawk
			}
			if acceptPos || ti.NACp == 0 {
				ti.NACp = msNACp
			}
			if acceptPos || msPriorityStatus > 0 {
				ti.PriorityStatus = msPriorityStatus
			}
		}

		if acceptPos {
			ti.Addr_type = addr_type
			ti.NIC = nic
			ti.SignalLevel = power
			ti.TargetType = incomingTargetType

			// This is a hack to show the source of the traffic on moving maps.
			if displayTrafficSource {
				type_code := " "
				switch ti.TargetType {
				case TARGET_TYPE_ADSB:
					type_code = "a"
				case TARGET_TYPE_ADSR, TARGET_TYPE_TISB_S:
					type_code = "r"
				case TARGET_TYPE_TISB:
					type_code = "t"
				}

				if len(ti.Tail) == 0 {
					ti.Tail = "u" + type_code
				} else if len(ti.Tail) < 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
					ti.Tail = "u" + type_code + ti.Tail
				} else if len(ti.Tail) == 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
					ti.Tail = "u" + type_code + ti.Tail[1:]
				} else if len(ti.Tail) > 1 { // bounds checking
					ti.Tail = "u" + type_code + ti.Tail[2:]
				}
			}
		}

		if acceptPos {
			ti.Position_valid = position_valid
			if ti.Position_valid {
				ti.Lat = lat
				ti.Lng = lng
				if isGPSValid() {
					mySituation.muGPS.Lock()
					ownLat := mySituation.GPSLatitude
					ownLng := mySituation.GPSLongitude
					mySituation.muGPS.Unlock()
					ti.Distance, ti.Bearing = common.Distance(float64(ownLat), float64(ownLng), float64(ti.Lat), float64(ti.Lng))
					ti.BearingDist_valid = true
				}
				ti.Last_seen = nowMono
				ti.Age = 0
				ti.ExtrapolatedPosition = false
			}

			if hasOnGround {
				ti.OnGround = onGround
			}
			ti.Track = track
			ti.Speed = speed
			ti.Vvel = vvel
			ti.Speed_valid = speed_valid
			if ti.Speed_valid {
				ti.Last_speed = nowMono
			}

			ti.Timestamp = time.Now()
			ti.Last_source = TRAFFIC_SOURCE_UAT
		}

		if raw_alt != 0 {
			MergeAltitude(&ti, alt, alt_geo, TRAFFIC_SOURCE_UAT, nowMono)
		} else if !matchedExisting {
			ti.Alt = 0
			ti.AltIsGNSS = false
			ti.Last_alt = nowMono
		}

		if (msg_type == 1 || msg_type == 2 || msg_type == 5 || msg_type == 6) && len(frame) >= 34 {
			// Read AUXSV (only compute GnssDiffFromBaroAlt when primary raw_alt != 0).
			raw_aux_alt := (int32(frame[29]) << 4) | ((int32(frame[30]) & 0xf0) >> 4)
			if raw_aux_alt != 0 && raw_alt != 0 {
				aux_alt := ((raw_aux_alt - 1) * 25) - 1000
				if ti.AltIsGNSS {
					// Current ti.Alt is GNSS. Swap it for the AUXSV alt, which is baro.
					gnss_alt := ti.Alt
					ti.Alt = aux_alt
					aux_alt = gnss_alt
					ti.AltIsGNSS = false
				}

				ti.GnssDiffFromBaroAlt = aux_alt - ti.Alt
				ti.Last_GnssDiff = nowMono
				ti.Last_GnssDiffAlt = ti.Alt
			}
		}

		addFusedSource(&ti, "978UAT")
		ti.SourceConfidence = TrafficSourcePriority(int(ti.Last_source), ti.TargetType, ti.ExtrapolatedPosition)

		postProcessTraffic(&ti)
		traffic[key] = ti
		markTrafficSeen(key)
	}()

	registerTrafficUpdate(ti)
}

func isESInputEnabled() bool {
	globalSettingsMutex.RLock()
	defer globalSettingsMutex.RUnlock()
	return globalSettings.ES_Enabled || globalSettings.Ping_Enabled || globalSettings.Pong_Enabled
}

func esListen() {
	for {
		if !isESInputEnabled() {
			time.Sleep(1 * time.Second) // Don't do much unless ES is actually enabled.
			continue
		}

		dump1090Addr := "127.0.0.1:30006"
		inConn, err := net.Dial("tcp", dump1090Addr)
		if err != nil { // Local connection failed.
			time.Sleep(1 * time.Second)
			continue
		}
		rdr := bufio.NewReader(inConn)
		for isESInputEnabled() {
			buf, err := rdr.ReadString('\n')
			if err != nil { // Must have disconnected?
				break
			}
			buf = strings.Trim(buf, "\r\n")
			TraceLog.Record(CONTEXT_DUMP1090, []byte(buf))
			parseDump1090Message(buf)
		}
		_ = inConn.Close()
		time.Sleep(500 * time.Millisecond)
	}
}

func parseDump1090Message(buf string) {
	nowMono := stratuxClock.GetTime()
	// Log the message to the message counter in any case.
	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_ES
	thisMsg.TimeReceived = nowMono
	thisMsg.Data = buf
	msgLogAppend(thisMsg)

	var eslog esmsg
	eslog.TimeReceived = nowMono
	eslog.Data = buf
	logESMsg(eslog) // log raw dump1090:30006 output to SQLite log

	globalSettingsMutex.RLock()
	pingEnabled := globalSettings.Ping_Enabled
	pongEnabled := globalSettings.Pong_Enabled
	debugEnabled := globalSettings.DEBUG
	displayTrafficSource := globalSettings.DisplayTrafficSource
	globalSettingsMutex.RUnlock()

	// Only increment if using an SDR for 1090 traffic
	if !pingEnabled && !pongEnabled {
		globalStatusMutex.Lock()
		globalStatus.ES_messages_total++
		globalStatusMutex.Unlock()
	}
	trimmed := strings.TrimSpace(buf)
	if len(trimmed) == 0 || trimmed == "null" {
		return
	}
	var newTi dump1090Data
	err := json.Unmarshal([]byte(trimmed), &newTi)
	if err != nil {
		log.Printf("can't read ES traffic information from %s: %s\n", buf, err.Error())
		return
	}

	if newTi.Icao_addr == 0x07FFFFFF { // used to signal heartbeat
		if debugEnabled {
			log.Printf("No traffic last 60 seconds. Heartbeat message from dump1090: %s\n", buf)
		}
		return // don't process heartbeat messages
	}

	if (newTi.Icao_addr & 0x01000000) != 0 { // bit 25 used by dump1090 to signal non-ICAO address
		newTi.Icao_addr = newTi.Icao_addr & 0x00FFFFFF
		if debugEnabled {
			log.Printf("Non-ICAO address %X sent by dump1090. This is typical for TIS-B.\n", newTi.Icao_addr)
		}
	}
	icao := uint32(newTi.Icao_addr)

	var (
		incomingTargetType uint8
		incomingAddrType   uint8
		hasTargetType      bool
	)
	if newTi.DF == 17 {
		incomingTargetType = TARGET_TYPE_ADSB
		incomingAddrType = 0
		hasTargetType = true
	} else if newTi.DF == 18 {
		if newTi.CA == 6 {
			incomingTargetType = TARGET_TYPE_ADSR
			incomingAddrType = 2
			hasTargetType = true
		} else if newTi.CA == 2 { // 2 = TIS-B with ICAO address, 5 = TIS-B without ICAO address
			incomingTargetType = TARGET_TYPE_TISB
			incomingAddrType = 2
			hasTargetType = true
		} else if newTi.CA == 5 {
			incomingTargetType = TARGET_TYPE_TISB
			incomingAddrType = 3
			hasTargetType = true
		}
	}

	var inLat, inLng float32
	inPosValid := newTi.Position_valid && newTi.Lat != nil && newTi.Lng != nil
	if inPosValid {
		inLat = float32(*newTi.Lat)
		inLng = float32(*newTi.Lng)
	}
	var inAlt int32
	if newTi.Alt != nil {
		inAlt = int32(*newTi.Alt)
	}
	var inTrack float32
	var inSpeed uint16
	inSpeedValid := newTi.Speed_valid && newTi.Track != nil && newTi.Speed != nil
	if inSpeedValid {
		inTrack = float32(*newTi.Track)
		inSpeed = uint16(*newTi.Speed)
	}

	var ti TrafficInfo

	func() {
		trafficMutex.Lock()
		defer trafficMutex.Unlock()

		updateNow := stratuxClock.GetTime()

		// Resolve canonical key via traffic fusion (exact ICAO, DDB alias, or 3D spatial/velocity correlation).
		key, matchedExisting := globalTrafficFusion.ResolveCanonicalKeyWithContext(
			icao, 0, TRAFFIC_SOURCE_1090ES,
			inLat, inLng, inAlt, inTrack, inSpeed, inSpeedValid, inPosValid, updateNow, traffic,
		)
		if matchedExisting {
			ti = traffic[key]
		} else {
			ti.Last_seen = updateNow // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
			ti.Last_alt = updateNow  // ditto.
			ti.Icao_addr = icao
			ti.ExtrapolatedPosition = false
			ti.Last_source = TRAFFIC_SOURCE_1090ES

			thisReg, validReg := icao2reg(icao)
			if validReg {
				ti.Reg = thisReg
				ti.Tail = thisReg
			}
		}

		effTargetType := ti.TargetType
		if hasTargetType {
			effTargetType = incomingTargetType
		}
		acceptPos := !matchedExisting || ShouldAcceptPositionUpdate(ti, TRAFFIC_SOURCE_1090ES, effTargetType, false, updateNow)

		if newTi.SignalLevel > 0 {
			power := 10 * math.Log10(newTi.SignalLevel)
			ti.SignalLevelHist = append(ti.SignalLevelHist, power)
			if len(ti.SignalLevelHist) > 8 {
				ti.SignalLevelHist = ti.SignalLevelHist[len(ti.SignalLevelHist)-8:]
			}
			ti.SignalLevel = -999
			for _, level := range ti.SignalLevelHist {
				if level > ti.SignalLevel {
					ti.SignalLevel = level
				}
			}
		} else {
			ti.SignalLevel = -999
		}

		// Altitude will be sent by dump1090 for ES ADS-B/TIS-B (DF=17 and DF=18)
		// and Mode S messages (DF=0, DF = 4, and DF = 20).
		if newTi.Alt != nil {
			MergeAltitude(&ti, int32(*newTi.Alt), newTi.AltIsGNSS, TRAFFIC_SOURCE_1090ES, updateNow)
		}

		if newTi.GnssDiffFromBaroAlt != nil {
			ti.GnssDiffFromBaroAlt = int32(*newTi.GnssDiffFromBaroAlt) // we can estimate pressure altitude from GNSS height with this parameter!
			ti.Last_GnssDiff = updateNow
			ti.Last_GnssDiffAlt = ti.Alt
		}

		// Position updates are provided only by ES messages (DF=17 and DF=18; multiple TCs)
		if newTi.Position_valid { // i.e. DF17 or DF18 message decoded successfully by dump1090
			if inPosValid && acceptPos {
				ti.Lat = inLat
				ti.Lng = inLng
				if isGPSValid() {
					mySituation.muGPS.Lock()
					ownLat := mySituation.GPSLatitude
					ownLng := mySituation.GPSLongitude
					mySituation.muGPS.Unlock()
					ti.Distance, ti.Bearing = common.Distance(float64(ownLat), float64(ownLng), float64(ti.Lat), float64(ti.Lng))
					ti.BearingDist_valid = true
				}
				ti.Position_valid = true
				ti.ExtrapolatedPosition = false
				ti.Last_seen = updateNow // only update "last seen" data on position updates
				ti.Age = 0
			}
		} else {
			// Old traffic had no position and update doesn't have a position either -> assume Mode-S only
			if !ti.Position_valid {
				ti.Last_seen = ti.Last_alt
			}
		}

		if newTi.Speed_valid { // i.e. DF17 or DF18, TC 19 message decoded successfully by dump1090
			if acceptPos {
				if newTi.Vvel != nil {
					ti.Vvel = int16(*newTi.Vvel)
				}
				if inSpeedValid {
					ti.Track = inTrack
					ti.Speed = inSpeed
					ti.Speed_valid = true
					ti.Last_speed = updateNow
				}
			}
		} else if ((newTi.DF == 17) || (newTi.DF == 18)) && (newTi.TypeCode == 19) && acceptPos { // invalid speed on velocity message only
			ti.Speed_valid = false
		}

		// Determine NIC (navigation integrity category) from type code and subtype code
		if ((newTi.DF == 17) || (newTi.DF == 18)) && (newTi.TypeCode >= 5 && newTi.TypeCode <= 22) && (newTi.TypeCode != 19) {
			nic := 0 // default for unknown or missing NIC
			switch newTi.TypeCode {
			case 0, 8, 18, 22:
				nic = 0
			case 17:
				nic = 1
			case 16:
				if newTi.SubtypeCode == 1 {
					nic = 3
				} else {
					nic = 2
				}
			case 15:
				nic = 4
			case 14:
				nic = 5
			case 13:
				nic = 6
			case 12:
				nic = 7
			case 11:
				if newTi.SubtypeCode == 1 {
					nic = 9
				} else {
					nic = 8
				}
			case 10, 21:
				nic = 10
			case 9, 20:
				nic = 11
			}
			ti.NIC = nic

			if (ti.NACp < 7) && (ti.NACp < ti.NIC) {
				ti.NACp = ti.NIC // initialize to NIC, since NIC is sent with every position report, and not all emitters report NACp.
			}
		}

		if newTi.NACp != nil {
			ti.NACp = *newTi.NACp
		}

		if newTi.Emitter_category != nil {
			ti.Emitter_category = uint8(*newTi.Emitter_category) // validate dump1090 on live traffic
		}

		if newTi.Squawk != nil {
			ti.Squawk = int(*newTi.Squawk) // only provided by Mode S messages, so we don't do this in parseUAT.
		}
		// Set the target type. DF=18 messages are sent by ground station, so we look at CA
		// (repurposed to Control Field in DF18) to determine if it's ADS-R or TIS-B.
		if hasTargetType && acceptPos {
			ti.TargetType = incomingTargetType
			ti.Addr_type = incomingAddrType
		}

		if newTi.OnGround != nil && acceptPos { // DF=11 messages don't report "on ground" status so we need to check for valid values.
			ti.OnGround = bool(*newTi.OnGround)
		}

		if (newTi.Tail != nil) && ((newTi.DF == 17) || (newTi.DF == 18) || (newTi.DF == 20) || (newTi.DF == 21)) { // DF=17 or DF=18, Type Code 1-4 , DF=20 Altitude Reply (often with Ident in Comm-B) DF=21 Identity Reply
			ti.Tail = *newTi.Tail
			ti.Tail = strings.Trim(ti.Tail, " ") // remove extraneous spaces
		}

		// This is a hack to show the source of the traffic on moving maps.
		if displayTrafficSource && acceptPos {
			type_code := " "
			switch ti.TargetType {
			case TARGET_TYPE_ADSB:
				type_code = "a"
			case TARGET_TYPE_ADSR:
				type_code = "r"
			case TARGET_TYPE_TISB:
				type_code = "t"
			}

			if len(ti.Tail) == 0 {
				ti.Tail = "e" + type_code
			} else if len(ti.Tail) < 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
				ti.Tail = "e" + type_code + ti.Tail
			} else if len(ti.Tail) == 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
				ti.Tail = "e" + type_code + ti.Tail[1:]
			} else if len(ti.Tail) > 1 { // bounds checking
				ti.Tail = "e" + type_code + ti.Tail[2:]
			}
		}

		if (newTi.DF == 17 || newTi.DF == 18) && acceptPos {
			ti.Last_source = TRAFFIC_SOURCE_1090ES // only update traffic source on ADS-B messages. Prevents source on UAT ADS-B targets with Mode S transponders from "flickering" every time we get an altitude or DF11 update.
		}
		if acceptPos {
			ti.Timestamp = newTi.Timestamp // only update "last seen" data on position updates
		}

		addFusedSource(&ti, "1090ES")
		ti.SourceConfidence = TrafficSourcePriority(int(ti.Last_source), ti.TargetType, ti.ExtrapolatedPosition)

		postProcessTraffic(&ti)
		traffic[key] = ti // Update information on this canonical target key.
		markTrafficSeen(key)
	}()

	registerTrafficUpdate(ti)
}

func trafficInfoExtrapolator() {
	for {
		time.Sleep(1 * time.Second)
		trafficMutex.Lock()
		for key, ti := range traffic {
			age := stratuxClock.Since(ti.Last_seen).Seconds()
			if age < 2 || !ti.Position_valid || !ti.Speed_valid {
				continue
			}
			extrapolateTraffic(&ti)
			traffic[key] = ti
		}
		trafficMutex.Unlock()
	}
}

func extrapolateTraffic(ti *TrafficInfo) {
	if !ti.ExtrapolatedPosition {
		ti.Lat_fix = ti.Lat
		ti.Lng_fix = ti.Lng
		ti.Alt_fix = ti.Alt
		ti.Last_extrapolation = ti.Last_seen // to make computation below simpler
	}

	seconds := stratuxClock.Since(ti.Last_extrapolation).Seconds()
	if seconds <= 0 {
		return
	}
	travelDist := float64(ti.Speed) * (seconds / 60 / 60) // speed is knots=nm per hour. /60/60 = nm per second

	// Estimate alt
	ti.Alt = int32(float64(ti.Alt) + (float64(ti.Vvel) * (seconds / 60)))
	// Estimate position
	lat, lng := calcLocationForBearingDistance(float64(ti.Lat), float64(ti.Lng), float64(ti.Track), travelDist)
	ti.Lat = float32(lat)
	ti.Lng = float32(lng)
	// Estimate track (O(1) wrap, guards NaN/Inf)
	ti.Track = float32(normalizeAngle360(float64(ti.Track) + seconds*float64(ti.TurnRate)))

	ti.ExtrapolatedPosition = true
	ti.Last_extrapolation = stratuxClock.GetTime()
}

/*
updateDemoTraffic creates / updates a simulated traffic target for demonstration / debugging
purpose. Target will circle clockwise around the current GPS position (if valid) or around
KOSH, once every five minutes.

Inputs are ICAO 24-bit hex code, tail number (8 chars max), relative altitude in feet,
groundspeed in knots, and bearing offset from 0 deg initial position.

Traffic on headings 150-240 (bearings 060-150) is intentionally suppressed from updating to allow
for testing of EFB and webUI response. Additionally, the "on ground" flag is set for headings 240-270,
and speed invalid flag is set for headings 135-150 to allow testing of response to those conditions.

*/
func updateDemoTraffic(icao uint32, tail string, relAlt float32, gs float64, offset int32) {
	var ti TrafficInfo
	var shouldBroadcast bool

	func() {
		trafficMutex.Lock()
		defer trafficMutex.Unlock()

		// Retrieve previous information on this ICAO code.
		if val, ok := traffic[icao]; ok { // if we've already seen it, copy it in to do updates
			ti = val
		} else {
			ti.Last_seen = stratuxClock.GetTime() // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
			ti.Icao_addr = icao
			ti.ExtrapolatedPosition = false
		}
		hdg := float64((int32(stratuxClock.GetMilliseconds()/1000)+offset)%720) / 2
		radius := gs * 0.2 / (2 * math.Pi)
		x := radius * math.Cos(hdg*math.Pi/180.0)
		y := radius * math.Sin(hdg*math.Pi/180.0)
		// default traffic location is Oshkosh if GPS not detected
		lat := 43.99
		lng := -88.56
		mySituation.muGPS.Lock()
		gpsLat := mySituation.GPSLatitude
		gpsLng := mySituation.GPSLongitude
		gpsAltMSL := mySituation.GPSAltitudeMSL
		mySituation.muGPS.Unlock()
		if isGPSValid() {
			lat = float64(gpsLat)
			lng = float64(gpsLng)
		}
		traffRelLat := y / 60
		traffRelLng := -x / (60 * math.Cos(lat*math.Pi/180.0))

		ti.Icao_addr = icao
		ti.OnGround = false
		ti.Addr_type = uint8(icao % 4) // 0 == ADS-B; 1 == reserved; 2 == TIS-B with ICAO address; 3 == TIS-B without ICAO address; 6 == ADS-R
		if ti.Addr_type == 1 {         // reassign "reserved value" to ADS-R
			ti.Addr_type = 6
		}

		if ti.Addr_type == 0 {
			ti.TargetType = TARGET_TYPE_ADSB
		} else if ti.Addr_type == 3 {
			ti.TargetType = TARGET_TYPE_TISB
		} else if ti.Addr_type == 6 {
			ti.TargetType = TARGET_TYPE_ADSR
		} else if ti.Addr_type == 2 {
			ti.TargetType = TARGET_TYPE_TISB_S
			if (ti.NIC >= 7) && (ti.Emitter_category > 0) { // If NIC is sufficiently high and emitter type is transmitted, we'll assume it's ADS-R.
				ti.TargetType = TARGET_TYPE_ADSR
			}
		}

		ti.Emitter_category = 1
		ti.Lat = float32(lat + traffRelLat)
		ti.Lng = float32(lng + traffRelLng)

		ti.Distance, ti.Bearing = common.Distance(float64(lat), float64(lng), float64(ti.Lat), float64(ti.Lng))
		ti.BearingDist_valid = true

		ti.Position_valid = true
		ti.ExtrapolatedPosition = false
		ti.Alt = int32(gpsAltMSL + relAlt)
		ti.Track = float32(hdg)
		ti.Speed = uint16(gs)
		if hdg >= 240 && hdg < 270 {
			ti.OnGround = true
		}
		if hdg > 135 && hdg < 150 {
			ti.Speed_valid = false
		} else {
			ti.Speed_valid = true
		}
		ti.Vvel = 0
		ti.Tail = tail // "DEMO1234"
		ti.Timestamp = time.Now()
		nowMono := stratuxClock.GetTime()
		ti.Last_seen = nowMono
		ti.Last_alt = nowMono
		ti.Last_speed = nowMono
		ti.Age = 0
		ti.NACp = 8
		ti.NIC = 8

		ti.Last_source = 1
		if icao%5 == 1 { // make some of the traffic look like it came from UAT
			ti.Last_source = 2
		}

		if hdg < 150 || hdg > 240 {
			// now insert this into the traffic map...
			postProcessTraffic(&ti)
			traffic[ti.Icao_addr] = ti
			markTrafficSeen(ti.Icao_addr)
			shouldBroadcast = true
		}
	}()

	if shouldBroadcast {
		registerTrafficUpdate(ti)
	}
}

/*
	icao2reg() : Converts 24-bit Mode S addresses to N-numbers and C-numbers.

			Input: uint32 representing the Mode S address. Valid range for
				translation is 0xA00001 - 0xADF7C7, inclusive.

				Values outside the range A000001-AFFFFFF or C00001-C3FFFF
				are flagged as foreign.

				Values between ADF7C8 - AFFFFF are allocated to the United States,
				but are not used for aicraft on the civil registry. These could be
				military, other public aircraft, or future use.

				Values between C0CDF9 - C3FFFF are allocated to Canada,
				but are not used for aicraft on the civil registry. These could be
				military, other public aircraft, or future use.

				Values between 7C0000 - 7FFFFF are allocated to Australia.


			Output:
				string: String containing the decoded tail number (if decoding succeeded),
					"NON-NA" (for non-US / non Canada allocation), and "US-MIL" or "CA-MIL" for non-civil US / Canada allocation.

				bool: True if the Mode S address successfully translated to an
					N number. False for all other conditions.
*/

func icao2reg(icao_addr uint32) (string, bool) {
	// Initialize local variables
	base34alphabet := string("ABCDEFGHJKLMNPQRSTUVWXYZ0123456789")
	nationalOffset := uint32(0xA00001) // default is US
	tail := ""
	nation := ""

	// Determine nationality
	if (icao_addr >= 0xA00001) && (icao_addr <= 0xAFFFFF) {
		nation = "US"
	} else if (icao_addr >= 0xC00001) && (icao_addr <= 0xC3FFFF) {
		nation = "CA"
	} else if (icao_addr >= 0x7C0000) && (icao_addr <= 0x7FFFFF) {
		nation = "AU"
	} else {
		//TODO: future national decoding.
		return "OTHER", false
	}

	if nation == "CA" { // Canada decoding
		// First, discard addresses that are not assigned to aircraft on the civil registry
		if icao_addr > 0xC0CDF8 {
			//fmt.Printf("%X is a Canada aircraft, but not a CF-, CG-, or CI- registration.\n", icao_addr)
			return "CA-MIL", false
		}

		nationalOffset := uint32(0xC00001)
		serial := int32(icao_addr - nationalOffset)

		// Fifth letter
		e := serial % 26

		// Fourth letter
		d := (serial / 26) % 26

		// Third letter
		c := (serial / 676) % 26 // 676 == 26*26

		// Second letter
		b := (serial / 17576) % 26 // 17576 == 26*26*26

		b_str := "FGI"

		//fmt.Printf("B = %d, C = %d, D = %d, E = %d\n",b,c,d,e)
		tail = fmt.Sprintf("C-%c%c%c%c", b_str[b], c+65, d+65, e+65)
	}

	if nation == "AU" { // Australia decoding

		nationalOffset := uint32(0x7C0000)
		offset := (icao_addr - nationalOffset)
		i1 := offset / 1296
		offset2 := offset % 1296
		i2 := offset2 / 36
		offset3 := offset2 % 36
		i3 := offset3

		var a_char, b_char, c_char string

		a_char = fmt.Sprintf("%c", i1+65)
		b_char = fmt.Sprintf("%c", i2+65)
		c_char = fmt.Sprintf("%c", i3+65)

		if i1 < 0 || i1 > 25 || i2 < 0 || i2 > 25 || i3 < 0 || i3 > 25 {
			return "OTHER", false
		}

		tail = "VH-" + a_char + b_char + c_char
	}

	if nation == "US" { // FAA decoding
		// First, discard addresses that are not assigned to aircraft on the civil registry
		if icao_addr > 0xADF7C7 {
			//fmt.Printf("%X is a US aircraft, but not on the civil registry.\n", icao_addr)
			return "US-MIL", false
		}

		serial := int32(icao_addr - nationalOffset)
		// First digit
		a := (serial / 101711) + 1

		// Second digit
		a_remainder := serial % 101711
		b := ((a_remainder + 9510) / 10111) - 1

		// Third digit
		b_remainder := (a_remainder + 9510) % 10111
		c := ((b_remainder + 350) / 951) - 1

		// This next bit is more convoluted. First, figure out if we're using the "short" method of
		// decoding the last two digits (two letters, one letter and one blank, or two blanks).
		// This will be the case if digit "B" or "C" are calculated as negative, or if c_remainder
		// is less than 601.

		c_remainder := (b_remainder + 350) % 951
		var d, e int32

		if (b >= 0) && (c >= 0) && (c_remainder > 600) { // alphanumeric decoding method
			d = 24 + (c_remainder-601)/35
			e = (c_remainder - 601) % 35

		} else { // two-letter decoding method
			if (b < 0) || (c < 0) {
				c_remainder -= 350 // otherwise "  " == 350, "A " == 351, "AA" == 352, etc.
			}

			d = (c_remainder - 1) / 25
			e = (c_remainder - 1) % 25

			if e < 0 {
				d -= 1
				e += 25
			}
		}

		a_char := fmt.Sprintf("%d", a)
		var b_char, c_char, d_char, e_char string

		if b >= 0 {
			b_char = fmt.Sprintf("%d", b)
		}

		if b >= 0 && c >= 0 {
			c_char = fmt.Sprintf("%d", c)
		}

		if d > -1 {
			d_char = string(base34alphabet[d])
			if e > 0 {
				e_char = string(base34alphabet[e-1])
			}
		}

		tail = "N" + a_char + b_char + c_char + d_char + e_char

	}

	return tail, true
}

func initTraffic(isTraceReplayMode bool) {
	traffic = make(map[uint32]TrafficInfo)
	seenTraffic = make(map[uint32]bool)
	trafficMutex = &sync.Mutex{}
	if !isTraceReplayMode {
		go esListen()
		go ognListen()
		go aprsListen()
		go aisListen()
	}
}
