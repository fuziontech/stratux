/*
	Copyright (c) 2022 Quentin Bossard
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	aprs.go: Routines for reading traffic from aprs
*/

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var aprsRegex = regexp.MustCompile(
	`(?P<protocol>ICA|FLR|SKY|PAW|OGN|RND|FMT|MTK|XCG|FAN|FNT)(?P<id>[\dA-Z]{6})>` + // 1: protocol, 2: id
		`[A-Z]+,qAS,([\d\w]+):[\/]` + // 3: receiver
		`(?P<time>\d{6})h(?P<latitude>\d{4}\.\d{2}[NS])[\/\\](?P<longitude>\d{5}\.\d{2}[EW])` + // 4: time, 5: lat, 6: lon
		`\D` + // sep
		`((?P<track>\d{3})\/(?P<speed>\d{3})\/A=(?P<altitude>\d+))*` + // 7, 8: track, 9: speed, 10: alt
		`(\s!W(?P<lonlatprecision>\d+)!\s)*` + // 11, 12: optional DAO precision
		`(id(?P<detailid>[\dA-F]{8}))*`) // 13, 14: optional detail id

func buildAprsFilterString() string {
	var fixQual uint8
	var lat, lng float32
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		fixQual = mySituation.GPSFixQuality
		lat = mySituation.GPSLatitude
		lng = mySituation.GPSLongitude
		mySituation.muGPS.Unlock()
	} else {
		fixQual = mySituation.GPSFixQuality
		lat = mySituation.GPSLatitude
		lng = mySituation.GPSLongitude
	}
	if fixQual > 0 {
		return fmt.Sprintf(
			"filter r/%.7f/%.7f/%d\r\n",
			lat, lng,
			globalSettings.RadarRange*2) // RadarRange is an int in NM, APRS wants an int in km and 2~=1.852
	}
	return ""
}

func authenticate(writeConn func(string) error) error {
	filter := buildAprsFilterString()
	auth := fmt.Sprintf("user OGNNOCALL pass -1 vers stratux %s %s\r\n", globalStatus.Version, filter)
	log.Printf("%s", auth)
	return writeConn(auth)
}

func keepalive(ctx context.Context, writeConn func(string) error) {
	go func() {
		ticker := time.NewTicker(240 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				if err := writeConn(fmt.Sprintf("# stratux keepalive %s\r\n", t.UTC().Format(time.RFC3339))); err != nil {
					return
				}
			}
		}
	}()
}

func updateFilter(ctx context.Context, writeConn func(string) error) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				filter := buildAprsFilterString()
				if filter != "" {
					if err := writeConn("#" + filter); err != nil {
						return
					}
				}
			}
		}
	}()
}

func handleAprsConnection(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		globalStatus.APRS_connected = false
		log.Printf("APRS closing connection")
		conn.Close()
	}()

	var writeMu sync.Mutex
	writeConn := func(s string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := fmt.Fprintf(conn, "%s", s)
		return err
	}

	if err := authenticate(writeConn); err != nil {
		log.Printf("APRS authentication write failed: %s", err.Error())
		return
	}

	log.Printf("APRS successfully connected")
	globalStatus.APRS_connected = true

	keepalive(ctx, writeConn)
	updateFilter(ctx, writeConn)

	incoming := make(chan string, 100)
	exitCh := make(chan struct{}, 1)

	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			temp := scanner.Text()
			select {
			case incoming <- temp: // Put in the channel unless it is full
			case <-ctx.Done():
				return
			default:
				if globalSettings.DEBUG {
					log.Printf("APRS incoming channel full. Discarding %s", temp)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("APRS issue: %s", err.Error())
		}
		select {
		case exitCh <- struct{}{}:
		default:
		}
	}()

	checkTicker := time.NewTicker(1 * time.Second)
	defer checkTicker.Stop()

	for globalSettings.APRS_Enabled {
		select {
		case data := <-incoming:
			TraceLog.Record(CONTEXT_APRS, []byte(data))
			parseAprsMessage(data, false)
		case <-checkTicker.C:
			// Periodically re-check globalSettings.APRS_Enabled even if stream is idle.
		case <-exitCh:
			return
		}
	}
}

func aprsListen() {
	for {
		if !globalSettings.APRS_Enabled || !isGPSValid() {
			// wait until APRS is enabled
			time.Sleep(1 * time.Second)
			continue
		}
		if globalSettings.DEBUG {
			log.Printf("aprs connecting...")
		}
		conn, err := net.DialTimeout("tcp", "aprs.glidernet.org:14580", 10*time.Second)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		handleAprsConnection(conn)
		time.Sleep(3 * time.Second)
	}
}

func parseAprsMessage(data string, fakeCurrentTime bool) {
	if globalSettings.DEBUG {
		log.Printf("%+v\n", data)
	}

	// APRS,qAS: aircraft beacon
	// APRS,TCPIP*,qAC: ground station beacon
	res := aprsRegex.FindStringSubmatch(data)
	if res == nil { // no match
		if !strings.Contains(data, "TCPIP*") && globalSettings.DEBUG {
			log.Printf("No match for: %s", data)
		}
		return
	}
	if len(res) < 15 { // too few captures
		log.Printf("Invalid APRS data format: %s", data)
		return
	}
	if len(res[14]) < 2 || len(res[4]) < 6 || len(res[5]) < 7 || len(res[6]) < 8 {
		return
	}

	hh, err1 := strconv.ParseInt(res[4][:2], 10, 8)
	mm, err2 := strconv.ParseInt(res[4][2:4], 10, 8)
	ss, err3 := strconv.ParseInt(res[4][4:6], 10, 8)
	if err1 != nil || err2 != nil || err3 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || ss < 0 || ss > 59 {
		return
	}

	now := time.Now().UTC()
	ts := time.Date(now.Year(), now.Month(), now.Day(), int(hh), int(mm), int(ss), 0, time.UTC)
	// Handle UTC midnight rollover (beacon just before 00:00Z received just after, or vice versa).
	if diff := ts.Sub(now); diff > 12*time.Hour {
		ts = ts.Add(-24 * time.Hour)
	} else if diff < -12*time.Hour {
		ts = ts.Add(24 * time.Hour)
	}

	lat, err := strconv.ParseFloat(res[5][:2], 64)
	if err != nil {
		return
	}
	lat_m, err := strconv.ParseFloat(res[5][2:len(res[5])-1], 64)
	if err != nil {
		return
	}
	lon, err := strconv.ParseFloat(res[6][:3], 64)
	if err != nil {
		return
	}
	lon_m, err := strconv.ParseFloat(res[6][3:len(res[6])-1], 64)
	if err != nil {
		return
	}

	// Optional DAO (!Wxy!) precision extension: default to 0 if absent or shorter than 2 digits.
	var lat_m3d, lon_m3d float64
	if len(res[12]) >= 2 {
		if v, err := strconv.ParseFloat(res[12][:1], 64); err == nil {
			lat_m3d = v
		}
		if v, err := strconv.ParseFloat(res[12][1:2], 64); err == nil {
			lon_m3d = v
		}
	}

	// Combine degrees + minutes + DAO before applying hemisphere sign.
	latVal := lat + lat_m/60.0 + lat_m3d/60000.0
	if strings.Contains(res[5], "S") {
		latVal = -latVal
	}
	lonVal := lon + lon_m/60.0 + lon_m3d/60000.0
	if strings.Contains(res[6], "W") {
		lonVal = -lonVal
	}

	track, err := strconv.ParseFloat(res[8], 64)
	if err != nil {
		return
	}
	speed, err := strconv.ParseFloat(res[9], 64)
	if err != nil {
		return
	}
	alt, err := strconv.ParseFloat(res[10], 64)
	if err != nil {
		return
	}

	details, err := hex.DecodeString(res[14][:2])
	if err != nil || len(details) < 1 {
		log.Printf("APRS invalid detail hex %q: %v", res[14], err)
		return
	}
	detail_byte := details[0]
	addr_type := detail_byte & 0b00000011
	acft_type := (detail_byte & 0b00111100) >> 2

	msg := OgnMessage{
		Sys:       res[1],
		Time:      float64(ts.Unix()),
		Addr:      res[2],
		Addr_type: int32(addr_type),
		Acft_type: fmt.Sprintf("%d", acft_type),
		Lat_deg:   float32(latVal),
		Lon_deg:   float32(lonVal),
		Alt_msl_m: float32(alt * 0.3048),
		Track_deg: track,
		Speed_mps: speed * 0.514444,
	}

	if globalSettings.DEBUG {
		log.Printf("%+v\n", msg)
	}

	importOgnTrafficMessageWithOrigin(msg, data, fakeCurrentTime, true)
}
