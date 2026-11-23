/*
	Copyright (c) 2020 Keith Tschohl, Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.
	flarm-nmea.go: Functions for generating FLARM-related NMEA sentences
		to communicate traffic bearing / distance to glider computers
		and UK / EU oriented EFBs.
	Additional functions to parse NMEA from external Flarm GPS Mouse/SoftRF
*/

package main

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/gpsnmea"
)

/*
	sendNetFLARM() is a shortcut to network.go 'sendMsg()', and will send the referenced byte slice to the UDP network port
		defined by NETWORK_FLARM_NMEA in gen_gdl90.go as a non-queueable message to be used in XCSoar. It will also queue
		the message into a channel so it can be	sent out to a TCP server.
*/

// Append checksum to nmea string
func appendNmeaChecksum(nmea string) string {
	return gpsnmea.AppendChecksum(nmea)
}

// sanitizeNmeaTail removes NMEA delimiter/control characters from callsigns
// before embedding them in $PFLAA/$PFLAU sentences.
func sanitizeNmeaTail(tail string) string {
	return gpsnmea.SanitizeTail(tail)
}

func clampInt32(v float64, minVal, maxVal int32) int32 {
	if math.IsNaN(v) {
		return 0
	}
	if v > float64(maxVal) {
		return maxVal
	}
	if v < float64(minVal) {
		return minVal
	}
	return int32(v)
}

func makeFlarmPFLAUString(ti TrafficInfo) (msg string) {
	// syntax: PFLAU,<RX>,<TX>,<GPS>,<Power>,<AlarmLevel>,<RelativeBearing>,<AlarmType>,<RelativeVertical>,<RelativeDistance>,<ID>
	gpsStatus := 0
	if isGPSValid() {
		gpsStatus = 2
	}

	var gpsLat, gpsLng, gpsCourse float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsCourse = mySituation.GPSTrueCourse
		mySituation.muGPS.Unlock()
	} else {
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsCourse = mySituation.GPSTrueCourse
	}

	var (
		dist             float64
		bearing          float64
		relativeVertical int32
		alarmLevel       uint8
		alarmType        int
		threatAddr       = ti.Icao_addr
	)

	threat, hasThreat := getHighestCPAThreat()
	if hasThreat && threat.Valid && threat.AlertLevel >= CPAAlertAdvisory {
		alarmLevel = threat.AlertLevel
		bearing = threat.RelativeBearingDeg
		if math.IsNaN(bearing) || math.IsInf(bearing, 0) {
			bearing = 0
		} else {
			bearing = math.Mod(bearing, 360.0)
			if bearing > 180 {
				bearing -= 360
			}
			if bearing < -180 {
				bearing += 360
			}
		}
		alarmType = 2
		dist = threat.CurrentDistMeters
		if math.IsNaN(dist) || math.IsInf(dist, 0) {
			dist = 0
		}
		if ti.Position_valid {
			relativeVertical = computeRelativeVertical(ti)
		} else {
			relativeVertical = clampInt32(threat.VertMissFeet*0.3048, -32767, 32767)
		}
		if threatAddr == 0 {
			highestCPAThreatMu.RLock()
			threatAddr = highestCPAThreatKey & 0xFFFFFF
			highestCPAThreatMu.RUnlock()
		}
	} else {
		globalSettingsMutex.RLock()
		cpaAlertsEnabled := globalSettings.CPA_Alerts_Enabled
		globalSettingsMutex.RUnlock()

		if !cpaAlertsEnabled {
			dist, bearing, _, _ = common.DistRect(float64(gpsLat), float64(gpsLng), float64(ti.Lat), float64(ti.Lng))
			relativeVertical = computeRelativeVertical(ti)
			alarmLevel = computeAlarmLevel(dist, relativeVertical)

			// make bearing relative to ground track, with +-180deg
			bearing = bearing - float64(gpsCourse)
			if !math.IsNaN(bearing) && !math.IsInf(bearing, 0) {
				bearing = math.Mod(bearing, 360.0)
				if bearing > 180 {
					bearing -= 360
				}
				if bearing < -180 {
					bearing += 360
				}
			} else {
				bearing = 0
			}
			if alarmLevel > 0 {
				alarmType = 2
			}
		}
	}

	idstr := fmt.Sprintf("%.6X", threatAddr&0xFFFFFF)
	cleanTail := sanitizeNmeaTail(ti.Tail)
	if len(cleanTail) > 0 {
		idstr += "!" + cleanTail
	}
	// TODO: we are always airborne for now
	if alarmLevel > 0 {
		msg = fmt.Sprintf("$PFLAU,%d,1,%d,1,%d,%d,%d,%d,%d,%s", len(traffic), gpsStatus, alarmLevel, int32(math.Round(bearing)), alarmType, relativeVertical, clampInt32(math.Abs(dist), 0, 2147483647), idstr)
	} else {
		msg = fmt.Sprintf("$PFLAU,%d,1,%d,1,0,,0,,,", len(traffic), gpsStatus)
	}
	msg = appendNmeaChecksum(msg)
	msg += "\r\n"
	return
}

// TODO: only very simplistic implementation
func computeAlarmLevel(dist float64, relativeVertical int32) (alarmLevel uint8) {
	if math.IsNaN(dist) || math.IsInf(dist, 0) {
		return 0
	}
	if (dist < 926) && (relativeVertical < 152) && (relativeVertical > -152) { // 926 m = 0.5 NM; 152m = 500'
		alarmLevel = 3
	} else if (dist < 1852) && (relativeVertical < 304) && (relativeVertical > -304) { // 1852 m = 1.0 NM ; 304 m = 1000'
		alarmLevel = 2
	} else {
		alarmLevel = 0
	}
	return
}

func computeRelativeVertical(ti TrafficInfo) (relativeVertical int32) {
	var baroAlt, gpsAltMSL, gpsHAE float32
	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
		baroAlt = mySituation.BaroPressureAltitude
		mySituation.muBaro.Unlock()
	} else {
		baroAlt = mySituation.BaroPressureAltitude
	}
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsAltMSL = mySituation.GPSAltitudeMSL
		gpsHAE = mySituation.GPSHeightAboveEllipsoid
		mySituation.muGPS.Unlock()
	} else {
		gpsAltMSL = mySituation.GPSAltitudeMSL
		gpsHAE = mySituation.GPSHeightAboveEllipsoid
	}

	altf := baroAlt
	if !isTempPressValid() && isGPSValid() { // if no pressure altitude available, use GPS altitude
		altf = gpsAltMSL
	}
	if ti.AltIsGNSS && isGPSValid() {
		// Altitude coming from OGN. We set the geoid separation to 0 in the OGN config, so OGN reports ellipsoid alt - we need to compare to that
		altf = gpsHAE
	}
	relativeVertical = clampInt32(float64(ti.Alt)*0.3048-float64(altf)*0.3048, -32767, 32767) // convert to meters
	return
}

func gdl90EmitterCatToNMEA(emitterCat uint8) string {
	acType := "0"
	switch emitterCat {
	case 1, 6:
		acType = "8" // light/"highly maneuverable > 5g" = piston
	case 2, 3, 4, 5:
		acType = "9" // small/large/heavy = jet
	case 7:
		acType = "3" // helicopter = helicopter
	case 9:
		acType = "1" // glider = glider
	case 10:
		acType = "B" // lighter than air = balloon
	case 11:
		acType = "4" // skydiver/parachute = sky diver
	case 12:
		acType = "7" // paraglider, hanglider
	case 14:
		acType = "D" // UAV
	case 17, 18:
		acType = "E" // Surface vehicle->Ground support (not in dataport spec, but OGN extension?)
	case 19:
		acType = "F" // static object / point obstacle
	}
	return acType
}

func nmeaAircraftTypeToGdl90(actype string) uint8 {
	acTypeInt, err := strconv.ParseInt(actype, 16, 8)
	if err != nil {
		return 0
	}
	cat := uint8(0)
	switch acTypeInt {
	case 1:
		cat = 9 // glider = glider
	case 2, 5, 8:
		cat = 1 // tow, drop, piston = light
	case 3:
		cat = 7 // helicopter = helicopter
	case 4:
		cat = 11 // skydiver
	case 6, 7:
		cat = 12 // hang glider / paraglider
	case 9:
		cat = 3 // jet = large
	case 0xB, 0xC:
		cat = 10 // Balloon, airship = lighter than air
	case 0xD:
		cat = 14 // UAV=UAV
	case 0xE:
		cat = 18 // Ground support = surface vehicle (OGN extension?)
	case 0xF:
		cat = 19 // point obstacle=static object
	}
	return cat
}

/*
	makeFlarmPFLAAString() creates a NMEA-formatted PFLAA string (FLARM traffic format) with checksum from the referenced
		traffic object.
*/

func makeFlarmPFLAAString(ti TrafficInfo) (msg string, valid bool, alarmLevel uint8) {

	/*	Format: $PFLAA,<AlarmLevel>,<RelativeNorth>,<RelativeEast>,<RelativeVertical>,<IDType>,<ID>,<Track>,<TurnRate>,<GroundSpeed>, <ClimbRate>,<AcftType>*<checksum>
		            $PFLAA,0,-10687,-22561,-10283,1,A4F2EE,136,,269,0.0,0*4E
			<AlarmLevel>  Decimal integer value. Range: from 0 to 3.
							Alarm level as assessed by FLARM:
							0 = no alarm (also used for no-alarm traffic information)
							1 = alarm, 13-18 seconds to impact
							2 = alarm, 9-12 seconds to impact
							3 = alarm, 0-8 seconds to impact
			<RelativeNorth>,<RelativeEast>,<RelativeVertical> are distances in meters. Decimal integer value. Range: from -32768 to 32767.
				For traffic without known bearing, assign estimated distance to <RelativeNorth> and leave <RelativeEast> empty
			<IDType>: 1 = official ICAO 24-bit aircraft address; 2 = stable FLARM ID (chosen by FLARM) 3 = anonymous ID, used if stealth mode is activated.
			For ADS-B traffic, we'll always pick 1.
			<ID>: 6-digit hexadecimal value (e.g. "5A77B1") as configured in the target's PFLAC,,ID sentence. For ADS-B targets always use reported 24-bit ICAO address.
					NOTE: Appending "!CALLSIGN" may allow certain receivers to display tail.
			<Track>: Decimal integer value. Range: from 0 to 359. The target's true ground track in degrees.
			<TurnRate>: Not used. Empty field.
			<GroundSpeed>: Decimal integer value. Range: from 0 to 32767. The target's ground speed in m/s
			<ClimbRate>: Decimal fixed point number with one digit after the radix point (dot). Range: from -32.7 to 32.7. The target's climb rate in m/s.
			Positive values indicate a climbing aircraft.
			<AcftType>: Hexadecimal value. Range: from 0 to F.
	*/

	var idType uint8
	var relativeNorth, relativeEast, relativeVertical, groundSpeed int32

	// Addr type "NON-ICAO" mapped to Flarm ID, rest mapped to ICAO.
	// Especially SkyDemon is picky and only accepts NMEA messages with 0-2, but nothing else.
	if ti.Addr_type == 1 {
		idType = 2
	} else {
		idType = 1
	}

	var gpsLat, gpsLng float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		mySituation.muGPS.Unlock()
	} else {
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
	}

	// determine distance and bearing to target
	dist, bearing, distN, distE := common.DistRect(float64(gpsLat), float64(gpsLng), float64(ti.Lat), float64(ti.Lng))
	if !ti.Position_valid {
		dist = ti.DistanceEstimated
		distN = ti.DistanceEstimated
	}
	globalSettingsMutex.RLock()
	debug := globalSettings.DEBUG
	cpaAlertsEnabled := globalSettings.CPA_Alerts_Enabled
	globalSettingsMutex.RUnlock()
	if debug {
		log.Printf("FLARM - ICAO target %X (%s) is %.1f meters away at %.1f degrees\n", ti.Icao_addr, ti.Tail, dist, bearing)
	}

	relativeNorth = clampInt32(distN, -32767, 32767)
	relativeEast = clampInt32(distE, -32767, 32767)

	relativeVertical = computeRelativeVertical(ti)
	if cpaAlertsEnabled && ti.Position_valid {
		alarmLevel = ti.AlertLevel
	} else {
		alarmLevel = computeAlarmLevel(dist, relativeVertical)
	}

	if ti.Speed_valid {
		groundSpeed = clampInt32(float64(ti.Speed)*0.5144, 0, 32767) // convert to m/s
	}

	var acType string
	if ti.TargetType == TARGET_TYPE_REMOTEID || ti.Emitter_category == 14 {
		acType = "D" // UAV (Remote ID / GDL90 category 14)
	} else {
		acType = gdl90EmitterCatToNMEA(ti.Emitter_category)
	}

	climbRate := float64(ti.Vvel) * 0.3048 / 60.0 // convert to m/s
	if climbRate > 32.7 {
		climbRate = 32.7
	} else if climbRate < -32.7 {
		climbRate = -32.7
	}

	idstr := fmt.Sprintf("%.6X", ti.Icao_addr&0xFFFFFF)
	cleanTail := sanitizeNmeaTail(ti.Tail)
	if len(cleanTail) > 0 {
		idstr += "!" + cleanTail
	}

	trackNorm := math.Mod(float64(ti.Track), 360.0)
	if math.IsNaN(trackNorm) || math.IsInf(trackNorm, 0) {
		trackNorm = 0
	} else if trackNorm < 0 {
		trackNorm += 360.0
	}
	trackInt := int(trackNorm) % 360

	if ti.Position_valid {
		// <TurnRate> is empty per FLARM dataport specification.
		msg = fmt.Sprintf("$PFLAA,%d,%d,%d,%d,%d,%s,%d,,%d,%0.1f,%s", alarmLevel, relativeNorth, relativeEast, relativeVertical, idType, idstr, trackInt, groundSpeed, climbRate, acType)
	} else {
		relDistClamped := clampInt32(math.Abs(dist), 0, 32767)
		msg = fmt.Sprintf("$PFLAA,%d,%d,,%d,%d,%s,,,,%0.1f,%s", alarmLevel, relDistClamped, relativeVertical, idType, idstr, climbRate, acType) // prototype for bearingless traffic
	}
	msg = appendNmeaChecksum(msg)
	msg += "\r\n"

	valid = true
	return
}

/*
	makeGPRMCString() creates a NMEA-formatted GPRMC string (GPS recommended minimum data) with checksum from the current GPS position.
		If current position is invalid, the GPRMC string will indicate no-fix.
*/

func makeGPRMCString() string {
	var (
		lastFixUTC float32
		fixQuality uint8
		gpsLat     float32
		gpsLng     float32
		gpsGS      float64
		gpsCourse  float32
		gpsTime    time.Time
	)
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		lastFixUTC = mySituation.GPSLastFixSinceMidnightUTC
		fixQuality = mySituation.GPSFixQuality
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsGS = mySituation.GPSGroundSpeed
		gpsCourse = mySituation.GPSTrueCourse
		gpsTime = mySituation.GPSTime
		mySituation.muGPS.Unlock()
	} else {
		lastFixUTC = mySituation.GPSLastFixSinceMidnightUTC
		fixQuality = mySituation.GPSFixQuality
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsGS = mySituation.GPSGroundSpeed
		gpsCourse = mySituation.GPSTrueCourse
		gpsTime = mySituation.GPSTime
	}

	lastFix := float64(lastFixUTC)
	hr := math.Floor(lastFix / 3600)
	lastFix -= 3600 * hr
	mins := math.Floor(lastFix / 60)
	sec := lastFix - mins*60

	gpsValid := isGPSValid()
	status := "V"
	if gpsValid && fixQuality > 0 {
		status = "A"
	}

	lat := float64(gpsLat)
	ns := "N"
	if lat < 0 {
		lat = -lat
		ns = "S"
	}

	deg := math.Floor(lat)
	min := (lat - deg) * 60
	lat = deg*100 + min

	ew := "E"
	lng := float64(gpsLng)
	if lng < 0 {
		lng = -lng
		ew = "W"
	}

	deg = math.Floor(lng)
	min = (lng - deg) * 60
	lng = deg*100 + min

	gs := float32(gpsGS)
	trueCourse := gpsCourse
	dateRef := gpsTime.UTC()
	if dateRef.IsZero() {
		dateRef = time.Now().UTC()
	}
	yy, mm, dd := dateRef.Date()
	yy = yy % 100
	var magVar, mvEW string
	mode := "N"
	if fixQuality == 1 {
		mode = "A"
	} else if fixQuality == 2 {
		mode = "D"
	}

	var msg string

	if gpsValid {
		msg = fmt.Sprintf("$GPRMC,%02.f%02.f%05.2f,%s,%010.5f,%s,%011.5f,%s,%.1f,%.1f,%02d%02d%02d,%s,%s,%s", hr, mins, sec, status, lat, ns, lng, ew, gs, trueCourse, dd, mm, yy, magVar, mvEW, mode)
	} else {
		msg = fmt.Sprintf("$GPRMC,,%s,,,,,,,%02d%02d%02d,%s,%s,%s", status, dd, mm, yy, magVar, mvEW, mode) // return null lat-lng and velocity if invalid GPS
	}
	msg = appendNmeaChecksum(msg)
	msg += "\r\n"
	return msg
}

func makeGPGGAString() string {
	/*
	 xxGGA
	 time
	 lat (degmin.mmm)
	 NS
	 long (degmin.mmm)
	 EW
	 quality
	 numSV
	 HDOP
	 alt
	 ualt
	 sep
	 uSep
	 diffAge
	 diffStation
	*/

	var (
		lastFixUTC float32
		gpsLat     float32
		gpsLng     float32
		numSV      uint16
		altMSL     float32
		geoidSepFt float32
		fixQuality uint8
	)
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		lastFixUTC = mySituation.GPSLastFixSinceMidnightUTC
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		numSV = mySituation.GPSSatellites
		altMSL = mySituation.GPSAltitudeMSL
		geoidSepFt = mySituation.GPSGeoidSep
		fixQuality = mySituation.GPSFixQuality
		mySituation.muGPS.Unlock()
	} else {
		lastFixUTC = mySituation.GPSLastFixSinceMidnightUTC
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		numSV = mySituation.GPSSatellites
		altMSL = mySituation.GPSAltitudeMSL
		geoidSepFt = mySituation.GPSGeoidSep
		fixQuality = mySituation.GPSFixQuality
	}

	lastFix := float64(lastFixUTC)
	hr := math.Floor(lastFix / 3600)
	lastFix -= 3600 * hr
	mins := math.Floor(lastFix / 60)
	sec := lastFix - mins*60

	lat := float64(gpsLat)
	ns := "N"
	if lat < 0 {
		lat = -lat
		ns = "S"
	}

	deg := math.Floor(lat)
	min := (lat - deg) * 60
	lat = deg*100 + min

	ew := "E"
	lng := float64(gpsLng)
	if lng < 0 {
		lng = -lng
		ew = "W"
	}

	deg = math.Floor(lng)
	min = (lng - deg) * 60
	lng = deg*100 + min

	hdop := 1.0

	alt := altMSL / 3.28084
	geoidSep := geoidSepFt / 3.28084

	var msg string

	if isGPSValid() {
		msg = fmt.Sprintf("$GPGGA,%02.f%02.f%05.2f,%010.5f,%s,%011.5f,%s,%d,%d,%.2f,%.1f,M,%.1f,M,,", hr, mins, sec, lat, ns, lng, ew, fixQuality, numSV, hdop, alt, geoidSep)
	} else {
		msg = fmt.Sprintf("$GPGGA,,,,,,0,%d,,,,,,,", numSV)
	}

	msg = appendNmeaChecksum(msg)
	msg += "\r\n"
	return msg
}

func makePGRMZString() string {
	var baroAlt float32
	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
		baroAlt = mySituation.BaroPressureAltitude
		mySituation.muBaro.Unlock()
	} else {
		baroAlt = mySituation.BaroPressureAltitude
	}
	msg := fmt.Sprintf("$PGRMZ,%d,f,3", int(baroAlt))
	msg = appendNmeaChecksum(msg)
	msg += "\r\n"
	return msg
}

func makeAHRSLevilReport() {
	globalStatusMutex.RLock()
	imuConnected := globalStatus.IMUConnected
	globalStatusMutex.RUnlock()
	if !imuConnected || !isAHRSValid() {
		return
	}

	var gpsTrueCourse float32
	var gpsTurnRate float64
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsTrueCourse = mySituation.GPSTrueCourse
		gpsTurnRate = mySituation.GPSTurnRate
		mySituation.muGPS.Unlock()
	} else {
		gpsTrueCourse = mySituation.GPSTrueCourse
		gpsTurnRate = mySituation.GPSTurnRate
	}

	var ahrsRoll, ahrsPitch, ahrsGyroHeading, ahrsSlipSkid, ahrsTurnRate, ahrsGLoad float64
	if mySituation.muAttitude != nil {
		mySituation.muAttitude.Lock()
		ahrsRoll = mySituation.AHRSRoll
		ahrsPitch = mySituation.AHRSPitch
		ahrsGyroHeading = mySituation.AHRSGyroHeading
		ahrsSlipSkid = mySituation.AHRSSlipSkid
		ahrsTurnRate = mySituation.AHRSTurnRate
		ahrsGLoad = mySituation.AHRSGLoad
		mySituation.muAttitude.Unlock()
	} else {
		ahrsRoll = mySituation.AHRSRoll
		ahrsPitch = mySituation.AHRSPitch
		ahrsGyroHeading = mySituation.AHRSGyroHeading
		ahrsSlipSkid = mySituation.AHRSSlipSkid
		ahrsTurnRate = mySituation.AHRSTurnRate
		ahrsGLoad = mySituation.AHRSGLoad
	}

	// Values if invalid (scaled to tenths of a degree / deg/s per Levil $RPYL spec)
	roll := int16(0)
	pitch := int16(0)
	hdg := common.RoundToInt16(float64(gpsTrueCourse) * 10)
	slip_skid := int16(0)
	yaw_rate := common.RoundToInt16(gpsTurnRate * 10)
	g := int16(0)
	if !isAHRSInvalidValue(ahrsRoll) {
		roll = common.RoundToInt16(ahrsRoll * 10)
	}
	if !isAHRSInvalidValue(ahrsPitch) {
		pitch = common.RoundToInt16(ahrsPitch * 10)
	}
	if !isAHRSInvalidValue(ahrsGyroHeading) {
		hdg = common.RoundToInt16(ahrsGyroHeading * 10)
	}
	if !isAHRSInvalidValue(ahrsSlipSkid) {
		slip_skid = common.RoundToInt16(-ahrsSlipSkid * 10)
	}
	if !isAHRSInvalidValue(ahrsTurnRate) {
		yaw_rate = common.RoundToInt16(ahrsTurnRate * 10)
	}
	if !isAHRSInvalidValue(ahrsGLoad) {
		g = common.RoundToInt16(ahrsGLoad * 1000)
	}

	msg := fmt.Sprintf("$RPYL,%d,%d,%d,%d,%d,%d,0", roll, pitch, hdg, slip_skid, yaw_rate, g)
	msg = appendNmeaChecksum(msg)
	sendNetFLARM(msg+"\r\n", 100*time.Millisecond, 4)
}

func atof32(val string) float32 {
	res, _ := strconv.ParseFloat(val, 32)
	return float32(res)
}

// Read data from a raw $PFLAU/$PFLAA message (i.e. when serial flarm device is connected)
func parseFlarmNmeaMessage(message []string) {
	if len(message) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Error parsing NMEA %s: %v", strings.Join(message, ","), r)
		}
	}()

	if message[0] == "PFLAU" {
		parseFlarmPFLAU(message)
	} else if message[0] == "PFLAA" {
		parseFlarmPFLAA(message)
	}
}

func relativeGpsAltToBaro(relVert float32) (alt int32, altIsGnss bool) {
	if isTempPressValid() {
		var baroAlt float32
		if mySituation.muBaro != nil {
			mySituation.muBaro.Lock()
			baroAlt = mySituation.BaroPressureAltitude
			mySituation.muBaro.Unlock()
		} else {
			baroAlt = mySituation.BaroPressureAltitude
		}
		return int32(baroAlt + relVert*3.28084), false
	} else if isGPSValid() {
		var gpsAltMSL float32
		if mySituation.muGPS != nil {
			mySituation.muGPS.Lock()
			gpsAltMSL = mySituation.GPSAltitudeMSL
			mySituation.muGPS.Unlock()
		} else {
			gpsAltMSL = mySituation.GPSAltitudeMSL
		}
		return int32(gpsAltMSL + relVert*3.28084), true
	}
	return 0, false
}

func getIdTail(idReceived string) (idStr string, tail string, address uint32) {
	ognIDAndTail := strings.Split(idReceived, "!")
	idStr = strings.TrimSpace(ognIDAndTail[0])
	if len(idStr) > 6 {
		// OGN Tracker sometimes encodes address type in the address.. strip that
		idStr = idStr[len(idStr)-6:]
	}
	tail = ""
	if len(ognIDAndTail) >= 2 {
		tail = ognIDAndTail[1]
	}
	// Some devices report ID as tail number, with a respective prefix. E.g. OGN_AAAAAA, FLR_BBBBBB, ....
	// Ignore that - it's not useful for us and we would rather check OGN DDB for a real tail number
	if len(tail) > 4 && tail[3] == '_' {
		tail = ""
	}

	if parsed, err := strconv.ParseUint(idStr, 16, 32); err == nil {
		address = uint32(parsed) & 0x00FFFFFF
	}

	return
}

func parseFlarmPFLAU(message []string) {
	// $PFLAU,<RX>,<TX>,<GPS>,<Power>,<AlarmLevel>,<RelativeBearing>,<AlarmType>,<RelativeVertical>,<RelativeDistance>,<ID>
	if len(message) < 11 {
		// PFLAU without ID is valid FLARM
		return
	}
	if len(message[10]) == 0 || len(message[9]) == 0 || len(message[8]) == 0 || len(message[6]) == 0 {
		return
	}
	now := stratuxClock.GetTime()
	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_OGN
	thisMsg.TimeReceived = now
	msgLogAppend(thisMsg)

	if !isGPSValid() {
		return // can't convert relative to absolute without GPS
	}

	ognID, tail, address := getIdTail(message[10])

	var gpsLat, gpsLng, gpsCourse float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsCourse = mySituation.GPSTrueCourse
		mySituation.muGPS.Unlock()
	} else {
		gpsLat = mySituation.GPSLatitude
		gpsLng = mySituation.GPSLongitude
		gpsCourse = mySituation.GPSTrueCourse
	}

	trafficBearing := int32(gpsCourse+atof32(message[6])) % 360
	if trafficBearing < 0 {
		trafficBearing += 360
	}
	relVertical := atof32(message[8])
	relDist := atof32(message[9])
	lat, lng := calcLocationForBearingDistance(float64(gpsLat), float64(gpsLng), float64(trafficBearing), float64(relDist/1852.0))
	inAlt, inAltIsGNSS := relativeGpsAltToBaro(relVertical)

	var ti TrafficInfo
	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	// Resolve canonical key via traffic fusion (direct key, bare ICAO, DDB alias, or 3D spatial correlation).
	key, matchedExisting := globalTrafficFusion.ResolveCanonicalKeyWithContext(
		address, 1, TRAFFIC_SOURCE_FLARM,
		float32(lat), float32(lng), inAlt, 0, 0, false, true, now, traffic,
	)
	if matchedExisting {
		ti = traffic[key]
		if !ShouldAcceptPositionUpdate(ti, TRAFFIC_SOURCE_FLARM, 0, false, now) {
			if len(ti.Tail) <= 3 {
				if len(tail) != 0 {
					ti.Tail = tail
				} else {
					ti.Tail = getTailNumber(ognID, "FLR")
				}
			}
			MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_FLARM, now)
			addFusedSource(&ti, "FLARM")
			traffic[key] = ti
			return
		}
	} else {
		ti.Icao_addr = address
		ti.Addr_type = uint8((key >> 24) & 0xFF)
	}
	if len(ti.Tail) <= 3 {
		if len(tail) != 0 {
			// Tail provided via NMEA (IDIDID!TAIL syntax)
			ti.Tail = tail
		} else {
			// OGN DDB fallback
			ti.Tail = getTailNumber(ognID, "FLR") // Might have better tail from ADS-B. Don't overwrite.
		}
	}
	ti.Timestamp = time.Now().UTC()
	ti.Last_source = TRAFFIC_SOURCE_FLARM
	MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_FLARM, now)

	ti.Lat = float32(lat)
	ti.Lng = float32(lng)
	ti.Distance = float64(relDist)
	ti.Bearing = float64(trafficBearing)
	ti.BearingDist_valid = true
	ti.Position_valid = true
	ti.ExtrapolatedPosition = false
	ti.Last_seen = now
	ti.SourceConfidence = TrafficSourcePriority(TRAFFIC_SOURCE_FLARM, ti.TargetType, ti.ExtrapolatedPosition)
	addFusedSource(&ti, "FLARM")

	// update traffic database
	traffic[key] = ti

	// notify
	registerTrafficUpdate(ti)

	// mark traffic as seen
	markTrafficSeen(key)
}

func parseFlarmPFLAA(message []string) {
	// $PFLAA,<AlarmLevel>,<RelativeNorth>,<RelativeEast>,<RelativeVertical>,<IDType>,<ID>,<Track>,<TurnRate>,<GroundSpeed>,<ClimbRate>,<AcftType>
	if len(message) < 12 {
		log.Printf("Discarding invalid NMEA: %s", strings.Join(message, ","))
		return
	}
	now := stratuxClock.GetTime()
	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_OGN
	thisMsg.TimeReceived = now
	msgLogAppend(thisMsg)

	bearingless := strings.TrimSpace(message[3]) == ""
	if !bearingless && !isGPSValid() {
		return // can't compute absolute lat/lon without valid GPS
	}

	relNorth := atof32(message[2])
	relEast := atof32(message[3])
	relVert := atof32(message[4])

	ognID, tail, address := getIdTail(message[6])
	idType, _ := strconv.ParseInt(message[5], 10, 8)
	if idType == 1 {
		idType = 0 // ICAO ID
	} else {
		idType = 1 // non-ICAO ID
	}

	track := atof32(message[7])
	turn := atof32(message[8])
	speed := atof32(message[9])
	vspeed := atof32(message[10])
	acType := message[11]

	if math.IsNaN(float64(speed)) || math.IsInf(float64(speed), 0) || speed < 0 {
		speed = 0
	}
	if math.IsNaN(float64(track)) || math.IsInf(float64(track), 0) {
		track = 0
	}
	if math.IsNaN(float64(turn)) || math.IsInf(float64(turn), 0) {
		turn = 0
	}
	if math.IsNaN(float64(vspeed)) || math.IsInf(float64(vspeed), 0) {
		vspeed = 0
	}

	var ownLat, ownLng float32
	if !bearingless {
		if mySituation.muGPS != nil {
			mySituation.muGPS.Lock()
			ownLat = mySituation.GPSLatitude
			ownLng = mySituation.GPSLongitude
			mySituation.muGPS.Unlock()
		} else {
			ownLat = mySituation.GPSLatitude
			ownLng = mySituation.GPSLongitude
		}
	}

	var targetLat, targetLng float32
	if !bearingless {
		// lat dist = 60nm = 111,12km
		targetLat = ownLat + (relNorth / 111120.0)
		avgLat := float64(targetLat)/2.0 + float64(ownLat)/2.0
		lngFactor := 111120.0 * math.Cos(common.Radians(avgLat))
		if math.IsNaN(lngFactor) || math.IsInf(lngFactor, 0) || math.Abs(lngFactor) < 1e-3 {
			return
		}
		targetLng = ownLng + float32(float64(relEast)/lngFactor)
	}
	inAlt, inAltIsGNSS := relativeGpsAltToBaro(relVert)
	speedKts := float64(speed) * 1.94384
	if speedKts > math.MaxUint16 {
		speedKts = math.MaxUint16
	}
	speedValid := strings.TrimSpace(message[9]) != ""

	var ti TrafficInfo

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	// Resolve canonical key via traffic fusion (exact key, bare ICAO, DDB alias, or 3D spatial/velocity correlation).
	key, matchedExisting := globalTrafficFusion.ResolveCanonicalKeyWithContext(
		address, uint8(idType), TRAFFIC_SOURCE_FLARM,
		targetLat, targetLng, inAlt, track, uint16(speedKts), speedValid, !bearingless, now, traffic,
	)
	if matchedExisting {
		ti = traffic[key]
		acceptPos := (!bearingless && ShouldAcceptPositionUpdate(ti, TRAFFIC_SOURCE_FLARM, 0, false, now)) ||
			(bearingless && !ti.Position_valid)
		if !acceptPos {
			if len(ti.Tail) <= 3 {
				if len(tail) != 0 {
					ti.Tail = tail
				} else {
					ti.Tail = getTailNumber(ognID, "FLR")
				}
			}
			MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_FLARM, now)
			addFusedSource(&ti, "FLARM")
			traffic[key] = ti
			return
		}
	} else {
		ti.Icao_addr = address
		// idType 1=ICAO, 2=Flarm ID, 3=anonymous ID. 0 is valid but not documented.
		// For us: 0=ICAO, 1=Non ICAO
		ti.Addr_type = uint8(idType)
	}

	if len(ti.Tail) <= 3 {
		if len(tail) != 0 {
			// Tail provided via NMEA (IDIDID!TAIL syntax)
			ti.Tail = tail
		} else {
			// OGN DDB fallback
			ti.Tail = getTailNumber(ognID, "FLR") // Might have better tail from ADS-B. Don't overwrite.
		}
	}
	ti.Timestamp = time.Now().UTC()
	ti.Last_source = TRAFFIC_SOURCE_FLARM
	MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_FLARM, now)

	if bearingless {
		ti.Position_valid = false
		ti.BearingDist_valid = false
		ti.DistanceEstimated = math.Abs(float64(relNorth))
		ti.DistanceEstimatedLastTs = now
	} else {
		ti.Lat = targetLat
		ti.Lng = targetLng
		ti.Distance, ti.Bearing = common.Distance(float64(ownLat), float64(ownLng), float64(ti.Lat), float64(ti.Lng))
		ti.BearingDist_valid = true
		ti.Position_valid = true
	}

	ti.Track = track
	ti.TurnRate = turn
	ti.Speed = uint16(speedKts) // m/s to knots
	ti.Speed_valid = speedValid
	ti.Vvel = int16(clampInt32(float64(vspeed)*196.85, math.MinInt16, math.MaxInt16)) // m/s to feet/min

	ti.ExtrapolatedPosition = false
	ti.Last_seen = now

	ti.Emitter_category = nmeaAircraftTypeToGdl90(acType)
	ti.SourceConfidence = TrafficSourcePriority(TRAFFIC_SOURCE_FLARM, ti.TargetType, ti.ExtrapolatedPosition)
	addFusedSource(&ti, "FLARM")

	// update traffic database
	traffic[key] = ti

	// notify
	registerTrafficUpdate(ti)

	// mark traffic as seen
	markTrafficSeen(key)
}
