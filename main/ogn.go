/*
	Copyright (c) 2020 Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	ogn.go: Routines for reading traffic from ogn-rx-eu
*/

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/common"
)

// {"sys":"OGN","addr":"395F39","addr_type":3,"acft_type":"1","lat_deg":51.7657533,"lon_deg":-1.1918533,"alt_msl_m":124,"alt_std_m":63,"track_deg":0.0,"speed_mps":0.3,"climb_mps":-0.5,"turn_dps":0.0,"DOP":1.5}
type OgnMessage struct {
	Sys       string
	Time      float64
	Addr      string
	Addr_type int32
	Acft_type string
	Acft_cat  string
	Reg       string
	Lat_deg   float32
	Lon_deg   float32
	Alt_msl_m float32
	Alt_hae_m float32
	Alt_std_m float32
	Track_deg float64
	Speed_mps float64
	Climb_mps float64
	Turn_dps  float64
	DOP       float64
	SNR_dB    float64
	Rx_err    int32
	Hard      string

	// Status message (Sys=status):
	Bkg_noise_db float32
	Gain_db      float32
	Tx_enabled   bool
}

func ognPublishNmea(nmea string) {
	if globalStatus.OGN_connected {
		if !strings.HasSuffix(nmea, "\r\n") {
			nmea += "\r\n"
		}
		select {
		case ognOutgoingMsgChan <- nmea:
		default:
		}
	}
}

var ognOutgoingMsgChan = make(chan string, 100)

func handleOgnConnection(conn net.Conn) {
	defer func() {
		globalStatus.OGN_connected = false
		conn.Close()
	}()

	ognReadWriter := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	globalStatus.OGN_connected = true

	var writeMu sync.Mutex
	writeLine := func(s string) error {
		if !strings.HasSuffix(s, "\r\n") {
			s += "\r\n"
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := ognReadWriter.WriteString(s); err != nil {
			return err
		}
		return ognReadWriter.Flush()
	}

	done := make(chan struct{})
	defer close(done)

	incoming := make(chan string, 100)
	exitCh := make(chan struct{}, 1)

	go func() {
		scanner := bufio.NewScanner(ognReadWriter.Reader)
		for scanner.Scan() {
			text := scanner.Text()
			select {
			case incoming <- text:
			case <-done:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("ogn-rx-eu connection lost: %s", err.Error())
		}
		select {
		case exitCh <- struct{}{}:
		default:
		}
	}()

	pgrmzTimer := time.NewTicker(100 * time.Millisecond)
	defer pgrmzTimer.Stop()

	for globalSettings.OGN_Enabled {
		select {
		case data := <-ognOutgoingMsgChan:
			if err := writeLine(data); err != nil {
				log.Printf("ogn-rx-eu write error: %s", err.Error())
				return
			}
		case data := <-incoming:
			TraceLog.Record(CONTEXT_OGN_RX, []byte(data))
			parseOgnMessageWithWriter(data, false, writeLine)
		case <-pgrmzTimer.C:
			if isTempPressValid() {
				mySituation.muBaro.Lock()
				baroSrc := mySituation.BaroSourceType
				mySituation.muBaro.Unlock()
				if baroSrc != BARO_TYPE_NONE && baroSrc != BARO_TYPE_ADSBESTIMATE {
					if err := writeLine(makePGRMZString()); err != nil {
						log.Printf("ogn-rx-eu PGRMZ write error: %s", err.Error())
						return
					}
				}
			}
		case <-exitCh:
			return
		}
	}
}

func ognListen() {
	for {
		if !globalSettings.OGN_Enabled || !hasOGNDev() {
			// wait until OGN is enabled
			time.Sleep(1 * time.Second)
			continue
		}
		ognAddr := "127.0.0.1:30010"
		conn, err := net.DialTimeout("tcp", ognAddr, 5*time.Second)
		if err != nil { // Local connection failed.
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("ogn-rx-eu successfully connected")
		handleOgnConnection(conn)
		time.Sleep(3 * time.Second)
	}
}

func parseOgnMessage(data string, fakeCurrentTime bool) {
	parseOgnMessageWithWriter(data, fakeCurrentTime, nil)
}

func parseOgnMessageWithWriter(data string, fakeCurrentTime bool, writeLine func(string) error) {
	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_OGN
	thisMsg.TimeReceived = stratuxClock.GetTime()
	thisMsg.Data = data

	var parsed OgnMessage
	err := json.Unmarshal([]byte(data), &parsed)
	if err != nil {
		log.Printf("Invalid Data from OGN: %s -> %s", data, err.Error())
		return
	}

	if parsed.Sys == "status" {
		importOgnStatusMessage(parsed, writeLine)
	} else {
		globalStatus.OGN_messages_total++
		msgLogAppend(thisMsg)
		logMsg(thisMsg) // writes to replay logs
		importOgnTrafficMessage(parsed, data, fakeCurrentTime)
	}
}

func importOgnStatusMessage(msg OgnMessage, writeLine func(string) error) {
	globalStatus.OGN_noise_db = msg.Bkg_noise_db
	globalStatus.OGN_gain_db = msg.Gain_db
	globalStatus.OGN_tx_enabled = msg.Tx_enabled

	// If we have an RFM95 or OGN Tracker connected, provide the config to ogn-rx-eu, so that it sends the same ID (either via RFM95 or internet)
	if msg.Tx_enabled || globalStatus.GPS_detected_type&GPS_TYPE_OGNTRACKER > 0 {
		cfg := formatOgnTrackerConfigString()
		if writeLine != nil {
			if err := writeLine(cfg); err != nil {
				log.Printf("ogn-rx-eu status config write error: %s", err.Error())
			}
		} else {
			ognPublishNmea(cfg)
		}
	}
}

func importOgnTrafficMessage(msg OgnMessage, data string, fakeCurrentTime bool) {
	importOgnTrafficMessageWithOrigin(msg, data, fakeCurrentTime, false)
}

func importOgnTrafficMessageWithOrigin(msg OgnMessage, data string, fakeCurrentTime bool, isAPRS bool) {
	var ti TrafficInfo
	addressBytes, err := hex.DecodeString(msg.Addr)
	if err != nil {
		log.Printf("Ignoring invalid ogn address: %s", msg.Addr)
		return
	}
	addressBytes = append([]byte{0}, addressBytes...) // prepend 0 byte
	if len(addressBytes) != 4 {
		log.Printf("Ignoring invalid ogn address: %s", msg.Addr)
		return
	}
	address := binary.BigEndian.Uint32(addressBytes)

	// GDL90 only knows 2 address types. ICAO and non-ICAO, so we map to those.
	// for OGN: 1=ICAO. For us: 0=ICAO, 1="ADS-B with Self-assigned address"
	addrType := uint8(1) // Non-ICAO Address
	otherAddrType := uint8(0)
	if msg.Addr_type == 1 { // ICAO Address
		addrType = 0
		otherAddrType = 1
	}

	if fakeCurrentTime {
		msg.Time = float64(time.Now().UTC().Unix())
	}

	gpsValid := isGPSValid()
	var ownLat, ownLng, ownAltMSL, ownGeoidSep float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		ownLat = mySituation.GPSLatitude
		ownLng = mySituation.GPSLongitude
		ownAltMSL = mySituation.GPSAltitudeMSL
		ownGeoidSep = mySituation.GPSGeoidSep
		mySituation.muGPS.Unlock()
	} else {
		ownLat = mySituation.GPSLatitude
		ownLng = mySituation.GPSLongitude
		ownAltMSL = mySituation.GPSAltitudeMSL
		ownGeoidSep = mySituation.GPSGeoidSep
	}

	var ownBaroAlt float32
	baroValid := isTempPressValid()
	if gpsValid && baroValid {
		if mySituation.muBaro != nil {
			mySituation.muBaro.Lock()
			ownBaroAlt = mySituation.BaroPressureAltitude
			mySituation.muBaro.Unlock()
		} else {
			ownBaroAlt = mySituation.BaroPressureAltitude
		}
	}

	// Pre-compute incoming altitude so canonical spatial matching has accurate vertical coords.
	alt := msg.Alt_msl_m * 3.28084
	if alt == 0 {
		alt = msg.Alt_hae_m*3.28084 - ownGeoidSep
	}
	var inAlt int32
	var inAltIsGNSS bool
	if gpsValid && baroValid {
		inAlt = int32(alt - ownAltMSL + ownBaroAlt)
		inAltIsGNSS = false
	} else if msg.Alt_std_m != 0 {
		inAlt = int32(msg.Alt_std_m * 3.28084)
		inAltIsGNSS = false
	} else {
		inAlt = int32(alt)
		inAltIsGNSS = true
	}

	inSpeed := uint16(0)
	if msg.Speed_mps > 0 {
		spd := msg.Speed_mps * 1.94384
		if spd > 65535 {
			spd = 65535
		}
		inSpeed = uint16(spd)
	}
	inTrack := float32(msg.Track_deg)

	posCoordsValid := msg.Lat_deg <= 360 && msg.Lat_deg >= -360 && msg.Lon_deg <= 360 && msg.Lon_deg >= -360
	posPlausible := false
	if posCoordsValid && !(msg.Lat_deg == 0 && msg.Lon_deg == 0) {
		dist, _, _, _ := common.DistRect(float64(ownLat), float64(ownLng), float64(msg.Lat_deg), float64(msg.Lon_deg))
		if !gpsValid || dist < 50000 {
			posPlausible = true
		}
	}

	otherKey := uint32(otherAddrType)<<24 | address

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	if msg.Sys == "PAW" || msg.Sys == "FNT" {
		// First, assume the AddrType guess is wrong and try to merge.. Only if that fails we use our guessed AddrType
		if _, otherAddrTypeOk := traffic[otherKey]; otherAddrTypeOk {
			addrType = otherAddrType
		}
	}

	nowMono := stratuxClock.GetTime()
	key, matchedExisting := globalTrafficFusion.ResolveCanonicalKeyWithContext(
		address, addrType, TRAFFIC_SOURCE_OGN,
		msg.Lat_deg, msg.Lon_deg, inAlt, inTrack, inSpeed, true, posPlausible, nowMono, traffic,
	)

	if matchedExisting {
		ti = traffic[key]
		// ogn-rx sends 2 types of messages.. normal ones with coords etc, and ones that only supply additional info (registration, Hardware, ...). These usually don't have
		// coords, so we can't validate them easily. Therefore, this is handled before other validations and only if we already received the traffic earlier
		hasInfo := false
		if len(msg.Reg) > 0 {
			ti.Tail = msg.Reg
			hasInfo = true
		}
		if msg.Hard == "STX" {
			ti.IsStratux = true
			hasInfo = true
		}
		if hasInfo {
			traffic[key] = ti
		}
		if msg.Time > 0 && !ti.Timestamp.IsZero() {
			msgtime := time.Unix(int64(msg.Time), 0)
			if ti.Position_valid && ti.Last_source == TRAFFIC_SOURCE_OGN && msgtime.Before(ti.Timestamp) {
				return // We already have a newer message for this target. This message was probably relayed by another tracker -- skip
			}
		}
	}

	// Sometimes there seems to be wildly invalid lat/lons, which can trip over distRect's normalization..
	if !posPlausible {
		return
	}

	var incomingTs time.Time
	if msg.Time > 0 {
		if matchedExisting && ti.Last_source == TRAFFIC_SOURCE_OGN && msg.Time < float64(ti.Timestamp.Unix()) {
			return
		}
		incomingTs = time.Unix(int64(msg.Time), 0)
	} else {
		incomingTs = time.Now().UTC()
	}
	incomingAge := time.Now().UTC().Sub(incomingTs).Seconds()
	if incomingAge > 30 || incomingAge < -2 {
		log.Printf("Discarding likely invalid OGN target: %s", data)
		return
	}

	msgMonoTime := nowMono.Add(-time.Duration(int64(incomingAge*1000)) * time.Millisecond)

	sourceName := "OGN"
	if isAPRS {
		sourceName = "APRS"
	}

	// Guard against delayed internet APRS or lower-priority OGN overwriting a fresher higher-priority direct-RF fix.
	acceptPos := !matchedExisting ||
		(isAPRS && ti.SourceConfidence == TrafficPriorityAPRS) ||
		ShouldAcceptPositionUpdate(ti, TRAFFIC_SOURCE_OGN, 0, isAPRS, nowMono)

	if len(ti.Tail) == 0 {
		ti.Tail = getTailNumber(msg.Addr, msg.Sys)
	}
	addFusedSource(&ti, sourceName)

	if !acceptPos {
		if !isAPRS {
			MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_OGN, msgMonoTime)
		}
		traffic[key] = ti
		return
	}

	if !matchedExisting || ti.Icao_addr == 0 {
		ti.Icao_addr = address
		ti.Addr_type = addrType
	}
	ti.Last_source = TRAFFIC_SOURCE_OGN
	ti.Timestamp = incomingTs
	ti.Age = incomingAge

	MergeAltitude(&ti, inAlt, inAltIsGNSS, TRAFFIC_SOURCE_OGN, msgMonoTime)

	ti.TurnRate = float32(msg.Turn_dps)
	if ti.TurnRate > 360 || ti.TurnRate < -360 {
		ti.TurnRate = 0
	}
	ti.Vvel = int16(msg.Climb_mps * 196.85)
	ti.Lat = msg.Lat_deg
	ti.Lng = msg.Lon_deg
	ti.Track = inTrack
	ti.Speed = inSpeed
	ti.Speed_valid = true
	ti.SignalLevel = msg.SNR_dB

	if gpsValid {
		ti.Distance, ti.Bearing = common.Distance(float64(ownLat), float64(ownLng), float64(ti.Lat), float64(ti.Lng))
		ti.BearingDist_valid = true
	}
	ti.Position_valid = true
	ti.ExtrapolatedPosition = false
	ti.Last_seen = msgMonoTime
	ti.Last_speed = msgMonoTime

	emitter, err := strconv.ParseInt(msg.Acft_cat, 16, 8)
	if len(msg.Acft_cat) == 2 && err == nil {
		ti.Emitter_category = uint8(emitter)
	} else {
		ti.Emitter_category = nmeaAircraftTypeToGdl90(msg.Acft_type)
	}

	ti.SourceConfidence = TrafficSourcePriorityWithAPRS(TRAFFIC_SOURCE_OGN, ti.TargetType, ti.ExtrapolatedPosition, isAPRS)

	postProcessTraffic(&ti)
	traffic[key] = ti
	registerTrafficUpdate(ti)
	markTrafficSeen(key)

	if globalSettings.DEBUG {
		txt, _ := json.Marshal(ti)
		log.Printf("OGN traffic imported: %s", string(txt))
	}
}

type ognDDBDevice struct {
	DeviceID     string `json:"device_id"`
	Registration string `json:"registration"`
}

type ognDDBFile struct {
	Devices []ognDDBDevice `json:"devices"`
}

var (
	ognTailCacheMu     sync.RWMutex
	ognTailNumberCache = make(map[string]string)
	ognTailCacheLoaded bool
)

func lookupOgnTailNumber(ognid string) string {
	ognTailCacheMu.RLock()
	loaded := ognTailCacheLoaded
	tail, ok := ognTailNumberCache[ognid]
	ognTailCacheMu.RUnlock()

	if !loaded {
		ognTailCacheMu.Lock()
		if !ognTailCacheLoaded {
			log.Printf("Parsing OGN device db")
			ddb, err := os.ReadFile(STRATUX_HOME + "/ogn/ddb.json")
			if err != nil {
				ognTailCacheMu.Unlock()
				log.Printf("Failed to read OGN device db: %s", err.Error())
				return ognid
			}
			var data ognDDBFile
			if err := json.Unmarshal(ddb, &data); err != nil {
				ognTailCacheMu.Unlock()
				log.Printf("Failed to parse OGN device db: %s", err.Error())
				return ognid
			}
			for _, dev := range data.Devices {
				if dev.DeviceID != "" {
					ognTailNumberCache[dev.DeviceID] = dev.Registration
				}
			}
			aliasCount := globalTrafficFusion.LoadDDBAliasesFromJSON(ddb)
			ognTailCacheLoaded = true
			log.Printf("Successfully parsed OGN device db (%d devices, %d fusion aliases)", len(ognTailNumberCache), aliasCount)
		}
		tail, ok = ognTailNumberCache[ognid]
		ognTailCacheMu.Unlock()
	}

	if ok {
		return tail
	}
	return ""
}

func getTailNumber(ognid string, sys string) string {
	tail := lookupOgnTailNumber(ognid)
	if globalSettings.DisplayTrafficSource {
		if sys == "" {
			sys = "un"
		}
		sys = strings.ToLower(sys)
		if len(sys) >= 2 {
			sys = sys[:2]
		}
		tail = sys + tail
	}
	return tail
}
