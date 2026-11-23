/*
	Copyright (c) 2021 R. van Twisk
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	ais.go: Routines for reading AIS traffic
*/

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"time"

	"github.com/stratux/stratux/common"

	"github.com/BertoldVdb/go-ais"
	"github.com/BertoldVdb/go-ais/aisnmea"
)

var aisNmeaParser = aisnmea.NMEACodecNew(ais.CodecNew(false, false))

func handleAisConnection(conn net.Conn) {
	defer func() {
		globalStatus.AIS_connected = false
		conn.Close()
	}()

	globalStatus.AIS_connected = true
	done := make(chan struct{})
	defer close(done)

	incoming := make(chan string, 100)
	exitCh := make(chan struct{}, 1)

	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			text := scanner.Text()
			select {
			case incoming <- text:
			case <-done:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("ais-rx-eu connection lost: %s", err.Error())
		}
		select {
		case exitCh <- struct{}{}:
		default:
		}
	}()

	checkTicker := time.NewTicker(1 * time.Second)
	defer checkTicker.Stop()

	for globalSettings.AIS_Enabled {
		select {
		case data := <-incoming:
			TraceLog.Record(CONTEXT_AIS, []byte(data))
			parseAisMessage(data)
		case <-checkTicker.C:
			// Periodically re-check globalSettings.AIS_Enabled when stream is idle.
		case <-exitCh:
			return
		}
	}
}

func aisListen() {
	for {
		if !globalSettings.AIS_Enabled || !hasAISDev() {
			// wait until AIS is enabled
			time.Sleep(1 * time.Second)
			continue
		}
		aisAddr := "127.0.0.1:10110"
		conn, err := net.DialTimeout("tcp", aisAddr, 5*time.Second)
		if err != nil { // Local connection failed.
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("ais successfully connected")
		handleAisConnection(conn)
		time.Sleep(3 * time.Second)
	}
}

func parseAisMessage(data string) {
	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_AIS
	thisMsg.TimeReceived = stratuxClock.GetTime()
	thisMsg.Data = data
	globalStatus.AIS_messages_total++
	msgLogAppend(thisMsg)
	logMsg(thisMsg) // writes to replay logs

	msg, err := aisNmeaParser.ParseSentence(data)
	if err == nil && msg != nil && msg.Packet != nil {
		importAISTrafficMessage(msg)
	} else if err != nil {
		log.Printf("Invalid Data from AIS: %s", err.Error())
	}
}

// Datastructure for AIS parsing
// explanation can be found at https://www.navcen.uscg.gov/?pageName=AISMessages
func importAISTrafficMessage(msg *aisnmea.VdmPacket) {
	var ti TrafficInfo

	header := msg.Packet.GetHeader()
	if header == nil {
		return
	}
	key := header.UserID

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	if existingTi, ok := traffic[key]; ok {
		ti = existingTi
	} else {
		ti.Reg = fmt.Sprintf("%d", header.UserID)
		ti.Emitter_category = 18 // Ground Vehicle, see also gdl90EmitterCatToNMEA
		ti.TargetType = TARGET_TYPE_AIS
		ti.Last_source = TRAFFIC_SOURCE_AIS
		ti.Alt = 0
		ti.Addr_type = uint8(1) // Non-ICAO Address
		ti.SignalLevel = 0.0
		ti.Squawk = 0
		ti.AltIsGNSS = false
		ti.GnssDiffFromBaroAlt = 0
		ti.NIC = 0
		ti.NACp = 0
		ti.Vvel = 0
		ti.PriorityStatus = 0

		ti.Age = 0
		ti.AgeLastAlt = 0
	}

	now := stratuxClock.GetTime()
	ti.Icao_addr = header.UserID
	ti.Timestamp = time.Now().UTC()
	ti.Last_seen = now
	ti.Last_alt = now
	ti.SourceConfidence = TrafficSourcePriority(TRAFFIC_SOURCE_AIS, TARGET_TYPE_AIS, false)
	addFusedSource(&ti, "AIS")

	// Handle ShipStaticData
	if header.MessageID == 5 {
		shipStaticData, ok := msg.Packet.(ais.ShipStaticData)
		if !ok {
			return
		}

		ti.Tail = strings.TrimSpace(shipStaticData.Name)
		ti.Reg = strings.TrimSpace(shipStaticData.CallSign)
		ti.SurfaceVehicleType = uint16(shipStaticData.Type)
		// Store in case this was the first message and we discard due to GPS not available
		traffic[key] = ti
	}

	// Handle LongRangeAisBroadcastMessage
	if header.MessageID == 27 {
		positionReport, ok := msg.Packet.(ais.LongRangeAisBroadcastMessage)
		if !ok {
			return
		}

		ti.OnGround = true
		ti.Position_valid = true
		ti.ExtrapolatedPosition = false
		ti.Lat = float32(positionReport.Latitude)
		ti.Lng = float32(positionReport.Longitude)

		if positionReport.Cog != 511 {
			ti.Track = float32(positionReport.Cog)
		}
		if positionReport.Sog < 63 {
			ti.Speed = uint16(positionReport.Sog)
			ti.Speed_valid = true
			ti.Last_speed = ti.Last_seen
		}
	}

	// Handle MessageID 1,2 & 3 Position reports
	if header.MessageID == 1 || header.MessageID == 2 || header.MessageID == 3 {
		positionReport, ok := msg.Packet.(ais.PositionReport)
		if !ok {
			return
		}

		ti.OnGround = true
		ti.Position_valid = true
		ti.Lat = float32(positionReport.Latitude)
		ti.Lng = float32(positionReport.Longitude)

		if positionReport.Sog < 102.3 {
			ti.Speed = uint16(positionReport.Sog) // Sog is in knots
			ti.Speed_valid = true
			ti.Last_speed = ti.Last_seen
		}

		// We assume that when we have speed,
		// we also have a proper course over ground so we take that over heading.
		// Otherwise Track will be heading so boats will orient correctly
		if positionReport.Sog > 0.0 && positionReport.Sog < 102.3 {
			var cog float32 = 0.0
			if positionReport.Cog != 360 {
				cog = float32(positionReport.Cog)
			}
			ti.Track = cog
		} else {
			var heading float32 = 0.0
			if positionReport.TrueHeading != 511 {
				heading = float32(positionReport.TrueHeading)
			}
			ti.Track = heading
		}

		// AIS ROT_ais = 4.733 * sqrt(ROT_deg_per_min), signed (+ right, - left).
		// Preserve sign and convert deg/min to deg/sec.
		if positionReport.RateOfTurn != -128 {
			rot := float64(positionReport.RateOfTurn)
			ti.TurnRate = float32(math.Copysign((rot/4.733)*(rot/4.733)/60.0, rot))
		} else {
			ti.TurnRate = 0
		}

		ti.ExtrapolatedPosition = false
	}

	// Prevent wild lat/long coordinates
	if ti.Lat > 360 || ti.Lat < -360 || ti.Lng > 360 || ti.Lng < -360 {
		return
	}

	// Validate the position report
	if isGPSValid() && (ti.Lat != 0 && ti.Lng != 0) {
		var ownLat, ownLng float32
		if mySituation.muGPS != nil {
			mySituation.muGPS.Lock()
			ownLat = mySituation.GPSLatitude
			ownLng = mySituation.GPSLongitude
			mySituation.muGPS.Unlock()
		} else {
			ownLat = mySituation.GPSLatitude
			ownLng = mySituation.GPSLongitude
		}
		ti.Distance, ti.Bearing = common.Distance(float64(ownLat), float64(ownLng), float64(ti.Lat), float64(ti.Lng))
		ti.BearingDist_valid = true
	}

	// Do not display targets more than 150km
	if !ti.BearingDist_valid || ti.Distance >= 150000 {
		return
	}

	traffic[key] = ti
	postProcessTraffic(&ti)
	registerTrafficUpdate(ti) // Sends this one to the web interface
	seenTraffic[key] = true

	if globalSettings.DEBUG {
		txt, _ := json.Marshal(ti)
		log.Printf("AIS traffic imported: %s", string(txt))
	}
}
