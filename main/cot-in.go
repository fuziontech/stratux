/*
	Copyright (c) 2024 Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.
	cot-in.go: Accept Cursor-on-Target (CoT) events on UDP port 8087
*/

package main

import (
	"encoding/json"
	"encoding/xml"
	"hash/fnv"
	"log"
	"math"
	"net"
	"strings"
	"time"
)

const (
	cotMaxBufSize = 65536
	cotEventOpen  = "<event"
	cotEventClose = "</event>"
)

type CotEvent struct {
	Version string    `xml:"version,attr"`
	Uid     string    `xml:"uid,attr"`
	Type_   string    `xml:"type,attr"`
	Time    string    `xml:"time,attr"`
	Start   string    `xml:"start,attr"`
	Stale   string    `xml:"stale,attr"`
	Point   CotPoint  `xml:"point"`
	Detail  CotDetail `xml:"detail"`
}

type CotPoint struct {
	Lat float32 `xml:"lat,attr"`
	Lon float32 `xml:"lon,attr"`
	Hae float32 `xml:"hae,attr"`
	//ce float32  `xml:"ce,attr"`
	//ce float32  `xml:"le,attr"`
}

type CotDetail struct {
	Track CotTrack `xml:"track"`
}

type CotTrack struct {
	Speed  float32 `xml:"speed,attr"`
	Course float32 `xml:"course,attr"`
}

func cotListen() {
	server, err := net.ListenPacket("udp", ":8087")
	if err != nil {
		log.Printf("cotListen() failed to listen on udp:8087. cot support disabled")
		return
	}
	defer server.Close()

	messageBuf := ""
	buf := make([]byte, 2048)
	for {
		n, _, err := server.ReadFrom(buf)
		if err != nil || n == 0 {
			continue
		}
		messageBuf += string(buf[0:n])

		for {
			startIndex := strings.Index(messageBuf, cotEventOpen)
			if startIndex < 0 {
				// Discard leading garbage; keep at most len("<event")-1 trailing bytes
				// in case "<event" is split across UDP datagrams.
				if len(messageBuf) > len(cotEventOpen)-1 {
					messageBuf = messageBuf[len(messageBuf)-(len(cotEventOpen)-1):]
				}
				break
			}
			if startIndex > 0 {
				messageBuf = messageBuf[startIndex:]
			}

			endIndex := strings.Index(messageBuf, cotEventClose)
			if endIndex < 0 {
				if len(messageBuf) > cotMaxBufSize {
					log.Printf("cotListen(): buffer exceeded %d bytes without closing %s, resetting", cotMaxBufSize, cotEventClose)
					messageBuf = ""
				}
				break
			}

			msg := messageBuf[:endIndex+len(cotEventClose)]
			messageBuf = messageBuf[endIndex+len(cotEventClose):]
			processCotMessage(msg)
		}
	}
}

func sanitizeCotTail(uid string) string {
	var b strings.Builder
	for i := 0; i < len(uid) && b.Len() < 32; i++ {
		c := uid[i]
		if c < 0x20 || c >= 0x7f || c == ',' || c == '*' || c == '$' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func processCotMessage(msg string) {
	var event CotEvent
	err := xml.Unmarshal([]byte(msg), &event)
	if err != nil {
		log.Printf("Failed to parse COT event: %s", msg)
		return
	}

	lat := float64(event.Point.Lat)
	lon := float64(event.Point.Lon)
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return
	}
	if lat == 0 && lon == 0 {
		return
	}

	hae := float64(event.Point.Hae)
	if math.IsNaN(hae) || math.IsInf(hae, 0) {
		hae = 0
	}

	speedMs := float64(event.Detail.Track.Speed)
	if math.IsNaN(speedMs) || math.IsInf(speedMs, 0) || speedMs < 0 {
		speedMs = 0
	}
	speedKts := speedMs * 1.94384449
	if speedKts > math.MaxUint16 {
		speedKts = math.MaxUint16
	}

	course := float64(event.Detail.Track.Course)
	if math.IsNaN(course) || math.IsInf(course, 0) {
		course = 0
	}

	// Ugly.. we just use the lower 24 bit of the hash of the uid to generate an address
	hasher := fnv.New32a()
	hasher.Write([]byte(event.Uid))
	addr := hasher.Sum32()
	addr = addr & 0x00FFFFFF
	key := 1<<24 | addr // mark as non-icao

	// Snapshot ownship altitude data before acquiring trafficMutex.
	var gpsHAE, baroAlt float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		gpsHAE = mySituation.GPSHeightAboveEllipsoid
		mySituation.muGPS.Unlock()
	} else {
		gpsHAE = mySituation.GPSHeightAboveEllipsoid
	}
	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
		baroAlt = mySituation.BaroPressureAltitude
		mySituation.muBaro.Unlock()
	} else {
		baroAlt = mySituation.BaroPressureAltitude
	}

	cleanTail := sanitizeCotTail(event.Uid)
	now := stratuxClock.GetTime()

	var ti TrafficInfo

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	if existingTi, ok := traffic[key]; ok {
		ti = existingTi
	}
	ti.Addr_type = 1
	ti.Icao_addr = addr
	ti.Timestamp = time.Now()
	ti.Lat = event.Point.Lat
	ti.Lng = event.Point.Lon
	ti.Position_valid = true
	ti.ExtrapolatedPosition = false
	ti.Reg = cleanTail
	ti.Tail = cleanTail
	ti.Last_source = TRAFFIC_SOURCE_OGN // TODO: properly implement TRAFFIC_SOURCE_COT
	ti.Age = 0
	ti.AgeLastAlt = 0
	ti.Last_seen = now
	ti.Last_alt = now
	ti.Speed = uint16(speedKts) // m/s to kts
	ti.Speed_valid = ti.Speed != 0
	ti.Track = float32(course)

	// Convert CoT HAE (meters above WGS84 ellipsoid) to feet, then to pressure altitude when possible.
	alt := float32(hae * 3.28084) // to feet HAE
	if isGPSValid() && isTempPressValid() {
		ti.Alt = int32(alt - gpsHAE + baroAlt)
		ti.AltIsGNSS = false
	} else {
		// Fall back to GNSS alt
		ti.Alt = int32(alt)
		ti.AltIsGNSS = true
	}

	postProcessTraffic(&ti)
	traffic[key] = ti
	registerTrafficUpdate(ti)
	seenTraffic[key] = true

	globalSettingsMutex.RLock()
	debug := globalSettings.DEBUG
	globalSettingsMutex.RUnlock()
	if debug {
		txt, _ := json.Marshal(ti)
		log.Printf("COT traffic imported: %s", string(txt))
	}
}
