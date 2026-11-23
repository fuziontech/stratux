/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	managementinterface.go: Web interfaces (JSON and websocket), web server for web interface HTML.
*/

package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	humanize "github.com/dustin/go-humanize"
	"golang.org/x/net/websocket"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/uatparse"
)

const (
	maxSettingsBodyBytes = 1 << 20   // 1 MB
	maxUpdateUploadBytes = 256 << 20 // 256 MB
	maxPongUploadBytes   = 64 << 20  // 64 MB
	maxTileUnzippedBytes = 16 << 20  // 16 MB
	wsWriteTimeout       = 1 * time.Second
)

var (
	updateFilenameRegex = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	otaStatusPaths      = []string{
		"/var/lib/stratux/ota_last_status",
		"/overlay/robase/var/lib/stratux/ota_last_status",
	}
	githubLatestReleaseURL = "https://api.github.com/repos/stratux/stratux/releases/latest"
	firmwareBaseDir        = STRATUX_HOME
)

type SettingMessage struct {
	Setting string `json:"setting"`
	Value   bool   `json:"state"`
}

type MbTileConnectionCacheEntry struct {
	Path     string
	Conn     *sql.DB
	Metadata map[string]string
	fileTime time.Time
}

func (this *MbTileConnectionCacheEntry) IsOutdated() bool {
	file, err := os.Stat(this.Path)
	if err != nil {
		return true
	}
	modTime := file.ModTime()
	return modTime != this.fileTime
}

func NewMbTileConnectionCacheEntry(path string, conn *sql.DB) *MbTileConnectionCacheEntry {
	file, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &MbTileConnectionCacheEntry{path, conn, nil, file.ModTime()}
}

var mbtileCacheLock = sync.Mutex{}
var mbtileConnectionCache = make(map[string]MbTileConnectionCacheEntry)

// Weather updates channel.
var weatherUpdate *uibroadcaster
var trafficUpdate *uibroadcaster
var radarUpdate *uibroadcaster
var gdl90Update *uibroadcaster

// Situation updates channel.
var situationUpdate *uibroadcaster

// Raw weather (UATFrame packet stream) update channel.
var weatherRawUpdate *uibroadcaster

// snapshotSituation takes a race-free copy of mySituation across its three subsystem mutexes.
func snapshotSituation() SituationData {
	var snap SituationData

	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
	}
	snap.GPSLastFixSinceMidnightUTC = mySituation.GPSLastFixSinceMidnightUTC
	snap.GPSLatitude = mySituation.GPSLatitude
	snap.GPSLongitude = mySituation.GPSLongitude
	snap.GPSFixQuality = mySituation.GPSFixQuality
	snap.GPSHeightAboveEllipsoid = mySituation.GPSHeightAboveEllipsoid
	snap.GPSGeoidSep = mySituation.GPSGeoidSep
	snap.GPSSatellites = mySituation.GPSSatellites
	snap.GPSSatellitesTracked = mySituation.GPSSatellitesTracked
	snap.GPSSatellitesSeen = mySituation.GPSSatellitesSeen
	snap.GPSHorizontalAccuracy = mySituation.GPSHorizontalAccuracy
	snap.GPSNACp = mySituation.GPSNACp
	snap.GPSAltitudeMSL = mySituation.GPSAltitudeMSL
	snap.GPSVerticalAccuracy = mySituation.GPSVerticalAccuracy
	snap.GPSVerticalSpeed = mySituation.GPSVerticalSpeed
	snap.GPSLastFixLocalTime = mySituation.GPSLastFixLocalTime
	snap.GPSTrueCourse = mySituation.GPSTrueCourse
	snap.GPSTurnRate = mySituation.GPSTurnRate
	snap.GPSGroundSpeed = mySituation.GPSGroundSpeed
	snap.GPSLastGroundTrackTime = mySituation.GPSLastGroundTrackTime
	snap.GPSTime = mySituation.GPSTime
	snap.GPSLastGPSTimeStratuxTime = mySituation.GPSLastGPSTimeStratuxTime
	snap.GPSLastValidNMEAMessageTime = mySituation.GPSLastValidNMEAMessageTime
	snap.GPSLastValidNMEAMessage = mySituation.GPSLastValidNMEAMessage
	snap.GPSLastAccuracyTime = mySituation.GPSLastAccuracyTime
	snap.GPSPositionSampleRate = mySituation.GPSPositionSampleRate
	if mySituation.muGPS != nil {
		mySituation.muGPS.Unlock()
	}

	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
	}
	snap.BaroTemperature = mySituation.BaroTemperature
	snap.BaroPressureAltitude = mySituation.BaroPressureAltitude
	snap.BaroVerticalSpeed = mySituation.BaroVerticalSpeed
	snap.BaroLastMeasurementTime = mySituation.BaroLastMeasurementTime
	snap.BaroSourceType = mySituation.BaroSourceType
	if mySituation.muBaro != nil {
		mySituation.muBaro.Unlock()
	}

	if mySituation.muAttitude != nil {
		mySituation.muAttitude.Lock()
	}
	snap.AHRSPitch = mySituation.AHRSPitch
	snap.AHRSRoll = mySituation.AHRSRoll
	snap.AHRSGyroHeading = mySituation.AHRSGyroHeading
	snap.AHRSMagHeading = mySituation.AHRSMagHeading
	snap.AHRSSlipSkid = mySituation.AHRSSlipSkid
	snap.AHRSTurnRate = mySituation.AHRSTurnRate
	snap.AHRSGLoad = mySituation.AHRSGLoad
	snap.AHRSGLoadMin = mySituation.AHRSGLoadMin
	snap.AHRSGLoadMax = mySituation.AHRSGLoadMax
	snap.AHRSLastAttitudeTime = mySituation.AHRSLastAttitudeTime
	snap.AHRSStatus = mySituation.AHRSStatus
	if mySituation.muAttitude != nil {
		mySituation.muAttitude.Unlock()
	}

	return snap
}

// snapshotStatus returns a race-free snapshot of globalStatus including its Errors slice.
func snapshotStatus() status {
	if systemErrsMutex != nil {
		systemErrsMutex.Lock()
		defer systemErrsMutex.Unlock()
	}
	globalStatusMutex.RLock()
	snap := globalStatus
	if len(globalStatus.Errors) > 0 {
		snap.Errors = append([]string(nil), globalStatus.Errors...)
	}
	globalStatusMutex.RUnlock()
	return snap
}

// sanitizedSettingsCopy returns a copy of globalSettings with Wi-Fi secrets stripped
// for broadcast over the /radar WebSocket.
func sanitizedSettingsCopy() settings {
	globalSettingsMutex.RLock()
	snap := globalSettings
	if len(globalSettings.WiFiClientNetworks) > 0 {
		redacted := make([]wifiClientNetwork, len(globalSettings.WiFiClientNetworks))
		for i, n := range globalSettings.WiFiClientNetworks {
			redacted[i] = wifiClientNetwork{SSID: n.SSID, Password: ""}
		}
		snap.WiFiClientNetworks = redacted
	}
	globalSettingsMutex.RUnlock()

	snap.WiFiPassphrase = ""
	snap.WiFiDirectPin = ""
	return snap
}

// snapshotValidTraffic copies valid traffic entries under trafficMutex so I/O never holds the lock.
func snapshotValidTraffic() []TrafficInfo {
	trafficMutex.Lock()
	snap := make([]TrafficInfo, 0, len(traffic))
	for _, traf := range traffic {
		if traf.Position_valid {
			snap = append(snap, traf)
		}
	}
	trafficMutex.Unlock()
	return snap
}

func validateSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // Same-origin requests without Origin header and non-browser tools (curl/EFB)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func requirePostSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !validateSameOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func handleGDL90WS(conn *websocket.Conn) {
	// Subscribe the socket to receive updates.
	gdl90Update.AddSocket(conn)

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

/*
The /weather websocket starts off by sending the current buffer of weather messages, then sends updates as they are received.
*/
func handleWeatherWS(conn *websocket.Conn) {
	// Subscribe the socket to receive updates.
	weatherUpdate.AddSocket(conn)

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

func handleJsonIo(conn *websocket.Conn) {
	// Snapshot valid traffic under trafficMutex, then perform all I/O and AddSocket outside the lock.
	snap := snapshotValidTraffic()
	for i := range snap {
		trafficJSON, err := json.Marshal(&snap[i])
		if err != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if _, err := conn.Write(trafficJSON); err != nil {
			return
		}
	}

	// Subscribe the socket to receive updates outside trafficMutex.
	trafficUpdate.AddSocket(conn)
	radarUpdate.AddSocket(conn)
	weatherRawUpdate.AddSocket(conn)
	situationUpdate.AddSocket(conn)

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

// Works just as weather updates do.
func handleTrafficWS(conn *websocket.Conn) {
	// Snapshot valid traffic under trafficMutex, then perform all I/O and AddSocket outside the lock.
	snap := snapshotValidTraffic()
	for i := range snap {
		trafficJSON, err := json.Marshal(&snap[i])
		if err != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if _, err := conn.Write(trafficJSON); err != nil {
			return
		}
	}

	// Subscribe the socket to receive updates outside trafficMutex.
	trafficUpdate.AddSocket(conn)

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

func handleRadarWS(conn *websocket.Conn) {
	// Subscribe the socket to receive updates. Not necessary to send old traffic.
	radarUpdate.AddSocket(conn)

	// Broadcast sanitized settings (Wi-Fi secrets stripped).
	radarUpdate.SendJSON(sanitizedSettingsCopy())

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

func handleStatusWS(conn *websocket.Conn) {
	timer := time.NewTicker(1 * time.Second)
	defer timer.Stop()

	for {
		snap := snapshotStatus()
		update, err := json.Marshal(&snap)
		if err == nil {
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if _, err := conn.Write(update); err != nil {
				break
			}
		}
		<-timer.C
	}
}

func handleSituationWS(conn *websocket.Conn) {
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()

	for {
		snap := snapshotSituation()
		situationJSON, err := json.Marshal(&snap)
		if err == nil {
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if _, err := conn.Write(situationJSON); err != nil {
				break
			}
		}
		<-timer.C
	}
}

// AJAX call - /getStatus. Responds with current global status
// a webservice call for the same data available on the websocket but when only a single update is needed
func handleStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)
	snap := snapshotStatus()
	statusJSON, _ := json.Marshal(&snap)
	fmt.Fprintf(w, "%s\n", statusJSON)
}

// AJAX call - /getSituation. Responds with current situation (lat/lon/gdspeed/track/pitch/roll/heading/etc.)
func handleSituationRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)
	snap := snapshotSituation()
	situationJSON, _ := json.Marshal(&snap)
	fmt.Fprintf(w, "%s\n", situationJSON)
}

// AJAX call - /getTowers. Responds with all ADS-B ground towers that have sent messages that we were able to parse, along with its stats.
func handleTowersRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	ADSBTowerMutex.Lock()
	towersJSON, err := json.Marshal(&ADSBTowers)
	ADSBTowerMutex.Unlock()
	if err != nil {
		log.Printf("Error sending tower JSON data: %s\n", err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", towersJSON)
}

// AJAX call - /getSatellites. Responds with all GNSS satellites that are being tracked, along with status information.
func handleSatellitesRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)
	mySituation.muSatellite.Lock()
	satellitesJSON, err := json.Marshal(&Satellites)
	mySituation.muSatellite.Unlock()
	if err != nil {
		log.Printf("Error sending GNSS satellite JSON data: %s\n", err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", satellitesJSON)
}

// AJAX call - /getSettings. Responds with all stratux.conf data (no wildcard CORS since it contains Wi-Fi config).
func handleSettingsGetRequest(w http.ResponseWriter, r *http.Request) {
	if !validateSameOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	setNoCache(w)
	setJSONHeaders(w)
	globalSettingsMutex.RLock()
	settingsJSON, err := json.Marshal(&globalSettings)
	globalSettingsMutex.RUnlock()
	if err != nil {
		log.Printf("%s", err)
		return
	}
	fmt.Fprintf(w, "%s\n", settingsJSON)
}

func handleRegionGet(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	globalSettingsMutex.RLock()
	regionSelected := globalSettings.RegionSelected
	globalSettingsMutex.RUnlock()

	var region RegionInfo
	switch regionSelected {
	case 1:
		region.IsSet = true
		region.Region = "US"
	case 2:
		region.IsSet = true
		region.Region = "EU"
	default:
		region.IsSet = false
	}

	regionJSON, err := json.Marshal(&region)
	if err != nil {
		log.Printf("%s", err)
		return
	}
	fmt.Fprintf(w, "%s\n", regionJSON)
}

// AJAX call - /setRegion. receives via POST command, region selection.
func handleRegionSet(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes)
	var msg map[string]any
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&msg); err != nil {
		if err != io.EOF {
			log.Printf("handleRegionSet:error: %s\n", err.Error())
			http.Error(w, "invalid JSON", http.StatusBadRequest)
		}
		return
	}

	for key, val := range msg {
		switch key {
		case "Region":
			regionStr, ok := val.(string)
			if !ok {
				http.Error(w, "invalid Region value", http.StatusBadRequest)
				return
			}
			log.Printf("Region set to %s\n", regionStr)
			globalSettingsMutex.Lock()
			switch regionStr {
			case "US":
				globalSettings.RegionSelected = 1
			case "EU":
				globalSettings.RegionSelected = 2
			default:
				globalSettings.RegionSelected = 0
			}
			globalSettingsMutex.Unlock()
			changeRegionSettings()
		default:
			log.Printf("handleRegionSet:json: unrecognized key:%s\n", key)
		}
	}
}

// AJAX call - /setSettings. receives via POST command, any/all stratux.conf data.
func handleSettingsSetRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes)
	var msg map[string]any
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&msg); err != nil {
		if err != io.EOF {
			log.Printf("handleSettingsSetRequest:error: %s\n", err.Error())
			http.Error(w, "invalid JSON", http.StatusBadRequest)
		}
		return
	}

	reconfigureTracker := false
	reconfigureFancontrol := false
	notifyRadar := false
	var persistentLoggingVal *bool

	for key, val := range msg {
		switch key {
		case "DarkMode":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.DarkMode = v
				globalSettingsMutex.Unlock()
			}
		case "UAT_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.UAT_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "ES_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.ES_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "OGN_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGN_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "AIS_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.AIS_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "APRS_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.APRS_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "Ping_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.Ping_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "Pong_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.Pong_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "OGNI2CTXEnabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNI2CTXEnabled = v
				globalSettingsMutex.Unlock()
			}
		case "GPS_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.GPS_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "IMU_Sensor_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.IMU_Sensor_Enabled = v
				globalSettingsMutex.Unlock()
				if !v {
					closeIMUSensor()
				}
			}
		case "BMP_Sensor_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.BMP_Sensor_Enabled = v
				globalSettingsMutex.Unlock()
				if !v {
					closePressureSensor()
				}
			}
		case "DEBUG":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.DEBUG = v
				globalSettingsMutex.Unlock()
			}
		case "DisplayTrafficSource":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.DisplayTrafficSource = v
				globalSettingsMutex.Unlock()
			}
		case "ReplayLog":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				if v != globalSettings.ReplayLog {
					globalSettings.ReplayLog = v
				}
				globalSettingsMutex.Unlock()
			}
		case "TraceLog":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.TraceLog = v
				globalSettingsMutex.Unlock()
			}
		case "AHRSLog":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.AHRSLog = v
				globalSettingsMutex.Unlock()
			}
		case "PersistentLogging":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.PersistentLogging = v
				globalSettingsMutex.Unlock()
				valCopy := v
				persistentLoggingVal = &valCopy
			}
		case "IMUMapping":
			if rawSlice, ok := val.([]any); ok && len(rawSlice) == 2 {
				v0, ok0 := rawSlice[0].(float64)
				v1, ok1 := rawSlice[1].(float64)
				if ok0 && ok1 {
					newMapping := [2]int{int(v0), int(v1)}
					globalSettingsMutex.Lock()
					changed := globalSettings.IMUMapping != newMapping
					if changed {
						globalSettings.IMUMapping = newMapping
					}
					globalSettingsMutex.Unlock()
					if changed {
						closeIMUSensor() // Force a restart of the IMU reader
					}
				}
			}
		case "Dump1090Gain":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.Dump1090Gain = v
				globalSettingsMutex.Unlock()
			}
		case "PPM":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.PPM = int(v)
				globalSettingsMutex.Unlock()
			}
		case "AltitudeOffset":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.AltitudeOffset = int(v)
				globalSettingsMutex.Unlock()
			}
		case "RadarLimits":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.RadarLimits = int(v)
				globalSettingsMutex.Unlock()
				notifyRadar = true
			}
		case "RadarRange":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.RadarRange = int(v)
				globalSettingsMutex.Unlock()
				notifyRadar = true
			}
		case "Baud":
			if v, ok := val.(float64); ok {
				newBaud := int(v)
				var devsToClose []string
				globalSettingsMutex.Lock()
				if globalSettings.SerialOutputs != nil {
					for dev, serialOut := range globalSettings.SerialOutputs {
						if newBaud == serialOut.Baud { // Same baud rate. No change.
							continue
						}
						log.Printf("changing %s baud rate from %d to %d.\n", dev, serialOut.Baud, newBaud)
						serialOut.Baud = newBaud
						globalSettings.SerialOutputs[dev] = serialOut
						devsToClose = append(devsToClose, dev)
					}
				}
				globalSettingsMutex.Unlock()
				for _, dev := range devsToClose {
					closeSerial(dev)
				}
			}
		case "WatchList":
			if v, ok := val.(string); ok {
				globalSettingsMutex.Lock()
				globalSettings.WatchList = v
				globalSettingsMutex.Unlock()
			}
		case "GLimits":
			if v, ok := val.(string); ok {
				globalSettingsMutex.Lock()
				globalSettings.GLimits = v
				globalSettingsMutex.Unlock()
			}
		case "OwnshipModeS":
			if v, ok := val.(string); ok {
				codes := strings.Split(v, ",")
				codesFinal := make([]string, 0, len(codes))
				for _, code := range codes {
					code = strings.TrimSpace(code)
					// Expecting a hex string 1..6 characters (24 bits) long.
					if len(code) == 0 || len(code) > 6 {
						continue
					}
					// Pad string, must be 6 characters long.
					vals := strings.ToUpper(code)
					for len(vals) < 6 {
						vals = "0" + vals
					}
					hexn, err := hex.DecodeString(vals)
					if err != nil || len(hexn) != 3 { // Number not valid.
						if err != nil {
							log.Printf("handleSettingsSetRequest:OwnshipModeS: %s\n", err.Error())
						}
						continue
					}
					codesFinal = append(codesFinal, fmt.Sprintf("%02X%02X%02X", hexn[0], hexn[1], hexn[2]))
				}
				globalSettingsMutex.Lock()
				globalSettings.OwnshipModeS = strings.Join(codesFinal, ",")
				globalSettingsMutex.Unlock()
			}
		case "StaticIps":
			if ipsStr, ok := val.(string); ok {
				ips := strings.Fields(ipsStr)
				validIPs := make([]string, 0, len(ips))
				var invalid []string
				for _, ip := range ips {
					parsed := net.ParseIP(ip)
					if parsed == nil || parsed.To4() == nil {
						invalid = append(invalid, ip)
						continue
					}
					validIPs = append(validIPs, parsed.To4().String())
				}
				if len(invalid) > 0 {
					log.Printf("handleSettingsSetRequest:StaticIps: Invalid IPs: %s\n", strings.Join(invalid, ", "))
					continue
				}
				globalSettingsMutex.Lock()
				globalSettings.StaticIps = validIPs
				globalSettingsMutex.Unlock()
			}
		case "WiFiCountry":
			if v, ok := val.(string); ok {
				setWifiCountry(v)
			}
		case "WiFiSSID":
			if v, ok := val.(string); ok {
				setWifiSSID(v)
			}
		case "WiFiChannel":
			if v, ok := val.(float64); ok {
				setWifiChannel(int(v))
			}
		case "WiFiSecurityEnabled":
			if v, ok := val.(bool); ok {
				setWifiSecurityEnabled(v)
			}
		case "WiFiPassphrase":
			if v, ok := val.(string); ok {
				setWifiPassphrase(v)
			}
		case "WiFiIPAddress":
			if v, ok := val.(string); ok {
				setWifiIPAddress(v)
			}
		case "WiFiMode":
			if v, ok := val.(float64); ok {
				setWiFiMode(int(v))
			}
		case "WiFiDirectPin":
			if v, ok := val.(string); ok {
				setWifiDirectPin(v)
			}
		case "WiFiClientNetworks":
			if rawList, ok := val.([]any); ok {
				networks := make([]wifiClientNetwork, 0, len(rawList))
				for _, rawNetwork := range rawList {
					network, ok := rawNetwork.(map[string]any)
					if !ok {
						continue
					}
					ssid, okSSID := network["SSID"].(string)
					if !okSSID {
						continue
					}
					pass, _ := network["Password"].(string)
					networks = append(networks, wifiClientNetwork{SSID: ssid, Password: pass})
				}
				setWifiClientNetworks(networks)
			}
		case "WiFiInternetPassThroughEnabled":
			if v, ok := val.(bool); ok {
				setWifiInternetPassthroughEnabled(v)
			}
		case "EstimateBearinglessDist":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.EstimateBearinglessDist = v
				globalSettingsMutex.Unlock()
			}
		case "OGNAddrType":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNAddrType = int(v)
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "OGNAddr":
			if v, ok := val.(string); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNAddr = v
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "OGNAcftType":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNAcftType = int(v)
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "OGNPilot":
			if v, ok := val.(string); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNPilot = v
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "OGNReg":
			if v, ok := val.(string); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNReg = v
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "OGNTxPower":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.OGNTxPower = int(v)
				globalSettingsMutex.Unlock()
				reconfigureTracker = true
			}
		case "PWMDutyMin":
			if v, ok := val.(float64); ok {
				globalSettingsMutex.Lock()
				globalSettings.PWMDutyMin = int(v)
				globalSettingsMutex.Unlock()
				reconfigureFancontrol = true
			}
		case "AutoGain_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.AutoGain_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "RemoteID_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.RemoteID_Enabled = v
				globalSettingsMutex.Unlock()
			}
		case "CPA_Alerts_Enabled":
			if v, ok := val.(bool); ok {
				globalSettingsMutex.Lock()
				globalSettings.CPA_Alerts_Enabled = v
				globalSettingsMutex.Unlock()
			}
		default:
			log.Printf("handleSettingsSetRequest:json: unrecognized key:%s\n", key)
		}
	}

	if persistentLoggingVal != nil {
		setPersistentLogging(*persistentLoggingVal)
	}
	if notifyRadar {
		radarUpdate.SendJSON(sanitizedSettingsCopy())
	}

	saveSettings()
	applyNetworkSettings(false, false)
	if reconfigureTracker && detectedTracker != nil {
		writeTrackerConfigFromSettings()
	}
	if reconfigureFancontrol {
		_ = exec.Command("killall", "-SIGUSR1", "fancontrol").Run()
	}

	// Return the latest settings.
	globalSettingsMutex.RLock()
	settingsJSON, _ := json.Marshal(&globalSettings)
	globalSettingsMutex.RUnlock()
	fmt.Fprintf(w, "%s\n", settingsJSON)
}

func setPersistentLogging(persistent bool) {
	if persistent {
		overlayctl("disable")
	} else {
		overlayctl("enable")
	}
}

func handleShutdownRequest(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	syscall.Sync()
	_ = exec.Command("systemctl", "poweroff").Run()
}

func doReboot() {
	syscall.Sync()
	_ = exec.Command("systemctl", "reboot").Run()
}

func handleDeleteLogFile(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	log.Println("handleDeleteLogFile called!!!")
	clearDebugLogFile()
}

func handleDeleteAHRSLogFiles(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	files, err := os.ReadDir("/var/log")
	if err != nil {
		http.Error(w, fmt.Sprintf("error deleting AHRS logs: %s", err), http.StatusNotFound)
		return
	}

	// Close any active analysis logger cleanly before unlinking log files.
	closeAnalysisLogger()

	for _, f := range files {
		fn := f.Name()
		if v, _ := filepath.Match("sensors_*.csv", fn); v {
			_ = os.Remove(filepath.Join("/var/log", fn))
			log.Printf("Deleting AHRS log file %s\n", fn)
		}
	}
}

func handleDevelModeToggle(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	log.Printf("handleDevelModeToggle called!!!\n")
	globalSettingsMutex.Lock()
	globalSettings.DeveloperMode = true
	globalSettingsMutex.Unlock()
	saveSettings()
}

func handleRestartRequest(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	log.Printf("handleRestartRequest called\n")
	go doRestartApp()
}

func handleRebootRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	if !requirePostSameOrigin(w, r) {
		return
	}
	go delayReboot()
}

func handleOrientAHRS(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}

	action := make([]byte, 1)
	if _, err := io.ReadFull(r.Body, action); err != nil {
		log.Println("AHRS Error: handleOrientAHRS received invalid request")
		http.Error(w, "orientation received invalid request", http.StatusBadRequest)
		return
	}

	switch action[0] {
	case 'f': // Set sensor "forward" direction (toward nose of airplane).
		f, err := getMinAccelDirection()
		if err != nil {
			log.Printf("AHRS Error: sensor orientation: couldn't read accelerometer: %s\n", err)
			http.Error(w, fmt.Sprintf("couldn't read accelerometer: %s\n", err), http.StatusBadRequest)
			return
		}
		log.Printf("AHRS Info: sensor orientation success! forward axis is %d\n", f)
		globalSettingsMutex.Lock()
		globalSettings.IMUMapping = [2]int{f, 0}
		globalSettingsMutex.Unlock()
	case 'd': // Set sensor "up" direction (toward top of airplane).
		globalSettingsMutex.Lock()
		globalSettings.SensorQuaternion = [4]float64{0, 0, 0, 0}
		globalSettingsMutex.Unlock()
		saveSettings()
		closeIMUSensor() // restart the processes depending on the orientation
		ResetAHRSGLoad()
		time.Sleep(2000 * time.Millisecond)
	}
}

func handleCageAHRS(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}
	CageAHRS()
}

func handleCalibrateAHRS(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}
	CalibrateAHRS()
}

func handleResetGMeter(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}
	ResetAHRSGLoad()
}

func doRestartApp() {
	time.Sleep(1 * time.Second)
	syscall.Sync()
	out, err := exec.Command("/bin/systemctl", "restart", "stratux").Output()
	if err != nil {
		log.Printf("restart error: %s\n%s", err.Error(), out)
	} else {
		log.Printf("restart: %s\n", out)
	}
}

// AJAX call - /getClients. Responds with all connected clients.
func handleClientsGetRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)
	netMutex.Lock()
	clientsJSON, _ := json.Marshal(&clientConnections)
	netMutex.Unlock()
	fmt.Fprintf(w, "%s\n", clientsJSON)
}

func delayReboot() {
	time.Sleep(1 * time.Second)
	doReboot()
}

func handleDownloadLogRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", "attachment; filename=stratux.log")
	http.ServeFile(w, r, "/var/log/stratux.log")
}

func handleDownloadAHRSLogsRequest(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir("/var/log")
	if err != nil {
		http.Error(w, fmt.Sprintf("error zipping AHRS logs: %s", err), http.StatusNotFound)
		return
	}

	// Set headers BEFORE creating/writing to zip.NewWriter(w) so they are actually sent.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=ahrs_logs.zip")

	z := zip.NewWriter(w)
	defer z.Close()

	for _, entry := range entries {
		fn := entry.Name()
		v1, _ := filepath.Match("sensors_*.csv", fn)
		v2, _ := filepath.Match("stratux.log", fn)
		if !(v1 || v2) {
			continue
		}

		if err := addFileToZip(z, filepath.Join("/var/log", fn), entry); err != nil {
			log.Printf("error zipping AHRS log %s: %s\n", fn, err.Error())
			return
		}
	}
}

func addFileToZip(z *zip.Writer, fullPath string, entry os.DirEntry) error {
	finfo, err := entry.Info()
	if err != nil {
		return err
	}
	unzippedFile, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer unzippedFile.Close()

	fh, err := zip.FileInfoHeader(finfo)
	if err != nil {
		return err
	}
	zippedFile, err := z.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = io.Copy(zippedFile, unzippedFile)
	return err
}

func handleDownloadDBRequest(w http.ResponseWriter, r *http.Request) {
	dbPath := dataLogFilef
	if dbPath == "" {
		dbPath = "/var/log/stratux.sqlite"
	}
	w.Header().Set("Content-Disposition", "attachment; filename=stratux.sqlite")
	http.ServeFile(w, r, dbPath)
}

func isValidUpdateFilename(fname string) bool {
	if fname == "" || filepath.Base(fname) != fname || strings.Contains(fname, "..") {
		return false
	}
	if !updateFilenameRegex.MatchString(fname) {
		return false
	}
	return strings.HasSuffix(fname, ".deb") || strings.HasSuffix(fname, ".sh")
}

// Upload an update file.
func handleUpdatePostRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if !requirePostSameOrigin(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
		http.Error(w, "invalid multipart upload", http.StatusBadRequest)
		return
	}

	var base_dir string
	if common.IsRunningAsRoot() {
		base_dir = "/boot/firmware/StratuxUpdates"
	} else {
		base_dir = "."
		log.Printf("not running as root, using base_dir of %s", base_dir)
	}

	// Ensure the upload directory exists. The SD card flow relies on the user
	// creating this path manually; the web flow has no such step, so create it
	// here to avoid a silent failure when os.OpenFile cannot create the file.
	if err := os.MkdirAll(base_dir, 0755); err != nil {
		log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
		http.Error(w, "failed to prepare update directory", http.StatusInternalServerError)
		return
	}

	var temp_filename string
	var upload_filename string
	uploaded := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
			http.Error(w, "failed reading upload", http.StatusBadRequest)
			return
		}
		if part == nil {
			break
		}

		if part.FormName() != "update_file" {
			_ = part.Close()
			continue
		}

		fname := part.FileName()
		if !isValidUpdateFilename(fname) {
			_ = part.Close()
			log.Printf("Update rejected from %s: invalid filename %q\n", r.RemoteAddr, fname)
			http.Error(w, "invalid update filename", http.StatusBadRequest)
			return
		}

		temp_filename = filepath.Join(base_dir, "TMP_"+fname)
		upload_filename = filepath.Join(base_dir, fname)

		fi, err := os.OpenFile(temp_filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			_ = part.Close()
			log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
			http.Error(w, "failed to create update file", http.StatusInternalServerError)
			return
		}

		_, copyErr := io.Copy(fi, part)
		_ = part.Close()
		syncErr := fi.Sync()
		closeErr := fi.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			_ = os.Remove(temp_filename)
			log.Printf("Update failed from %s (copy=%v, sync=%v, close=%v).\n", r.RemoteAddr, copyErr, syncErr, closeErr)
			http.Error(w, "failed writing update file", http.StatusInternalServerError)
			return
		}

		uploaded = true
		break
	}

	if !uploaded {
		log.Printf("Update failed from %s: missing update_file part.\n", r.RemoteAddr)
		http.Error(w, "missing update_file", http.StatusBadRequest)
		return
	}

	if err := os.Rename(temp_filename, upload_filename); err != nil {
		_ = os.Remove(temp_filename)
		log.Printf("Update rename failed from %s (%s).\n", r.RemoteAddr, err.Error())
		http.Error(w, "failed finalizing update file", http.StatusInternalServerError)
		return
	}

	log.Printf("%s uploaded %s for update.\n", r.RemoteAddr, upload_filename)
	// Successful update upload. stratux-pre-start.sh handles the rest on next boot.
	go delayReboot()
}

// Upload an update file for Pong
func handlePongUpdatePostRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if !requirePostSameOrigin(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPongUploadBytes)
	log.Printf("request: %s\n", r.URL.Path)
	err := r.ParseMultipartForm(8 << 20)
	if err != nil {
		log.Printf("Step 1 Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
		http.Error(w, "failed parsing multipart form", http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("pong_update_file")
	if err != nil {
		log.Printf("FormFile returned error %s\n", err.Error())
		http.Error(w, "missing pong_update_file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fi, err := os.OpenFile("/tmp/update_pong.zip", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("Cannot open file for saving (%s)\n", err.Error())
		http.Error(w, "failed saving update", http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(fi, file)
	syncErr := fi.Sync()
	closeErr := fi.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		log.Printf("Could not copy file (copy=%v, sync=%v, close=%v)\n", copyErr, syncErr, closeErr)
		http.Error(w, "failed writing update", http.StatusInternalServerError)
		return
	}

	log.Printf("Set update mode flag to signal Pong to run the update\n")
	pongSetUpdateMode()
}

func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func setJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func setPublicJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func defaultServer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=360") // 5 min, so that if user installs update, he will revalidate soon enough
	http.FileServer(http.Dir(STRATUX_WWW_DIR)).ServeHTTP(w, r)
}

func handleroPartitionRebuild(w http.ResponseWriter, r *http.Request) {
	if !requirePostSameOrigin(w, r) {
		return
	}
	out, err := exec.Command("/usr/sbin/rebuild_ro_part.sh").Output()

	if err != nil {
		addSingleSystemErrorf("partition-rebuild", "Rebuild RO Partition error: %s", err.Error())
	} else {
		addSingleSystemErrorf("partition-rebuild", "Rebuild RO Partition success: %s", out)
	}
}

// https://gist.github.com/alexisrobert/982674.
// Copyright (c) 2010-2014 Alexis ROBERT <alexis.robert@gmail.com>.
const dirlisting_tpl = `<?xml version="1.0" encoding="iso-8859-1"?>
<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "http://www.w3.org/TR/xhtml11/DTD/xhtml11.dtd">
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="en">
<!-- Modified from lighttpd directory listing -->
<head>
<title>Index of {{.Name}}</title>
<style type="text/css">
a, a:active {text-decoration: none; color: blue;}
a:visited {color: #48468F;}
a:hover, a:focus {text-decoration: underline; color: red;}
body {background-color: #F5F5F5;}
h2 {margin-bottom: 12px;}
table {margin-left: 12px;}
th, td { font: 90% monospace; text-align: left;}
th { font-weight: bold; padding-right: 14px; padding-bottom: 3px;}
td {padding-right: 14px;}
td.s, th.s {text-align: right;}
div.list { background-color: white; border-top: 1px solid #646464; border-bottom: 1px solid #646464; padding-top: 10px; padding-bottom: 14px;}
div.foot { font: 90% monospace; color: #787878; padding-top: 4px;}
</style>
</head>
<body>
<h2>Index of {{.Name}}</h2>
<div class="list">
<table summary="Directory Listing" cellpadding="0" cellspacing="0">
<thead><tr><th class="n">Name</th><th>Last Modified</th><th>Size (bytes)</th><th class="dl">Options</th></tr></thead>
<tbody>
{{range .Children_files}}
<tr><td class="n"><a href="/logs/{{.Path}}">{{.Name}}</a></td><td>{{.Mtime}}</td><td>{{.Size}}</td><td class="dl"><a href="/logs/{{.Path}}">Download</a></td></tr>
{{end}}
</tbody>
</table>
</div>
<div class="foot">{{.ServerUA}}</div>
</body>
</html>`

var dirlistingTemplate = template.Must(template.New("dirlisting").Parse(dirlisting_tpl))

type fileInfo struct {
	Name  string
	Path  string
	Mtime string
	Size  string
}

// Manages directory listings
type dirlisting struct {
	Name           string
	Children_files []fileInfo
	ServerUA       string
}

// FIXME: This needs to be switched to show a "sessions log" from the sqlite database.
func viewLogs(w http.ResponseWriter, r *http.Request) {
	const logBaseDir = "/var/log"
	evalBase, err := filepath.EvalSymlinks(logBaseDir)
	if err != nil {
		evalBase = filepath.Clean(logBaseDir)
	}

	urlpath := strings.TrimPrefix(r.URL.Path, "/logs/")
	candidate := filepath.Join(logBaseDir, filepath.FromSlash(urlpath))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if resolved != evalBase && !strings.HasPrefix(resolved, evalBase+string(os.PathSeparator)) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	finfo, err := os.Stat(resolved)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	if !finfo.IsDir() {
		http.ServeFile(w, r, resolved)
		return
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Failed to read directory", http.StatusInternalServerError)
		return
	}

	relPrefix := strings.Trim(urlpath, "/")
	fi := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == 0 || name[0] == '.' {
			continue // Remove hidden files from listing
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		childPath := name
		if relPrefix != "" {
			childPath = relPrefix + "/" + name
		}
		mtime := info.ModTime().Format("2006-Jan-02 15:04:05")
		if entry.IsDir() {
			fi = append(fi, fileInfo{Name: name + "/", Path: childPath, Mtime: mtime, Size: ""})
		} else {
			sz := humanize.Comma(info.Size())
			fi = append(fi, fileInfo{Name: name, Path: childPath, Mtime: mtime, Size: sz})
		}
	}

	data := dirlisting{
		Name:           r.URL.Path,
		ServerUA:       "Stratux " + stratuxVersion + "/" + stratuxBuild,
		Children_files: fi,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dirlistingTemplate.Execute(w, data); err != nil {
		log.Printf("viewLogs() error: %s\n", err.Error())
	}
}

func isValidTileFilename(file string) bool {
	if file == "" || filepath.Base(file) != file {
		return false
	}
	if strings.ContainsAny(file, `/\?#`+"\x00") || strings.Contains(file, "..") {
		return false
	}
	return strings.HasSuffix(file, ".mbtiles") || strings.HasSuffix(file, ".db")
}

func resolveMapDataPath(file string) (string, error) {
	if !isValidTileFilename(file) {
		return "", fmt.Errorf("invalid tileset filename")
	}
	baseDir := filepath.Clean(filepath.Join(STRATUX_HOME, "mapdata"))
	fullPath := filepath.Clean(filepath.Join(baseDir, file))
	if !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("tileset path escapes mapdata directory")
	}
	return fullPath, nil
}

func connectMbTilesArchive(path string) (*sql.DB, map[string]string, error) {
	baseDir := filepath.Clean(filepath.Join(STRATUX_HOME, "mapdata"))
	cleanPath := filepath.Clean(path)
	if strings.ContainsAny(cleanPath, "?#\x00") || !strings.HasPrefix(cleanPath, baseDir+string(os.PathSeparator)) {
		return nil, nil, fmt.Errorf("invalid tileset path")
	}

	mbtileCacheLock.Lock()
	defer mbtileCacheLock.Unlock()
	if cached, ok := mbtileConnectionCache[cleanPath]; ok {
		if !cached.IsOutdated() {
			return cached.Conn, cached.Metadata, nil
		}
		log.Printf("Reloading MBTiles %s", cleanPath)
		if cached.Conn != nil {
			_ = cached.Conn.Close()
		}
		delete(mbtileConnectionCache, cleanPath)
	}

	// Verify file exists before calling sql.Open (which succeeds lazily for SQLite).
	if _, err := os.Stat(cleanPath); err != nil {
		return nil, nil, err
	}

	conn, err := sql.Open("sqlite3", "file:"+cleanPath+"?mode=ro")
	if err != nil {
		return nil, nil, err
	}
	cacheEntry := NewMbTileConnectionCacheEntry(cleanPath, conn)
	if cacheEntry == nil {
		_ = conn.Close()
		return nil, nil, os.ErrNotExist
	}
	cacheEntry.Metadata = readMbTilesMetadata(cleanPath, conn)
	mbtileConnectionCache[cleanPath] = *cacheEntry
	return conn, cacheEntry.Metadata, nil
}

func tileToDegree(z, x, y int) (lon, lat float64) {
	// osm-like schema:
	y = (1 << z) - y - 1
	n := math.Pi - 2.0*math.Pi*float64(y)/math.Exp2(float64(z))
	lat = 180.0 / math.Pi * math.Atan(0.5*(math.Exp(n)-math.Exp(-n)))
	lon = float64(x)/math.Exp2(float64(z))*360.0 - 180.0
	return lon, lat
}

func readMbTilesMetadata(fname string, db *sql.DB) map[string]string {
	rows, err := db.Query(`SELECT name, value FROM metadata 
		UNION SELECT 'minzoom', min(zoom_level) FROM tiles WHERE NOT EXISTS (SELECT * FROM metadata WHERE name='minzoom' and value is not null and value != '')
		UNION SELECT 'maxzoom', max(zoom_level) FROM tiles WHERE NOT EXISTS (SELECT * FROM metadata WHERE name='maxzoom' and value is not null and value != '')`)
	if err != nil {
		log.Printf("SQLite read error %s: %s", fname, err.Error())
		return nil
	}
	defer rows.Close()
	meta := make(map[string]string)
	for rows.Next() {
		var name, val string
		_ = rows.Scan(&name, &val)
		if len(val) > 0 {
			meta[name] = val
		}
	}
	// determine extent of layer if not given.. Openlayers kinda needs this, or it can happen that it tries to do
	// a billion requests to down-scale high-res pngs that aren't even there (i.e. all 404s)
	if _, ok := meta["bounds"]; !ok {
		maxZoomInt, _ := strconv.ParseInt(meta["maxzoom"], 10, 32)
		rows2, err := db.Query("SELECT min(tile_column), min(tile_row), max(tile_column), max(tile_row) FROM tiles WHERE zoom_level=?", maxZoomInt)
		if err != nil {
			log.Printf("SQLite read error %s: %s", fname, err.Error())
			return meta
		}
		defer rows2.Close()
		if rows2.Next() {
			var xmin, ymin, xmax, ymax int
			if err := rows2.Scan(&xmin, &ymin, &xmax, &ymax); err == nil {
				lonmin, latmin := tileToDegree(int(maxZoomInt), xmin, ymin)
				lonmax, latmax := tileToDegree(int(maxZoomInt), xmax+1, ymax+1)
				meta["bounds"] = fmt.Sprintf("%f,%f,%f,%f", lonmin, latmin, lonmax, latmax)
			}
		}
	}

	// check if it is vectortiles and we have a style, then add the URL to metadata...
	if format, ok := meta["format"]; ok && format == "pbf" {
		_, file := filepath.Split(fname)
		if _, err := os.Stat(filepath.Join(STRATUX_HOME, "mapdata", "styles", file, "style.json")); err == nil {
			// We found a style!
			meta["stratux_style_url"] = "/mapdata/styles/" + file + "/style.json"
		}
	}
	return meta
}

// Scans mapdata dir for all .db and .mbtiles files and returns json representation of all metadata values
func handleTilesets(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(filepath.Join(STRATUX_HOME, "mapdata"))
	if err != nil {
		log.Printf("handleTilesets() error: %s\n", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make(map[string]map[string]string)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if isValidTileFilename(f.Name()) {
			fullPath, err := resolveMapDataPath(f.Name())
			if err != nil {
				continue
			}
			_, meta, err := connectMbTilesArchive(fullPath)
			if err != nil {
				log.Printf("SQLite open %s failed: %s", f.Name(), err.Error())
				continue
			}
			result[f.Name()] = meta
		}
	}
	setJSONHeaders(w)
	resJson, _ := json.Marshal(result)
	_, _ = w.Write(resJson)
}

func loadTile(fname string, z, x, y int) ([]byte, error) {
	fullPath, err := resolveMapDataPath(fname)
	if err != nil {
		return nil, err
	}
	db, meta, err := connectMbTilesArchive(fullPath)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?", z, x, y)
	if err != nil {
		log.Printf("Failed to query mbtiles: %s", err.Error())
		return nil, nil
	}

	defer rows.Close()
	for rows.Next() {
		var res []byte
		if err := rows.Scan(&res); err != nil {
			return nil, nil
		}
		// sometimes pbfs are gzipped...
		if format, ok := meta["format"]; ok && format == "pbf" && len(res) >= 2 && res[0] == 0x1f && res[1] == 0x8b {
			reader := bytes.NewReader(res)
			gzreader, err := gzip.NewReader(reader)
			if err != nil {
				log.Printf("Failed to open gzipped PBF data: %s", err.Error())
				return nil, nil
			}
			defer gzreader.Close()
			unzipped, err := io.ReadAll(io.LimitReader(gzreader, maxTileUnzippedBytes))
			if err != nil {
				log.Printf("Failed to unzip gzipped PBF data: %s", err.Error())
				return nil, nil
			}
			res = unzipped
		}
		return res, nil
	}
	return nil, nil
}

func handleTile(w http.ResponseWriter, r *http.Request) {
	// Parse cleaned r.URL.Path (never raw r.RequestURI).
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	// Expected: tiles/<file>/<z>/<x>/<y>.<ext>
	if len(parts) < 5 {
		http.Error(w, "Invalid tile path", http.StatusBadRequest)
		return
	}
	idx := len(parts) - 1
	y, err := strconv.Atoi(strings.Split(parts[idx], ".")[0])
	if err != nil {
		http.Error(w, "Failed to parse y", http.StatusBadRequest)
		return
	}
	idx--
	x, err := strconv.Atoi(parts[idx])
	if err != nil {
		http.Error(w, "Failed to parse x", http.StatusBadRequest)
		return
	}
	idx--
	z, err := strconv.Atoi(parts[idx])
	if err != nil {
		http.Error(w, "Failed to parse z", http.StatusBadRequest)
		return
	}
	idx--
	file := parts[idx]
	if !isValidTileFilename(file) {
		http.Error(w, "Invalid tileset filename", http.StatusBadRequest)
		return
	}
	tileData, err := loadTile(file, z, x, y)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Tileset not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	} else if tileData == nil {
		http.Error(w, "Tile not found", http.StatusNotFound)
	} else {
		_, _ = w.Write(tileData)
	}
}

// handleGraphicalWeather serves GET /getGraphicalWeather returning decoded FIS-B
// graphical weather products (AIRMET, SIGMET, G-AIRMET, SUA, TFR, etc.) as a JSON array.
func handleGraphicalWeather(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	products := getGraphicalProductsSnapshot()
	if products == nil {
		products = []uatparse.GraphicalProduct{}
	}
	out, err := json.Marshal(products)
	if err != nil {
		log.Printf("handleGraphicalWeather: marshal error: %s\n", err.Error())
		http.Error(w, "failed to marshal graphical weather", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

type updateCheckResponse struct {
	CurrentVersion  string `json:"CurrentVersion"`
	LatestVersion   string `json:"LatestVersion"`
	UpdateAvailable bool   `json:"UpdateAvailable"`
	ReleaseURL      string `json:"ReleaseURL"`
	OTAStatus       string `json:"OTAStatus"`
}

func readOTAStatus() string {
	for _, p := range otaStatusPaths {
		if data, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return "none"
}

func parseSemverTuple(ver string) ([]int, bool) {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(ver, "v"), "V"))
	if v == "" {
		return nil, false
	}
	if idx := strings.IndexAny(v, "-+ "); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		// Extract leading numeric prefix (e.g. "6r1" -> "6").
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return nil, false
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return nil, false
		}
		nums = append(nums, n)
	}
	return nums, len(nums) > 0
}

func isVersionNewer(current, latest string) bool {
	cur := strings.TrimSpace(current)
	lat := strings.TrimSpace(latest)
	if lat == "" || cur == "" {
		return false
	}
	if strings.EqualFold(strings.TrimPrefix(cur, "v"), strings.TrimPrefix(lat, "v")) {
		return false
	}
	curParsed := parseStratuxVersion(cur)
	latParsed := parseStratuxVersion(lat)
	if curParsed != ([4]byte{}) && latParsed != ([4]byte{}) {
		for i := 0; i < 4; i++ {
			if latParsed[i] != curParsed[i] {
				return latParsed[i] > curParsed[i]
			}
		}
		return false
	}
	curNums, curOK := parseSemverTuple(cur)
	latNums, latOK := parseSemverTuple(lat)
	if !curOK || !latOK {
		return false
	}
	maxLen := len(curNums)
	if len(latNums) > maxLen {
		maxLen = len(latNums)
	}
	for i := 0; i < maxLen; i++ {
		c, l := 0, 0
		if i < len(curNums) {
			c = curNums[i]
		}
		if i < len(latNums) {
			l = latNums[i]
		}
		if l != c {
			return l > c
		}
	}
	return false
}

// handleCheckUpdate serves GET /checkUpdate returning current version, OTA rollback/commit
// status, and optional latest GitHub release information (failing gracefully when offline).
func handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	resp := updateCheckResponse{
		CurrentVersion:  stratuxVersion,
		LatestVersion:   "",
		UpdateAvailable: false,
		ReleaseURL:      "",
		OTAStatus:       readOTAStatus(),
	}

	const defaultGitHubURL = "https://api.github.com/repos/stratux/stratux/releases/latest"
	skipNetwork := githubLatestReleaseURL == "" ||
		r.URL.Query().Get("offline") == "1" ||
		(githubLatestReleaseURL == defaultGitHubURL && flag.Lookup("test.v") != nil)

	if !skipNetwork {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubLatestReleaseURL, nil)
		if err == nil {
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("User-Agent", "Stratux")
			client := &http.Client{Timeout: 4 * time.Second}
			httpResp, reqErr := client.Do(req)
			if reqErr == nil {
				defer httpResp.Body.Close()
				if httpResp.StatusCode == http.StatusOK {
					var ghRelease struct {
						TagName string `json:"tag_name"`
						HTMLURL string `json:"html_url"`
					}
					if decErr := json.NewDecoder(io.LimitReader(httpResp.Body, 1<<20)).Decode(&ghRelease); decErr == nil {
						resp.LatestVersion = strings.TrimSpace(ghRelease.TagName)
						resp.ReleaseURL = strings.TrimSpace(ghRelease.HTMLURL)
						resp.UpdateAvailable = isVersionNewer(resp.CurrentVersion, resp.LatestVersion)
					}
				}
			}
		}
	}

	out, err := json.Marshal(&resp)
	if err != nil {
		http.Error(w, "failed to marshal update check", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

type firmwareListResponse struct {
	OGN    []string `json:"OGN"`
	SoftRF []string `json:"SoftRF"`
}

func listFirmwareZips(dir string) []string {
	result := make([]string, 0)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Base(name) != name || strings.Contains(name, "..") {
			continue
		}
		if !strings.HasSuffix(name, ".zip") || !updateFilenameRegex.MatchString(name) {
			continue
		}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// handleGetFirmwareList serves GET /getFirmwareList returning available OGN and SoftRF .zip images.
func handleGetFirmwareList(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	resp := firmwareListResponse{
		OGN:    listFirmwareZips(filepath.Join(firmwareBaseDir, "ogn")),
		SoftRF: listFirmwareZips(filepath.Join(firmwareBaseDir, "softrf")),
	}
	out, err := json.Marshal(&resp)
	if err != nil {
		http.Error(w, "failed to marshal firmware list", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

type flashFirmwareRequest struct {
	Target   string `json:"Target"`
	Firmware string `json:"Firmware"`
}

type flashFirmwareResponse struct {
	Success bool   `json:"Success"`
	Output  string `json:"Output"`
	Error   string `json:"Error"`
}

func isExistingFirmwareZip(dir, fw string) bool {
	if fw == "" || filepath.Base(fw) != fw || strings.Contains(fw, "..") {
		return false
	}
	if !strings.HasSuffix(fw, ".zip") || !updateFilenameRegex.MatchString(fw) {
		return false
	}
	// Exact match against directory listing prevents path traversal and symlink escapes.
	for _, allowed := range listFirmwareZips(dir) {
		if allowed == fw {
			cleanDir := filepath.Clean(dir)
			fullPath := filepath.Clean(filepath.Join(cleanDir, fw))
			if !strings.HasPrefix(fullPath, cleanDir+string(os.PathSeparator)) {
				return false
			}
			info, err := os.Stat(fullPath)
			return err == nil && !info.IsDir()
		}
	}
	return false
}

// handleFlashFirmware serves POST /flashFirmware to flash validated OGN or SoftRF firmware archives.
func handleFlashFirmware(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	if !requirePostSameOrigin(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes)
	var req flashFirmwareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return
	}

	target := strings.ToLower(strings.TrimSpace(req.Target))
	if target != "ogn" && target != "softrf" {
		http.Error(w, "invalid target; expected ogn or softrf", http.StatusBadRequest)
		return
	}

	targetDir := filepath.Join(firmwareBaseDir, target)
	if !isExistingFirmwareZip(targetDir, req.Firmware) {
		http.Error(w, "invalid or unknown firmware archive", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if target == "ogn" {
		scriptPath := filepath.Join(firmwareBaseDir, "ogn", "install-ogntracker-firmware-pi.sh")
		fwArg := strings.TrimSuffix(req.Firmware, ".zip")
		cmd = exec.CommandContext(ctx, scriptPath, fwArg)
	} else {
		scriptPath := filepath.Join(firmwareBaseDir, "softrf", "install-softrf-firmware-pi.sh")
		cmd = exec.CommandContext(ctx, scriptPath)
	}

	outBytes, err := cmd.CombinedOutput()
	resp := flashFirmwareResponse{
		Success: err == nil,
		Output:  string(outBytes),
	}
	if err != nil {
		resp.Error = err.Error()
	}

	encoded, marshalErr := json.Marshal(&resp)
	if marshalErr != nil {
		http.Error(w, "failed to marshal flash response", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", encoded)
}

func managementInterface() {
	if weatherUpdate == nil {
		weatherUpdate = NewUIBroadcaster()
	}
	if trafficUpdate == nil {
		trafficUpdate = NewUIBroadcaster()
	}
	if radarUpdate == nil {
		radarUpdate = NewUIBroadcaster()
	}
	if situationUpdate == nil {
		situationUpdate = NewUIBroadcaster()
	}
	if weatherRawUpdate == nil {
		weatherRawUpdate = NewUIBroadcaster()
	}
	if gdl90Update == nil {
		gdl90Update = NewUIBroadcaster()
	}

	http.HandleFunc("/", defaultServer)
	http.Handle("/mapdata/styles/", http.StripPrefix("/mapdata/styles/", http.FileServer(http.Dir(STRATUX_HOME+"/mapdata/styles"))))
	http.HandleFunc("/logs/", viewLogs)

	http.HandleFunc("/gdl90",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleGDL90WS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/status",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleStatusWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/situation",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleSituationWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/weather",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleWeatherWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/traffic",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleTrafficWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/radar",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleRadarWS)}
			s.ServeHTTP(w, req)
		})

	http.HandleFunc("/jsonio",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleJsonIo)}
			s.ServeHTTP(w, req)
		})

	http.HandleFunc("/getStatus", handleStatusRequest)
	http.HandleFunc("/getSituation", handleSituationRequest)
	http.HandleFunc("/getTowers", handleTowersRequest)
	http.HandleFunc("/getSatellites", handleSatellitesRequest)
	http.HandleFunc("/getSettings", handleSettingsGetRequest)
	http.HandleFunc("/getRegion", handleRegionGet)
	http.HandleFunc("/setRegion", handleRegionSet)
	http.HandleFunc("/setSettings", handleSettingsSetRequest)
	http.HandleFunc("/restart", handleRestartRequest)
	http.HandleFunc("/shutdown", handleShutdownRequest)
	http.HandleFunc("/reboot", handleRebootRequest)
	http.HandleFunc("/getClients", handleClientsGetRequest)
	http.HandleFunc("/updateUpload", handleUpdatePostRequest)
	http.HandleFunc("/updatePong", handlePongUpdatePostRequest)
	http.HandleFunc("/roPartitionRebuild", handleroPartitionRebuild)
	http.HandleFunc("/develmodetoggle", handleDevelModeToggle)
	http.HandleFunc("/orientAHRS", handleOrientAHRS)
	http.HandleFunc("/calibrateAHRS", handleCalibrateAHRS)
	http.HandleFunc("/cageAHRS", handleCageAHRS)
	http.HandleFunc("/resetGMeter", handleResetGMeter)
	http.HandleFunc("/deletelogfile", handleDeleteLogFile)
	http.HandleFunc("/downloadlog", handleDownloadLogRequest)
	http.HandleFunc("/deleteahrslogfiles", handleDeleteAHRSLogFiles)
	http.HandleFunc("/downloadahrslogs", handleDownloadAHRSLogsRequest)
	http.HandleFunc("/downloaddb", handleDownloadDBRequest)
	http.HandleFunc("/tiles/tilesets", handleTilesets)
	http.HandleFunc("/tiles/", handleTile)
	http.HandleFunc("/getHealth", handleHealthRequest)
	http.HandleFunc("/getGraphicalWeather", handleGraphicalWeather)
	http.HandleFunc("/getFlights", handleGetFlights)
	http.HandleFunc("/getFlightTrack", handleGetFlightTrack)
	http.HandleFunc("/exportFlight", handleExportFlight)
	http.HandleFunc("/checkUpdate", handleCheckUpdate)
	http.HandleFunc("/getFirmwareList", handleGetFirmwareList)
	http.HandleFunc("/flashFirmware", handleFlashFirmware)

	addr := fmt.Sprintf(":%d", ManagementAddr)
	log.Printf("web configuration console on port %s", addr)
	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("managementInterface ListenAndServe: %s\n", err.Error())
	}
}
