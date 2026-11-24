/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	---
	gps.go: GPS functions, GPS init, AHRS status messages, other external sensor monitoring.
	compile and install: clear && make www && make stratuxrun && mv stratuxrun /opt/stratux/bin/ && stxrestart
*/

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratux/serial"
	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/gpsnmea"
)

const (
	SAT_TYPE_UNKNOWN = 0  // default type
	SAT_TYPE_GPS     = 1  // GPxxx; NMEA IDs 1-32
	SAT_TYPE_GLONASS = 2  // GLxxx; NMEA IDs 65-96
	SAT_TYPE_GALILEO = 3  // GAxxx; NMEA IDs 301-336
	SAT_TYPE_BEIDOU  = 4  // GBxxx; NMEA IDs 401-463
	SAT_TYPE_QZSS    = 5  // QZSS; NMEA IDs 193-202
	SAT_TYPE_SBAS    = 10 // NMEA IDs 33-64, 152-158
)

const (
	BARO_TYPE_NONE         = 0 // No baro present
	BARO_TYPE_BMP280       = 1 // Stratux AHRS module or similar internal baro
	BARO_TYPE_OGNTRACKER   = 2 // OGN Tracker with baro pressure
	BARO_TYPE_NMEA         = 3 // Other NMEA provider that reports $PGRMZ (SoftRF)
	BARO_TYPE_ADSBESTIMATE = 4 // If we have no baro, we will try to estimate baro pressure from ADS-B targets reporting GnssDiffFromBaroAlt (HAE<->Baro difference)
)

type SatelliteInfo struct {
	SatelliteNMEA    uint16    // NMEA ID of the satellite. 1-32 GPS, 33-64 SBAS, 65-96 GLONASS, 301-336 Galileo, 401-463 Beidou.
	SatelliteID      string    // Formatted code indicating source and PRN code. e.g. S138==WAAS satellite 138, G2==GPS satellites 2
	Elevation        int16     // Angle above local horizon, -xx to +90
	Azimuth          int16     // Bearing (degrees true), 0-359
	Signal           int8      // Signal strength, 0 - 99; -99 indicates no reception
	Type             uint8     // Type of satellite (GPS, GLONASS, Galileo, SBAS)
	TimeLastSolution time.Time // Time (system ticker) a solution was last calculated using this satellite
	TimeLastSeen     time.Time // Time (system ticker) a signal was last received from this satellite
	TimeLastTracked  time.Time // Time (system ticker) this satellite was tracked (almanac data)
	InSolution       bool      // True if satellite is used in the position solution (reported by GSA message or PUBX,03)
}

type SituationData struct {
	// From GPS.
	muGPS                       *sync.Mutex
	muGPSPerformance            *sync.Mutex
	muSatellite                 *sync.Mutex
	GPSLastFixSinceMidnightUTC  float32
	GPSLatitude                 float32
	GPSLongitude                float32
	GPSFixQuality               uint8
	GPSHeightAboveEllipsoid     float32 // GPS height above WGS84 ellipsoid, ft. This is specified by the GDL90 protocol, but most EFBs use MSL altitude instead. HAE is about 70-100 ft below GPS MSL altitude over most of the US.
	GPSGeoidSep                 float32 // geoid separation, ft, MSL minus HAE (used in altitude calculation)
	GPSSatellites               uint16  // satellites used in solution
	GPSSatellitesTracked        uint16  // satellites tracked (almanac data received)
	GPSSatellitesSeen           uint16  // satellites seen (signal received)
	GPSHorizontalAccuracy       float32 // 95% confidence for horizontal position, meters.
	GPSNACp                     uint8   // NACp categories are defined in AC 20-165A
	GPSAltitudeMSL              float32 // Feet MSL
	GPSVerticalAccuracy         float32 // 95% confidence for vertical position, meters
	GPSVerticalSpeed            float32 // GPS vertical velocity, feet per second
	GPSLastFixLocalTime         time.Time
	GPSTrueCourse               float32
	GPSTurnRate                 float64 // calculated GPS rate of turn, degrees per second
	GPSGroundSpeed              float64
	GPSLastGroundTrackTime      time.Time
	GPSTime                     time.Time
	GPSLastGPSTimeStratuxTime   time.Time // stratuxClock time since last GPS time received.
	GPSLastValidNMEAMessageTime time.Time // time valid NMEA message last seen
	GPSLastValidNMEAMessage     string    // last NMEA message processed.
	GPSLastAccuracyTime         time.Time // time of last GNGST
	GPSPositionSampleRate       float64   // calculated sample rate of GPS positions

	// From pressure sensor.
	muBaro                  *sync.Mutex
	BaroTemperature         float32
	BaroPressureAltitude    float32
	BaroVerticalSpeed       float32
	BaroLastMeasurementTime time.Time
	BaroSourceType          uint8

	// From AHRS source.
	muAttitude           *sync.Mutex
	AHRSPitch            float64
	AHRSRoll             float64
	AHRSGyroHeading      float64
	AHRSMagHeading       float64
	AHRSSlipSkid         float64
	AHRSTurnRate         float64
	AHRSGLoad            float64
	AHRSGLoadMin         float64
	AHRSGLoadMax         float64
	AHRSLastAttitudeTime time.Time
	AHRSStatus           uint8
}

/*
myGPSPerfStats used to track short-term position / velocity trends, used to feed dynamic AHRS model. Use floats for better resolution of calculated data.
*/
type gpsPerfStats struct {
	stratuxTime   uint64  // time since Stratux start, msec
	nmeaTime      float32 // timestamp from NMEA message
	msgType       string  // NMEA message type
	gsf           float32 // knots
	coursef       float32 // true course [degrees]
	alt           float32 // gps altitude, ft msl
	vv            float32 // vertical velocity, ft/sec
	gpsTurnRate   float64 // calculated turn rate, deg/sec. Right turn is positive.
	gpsPitch      float64 // estimated pitch angle, deg. Calculated from gps ground speed and VV. Equal to flight path angle.
	gpsRoll       float64 // estimated roll angle from turn rate and groundspeed, deg. Assumes airplane in coordinated turns.
	gpsLoadFactor float64 // estimated load factor from turn rate and groundspeed, "gee". Assumes airplane in coordinated turns.
}

var (
	gpsPerf            gpsPerfStats
	myGPSPerfStats     []gpsPerfStats
	gpsTimeOffsetPpsMs = 100.0 * time.Millisecond

	trackerMutex     sync.RWMutex
	serialConfig     *serial.Config
	serialPort       *serial.Port
	serialDevicePath string
	gpsPortOpenedAt  time.Time
	detectedTracker  Tracker

	readyToInitGPS    atomic.Bool
	systemTimeSetting atomic.Bool
	nmeaChecksumFails atomic.Uint64

	Satellites map[string]SatelliteInfo

	trackerDrivers = []Tracker{&OgnTracker{}, &GxAirCom{}, &SoftRF{}}
)

func getGPSConnected() bool {
	globalStatusMutex.RLock()
	v := globalStatus.GPS_connected
	globalStatusMutex.RUnlock()
	return v
}

func setGPSConnected(v bool) {
	globalStatusMutex.Lock()
	globalStatus.GPS_connected = v
	globalStatusMutex.Unlock()
}

func getGPSDetectedType() uint {
	globalStatusMutex.RLock()
	v := globalStatus.GPS_detected_type
	globalStatusMutex.RUnlock()
	return v
}

func setGPSDetectedType(v uint) {
	globalStatusMutex.Lock()
	globalStatus.GPS_detected_type = v
	globalStatusMutex.Unlock()
}

func orGPSDetectedType(mask uint) {
	globalStatusMutex.Lock()
	globalStatus.GPS_detected_type |= mask
	globalStatusMutex.Unlock()
}

func getGPSEnabled() bool {
	globalSettingsMutex.RLock()
	v := globalSettings.GPS_Enabled
	globalSettingsMutex.RUnlock()
	return v
}

func getSerialPort() *serial.Port {
	trackerMutex.RLock()
	defer trackerMutex.RUnlock()
	return serialPort
}

func getSerialDevicePath() string {
	trackerMutex.RLock()
	defer trackerMutex.RUnlock()
	return serialDevicePath
}

func setSerialPort(p *serial.Port) {
	trackerMutex.Lock()
	serialPort = p
	if p != nil {
		gpsPortOpenedAt = stratuxClock.GetTime()
	} else {
		gpsPortOpenedAt = time.Time{}
	}
	trackerMutex.Unlock()
}

func getGPSPortOpenedAt() time.Time {
	trackerMutex.RLock()
	defer trackerMutex.RUnlock()
	return gpsPortOpenedAt
}

func getDetectedTracker() Tracker {
	trackerMutex.RLock()
	defer trackerMutex.RUnlock()
	return detectedTracker
}

func setDetectedTracker(t Tracker) {
	trackerMutex.Lock()
	detectedTracker = t
	trackerMutex.Unlock()
}

func constellationToSatType(c string) uint8 {
	switch c {
	case gpsnmea.ConstellationGPS:
		return SAT_TYPE_GPS
	case gpsnmea.ConstellationSBAS:
		return SAT_TYPE_SBAS
	case gpsnmea.ConstellationGLONASS:
		return SAT_TYPE_GLONASS
	case gpsnmea.ConstellationQZSS:
		return SAT_TYPE_QZSS
	case gpsnmea.ConstellationGalileo:
		return SAT_TYPE_GALILEO
	case gpsnmea.ConstellationBeidou:
		return SAT_TYPE_BEIDOU
	default:
		return SAT_TYPE_UNKNOWN
	}
}

func classifySatellite(sv int) (uint8, string) {
	c, _ := gpsnmea.ClassifySatellite(sv)
	return constellationToSatType(c), gpsnmea.FormatSatelliteID(sv)
}

func classifySatelliteWithTalker(talker string, sv int) (uint8, string) {
	c, _ := gpsnmea.ClassifySatelliteWithTalker(talker, sv)
	return constellationToSatType(c), gpsnmea.FormatSatelliteIDWithTalker(talker, sv)
}

/*
	chksumUBX()
		returns the two-byte Fletcher algorithm checksum of byte array msg.
		This is used in configuration messages for the u-blox GPS. See p. 97 of the
		u-blox M8 Receiver Description.
*/

func chksumUBX(msg []byte) []byte {
	ret := make([]byte, 2)
	for i := 0; i < len(msg); i++ {
		ret[0] = ret[0] + msg[i]
		ret[1] = ret[1] + ret[0]
	}
	return ret
}

/*
makeUBXCFG()

	creates a UBX-formatted package consisting of two sync characters,
	class, ID, payload length in bytes (2-byte little endian), payload, and checksum.
	See p. 95 of the u-blox M8 Receiver Description.
*/
func makeUBXCFG(class, id byte, msglen uint16, msg []byte) []byte {
	ret := make([]byte, 6)
	ret[0] = 0xB5
	ret[1] = 0x62
	ret[2] = class
	ret[3] = id
	ret[4] = byte(msglen & 0xFF)
	ret[5] = byte((msglen >> 8) & 0xFF)
	ret = append(ret, msg...)
	chk := chksumUBX(ret[2:])
	ret = append(ret, chk[0])
	ret = append(ret, chk[1])
	return ret
}

func makeNMEACmd(cmd string) []byte {
	chk_sum := byte(0)
	for i := range cmd {
		chk_sum = chk_sum ^ byte(cmd[i])
	}
	return []byte(fmt.Sprintf("$%s*%02x\x0d\x0a", cmd, chk_sum))
}

func logChipConfig(line1 string, chip string, device string, baudrate int, appendStr string) {
	if line1 == "auto" {
		logInf("Gps - autodected gps, using following parameters:")
	} else if line1 == "man" {
		logInf("GPS - manual configuration with following parameters from /boot/firmware/stratux.conf:")
	}
	msg := "GPS - chip: %s, device: %s, baudrate: %d"
	if appendStr != "" {
		msg += ", " + appendStr
	}
	logInf(msg, chip, device, baudrate)
}

// discoverFallbackUSBGPS returns candidate /dev/ttyACM* and /dev/ttyUSB* paths
// that are not claimed by other known Stratux peripherals (uatradio, ping, pong, serialout).
func discoverFallbackUSBGPS() []string {
	excluded := make(map[string]bool)
	for _, pattern := range []string{"/dev/uatradio", "/dev/ping", "/dev/pong", "/dev/serialout*"} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if target, err := filepath.EvalSymlinks(m); err == nil {
				excluded[target] = true
			}
		}
	}
	var candidates []string
	for _, pattern := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		for _, m := range matches {
			realPath, err := filepath.EvalSymlinks(m)
			if err != nil {
				realPath = m
			}
			if !excluded[realPath] {
				candidates = append(candidates, m)
			}
		}
	}
	return candidates
}

func initGPSSerial() bool {
	var device string
	var targetBaudRate int = 115200
	if (getGPSDetectedType() & 0x0f) == GPS_TYPE_NETWORK {
		return true
	}
	// Possible baud rates for this device. We will try to auto detect the correct one.
	baudrates := []int{9600, 38400, 115200}
	isSirfIV := false
	detectedType := uint(0)
	setGPSDetectedType(0) // reset detected type on each initialization
	setDetectedTracker(nil)

	globalSettingsMutex.RLock()
	manualCfg := globalSettings.GpsManualConfig
	manualChip := globalSettings.GpsManualChip
	manualDevice := globalSettings.GpsManualDevice
	manualTargetBaud := globalSettings.GpsManualTargetBaud
	globalSettingsMutex.RUnlock()

	if manualCfg {
		device = manualDevice
		targetBaudRate = manualTargetBaud
		baudrates = []int{115200, 38400, 9600, 230400, 500000, 1000000, 2000000}

		switch manualChip {
		case "ublox6", "ublox7":
			detectedType = GPS_TYPE_UBX6or7
			logChipConfig("man", "ublox 6 or 7", device, targetBaudRate, "")
		case "ublox8":
			detectedType = GPS_TYPE_UBX8
			logChipConfig("man", "ublox 8", device, targetBaudRate, "")
		case "ublox9":
			detectedType = GPS_TYPE_UBX9
			logChipConfig("man", "ublox 9", device, targetBaudRate, "")
		case "ublox10":
			detectedType = GPS_TYPE_UBX10
			logChipConfig("man", "ublox 10", device, targetBaudRate, "")
		case "ublox":
			detectedType = GPS_TYPE_UBX_GEN
			logChipConfig("man", "generic ublox", device, targetBaudRate, "")
		default:
			detectedType = GPS_TYPE_ANY
			logInf("GPS - configuring gps chip as other -> no further configuration will be done, use gps as it is")
		}
	} else if _, err := os.Stat("/dev/ublox10"); err == nil {
		device = "/dev/ublox10"
		detectedType = GPS_TYPE_UBX10
		baudrates = []int{38400, 9600, 115200}
		logChipConfig("auto", "ublox 10", device, targetBaudRate, "")
	} else if _, err := os.Stat("/dev/ublox9"); err == nil {
		device = "/dev/ublox9"
		detectedType = GPS_TYPE_UBX9
		baudrates = []int{38400, 9600, 115200}
		logChipConfig("auto", "ublox 9", device, targetBaudRate, "")
	} else if _, err := os.Stat("/dev/ublox8"); err == nil { // u-blox 8 (RY83xAI or GPYes 2.0).
		device = "/dev/ublox8"
		detectedType = GPS_TYPE_UBX8
		gpsTimeOffsetPpsMs = 80 * time.Millisecond // Ublox 8 seems to have higher delay
		logChipConfig("auto", "ublox 8", device, targetBaudRate, "")
	} else if _, err := os.Stat("/dev/ublox7"); err == nil { // u-blox 7 (VK-172, VK-162 Rev 2, GPYes, RY725AI over USB).
		device = "/dev/ublox7"
		detectedType = GPS_TYPE_UBX6or7
		logChipConfig("auto", "ublox 7", device, targetBaudRate, "")
	} else if _, err := os.Stat("/dev/ublox6"); err == nil { // u-blox 6 (VK-162 Rev 1).
		device = "/dev/ublox6"
		detectedType = GPS_TYPE_UBX6or7
		logChipConfig("auto", "ublox 6", device, targetBaudRate, "")
	} else if _, err := os.Stat("/dev/prolific0"); err == nil { // Assume it's a BU-353-S4 SIRF IV.
		isSirfIV = true
		baudrates = []int{4800, 38400, 9600}
		device = "/dev/prolific0"
		detectedType = GPS_TYPE_PROLIFIC
	} else if _, err := os.Stat("/dev/serialin"); err == nil {
		device = "/dev/serialin"
		detectedType = GPS_TYPE_SERIAL
		// OGN Tracker uses 115200, SoftRF 38400
		baudrates = []int{115200, 38400, 9600}
	} else if _, err := os.Stat("/dev/softrf_dongle"); err == nil {
		device = "/dev/softrf_dongle"
		detectedType = GPS_TYPE_SOFTRF_DONGLE
		baudrates = []int{115200}
	} else {
		// Probe unclaimed USB serial devices (/dev/ttyACM*, /dev/ttyUSB*) before falling back to /dev/serial0.
		if usbDevs := discoverFallbackUSBGPS(); len(usbDevs) > 0 {
			for _, cand := range usbDevs {
				logDbg("GPS - probing unclaimed USB serial candidate %s", cand)
				if p, err := detectOpenSerialPort(cand, []int{9600, 38400, 115200, 4800}); err == nil {
					logInf("GPS - detected NMEA GPS on %s", cand)
					setGPSDetectedType(GPS_TYPE_ANY)
					trackerMutex.Lock()
					serialDevicePath = cand
					trackerMutex.Unlock()
					setSerialPort(p)
					for _, tracker := range trackerDrivers {
						tracker.initNewConnection(p)
					}
					return true
				}
			}
		}
		if _, err := os.Stat("/dev/serial0"); err == nil {
			// ttyS0 / ttyAMA0 is UART (GPIO pins 8 and 10) on RPi.
			device = "/dev/serial0"
			detectedType = GPS_TYPE_UBX_GEN
			baudrates = []int{115200, 38400, 9600}
			logDbg("GPS - probing GPIO serial port /dev/serial0")
		} else {
			logDbg("GPS - no gps device found.\n")
			return false
		}
	}

	logDbg("GPS - using device: %s", device)

	p, err := detectOpenSerialPort(device, baudrates)
	if err != nil {
		log.Printf("GPS - serial port/baudrate detection err on %s: %s\n", device, err.Error())
		setGPSDetectedType(0)
		return false
	}
	// Only publish detected hardware type once the port actually opened and produced valid NMEA.
	setGPSDetectedType(detectedType)
	baudChanged := false

	if isSirfIV {
		log.Printf("Using SiRFIV config.\n")

		// Enable 5Hz. (To switch back to 1Hz: $PSRF103,00,7,00,0*22)
		p.Write(makeNMEACmd("PSRF103,00,6,00,0"))
		// Enable GGA.
		p.Write(makeNMEACmd("PSRF103,00,00,01,01"))
		// Enable GSA.
		p.Write(makeNMEACmd("PSRF103,02,00,01,01"))
		// Enable RMC.
		p.Write(makeNMEACmd("PSRF103,04,00,01,01"))
		// Enable VTG.
		p.Write(makeNMEACmd("PSRF103,05,00,01,01"))
		// Enable GSV (once every 5 position updates)
		p.Write(makeNMEACmd("PSRF103,03,00,05,01"))
		// Enable 38400 baud.
		p.Write(makeNMEACmd("PSRF100,1,38400,8,1,0"))
		baudChanged = true

		if globalSettings.DEBUG {
			log.Printf("Finished writing SiRF GPS config to %s. Opening port to test connection.\n", device)
		}
	} else if detectedType == GPS_TYPE_UBX6or7 ||
		detectedType == GPS_TYPE_UBX8 ||
		detectedType == GPS_TYPE_UBX9 ||
		detectedType == GPS_TYPE_UBX10 ||
		detectedType == GPS_TYPE_UBX_GEN {

		if detectedType == GPS_TYPE_UBX10 {
			logDbg("GPS - configuring as ublox 10 (UBX-CFG-VALSET)\n")
			writeUblox10ConfigCommands(10, targetBaudRate, p)
		} else {
			// u-blox 6/7/8 max out at 5 Hz with multi-GNSS; M9 supports 10 Hz cleanly.
			navRate := uint16(5)
			if detectedType == GPS_TYPE_UBX9 {
				navRate = 10
				logDbg("GPS - configuring as ublox 9\n")
				writeUblox9ConfigCommands(p)
			} else if detectedType == GPS_TYPE_UBX8 {
				logDbg("GPS - configuring as ublox 8\n")
				writeUblox8ConfigCommands(p)
			} else if detectedType == GPS_TYPE_UBX6or7 {
				logDbg("GPS - configuring as ublox 6 or 7\n")
				cfgGnss := []byte{0x00, 0x00, 0xFF, 0x04}
				gps := []byte{0x00, 0x04, 0xFF, 0x00, 0x01, 0x00, 0x01, 0x01}
				sbas := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01}
				qzss := []byte{0x05, 0x00, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01}
				glonass := []byte{0x06, 0x08, 0xFF, 0x00, 0x00, 0x00, 0x01, 0x01}
				cfgGnss = append(cfgGnss, gps...)
				cfgGnss = append(cfgGnss, sbas...)
				cfgGnss = append(cfgGnss, qzss...)
				cfgGnss = append(cfgGnss, glonass...)
				p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
				time.Sleep(350 * time.Millisecond)
			}

			writeUbloxGenericCommands(navRate, p)

			// Reconfigure UART1 serial port.
			cfg := make([]byte, 20)
			cfg[0] = 0x01 // portID (UART1).
			cfg[1] = 0x00 // res0.
			cfg[2] = 0x00 // res1.
			cfg[3] = 0x00 // res1.

			// UART mode. 0 stop bits, no parity, 8 data bits. Little endian order.
			cfg[4] = 0xC0
			cfg[5] = 0x08
			cfg[6] = 0x00
			cfg[7] = 0x00

			// Baud rate. Little endian order.
			bdrt := uint32(targetBaudRate)
			cfg[11] = byte((bdrt >> 24) & 0xFF)
			cfg[10] = byte((bdrt >> 16) & 0xFF)
			cfg[9] = byte((bdrt >> 8) & 0xFF)
			cfg[8] = byte(bdrt & 0xFF)

			// inProtoMask. NMEA and UBX. Little endian.
			cfg[12] = 0x03
			cfg[13] = 0x00

			// outProtoMask. NMEA. Little endian.
			cfg[14] = 0x02
			cfg[15] = 0x00

			// UBX-CFG-PRT (Port Configuration for UART)
			p.Write(makeUBXCFG(0x06, 0x00, 20, cfg))
		}

		baudrates = append([]int{targetBaudRate}, baudrates...)
		baudChanged = true
		logDbg("GPS - finished writing u-blox GPS config to %s. Opening port to test connection.\n", device)
	} else if detectedType == GPS_TYPE_SOFTRF_DONGLE {
		p.Write([]byte("@GNS 0x7\r\n"))    // enable SBAS
		time.Sleep(250 * time.Millisecond) // Otherwise second command doesn't seem to work?
		p.Write([]byte("@BSSL 0x2D\r\n"))  // enable GNGSV
	}

	if baudChanged {
		// Allow queued config bytes (~550 bytes takes ~570 ms at 9600 baud) to drain before closing.
		time.Sleep(650 * time.Millisecond)
		p.Close()
		time.Sleep(250 * time.Millisecond)

		// Re-open port at newly configured baud so we can read messages. ReadTimeout keeps gpsSerialReader responsive on disconnects.
		p, err = detectOpenSerialPort(device, baudrates)
		if err != nil {
			logErr("GPS - serial port err after config on %s: %s\n", device, err.Error())
			setGPSDetectedType(0)
			return false
		}
	}

	trackerMutex.Lock()
	serialDevicePath = device
	trackerMutex.Unlock()
	setSerialPort(p)

	for _, tracker := range trackerDrivers {
		tracker.initNewConnection(p)
	}
	return true
}

func detectOpenSerialPort(device string, baudrates []int) (*serial.Port, error) {
	if len(baudrates) == 1 {
		serialConfig := &serial.Config{Name: device, Baud: baudrates[0], ReadTimeout: time.Millisecond * 2500}
		return serial.OpenPort(serialConfig)
	}
	seen := make(map[int]bool, len(baudrates))
	for _, baud := range baudrates {
		if seen[baud] {
			continue
		}
		seen[baud] = true
		logDbg("GPS - trying to open serial %s with %d baud ...", device, baud)
		serialConfig := &serial.Config{Name: device, Baud: baud, ReadTimeout: time.Millisecond * 500}
		p, err := serial.OpenPort(serialConfig)
		if err != nil {
			return nil, err
		}
		p.Flush() // make sure input buffer is clean

		// Stream-read incrementally for up to 2.5s instead of sleeping blindly (avoids 4KB kernel TTY overflow).
		deadline := time.Now().Add(2500 * time.Millisecond)
		var lineBuf strings.Builder
		chunk := make([]byte, 512)
		found := false
		for time.Now().Before(deadline) && !found {
			n, readErr := p.Read(chunk)
			if n > 0 {
				lineBuf.Write(chunk[:n])
				data := lineBuf.String()
				if idx := strings.LastIndexByte(data, '\n'); idx >= 0 {
					complete := data[:idx]
					rem := data[idx+1:]
					lineBuf.Reset()
					if len(rem) < 512 {
						lineBuf.WriteString(rem)
					}
					for _, rawLine := range strings.Split(complete, "\n") {
						for _, seg := range strings.Split(rawLine, "$") {
							if len(seg) == 0 {
								continue
							}
							cand := "$" + seg
							if _, validNMEAcs := validateNMEAChecksum(cand); validNMEAcs {
								logInf("GPS - successfully opened serial port %s with baud %d (Valid NMEA msg received)", device, baud)
								processNMEALine(cand)
								found = true
								break
							}
						}
						if found {
							break
						}
					}
				} else if lineBuf.Len() > 2048 {
					lineBuf.Reset()
				}
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				break
			}
		}

		if found {
			// Re-open with normal 2500ms ReadTimeout for steady-state reading.
			p.Close()
			finalCfg := &serial.Config{Name: device, Baud: baud, ReadTimeout: time.Millisecond * 2500}
			return serial.OpenPort(finalCfg)
		}

		logDbg("GPS - could not open serial port %s with %d baud, no valid NMEA received ...", device, baud)
		p.Close()
		time.Sleep(150 * time.Millisecond)
	}
	logErr("GPS - none of the baud rates worked on %s, gps not connected ...", device)
	return nil, errors.New("GPS - Failed to detect gps serial baud rate")
}

func writeUblox8ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x05}
	gps := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}
	sbas := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01}
	galileo := []byte{0x02, 0x08, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01}
	beidou := []byte{0x03, 0x08, 0x10, 0x00, 0x00, 0x00, 0x01, 0x01}
	qzss := []byte{0x05, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01}
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}

	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds on all chips supporting GPS+GLO
	time.Sleep(350 * time.Millisecond)

	cfgGnss[3] = 0x06
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds only on chips that support GPS+GLO+GAL
	time.Sleep(350 * time.Millisecond)
}

func writeUblox9ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x06}
	gps := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}
	sbas := []byte{0x01, 0x03, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01}
	galileo := []byte{0x02, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}
	beidou := []byte{0x03, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}
	qzss := []byte{0x05, 0x03, 0x04, 0x00, 0x01, 0x00, 0x05, 0x01}
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01}

	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
	time.Sleep(350 * time.Millisecond)
}

// makeUBXCfgValset builds a UBX-CFG-VALSET (0x06 0x8A) packet writing key-value
// pairs to the RAM layer (0x01) for u-blox M10 / Protocol 34+ receivers.
func makeUBXCfgValset(kvBytes []byte) []byte {
	payload := make([]byte, 4, 4+len(kvBytes))
	payload[0] = 0x00 // version
	payload[1] = 0x01 // layers: RAM only
	payload[2] = 0x00 // reserved
	payload[3] = 0x00 // reserved
	payload = append(payload, kvBytes...)
	return makeUBXCFG(0x06, 0x8A, uint16(len(payload)), payload)
}

func appendUBXValU1(buf []byte, key uint32, val byte) []byte {
	return append(buf,
		byte(key&0xFF), byte((key>>8)&0xFF), byte((key>>16)&0xFF), byte((key>>24)&0xFF),
		val,
	)
}

func appendUBXValU2(buf []byte, key uint32, val uint16) []byte {
	return append(buf,
		byte(key&0xFF), byte((key>>8)&0xFF), byte((key>>16)&0xFF), byte((key>>24)&0xFF),
		byte(val&0xFF), byte((val>>8)&0xFF),
	)
}

func appendUBXValU4(buf []byte, key uint32, val uint32) []byte {
	return append(buf,
		byte(key&0xFF), byte((key>>8)&0xFF), byte((key>>16)&0xFF), byte((key>>24)&0xFF),
		byte(val&0xFF), byte((val>>8)&0xFF), byte((val>>16)&0xFF), byte((val>>24)&0xFF),
	)
}

func writeUblox10ConfigCommands(navrate uint16, targetBaud int, p *serial.Port) {
	measMs := uint16(200) // default 5 Hz
	if navrate == 10 {
		measMs = 100
	} else if navrate == 2 {
		measMs = 500
	} else if navrate == 1 {
		measMs = 1000
	}

	var kv []byte
	kv = appendUBXValU1(kv, 0x20110021, 7)      // CFG-NAVSPG-DYNMODEL = Airborne <2g
	kv = appendUBXValU2(kv, 0x30210001, measMs) // CFG-RATE-MEAS (ms)
	kv = appendUBXValU2(kv, 0x30210002, 1)      // CFG-RATE-NAV = 1
	// Enable GGA, RMC, VTG every nav epoch; GSA, GSV, GST every 5 epochs (UART1 & USB)
	kv = appendUBXValU1(kv, 0x209100bb, 1) // CFG-MSGOUT-NMEA_ID_GGA_UART1
	kv = appendUBXValU1(kv, 0x209100bd, 1) // CFG-MSGOUT-NMEA_ID_GGA_USB
	kv = appendUBXValU1(kv, 0x209100ac, 1) // CFG-MSGOUT-NMEA_ID_RMC_UART1
	kv = appendUBXValU1(kv, 0x209100ae, 1) // CFG-MSGOUT-NMEA_ID_RMC_USB
	kv = appendUBXValU1(kv, 0x209100b1, 1) // CFG-MSGOUT-NMEA_ID_VTG_UART1
	kv = appendUBXValU1(kv, 0x209100b3, 1) // CFG-MSGOUT-NMEA_ID_VTG_USB
	kv = appendUBXValU1(kv, 0x209100c0, 5) // CFG-MSGOUT-NMEA_ID_GSA_UART1
	kv = appendUBXValU1(kv, 0x209100c2, 5) // CFG-MSGOUT-NMEA_ID_GSA_USB
	kv = appendUBXValU1(kv, 0x209100c5, 5) // CFG-MSGOUT-NMEA_ID_GSV_UART1
	kv = appendUBXValU1(kv, 0x209100c7, 5) // CFG-MSGOUT-NMEA_ID_GSV_USB
	kv = appendUBXValU1(kv, 0x209100d4, 5) // CFG-MSGOUT-NMEA_ID_GST_UART1
	kv = appendUBXValU1(kv, 0x209100d6, 5) // CFG-MSGOUT-NMEA_ID_GST_USB
	kv = appendUBXValU1(kv, 0x209100ca, 0) // CFG-MSGOUT-NMEA_ID_GLL_UART1 = off
	kv = appendUBXValU1(kv, 0x209100cc, 0) // CFG-MSGOUT-NMEA_ID_GLL_USB = off
	p.Write(makeUBXCfgValset(kv))
	time.Sleep(100 * time.Millisecond)

	if targetBaud > 0 {
		var baudKV []byte
		baudKV = appendUBXValU4(baudKV, 0x40520001, uint32(targetBaud)) // CFG-UART1-BAUDRATE
		p.Write(makeUBXCfgValset(baudKV))
	}
}

func writeUbloxGenericCommands(navrate uint16, p *serial.Port) {
	// UBX-CFG-TP5 (turn off "time pulse" which usually drives the GPS LED)
	tp5 := make([]byte, 32)
	tp5[4] = 0x32
	tp5[8] = 0x40
	tp5[9] = 0x42
	tp5[10] = 0x0F
	tp5[12] = 0x40
	tp5[13] = 0x42
	tp5[14] = 0x0F
	tp5[28] = 0xE7
	p.Write(makeUBXCFG(0x06, 0x31, 32, tp5))

	// UBX-CFG-NMEA (change NMEA protocol version to 4.0 extended)
	p.Write(makeUBXCFG(0x06, 0x17, 20, []byte{0x00, 0x40, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-NAV5                           |mask1...|  dyn
	p.Write(makeUBXCFG(0x06, 0x24, 36, []byte{0x01, 0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Dynamic platform model: airborne with <2g acceleration

	// UBX-CFG-SBAS (disable integrity, enable auto-scan)
	p.Write(makeUBXCFG(0x06, 0x16, 8, []byte{0x01, 0x03, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-MSG (NMEA Standard Messages)
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // GGA
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GLL
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x02, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSA
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x03, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSV
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x04, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // RMC
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x05, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // VTG
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GRS
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x07, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GST
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // ZDA
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x09, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GBS
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // DTM
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GNS
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // VLW

	// UBX-CFG-MSG (NMEA PUBX Messages)
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))

	if navrate == 10 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0x64, 0x00, 0x01, 0x00, 0x01, 0x00})) // 100ms -> 10Hz
	} else if navrate == 5 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xC8, 0x00, 0x01, 0x00, 0x01, 0x00})) // 200ms -> 5Hz
	} else if navrate == 2 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xF4, 0x01, 0x01, 0x00, 0x01, 0x00})) // 500ms -> 2Hz
	} else if navrate == 1 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xE8, 0x03, 0x01, 0x00, 0x01, 0x00})) // 1000ms -> 1Hz
	}

	logDbg("GPS - applying generic ublox settings (refresh rate: %d)", navrate)
}

func writeTrackerConfigFromSettings() {
	trackerMutex.RLock()
	tracker := detectedTracker
	sp := serialPort
	trackerMutex.RUnlock()

	if tracker != nil && sp != nil {
		changed := tracker.writeConfigFromSettings(sp)
		if !changed {
			return
		}
		delay := tracker.writeReadDelay()
		go func() {
			time.Sleep(delay)
			trackerMutex.RLock()
			curTracker := detectedTracker
			curSP := serialPort
			trackerMutex.RUnlock()
			if curTracker == tracker && curSP != nil {
				curTracker.requestTrackerConfig(curSP)
			}
		}()
	}
}

// func validateNMEAChecksum determines if a string is a properly formatted NMEA sentence with a valid checksum.
//
// If the input string is valid, output is the input stripped of the "$" token and checksum, along with a boolean 'true'
// If the input string is the incorrect format, the checksum is missing/invalid, or checksum calculation fails, an error string and
// boolean 'false' are returned
//
// Checksum is calculated as XOR of all bytes between "$" and "*"

func validateNMEAChecksum(s string) (string, bool) {
	return gpsnmea.ValidateChecksum(s)
}

// Only count this heading if a "sustained" >7 kts is obtained. This filters out a lot of heading
// changes while on the ground and "movement" is really only changes in GPS fix as it settles down.
func setTrueCourse(groundSpeed uint16, trueCourse float64) {
	if mySituation.GPSGroundSpeed >= 7 && groundSpeed >= 7 {
		_ = trueCourse
		_ = groundSpeed
	}
}

/*
calcGPSAttitude estimates turn rate, pitch, and roll based on recent GPS groundspeed, track, and altitude / vertical speed.

Method uses stored performance statistics from myGPSPerfStats[]. Ideally, calculation is based on most recent 1.5 seconds of data,
assuming 10 Hz sampling frequency. Lower frequency sample rates will increase calculation window for smoother response, at the
cost of slightly increased lag.

(c) 2016 Keith Tschohl. All rights reserved.
Distributable under the terms of the "BSD-New" License that can be found in
the LICENSE file, herein included as part of this header.
*/

func calcGPSAttitude() bool {
	// check slice length. Return error if empty set or set zero values
	mySituation.muGPSPerformance.Lock()
	defer mySituation.muGPSPerformance.Unlock()
	length := len(myGPSPerfStats)
	index := length - 1

	if length == 0 {
		log.Printf("GPS attitude: No data received yet. Not calculating attitude.\n")
		return false
	} else if length == 1 {
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		return false
	}

	// check if GPS data was put in the structure more than three seconds ago -- this shouldn't happen unless something is wrong.
	nowMs := stratuxClock.GetMilliseconds()
	if (nowMs - myGPSPerfStats[index].stratuxTime) > 3000 {
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		log.Printf("GPS attitude: GPS data is more than three seconds old. Setting attitude to zero.\n")
		return false
	}

	// check time interval between samples
	t1 := myGPSPerfStats[index].nmeaTime
	t0 := myGPSPerfStats[index-1].nmeaTime
	dt := t1 - t0

	// first time error case: index is more than three seconds ahead of index-1
	if dt > 3 {
		log.Printf("GPS attitude: Can't calculate GPS attitude. Reference data is old. dt = %v\n", dt)
		return false
	}

	// second case: index is behind index-1. This could be result of day rollover.
	if dt < 0 {
		log.Printf("GPS attitude: Current GPS time (%.2f) is older than last GPS time (%.2f). Checking for 0000Z rollover.\n", t1, t0)
		if myGPSPerfStats[index-1].nmeaTime > 86300 && myGPSPerfStats[index].nmeaTime < 100 { // be generous with the time window at rollover
			myGPSPerfStats[index].nmeaTime += 86400
		} else {
			log.Printf("GPS attitude: Time isn't near 0000Z. Unknown reason for offset. Can't calculate GPS attitude.\n")
			return false
		}

		// check time array to see if all timestamps are > 86401 seconds since midnight
		tempTime := make([]float64, length)
		for i := 0; i < length; i++ {
			tempTime[i] = float64(myGPSPerfStats[i].nmeaTime)
		}
		minTime, _ := common.ArrayMin(tempTime)
		if minTime > 86401.0 {
			log.Printf("GPS attitude: Rebasing GPS time since midnight to current day.\n")
			for i := 0; i < length; i++ {
				myGPSPerfStats[i].nmeaTime -= 86400
			}
		}

		// Verify adjustment
		dt = myGPSPerfStats[index].nmeaTime - myGPSPerfStats[index-1].nmeaTime
		log.Printf("GPS attitude: New dt = %f\n", dt)
		if dt > 3 {
			log.Printf("GPS attitude: Can't calculate GPS attitude. Reference data is old. dt = %v\n", dt)
			return false
		} else if dt < 0 {
			log.Printf("GPS attitude: Something went wrong rebasing the time.\n")
			return false
		}
	}

	// local variables
	var headingAvg, dh, v_x, v_z, a_c, omega, slope, intercept float64
	var tempHdg, tempHdgUnwrapped, tempHdgTime, tempSpeed, tempVV, tempSpeedTime, tempRegWeights []float64
	var valid bool
	var lengthHeading, lengthSpeed int

	center := float64(myGPSPerfStats[index].nmeaTime) // current time for calculating regression weights
	halfwidth := calculateNavRate()

	// first, parse groundspeed from RMC messages.
	tempSpeedTime = make([]float64, 0)
	tempSpeed = make([]float64, 0)
	tempRegWeights = make([]float64, 0)

	for i := 0; i < length; i++ {
		if myGPSPerfStats[i].msgType == "GPRMC" || myGPSPerfStats[i].msgType == "GNRMC" {
			tempSpeed = append(tempSpeed, float64(myGPSPerfStats[i].gsf))
			tempSpeedTime = append(tempSpeedTime, float64(myGPSPerfStats[i].nmeaTime))
			tempRegWeights = append(tempRegWeights, common.TriCubeWeight(center, halfwidth, float64(myGPSPerfStats[i].nmeaTime)))
		}
	}
	lengthSpeed = len(tempSpeed)
	if lengthSpeed == 0 {
		log.Printf("GPS Attitude: No groundspeed data could be parsed from NMEA RMC messages\n")
		return false
	} else if lengthSpeed == 1 {
		v_x = tempSpeed[0] * 1.687810
	} else {
		slope, intercept, valid = common.LinRegWeighted(tempSpeedTime, tempSpeed, tempRegWeights)
		if !valid {
			log.Printf("GPS attitude: Error calculating speed regression from NMEA RMC position messages")
			return false
		}
		v_x = (slope*float64(myGPSPerfStats[index].nmeaTime) + intercept) * 1.687810 // units are knots, converted to feet/sec
	}

	// next, calculate vertical velocity from GGA altitude data.
	tempSpeedTime = make([]float64, 0)
	tempVV = make([]float64, 0)
	tempRegWeights = make([]float64, 0)

	for i := 0; i < length; i++ {
		if myGPSPerfStats[i].msgType == "GPGGA" || myGPSPerfStats[i].msgType == "GNGGA" {
			tempVV = append(tempVV, float64(myGPSPerfStats[i].alt))
			tempSpeedTime = append(tempSpeedTime, float64(myGPSPerfStats[i].nmeaTime))
			tempRegWeights = append(tempRegWeights, common.TriCubeWeight(center, halfwidth, float64(myGPSPerfStats[i].nmeaTime)))
		}
	}
	lengthSpeed = len(tempVV)
	if lengthSpeed < 2 {
		log.Printf("GPS Attitude: Not enough points to calculate vertical speed from NMEA GGA messages\n")
		return false
	}
	slope, _, valid = common.LinRegWeighted(tempSpeedTime, tempVV, tempRegWeights)
	if !valid {
		log.Printf("GPS attitude: Error calculating vertical speed regression from NMEA GGA messages")
		return false
	}
	v_z = slope // units are feet/sec
	mySituation.muGPS.Lock()
	mySituation.GPSVerticalSpeed = float32(v_z)
	mySituation.muGPS.Unlock()

	if v_x < 6 { // ~3.55 knots
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsLoadFactor = 1.0
		mySituation.muGPS.Lock()
		mySituation.GPSTurnRate = 0
		mySituation.muGPS.Unlock()

		if globalSettings.DEBUG {
			buf := fmt.Sprintf("GPSAttitude,%.1f,%.2f,%s,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f\n", float64(stratuxClock.GetMilliseconds())/1000, myGPSPerfStats[index].nmeaTime, myGPSPerfStats[index].msgType, myGPSPerfStats[index].gsf, myGPSPerfStats[index].coursef, myGPSPerfStats[index].alt, myGPSPerfStats[index].vv, v_x/1.687810, headingAvg, myGPSPerfStats[index].gpsTurnRate, v_z, myGPSPerfStats[index].gpsPitch, myGPSPerfStats[index].gpsRoll, myGPSPerfStats[index].gpsLoadFactor)
			log.Printf("%s", buf)
		}
		logGPSAttitude(myGPSPerfStats[index])
		return true
	}

	// Heading. Same method used for UBX and generic.
	for i := 0; i < length; i++ {
		if myGPSPerfStats[i].coursef >= 0 { // negative values are used to flag invalid / unavailable course
			tempHdg = append(tempHdg, float64(myGPSPerfStats[i].coursef))
			tempHdgTime = append(tempHdgTime, float64(myGPSPerfStats[i].nmeaTime))
		}
	}

	// Unwrap the heading so we don't mess up the regression across the 0/360 deg discontinuity.
	lengthHeading = len(tempHdg)
	tempHdgUnwrapped = make([]float64, lengthHeading)
	tempRegWeights = make([]float64, lengthHeading)

	if lengthHeading > 1 {
		tempHdgUnwrapped[0] = tempHdg[0]
		tempRegWeights[0] = common.TriCubeWeight(center, halfwidth, tempHdgTime[0])
		for i := 1; i < lengthHeading; i++ {
			tempRegWeights[i] = common.TriCubeWeight(center, halfwidth, tempHdgTime[i])
			if math.Abs(tempHdg[i]-tempHdg[i-1]) < 180 {
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1]
			} else if tempHdg[i] > tempHdg[i-1] {
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1] - 360
			} else {
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1] + 360
			}
		}
	} else {
		if globalSettings.DEBUG {
			log.Printf("GPS attitude: Can't calculate turn rate with less than two points.\n")
		}
		return false
	}

	// Calculate turn rate as the slope of the weighted linear regression of unwrapped heading.
	slope, intercept, valid = common.LinRegWeighted(tempHdgTime, tempHdgUnwrapped, tempRegWeights)
	if !valid {
		log.Printf("GPS attitude: Regression error calculating turn rate")
		return false
	}
	headingAvg = slope*float64(myGPSPerfStats[index].nmeaTime) + intercept
	dh = slope // units are deg per sec

	myGPSPerfStats[index].gpsTurnRate = dh
	mySituation.muGPS.Lock()
	mySituation.GPSTurnRate = dh
	mySituation.muGPS.Unlock()

	if v_x > 20 { // reduce nuisance 'bounce' at low speeds. 20 ft/sec = 11.9 knots.
		myGPSPerfStats[index].gpsPitch = math.Atan2(v_z, v_x) * 180.0 / math.Pi
		g := 32.174 // ft/(s^2)
		omega = common.Radians(myGPSPerfStats[index].gpsTurnRate)
		a_c = v_x * omega
		myGPSPerfStats[index].gpsRoll = math.Atan2(a_c, g) * 180 / math.Pi
		myGPSPerfStats[index].gpsLoadFactor = math.Sqrt(a_c*a_c+g*g) / g
	} else {
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		myGPSPerfStats[index].gpsLoadFactor = 1
	}

	if globalSettings.DEBUG {
		buf := fmt.Sprintf("GPSAttitude,%.1f,%.2f,%s,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f\n", float64(stratuxClock.GetMilliseconds())/1000, myGPSPerfStats[index].nmeaTime, myGPSPerfStats[index].msgType, myGPSPerfStats[index].gsf, myGPSPerfStats[index].coursef, myGPSPerfStats[index].alt, myGPSPerfStats[index].vv, v_x/1.687810, headingAvg, myGPSPerfStats[index].gpsTurnRate, v_z, myGPSPerfStats[index].gpsPitch, myGPSPerfStats[index].gpsRoll, myGPSPerfStats[index].gpsLoadFactor)
		log.Printf("%s", buf)
	}

	logGPSAttitude(myGPSPerfStats[index])
	return true
}

func calculateNACp(accuracy float32) uint8 {
	ret := uint8(0)

	if accuracy < 3 {
		ret = 11
	} else if accuracy < 10 {
		ret = 10
	} else if accuracy < 30 {
		ret = 9
	} else if accuracy < 92.6 {
		ret = 8
	} else if accuracy < 185.2 {
		ret = 7
	} else if accuracy < 555.6 {
		ret = 6
	}

	return ret
}

/*
registerSituationUpdate().

	Called whenever there is a change in mySituation.
*/
func registerSituationUpdate() {
	logSituation()
	if situationUpdate != nil {
		situationUpdate.SendJSON(snapshotSituation())
	}
}

func calculateNavRate() float64 {
	length := len(myGPSPerfStats)
	tempSpeedTime := make([]float64, 0)

	for i := 1; i < length; i++ {
		dt := myGPSPerfStats[i].nmeaTime - myGPSPerfStats[i-1].nmeaTime
		if dt > 0.05 { // avoid double counting messages with same / similar timestamps
			tempSpeedTime = append(tempSpeedTime, float64(dt))
		}
	}

	var halfwidth float64
	var sampleRate float64
	dt_avg, valid := common.Mean(tempSpeedTime)
	if valid && dt_avg > 0 {
		logDbg("GPS attitude: Average delta time is %.2f s (%.1f Hz)\n", dt_avg, 1/dt_avg)
		halfwidth = 9 * dt_avg
		sampleRate = 1 / dt_avg
	} else {
		halfwidth = 3.5
		sampleRate = 0
	}

	mySituation.muGPS.Lock()
	mySituation.GPSPositionSampleRate = sampleRate
	mySituation.muGPS.Unlock()

	if halfwidth > 3.5 {
		halfwidth = 3.5 // limit calculation window to 3.5 seconds of data for 1 Hz or slower samples
	} else if halfwidth < 1.5 {
		halfwidth = 1.5 // use minimum of 1.5 seconds for sample rates faster than 5 Hz
	}

	return halfwidth
}

func appendGPSPerfStat(stat gpsPerfStats) {
	mySituation.muGPSPerformance.Lock()
	myGPSPerfStats = append(myGPSPerfStats, stat)
	lenGPSPerfStats := len(myGPSPerfStats)
	if lenGPSPerfStats > 299 { // 30 seconds @ 10 Hz for UBX
		myGPSPerfStats = myGPSPerfStats[(lenGPSPerfStats - 299):]
	}
	mySituation.muGPSPerformance.Unlock()
}

func setSystemTimeAsync(gpsTime time.Time) {
	if !common.IsRunningAsRoot() {
		return
	}
	if !systemTimeSetting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer systemTimeSetting.Store(false)
		setStr := gpsTime.Format("20060102 15:04:05.000") + " UTC"
		log.Printf("setting system time from %s to: '%s'\n", time.Now().Format("20060102 15:04:05.000"), setStr)
		if err := exec.Command("date", "-s", setStr).Run(); err != nil {
			log.Printf("Set Date failure: %s error\n", err)
		} else {
			log.Printf("Time set from GPS. Current time is %v\n", time.Now())
		}
	}()
}

/*
processNMEALine parses NMEA-0183 formatted strings against several message types.

Standard messages supported: RMC GGA VTG GSA

return is false if errors occur during parse, or if GPS position is invalid
return is true if parse occurs correctly and position is valid.
*/
func processNMEALine(l string) (sentenceUsed bool) {
	return processNMEALineLow(l, false)
}

func processNMEALineLow(l string, fakeGpsTimeToCurr bool) (sentenceUsed bool) {
	TraceLog.Record(CONTEXT_NMEA, []byte(l))

	defer func() {
		if sentenceUsed || globalSettings.DEBUG {
			registerSituationUpdate()
		}
	}()

	// Local variables for GPS attitude estimation
	thisGpsPerf := gpsPerf
	thisGpsPerf.coursef = -999.9 // default value of -999.9 indicates invalid heading to regression calculation
	thisGpsPerf.stratuxTime = stratuxClock.GetMilliseconds()

	l_valid, validNMEAcs := validateNMEAChecksum(l)
	if !validNMEAcs {
		if strings.HasPrefix(l, "$") {
			n := nmeaChecksumFails.Add(1)
			if n <= 5 || n%100 == 0 {
				log.Printf("GPS error. Invalid NMEA checksum (#%d): %s\n", n, strings.TrimSpace(l))
			}
		}
		return false
	}

	// Publish to OGN outside muGPS to avoid any lock-ordering or channel stalls.
	ognPublishNmea(l)
	x := strings.Split(l_valid, ",")

	now := stratuxClock.GetTime()
	mySituation.muGPS.Lock()
	mySituation.GPSLastValidNMEAMessageTime = now
	mySituation.GPSLastValidNMEAMessage = l
	mySituation.muGPS.Unlock()

	msgType := x[0]
	isTalker := func(suffix string) bool {
		if len(msgType) != 2+len(suffix) || !strings.HasSuffix(msgType, suffix) {
			return false
		}
		switch msgType[:2] {
		case "GN", "GP", "GL", "GA", "GB", "BD", "GQ":
			return true
		default:
			return false
		}
	}

	if isTalker("VTG") { // Ground track information.
		if len(x) < 9 { // Reduce from 10 to 9 to allow parsing by devices pre-NMEA v2.3
			return false
		}

		groundspeed, err := strconv.ParseFloat(x[5], 32) // Knots.
		if err != nil {
			return false
		}
		tc, tcErr := strconv.ParseFloat(x[1], 32)
		if tcErr != nil && groundspeed > 3 {
			return false
		}

		gtTime := stratuxClock.GetTime()
		mySituation.muGPS.Lock()
		mySituation.GPSGroundSpeed = groundspeed
		if tcErr == nil && groundspeed > 3 {
			setTrueCourse(uint16(groundspeed), tc)
			mySituation.GPSTrueCourse = float32(tc)
		}
		mySituation.GPSLastGroundTrackTime = gtTime
		mySituation.muGPS.Unlock()
		return true

	} else if isTalker("GGA") { // Position fix.
		if len(x) < 15 {
			return false
		}

		// use RMC / GGA message detection to sense "NMEA" type.
		if (getGPSDetectedType() & 0xf0) == 0 {
			orGPSDetectedType(GPS_PROTOCOL_NMEA)
		}

		// GPSFixQuality indicator.
		q, err1 := strconv.Atoi(x[6])
		if err1 != nil {
			return false
		}
		if q == 0 {
			mySituation.muGPS.Lock()
			mySituation.GPSFixQuality = 0
			mySituation.muGPS.Unlock()
			return false
		}

		// Timestamp (hhmmss or hhmmss.ss).
		if len(x[1]) < 6 {
			return false
		}
		hr, err1 := strconv.Atoi(x[1][0:2])
		min, err2 := strconv.Atoi(x[1][2:4])
		sec, err3 := strconv.ParseFloat(x[1][4:], 32)
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}
		fixSinceMidnight := float32(3600*hr+60*min) + float32(sec)

		// Latitude.
		if len(x[2]) < 4 {
			return false
		}
		hr, err1 = strconv.Atoi(x[2][0:2])
		minf, err2 := strconv.ParseFloat(x[2][2:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		lat := float32(hr) + float32(minf/60.0)
		if x[3] == "S" { // South = negative.
			lat = -lat
		}

		// Longitude.
		if len(x[4]) < 5 {
			return false
		}
		hr, err1 = strconv.Atoi(x[4][0:3])
		minf, err2 = strconv.ParseFloat(x[4][3:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		lng := float32(hr) + float32(minf/60.0)
		if x[5] == "W" { // West = negative.
			lng = -lng
		}

		// Altitude.
		alt, err1 := strconv.ParseFloat(x[9], 32)
		if err1 != nil {
			return false
		}
		altMSL := float32(alt * 3.28084) // Convert to feet.

		// Geoid separation (Sep = HAE - MSL). Default to 0 if omitted by receiver.
		var geoidSep float64
		if strings.TrimSpace(x[11]) != "" {
			if parsedSep, sepErr := strconv.ParseFloat(x[11], 32); sepErr == nil {
				geoidSep = parsedSep
			}
		}
		geoidSepFt := float32(geoidSep * 3.28084)
		haeFt := geoidSepFt + altMSL

		// Number of satellites in use (fallback when GSA is absent).
		ggaSats, ggaSatsErr := strconv.Atoi(x[7])

		fixLocalTime := stratuxClock.GetTime()
		mySituation.muGPS.Lock()
		mySituation.GPSFixQuality = uint8(q) // 1 = 3D GPS; 2 = DGPS (SBAS /WAAS); 4/5 = RTK
		mySituation.GPSLastFixSinceMidnightUTC = fixSinceMidnight
		mySituation.GPSLatitude = lat
		mySituation.GPSLongitude = lng
		mySituation.GPSAltitudeMSL = altMSL
		mySituation.GPSGeoidSep = geoidSepFt
		mySituation.GPSHeightAboveEllipsoid = haeFt
		mySituation.GPSLastFixLocalTime = fixLocalTime
		if ggaSatsErr == nil && ggaSats > 0 && mySituation.GPSSatellites == 0 {
			mySituation.GPSSatellites = uint16(ggaSats)
		}
		mySituation.muGPS.Unlock()

		thisGpsPerf.nmeaTime = fixSinceMidnight
		thisGpsPerf.alt = altMSL
		thisGpsPerf.msgType = x[0]
		appendGPSPerfStat(thisGpsPerf)

		return true

	} else if isTalker("RMC") { // Recommended Minimum data.
		if len(x) < 11 {
			return false
		}

		// use RMC / GGA message detection to sense "NMEA" type.
		if (getGPSDetectedType() & 0xf0) == 0 {
			orGPSDetectedType(GPS_PROTOCOL_NMEA)
		}

		if x[2] != "A" { // invalid fix
			return false
		}

		// Timestamp (hhmmss or hhmmss.ss).
		if len(x[1]) < 6 {
			return false
		}
		hr, err1 := strconv.Atoi(x[1][0:2])
		min, err2 := strconv.Atoi(x[1][2:4])
		sec, err3 := strconv.ParseFloat(x[1][4:], 32)
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}
		fixSinceMidnight := float32(3600*hr+60*min) + float32(sec)

		var hasValidGPSTime bool
		var gpsTime time.Time
		var gpsTimeStratux time.Time
		if len(x[9]) == 6 {
			// Date of Fix, i.e 191115 = 19 November 2015 UTC field 9
			gpsTimeStr := fmt.Sprintf("%s %02d:%02d:%06.3f", x[9], hr, min, sec)
			parsedTime, parseErr := time.Parse("020106 15:04:05.000", gpsTimeStr)
			parsedTime = parsedTime.Add(gpsTimeOffsetPpsMs) // rough estimate for PPS offset

			if fakeGpsTimeToCurr {
				// Used during trace replay: pretend gps time equals current time, so stratux accepts the NMEA as current
				parsedTime = time.Now().UTC()
				parseErr = nil
			}

			if parseErr == nil && parsedTime.After(time.Date(2016, time.January, 0, 0, 0, 0, 0, time.UTC)) { // Ignore dates before 2016-JAN-01.
				hasValidGPSTime = true
				gpsTime = parsedTime
				gpsTimeStratux = stratuxClock.GetTime()
				stratuxClock.SetRealTimeReference(gpsTime)
				if diff := time.Since(gpsTime); diff > 300*time.Millisecond || diff < -300*time.Millisecond {
					setSystemTimeAsync(gpsTime)
				}
				TraceLog.OnTimestamp(gpsTime)
			}
		}

		// Latitude.
		if len(x[3]) < 4 {
			return false
		}
		hr, err1 = strconv.Atoi(x[3][0:2])
		minf, err2 := strconv.ParseFloat(x[3][2:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		lat := float32(hr) + float32(minf/60.0)
		if x[4] == "S" { // South = negative.
			lat = -lat
		}

		// Longitude.
		if len(x[5]) < 5 {
			return false
		}
		hr, err1 = strconv.Atoi(x[5][0:3])
		minf, err2 = strconv.ParseFloat(x[5][3:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		lng := float32(hr) + float32(minf/60.0)
		if x[6] == "W" { // West = negative.
			lng = -lng
		}

		// ground speed in kts (field 7)
		groundspeed, err := strconv.ParseFloat(x[7], 32)
		if err != nil {
			return false
		}

		// ground track "True" (field 8)
		tc, err := strconv.ParseFloat(x[8], 32)
		if err != nil && groundspeed > 3 { // some receivers return null COG at low speeds. Need to ignore this condition.
			return false
		}

		fixLocalTime := stratuxClock.GetTime()
		mySituation.muGPS.Lock()
		mySituation.GPSLastFixSinceMidnightUTC = fixSinceMidnight
		if hasValidGPSTime {
			mySituation.GPSLastGPSTimeStratuxTime = gpsTimeStratux
			mySituation.GPSTime = gpsTime
		}
		mySituation.GPSLatitude = lat
		mySituation.GPSLongitude = lng
		mySituation.GPSLastFixLocalTime = fixLocalTime
		mySituation.GPSGroundSpeed = groundspeed
		if err == nil && groundspeed > 3 {
			setTrueCourse(uint16(groundspeed), tc)
			mySituation.GPSTrueCourse = float32(tc)
		}
		mySituation.GPSLastGroundTrackTime = fixLocalTime
		mySituation.muGPS.Unlock()

		thisGpsPerf.nmeaTime = fixSinceMidnight
		thisGpsPerf.gsf = float32(groundspeed)
		if err == nil && groundspeed > 3 {
			thisGpsPerf.coursef = float32(tc)
		} else {
			thisGpsPerf.coursef = -999.9
		}
		thisGpsPerf.msgType = x[0]
		appendGPSPerfStat(thisGpsPerf)

		setDataLogTimeWithGPS(snapshotSituation())
		return true

	} else if isTalker("GSA") { // Satellite data.
		if len(x) < 18 {
			return false
		}

		// field 2: solution type (1 = no solution; 2 = 2D fix, 3 = 3D fix)
		if (x[2] == "") || (x[2] == "1") {
			return false
		}

		// NMEA 4.10+ System ID in field 18 (1=GPS, 2=GLONASS, 3=Galileo, 4=BeiDou, 5=QZSS)
		gsaTalker := x[0]
		if len(x) >= 19 {
			switch strings.TrimSpace(x[18]) {
			case "1":
				gsaTalker = "GP"
			case "2":
				gsaTalker = "GL"
			case "3":
				gsaTalker = "GA"
			case "4":
				gsaTalker = "GB"
			case "5":
				gsaTalker = "GQ"
			}
		}

		satTime := stratuxClock.GetTime()
		mySituation.muSatellite.Lock()
		for _, svtxt := range x[3:15] {
			sv, err := strconv.Atoi(svtxt)
			if err != nil {
				continue
			}
			svType, svStr := classifySatelliteWithTalker(gsaTalker, sv)

			var thisSatellite SatelliteInfo
			if val, ok := Satellites[svStr]; ok {
				thisSatellite = val
			} else {
				thisSatellite.SatelliteID = svStr
				if sv >= 0 && sv <= math.MaxUint16 {
					thisSatellite.SatelliteNMEA = uint16(sv)
				}
				thisSatellite.Type = svType
			}
			thisSatellite.InSolution = true
			thisSatellite.TimeLastSolution = satTime
			thisSatellite.TimeLastSeen = satTime    // implied, since this satellite is used in the position solution
			thisSatellite.TimeLastTracked = satTime // implied, since this satellite is used in the position solution

			Satellites[thisSatellite.SatelliteID] = thisSatellite
		}
		sats, tracked, seen := updateConstellationCounts()
		mySituation.muSatellite.Unlock()

		mySituation.muGPS.Lock()
		mySituation.GPSSatellites = sats
		mySituation.GPSSatellitesTracked = tracked
		mySituation.GPSSatellitesSeen = seen
		lastAccTime := mySituation.GPSLastAccuracyTime
		fixQuality := mySituation.GPSFixQuality
		mySituation.muGPS.Unlock()

		// Prefer accuracy from G?GST. Only if never received or stale (>10s), estimate from hdop/vdop.
		if lastAccTime.IsZero() || stratuxClock.Since(lastAccTime) > 10*time.Second {
			hdop, err1 := strconv.ParseFloat(x[16], 32)
			if err1 != nil {
				return true
			}
			vdop, vdopErr := strconv.ParseFloat(x[17], 32)

			detType := getGPSDetectedType() & 0x0f
			var hacc float32
			if fixQuality == 2 { // Rough 95% confidence estimate for SBAS solution
				if detType == GPS_TYPE_UBX9 || detType == GPS_TYPE_UBX10 {
					hacc = float32(hdop * 3.0) // ublox 9/10
				} else {
					hacc = float32(hdop * 4.0) // ublox 6/7/8
				}
			} else { // Rough 95% confidence estimate non-SBAS solution
				if detType == GPS_TYPE_UBX9 || detType == GPS_TYPE_UBX10 {
					hacc = float32(hdop * 4.0) // ublox 9/10
				} else {
					hacc = float32(hdop * 5.0) // ublox 6/7/8
				}
			}
			nacp := calculateNACp(hacc)
			vacc := float32(999999)
			if vdopErr == nil && vdop > 0 {
				vacc = float32(vdop * 5) // rough estimate for 95% confidence
			}

			mySituation.muGPS.Lock()
			mySituation.GPSHorizontalAccuracy = hacc
			mySituation.GPSNACp = nacp
			mySituation.GPSVerticalAccuracy = vacc
			mySituation.muGPS.Unlock()
		}

		return true
	}

	if isTalker("GST") {
		if len(x) < 9 {
			return false
		}

		// Ignore GST accuracy reports when there is no active fix (u-blox emits huge/bogus values on no-fix).
		mySituation.muGPS.Lock()
		fixQuality := mySituation.GPSFixQuality
		mySituation.muGPS.Unlock()
		if fixQuality == 0 {
			return false
		}

		// $GNGST,205246.00,1.19,0.02,0.01,-2.4501,0.02,0.01,0.03*5B
		// Care: GNGST uses 1-sigma (68%) deviation. We use 2-sigma (~95%)
		stdDevLat, err := strconv.ParseFloat(x[6], 32)
		if err != nil {
			return false
		}
		stdDevLon, err := strconv.ParseFloat(x[7], 32)
		if err != nil {
			return false
		}
		stdDevAlt, err := strconv.ParseFloat(x[8], 32)
		if err != nil {
			return false
		}
		hacc := float32(2 * math.Sqrt(stdDevLat*stdDevLat+stdDevLon*stdDevLon))
		vacc := float32(2 * stdDevAlt)
		nacp := calculateNACp(hacc)
		accTime := stratuxClock.GetTime()

		mySituation.muGPS.Lock()
		mySituation.GPSLastAccuracyTime = accTime
		mySituation.GPSHorizontalAccuracy = hacc
		mySituation.GPSVerticalAccuracy = vacc
		mySituation.GPSNACp = nacp
		mySituation.muGPS.Unlock()
		return true
	}

	if isTalker("GSV") { // Satellites in view message.
		if len(x) < 4 {
			return false
		}

		// field 1 = number of GSV messages of this type
		msgNum, err := strconv.Atoi(x[1])
		if err != nil {
			return false
		}

		// field 2 = index of this GSV message
		msgIndex, err := strconv.Atoi(x[2])
		if err != nil {
			return false
		}

		// field 4-7 = repeating block with satellite id, elevation, azimuth, and signal strength (Cno)
		lenGSV := len(x)
		satsThisMsg := (lenGSV - 4) / 4

		logDbg("%s message [%d of %d] is %v fields long and describes %v satellites\n", x[0], msgIndex, msgNum, lenGSV, satsThisMsg)

		mySituation.muGPS.Lock()
		fixQuality := mySituation.GPSFixQuality
		mySituation.muGPS.Unlock()

		var sats, tracked, seen uint16
		satTime := stratuxClock.GetTime()

		mySituation.muSatellite.Lock()
		for i := 0; i < satsThisMsg; i++ {
			sv, err := strconv.Atoi(x[4+4*i]) // sv number
			if err != nil {
				continue
			}

			svType, svStr := classifySatelliteWithTalker(x[0], sv)

			elev, err := strconv.Atoi(x[5+4*i]) // elevation
			if err != nil {                     // some firmwares leave this blank if there's no position fix. Represent as -999.
				elev = -999
			}
			az, err := strconv.Atoi(x[6+4*i]) // azimuth
			if err != nil {                   // Represent invalid as -999.
				az = -999
			}
			cno, cnoErr := strconv.Atoi(x[7+4*i]) // signal
			if cnoErr != nil {
				cno = -99
			} else if cno > 127 {
				cno = 127
			}

			var thisSatellite SatelliteInfo
			if val, ok := Satellites[svStr]; ok {
				thisSatellite = val
			} else {
				thisSatellite.SatelliteID = svStr
				if sv >= 0 && sv <= math.MaxUint16 {
					thisSatellite.SatelliteNMEA = uint16(sv)
				}
				thisSatellite.Type = svType
			}
			thisSatellite.TimeLastTracked = satTime
			thisSatellite.Elevation = int16(elev)
			thisSatellite.Azimuth = int16(az)
			if cnoErr != nil {
				thisSatellite.InSolution = false
			} else if cno > 0 {
				thisSatellite.TimeLastSeen = satTime
			}
			thisSatellite.Signal = int8(cno)

			// Workaround for GSA 12-sv limitation for SBAS satellites
			if thisSatellite.Type == SAT_TYPE_SBAS {
				if fixQuality == 2 {
					if thisSatellite.Signal > 16 {
						thisSatellite.InSolution = true
						thisSatellite.TimeLastSolution = satTime
					}
				} else {
					thisSatellite.InSolution = false
				}
			}

			if globalSettings.DEBUG {
				inSolnStr := " "
				if thisSatellite.InSolution {
					inSolnStr = "+"
				}
				log.Printf("GSV: Satellite %s%s at index %d. Type = %d, NMEA-ID = %d, Elev = %d, Azimuth = %d, Cno = %d\n", inSolnStr, svStr, i, svType, sv, elev, az, cno)
			}

			Satellites[thisSatellite.SatelliteID] = thisSatellite
		}
		sats, tracked, seen = updateConstellationCounts()
		mySituation.muSatellite.Unlock()

		mySituation.muGPS.Lock()
		mySituation.GPSSatellites = sats
		mySituation.GPSSatellitesTracked = tracked
		mySituation.GPSSatellitesSeen = seen
		mySituation.muGPS.Unlock()

		return true
	}

	// OGN Tracker pressure data:
	// $POGNB,22.0,+29.1,100972.3,3.8,+29.4,+87.2,-0.04,+32.6,*6B
	if x[0] == "POGNB" {
		if len(x) < 8 {
			return false
		}

		pressureAlt, err := strconv.ParseFloat(x[5], 32)
		if err != nil {
			return false
		}

		vspeed, err := strconv.ParseFloat(x[7], 32)
		if err != nil {
			return false
		}

		tempPressValid := isTempPressValid()
		mySituation.muBaro.Lock()
		if !tempPressValid || mySituation.BaroSourceType != BARO_TYPE_BMP280 {
			mySituation.BaroPressureAltitude = float32(pressureAlt * 3.28084) // meters to feet
			mySituation.BaroVerticalSpeed = float32(vspeed * 196.85)          // m/s in ft/min
			mySituation.BaroLastMeasurementTime = stratuxClock.GetTime()
			mySituation.BaroSourceType = BARO_TYPE_OGNTRACKER
		}
		mySituation.muBaro.Unlock()
		return true
	}

	// Handling of all trackers via drivers. Care: serialPort might be nil during baud detection
	sp := getSerialPort()
	curTracker := getDetectedTracker()
	if curTracker != nil && sp != nil {
		if curTracker.onNmea(sp, x) {
			return true
		}
	} else if sp != nil {
		handled := false
		for _, tracker := range trackerDrivers {
			handled = tracker.onNmea(sp, x) || handled
			if tracker.isDetected() {
				// First detected tracker. Write transient config and read back
				setDetectedTracker(tracker)
				setGPSDetectedType(tracker.getGpsHardwareType())
				gpsTimeOffsetPpsMs = tracker.gpsTimeOffsetPps()
				tracker.requestTrackerConfig(sp)
				timelimit := stratuxClock.GetTime().Add(5 * time.Second)
				capturedTracker := tracker
				go func() {
					for stratuxClock.GetTime().Before(timelimit) {
						curSP := getSerialPort()
						if curSP == nil || getDetectedTracker() != capturedTracker {
							break
						}
						if capturedTracker.isConfigRead() {
							capturedTracker.writeInitialConfig(curSP)
							break
						}
						time.Sleep(20 * time.Millisecond)
					}
				}()
				break
			}
		}

		if handled {
			return true
		}
	}

	// Only evaluate PGRMZ for SoftRF/Flarm, where we know that it is standard barometric pressure.
	// $PGRMZ,1089,f,3*2B
	if x[0] == "PGRMZ" && ((getGPSDetectedType()&0x0f) == GPS_TYPE_SERIAL || (getGPSDetectedType()&0x0f) == GPS_TYPE_SOFTRF_DONGLE || (getGPSDetectedType()&0x0f) == GPS_TYPE_SOFTRF) {
		if len(x) < 3 {
			return false
		}
		// Assume pressure altitude in PGRMZ if we don't have any other baro (SoftRF style)
		pressureAlt, err := strconv.ParseFloat(x[1], 32)
		if err != nil {
			return false
		}
		unit := x[2]
		if unit == "m" {
			pressureAlt *= 3.28084
		}
		// Prefer internal sensor and OGN tracker over this...
		tempPressValid := isTempPressValid()
		updated := false
		mySituation.muBaro.Lock()
		if !tempPressValid || (mySituation.BaroSourceType != BARO_TYPE_BMP280 && mySituation.BaroSourceType != BARO_TYPE_OGNTRACKER) {
			mySituation.BaroPressureAltitude = float32(pressureAlt)
			mySituation.BaroLastMeasurementTime = stratuxClock.GetTime()
			mySituation.BaroSourceType = BARO_TYPE_NMEA
			updated = true
		}
		mySituation.muBaro.Unlock()
		if updated {
			return true
		}
	}

	// Flarm NMEA traffic data
	if x[0] == "PFLAU" || x[0] == "PFLAA" {
		parseFlarmNmeaMessage(x)
		return true
	}

	// If we've gotten this far, the message isn't one that we can use.
	return false
}

var gnssBaroAltDiffs = make(map[int]int)

// Maps 100ft bands to gnssBaroAltDiffs of known traffic.
// This will then be used to estimate our own baro altitude from GNSS if we don't have a pressure sensor connected...
func baroAltGuesser() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		// Create linear regression from GnssBaroAltDiffs we have confirmed already
		var alts, diffs []float64
		for k, v := range gnssBaroAltDiffs {
			alts = append(alts, float64(k*100))
			diffs = append(diffs, float64(v))
		}
		slope, intercept, valid := common.LinReg(alts, diffs)

		trafficMutex.Lock()
		for _, ti := range traffic {
			if ti.ReceivedMsgs < 30 || ti.SignalLevel < -28 || ti.SignalLevel > -3 {
				continue // Make sure it is actually a confirmed target
			}
			if stratuxClock.Since(ti.Last_GnssDiff) > 1*time.Second || ti.Alt <= 1 || stratuxClock.Since(ti.Last_alt) > 1*time.Second {
				continue
			}

			bucket := int(ti.Alt / 100)
			if bucket <= 0 {
				continue
			}

			if len(gnssBaroAltDiffs) >= 30 && valid {
				estimatedDiff := float64(ti.Alt)*slope + intercept
				if math.Abs(float64(ti.GnssDiffFromBaroAlt)-estimatedDiff) > 400 {
					fmt.Printf("Ignoring %d, alt=%d for baro computation. Expected GnssDiff: %f, Received: %d \n", ti.Icao_addr, ti.Alt, estimatedDiff, ti.GnssDiffFromBaroAlt)
					continue
				}
			}

			if val, ok := gnssBaroAltDiffs[bucket]; ok {
				gnssBaroAltDiffs[bucket] = (val*59 + int(ti.GnssDiffFromBaroAlt)) / 60
			} else {
				gnssBaroAltDiffs[bucket] = int(ti.GnssDiffFromBaroAlt)
			}
		}
		trafficMutex.Unlock()

		if len(gnssBaroAltDiffs) < 30 {
			continue // not enough data
		}
		tempPressValid := isTempPressValid()
		mySituation.muBaro.Lock()
		baroSource := mySituation.BaroSourceType
		baroAlt := mySituation.BaroPressureAltitude
		mySituation.muBaro.Unlock()

		if isGPSValid() && (!tempPressValid || baroSource == BARO_TYPE_NONE || baroSource == BARO_TYPE_ADSBESTIMATE) {
			mySituation.muGPS.Lock()
			myAlt := mySituation.GPSAltitudeMSL
			gpsHAE := mySituation.GPSHeightAboveEllipsoid
			mySituation.muGPS.Unlock()

			if tempPressValid {
				myAlt = baroAlt
			}
			alts := make([]float64, 0, len(gnssBaroAltDiffs))
			diffs := make([]float64, 0, len(gnssBaroAltDiffs))
			weights := make([]float64, 0, len(gnssBaroAltDiffs))
			for k, v := range gnssBaroAltDiffs {
				bucketAlt := float64(k*100 + 50)
				alts = append(alts, bucketAlt)
				diffs = append(diffs, float64(v))
				weight := math.Abs(float64(myAlt) - bucketAlt)
				if weight == 0 {
					weight = 1
				} else {
					weight = math.Min(1/(weight/1000), 1)
				}
				weights = append(weights, weight*5)
			}
			if len(gnssBaroAltDiffs) >= 2 {
				slope, intercept, valid := common.LinRegWeighted(alts, diffs, weights)
				if valid {
					gnssBaroDiff := float64(myAlt)*slope + intercept
					mySituation.muBaro.Lock()
					mySituation.BaroLastMeasurementTime = stratuxClock.GetTime()
					mySituation.BaroPressureAltitude = gpsHAE - float32(gnssBaroDiff)
					mySituation.BaroSourceType = BARO_TYPE_ADSBESTIMATE
					mySituation.muBaro.Unlock()
				}
			}
		}
	}
}

func gpsSerialReader() {
	sp := getSerialPort()
	if sp == nil {
		readyToInitGPS.Store(true)
		return
	}
	devPath := getSerialDevicePath()
	defer func() {
		setSerialPort(nil)
		setDetectedTracker(nil)
		sp.Close()
		setGPSConnected(false)
		readyToInitGPS.Store(true)
	}()
	readyToInitGPS.Store(false)

	i := 0
	consecutiveTimeouts := 0
	buf := make([]byte, 1024)
	var lineBuf strings.Builder

	for getGPSConnected() && getGPSEnabled() {
		n, err := sp.Read(buf)
		if n > 0 {
			consecutiveTimeouts = 0
			lineBuf.Write(buf[:n])
			for {
				data := lineBuf.String()
				nl := strings.IndexByte(data, '\n')
				if nl < 0 {
					if lineBuf.Len() > 4096 {
						lineBuf.Reset()
					}
					break
				}
				line := data[:nl]
				rem := data[nl+1:]
				lineBuf.Reset()
				lineBuf.WriteString(rem)

				i++
				if globalSettings.DEBUG && i%100 == 0 {
					log.Printf("gpsSerialReader() loop iteration i=%d\n", i)
				}
				// Split the line into individual NMEA messages.
				for _, msg := range strings.Split(line, "$") {
					if len(msg) == 0 {
						continue
					}
					msg = "$" + msg
					if !processNMEALine(msg) && globalSettings.DEBUG {
						fmt.Printf("processNMEALine() exited early -- %s\n", msg)
					}
				}
			}
		}
		if err != nil {
			// On Linux with VMIN=0/VTIME>0, a ReadTimeout returns (0, io.EOF).
			// Allow up to 3 consecutive timeouts (~7.5s) before tearing down and resetting the GNSS chip.
			if errors.Is(err, io.EOF) {
				consecutiveTimeouts++
				if devPath != "" {
					if _, statErr := os.Stat(devPath); statErr != nil {
						log.Printf("GPS device %s disappeared: %s\n", devPath, statErr.Error())
						break
					}
				}
				if consecutiveTimeouts >= 3 {
					log.Printf("GPS serial read timed out %d times on %s; reinitializing\n", consecutiveTimeouts, devPath)
					break
				}
				continue
			}
			log.Printf("GPS serial read error on %s: %s\n", devPath, err.Error())
			break
		}
	}

	if globalSettings.DEBUG {
		log.Printf("Exiting gpsSerialReader() after i=%d loops\n", i)
	}
}

func makeAHRSSimReport() {
	mySituation.muAttitude.Lock()
	hdg := float32(mySituation.AHRSGyroHeading)
	pitch := float32(mySituation.AHRSPitch)
	roll := float32(mySituation.AHRSRoll)
	mySituation.muAttitude.Unlock()

	msg := createXPlaneAttitudeMsg(hdg, pitch, roll)
	sendMsg(msg, NETWORK_AHRS_FFSIM, 100*time.Millisecond, 1)
}

/*
	ForeFlight "AHRS Message".
	Sends AHRS information to ForeFlight.
*/

func makeFFAHRSMessage() {
	msg := make([]byte, 12)
	msg[0] = 0x65 // Message type "ForeFlight".
	msg[1] = 0x01 // AHRS message identifier.

	// Values if invalid
	pitch := int16(0x7FFF)
	roll := int16(0x7FFF)
	hdg := uint16(0xFFFF)
	ias := uint16(0xFFFF)
	tas := uint16(0xFFFF)

	if isAHRSValid() {
		mySituation.muAttitude.Lock()
		p := mySituation.AHRSPitch
		r := mySituation.AHRSRoll
		mySituation.muAttitude.Unlock()

		if !isAHRSInvalidValue(p) {
			pitch = common.RoundToInt16(p * 10)
		}
		if !isAHRSInvalidValue(r) {
			roll = common.RoundToInt16(r * 10)
		}
	}

	// Roll.
	msg[2] = byte((roll >> 8) & 0xFF)
	msg[3] = byte(roll & 0xFF)

	// Pitch.
	msg[4] = byte((pitch >> 8) & 0xFF)
	msg[5] = byte(pitch & 0xFF)

	// Heading.
	msg[6] = byte((hdg >> 8) & 0xFF)
	msg[7] = byte(hdg & 0xFF)

	// Indicated Airspeed.
	msg[8] = byte((ias >> 8) & 0xFF)
	msg[9] = byte(ias & 0xFF)

	// True Airspeed.
	msg[10] = byte((tas >> 8) & 0xFF)
	msg[11] = byte(tas & 0xFF)

	sendMsg(prepareMessage(msg), NETWORK_AHRS_GDL90, 200*time.Millisecond, 3)
}

/*
	ffAttitudeSender()
	 Send AHRS message in FF format every 200ms.
*/

func ffAttitudeSender() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		<-ticker.C
		makeFFAHRSMessage()
	}
}

func makeAHRSGDL90Report() {
	msg := make([]byte, 24)
	msg[0] = 0x4c
	msg[1] = 0x45
	msg[2] = 0x01
	msg[3] = 0x01

	// Values if invalid
	pitch := int16(0x7FFF)
	roll := int16(0x7FFF)
	hdg := int16(0x7FFF)
	slip_skid := int16(0x7FFF)
	yaw_rate := int16(0x7FFF)
	g := int16(0x7FFF)
	airspeed := int16(0x7FFF) // Can add this once we can read airspeed
	palt := uint16(0xFFFF)
	vs := int16(0x7FFF)
	if isAHRSValid() {
		mySituation.muAttitude.Lock()
		p := mySituation.AHRSPitch
		r := mySituation.AHRSRoll
		h := mySituation.AHRSGyroHeading
		ss := mySituation.AHRSSlipSkid
		tr := mySituation.AHRSTurnRate
		gl := mySituation.AHRSGLoad
		mySituation.muAttitude.Unlock()

		if !isAHRSInvalidValue(p) {
			pitch = common.RoundToInt16(p * 10)
		}
		if !isAHRSInvalidValue(r) {
			roll = common.RoundToInt16(r * 10)
		}
		if !isAHRSInvalidValue(h) {
			hdg = common.RoundToInt16(h * 10)
		}
		if !isAHRSInvalidValue(ss) {
			slip_skid = common.RoundToInt16(-ss * 10)
		}
		if !isAHRSInvalidValue(tr) {
			yaw_rate = common.RoundToInt16(tr * 10)
		}
		if !isAHRSInvalidValue(gl) {
			g = common.RoundToInt16(gl * 10)
		}
	}
	if isTempPressValid() {
		mySituation.muBaro.Lock()
		baroAlt := mySituation.BaroPressureAltitude
		baroVS := mySituation.BaroVerticalSpeed
		mySituation.muBaro.Unlock()

		palt = uint16(baroAlt + 5000.5)
		vs = common.RoundToInt16(float64(baroVS))
	}

	// Roll.
	msg[4] = byte((roll >> 8) & 0xFF)
	msg[5] = byte(roll & 0xFF)

	// Pitch.
	msg[6] = byte((pitch >> 8) & 0xFF)
	msg[7] = byte(pitch & 0xFF)

	// Heading.
	msg[8] = byte((hdg >> 8) & 0xFF)
	msg[9] = byte(hdg & 0xFF)

	// Slip/skid.
	msg[10] = byte((slip_skid >> 8) & 0xFF)
	msg[11] = byte(slip_skid & 0xFF)

	// Yaw rate.
	msg[12] = byte((yaw_rate >> 8) & 0xFF)
	msg[13] = byte(yaw_rate & 0xFF)

	// "G".
	msg[14] = byte((g >> 8) & 0xFF)
	msg[15] = byte(g & 0xFF)

	// Indicated Airspeed
	msg[16] = byte((airspeed >> 8) & 0xFF)
	msg[17] = byte(airspeed & 0xFF)

	// Pressure Altitude
	msg[18] = byte((palt >> 8) & 0xFF)
	msg[19] = byte(palt & 0xFF)

	// Vertical Speed
	msg[20] = byte((vs >> 8) & 0xFF)
	msg[21] = byte(vs & 0xFF)

	// Reserved
	msg[22] = 0x7F
	msg[23] = 0xFF

	sendMsg(prepareMessage(msg), NETWORK_AHRS_GDL90, 100*time.Millisecond, 3)
}

func gpsAttitudeSender() {
	timer := time.NewTicker(100 * time.Millisecond) // ~10Hz update.
	defer timer.Stop()
	for {
		<-timer.C
		gpsConn := getGPSConnected()
		globalStatusMutex.RLock()
		imuConn := globalStatus.IMUConnected
		globalStatusMutex.RUnlock()
		if !(gpsConn || imuConn) {
			mySituation.muGPSPerformance.Lock()
			myGPSPerfStats = make([]gpsPerfStats, 0) // reinitialize statistics on disconnect / reconnect
			mySituation.muGPSPerformance.Unlock()
		} else {
			mySituation.muGPSPerformance.Lock()
			calculateNavRate()
			mySituation.muGPSPerformance.Unlock()
		}

		for {
			globalSettingsMutex.RLock()
			imuEn := globalSettings.IMU_Sensor_Enabled
			gpsEn := globalSettings.GPS_Enabled
			globalSettingsMutex.RUnlock()
			globalStatusMutex.RLock()
			imuC := globalStatus.IMUConnected
			gpsC := globalStatus.GPS_connected
			globalStatusMutex.RUnlock()
			if !(!(imuEn && imuC) && (gpsEn && gpsC)) {
				break
			}
			<-timer.C

			if !isGPSValid() || !calcGPSAttitude() {
				logDbg("Couldn't calculate GPS-based attitude statistics\n")
			} else {
				mySituation.muGPSPerformance.Lock()
				index := len(myGPSPerfStats) - 1
				var hasUpdate bool
				var pitch, roll float64
				if index > 1 {
					pitch = myGPSPerfStats[index].gpsPitch
					roll = myGPSPerfStats[index].gpsRoll
					hasUpdate = true
				}
				mySituation.muGPSPerformance.Unlock()

				if hasUpdate {
					mySituation.muGPS.Lock()
					trueCourse := float64(mySituation.GPSTrueCourse)
					mySituation.muGPS.Unlock()

					mySituation.muAttitude.Lock()
					mySituation.AHRSPitch = pitch
					mySituation.AHRSRoll = roll
					mySituation.AHRSGyroHeading = trueCourse
					mySituation.AHRSLastAttitudeTime = stratuxClock.GetTime()
					mySituation.muAttitude.Unlock()

					makeAHRSGDL90Report()
					makeAHRSSimReport()
					makeAHRSLevilReport()
				}
			}
		}
	}
}

/*
updateConstellationCounts(): Periodic cleanup and statistics calculation for 'Satellites'

	data structure. Calling functions must protect this in mySituation.muSatellite.
*/
func updateConstellationCounts() (uint16, uint16, uint16) {
	var sats, tracked, seen uint16
	for svStr, thisSatellite := range Satellites {
		if stratuxClock.Since(thisSatellite.TimeLastTracked) > 10*time.Second { // remove stale satellites if they haven't been tracked for 10 seconds
			delete(Satellites, svStr)
		} else { // satellite almanac data is "fresh" even if it isn't being received.
			tracked++
			if thisSatellite.Signal > 0 {
				seen++
			}
			if stratuxClock.Since(thisSatellite.TimeLastSolution) > 5*time.Second {
				thisSatellite.InSolution = false
				Satellites[svStr] = thisSatellite
			}
			if thisSatellite.InSolution {
				sats++
			}
		}
	}
	return sats, tracked, seen
}

func updateConstellation() {
	sats, tracked, seen := updateConstellationCounts()
	mySituation.muGPS.Lock()
	mySituation.GPSSatellites = sats
	mySituation.GPSSatellitesTracked = tracked
	mySituation.GPSSatellitesSeen = seen
	mySituation.muGPS.Unlock()
}

func isGPSConnected() bool {
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		lastNMEA := mySituation.GPSLastValidNMEAMessageTime
		mySituation.muGPS.Unlock()
		if !lastNMEA.IsZero() && stratuxClock.Since(lastNMEA) < 5*time.Second {
			return true
		}
	} else if !mySituation.GPSLastValidNMEAMessageTime.IsZero() && stratuxClock.Since(mySituation.GPSLastValidNMEAMessageTime) < 5*time.Second {
		return true
	}
	// Grace window right after opening a single-baud serial port before the first NMEA arrives.
	if openedAt := getGPSPortOpenedAt(); !openedAt.IsZero() && stratuxClock.Since(openedAt) < 4*time.Second {
		return true
	}
	return false
}

/*
isGPSValid returns true only if a valid position fix has been seen in the last 3 seconds,
and if the GPS subsystem has recently detected a GPS device.

If false, 'GPSFixQuality' is set to 0 ("No fix"), as is the number of satellites in solution.
*/

func isGPSValid() bool {
	gpsConn := getGPSConnected()
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		defer mySituation.muGPS.Unlock()
	}
	if !mySituation.GPSLastFixLocalTime.IsZero() && stratuxClock.Since(mySituation.GPSLastFixLocalTime) < 3*time.Second && gpsConn && mySituation.GPSFixQuality > 0 {
		return true
	}
	if mySituation.GPSFixQuality != 0 ||
		mySituation.GPSSatellites != 0 ||
		mySituation.GPSNACp != 0 ||
		mySituation.GPSHorizontalAccuracy != 999999 ||
		mySituation.GPSVerticalAccuracy != 999999 {
		mySituation.GPSFixQuality = 0
		mySituation.GPSSatellites = 0
		mySituation.GPSHorizontalAccuracy = 999999
		mySituation.GPSVerticalAccuracy = 999999
		mySituation.GPSNACp = 0
	}
	return false
}

/*
isGPSGroundTrackValid returns true only if a valid ground track was obtained in the last 3 seconds,
and if NACp >= 9.
*/

func isGPSGroundTrackValid() bool {
	if !isGPSValid() {
		return false
	}
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		defer mySituation.muGPS.Unlock()
	}
	return mySituation.GPSHorizontalAccuracy > 0 && mySituation.GPSHorizontalAccuracy < 30
}

func isGPSClockValid() bool {
	if mySituation.muGPS != nil {
		mySituation.muGPS.Lock()
		defer mySituation.muGPS.Unlock()
	}
	return !mySituation.GPSLastGPSTimeStratuxTime.IsZero() && stratuxClock.Since(mySituation.GPSLastGPSTimeStratuxTime).Seconds() < 15
}

func isAHRSValid() bool {
	// If attitude information gets to be over 1 second old, declare invalid.
	// If no GPS then we won't use or send attitude information.
	if !isGPSValid() {
		return false
	}
	if mySituation.muAttitude != nil {
		mySituation.muAttitude.Lock()
		defer mySituation.muAttitude.Unlock()
	}
	return !mySituation.AHRSLastAttitudeTime.IsZero() && stratuxClock.Since(mySituation.AHRSLastAttitudeTime).Seconds() < 1
}

func isTempPressValid() bool {
	if mySituation.muBaro != nil {
		mySituation.muBaro.Lock()
		defer mySituation.muBaro.Unlock()
	}
	return !mySituation.BaroLastMeasurementTime.IsZero() && stratuxClock.Since(mySituation.BaroLastMeasurementTime).Seconds() < 15
}

func pollGPS() {
	readyToInitGPS.Store(true)
	timer := time.NewTicker(4 * time.Second)
	defer timer.Stop()
	go gpsAttitudeSender()
	go ffAttitudeSender()
	for {
		<-timer.C
		// GPS enabled, was not connected previously?
		if getGPSEnabled() && !getGPSConnected() && readyToInitGPS.Load() {
			ok := initGPSSerial()
			setGPSConnected(ok)
			if ok && (getGPSDetectedType()&0x0f) != GPS_TYPE_NETWORK {
				go gpsSerialReader()
			}
		}
	}
}

func initGPS(isReplayMode bool) {
	Satellites = make(map[string]SatelliteInfo)

	if !isReplayMode {
		go pollGPS()
	}
}
