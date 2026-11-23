/*
	Copyright (c) 2016 uAvionix
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	ping.go: uAvionix Ping ADS-B monitoring and management.
    2023 Added PingUSB MavLink support device 0403:6015
*/

package main

import (
	"bufio"
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratux/serial"
	"github.com/stratux/stratux/common"
)

// dump1090Repeater manages a --net-only dump1090 subprocess for Ping/Pong devices.
type dump1090Repeater struct {
	name    string
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

func (r *dump1090Repeater) start(onStartErr func()) {
	r.stop()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.running = true
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		log.Printf("Entered %s network repeater ...\n", r.name)

		cmd := exec.CommandContext(ctx, STRATUX_HOME+"/bin/dump1090", "--net-only", "--net-stratux-port", "30006")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("%s repeater: error creating stdout pipe: %s\n", r.name, err)
			if onStartErr != nil {
				onStartErr()
			}
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			log.Printf("%s repeater: error creating stderr pipe: %s\n", r.name, err)
			if onStartErr != nil {
				onStartErr()
			}
			return
		}

		if err := cmd.Start(); err != nil {
			log.Printf("%s repeater: error executing %s/bin/dump1090: %s\n", r.name, STRATUX_HOME, err)
			if onStartErr != nil {
				onStartErr()
			}
			return
		}

		log.Printf("%s repeater: executed %s successfully...\n", r.name, cmd.String())

		var pipeWG sync.WaitGroup
		pipeWG.Add(2)

		go func() {
			defer pipeWG.Done()
			scanStdout := bufio.NewScanner(stdout)
			for scanStdout.Scan() {
				logDump1090TermMessage(Dump1090TermMessage{Text: scanStdout.Text(), Source: "stdout"})
			}
			if err := scanStdout.Err(); err != nil {
				log.Printf("%s repeater scanStdout error: %s\n", r.name, err)
			}
		}()

		go func() {
			defer pipeWG.Done()
			scanStderr := bufio.NewScanner(stderr)
			for scanStderr.Scan() {
				logDump1090TermMessage(Dump1090TermMessage{Text: scanStderr.Text(), Source: "stderr"})
			}
			if err := scanStderr.Err(); err != nil {
				log.Printf("%s repeater scanStderr error: %s\n", r.name, err)
			}
		}()

		// Drain pipes before cmd.Wait() per os/exec contract.
		pipeWG.Wait()
		_ = cmd.Wait()
		log.Printf("Exiting %s network repeater ...\n", r.name)
	}()
}

func (r *dump1090Repeater) stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	cancel := r.cancel
	r.cancel = nil
	r.running = false
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
}

// Ping device data
var (
	pingMu                        sync.Mutex
	pingSerialConfig              *serial.Config
	pingSerialPort                *serial.Port
	pingReaderWG                  sync.WaitGroup
	pingRepeater                  = &dump1090Repeater{name: "Ping"}
	dump1090Connection            net.Conn
	connectionError               error
	pingDeviceModel               int // 0 => pingEFB - 1090ES, 1 => pingUSB - MavLink
	pingDeviceSuccessfullyWorking atomic.Bool
	shutdownPing                  atomic.Bool
	pingWatcherStarted            atomic.Bool
	pingDoneCh                    = make(chan struct{})
)

func initPingSerial() bool {
	var device string
	baudrate := 2000000

	pingDeviceModel = 0
	log.Printf("Configuring Ping ADS-B\n")

	if _, err := os.Stat("/dev/ping"); err == nil {
		device = "/dev/ping"
	} else if _, err := os.Stat("/dev/softrf"); err == nil {
		device = "/dev/softrf"
		baudrate = 38400
	} else if _, err := os.Stat("/dev/pingusb"); err == nil {
		// 99-uavionix.rules 0403:6015
		device = "/dev/pingusb"
		baudrate = 57600
		pingDeviceModel = 1
	} else {
		log.Printf("No suitable Ping device found.\n")
		return false
	}
	log.Printf("Using %s for Ping\n", device)

	// Open port
	// No timeout specified as Ping does not heartbeat
	pingSerialConfig = &serial.Config{Name: device, Baud: baudrate}
	p, err := serial.OpenPort(pingSerialConfig)
	if err != nil {
		log.Printf("Error opening serial port: %s\n", err.Error())
		return false
	}
	log.Printf("Ping opened serial port")

	pingMu.Lock()
	pingSerialPort = p
	pingMu.Unlock()
	return true
}

func closePingSerialPort() {
	pingMu.Lock()
	p := pingSerialPort
	pingSerialPort = nil
	pingMu.Unlock()
	if p != nil {
		p.Close()
	}
}

func closePingDump1090Conn() {
	pingMu.Lock()
	conn := dump1090Connection
	dump1090Connection = nil
	pingMu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func pingNetworkConnection() net.Conn {
	// Send to dump1090 on port 30001
	dump1090Addr := "127.0.0.1:30001"
	conn, err := net.DialTimeout("tcp", dump1090Addr, 2*time.Second)
	pingMu.Lock()
	dump1090Connection = conn
	connectionError = err
	pingMu.Unlock()
	return conn
}

func pingSerialReader(port *serial.Port) {
	defer pingReaderWG.Done()
	defer func() {
		globalStatus.Ping_connected = false
		closePingDump1090Conn()
		closePingSerialPort()
		log.Printf("Exiting Ping serial reader")
	}()

	log.Printf("Starting Ping serial reader")

	scanner := bufio.NewScanner(port)
	for scanner.Scan() && globalStatus.Ping_connected && globalSettings.Ping_Enabled && !shutdownPing.Load() {
		pingDeviceSuccessfullyWorking.Store(true)
		s := strings.TrimSpace(scanner.Text())
		if len(s) == 0 {
			continue
		}

		if s[0] == '*' {
			// 1090ES report
			// Ping appends a signal strength at the end of the message
			// e.g. *8DC01C2860C37797E9732E555B23;ss=049D;
			// Remove this before forwarding to dump1090
			report := strings.Split(s, ";")
			pingMu.Lock()
			conn := dump1090Connection
			pingMu.Unlock()
			if conn == nil {
				log.Println("Starting dump1090 network connection")
				conn = pingNetworkConnection()
			}
			if len(report[0]) != 0 && conn != nil {
				globalStatus.ES_messages_total++
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, err := conn.Write([]byte(report[0] + ";\r\n")); err != nil {
					log.Printf("Ping: error writing to dump1090: %s\n", err)
					closePingDump1090Conn()
				}
			}
		} else if s[0] == '+' || s[0] == '-' {
			// UAT report
			o, msgtype := parseInput(s)
			if o != nil && msgtype != 0 {
				relayMessage(msgtype, o)
			}
		}
	}
}

func pingShutdown() {
	log.Println("Entered Ping shutdown() ...")
	globalStatus.Ping_connected = false
	closePingSerialPort()
	pingReaderWG.Wait()
	closePingDump1090Conn()
	pingRepeater.stop()
	log.Println("Ping shutdown() complete ...")
}

func pingKill() {
	// Send signal to shutdown to pingWatcher().
	shutdownPing.Store(true)
	if !pingWatcherStarted.Load() {
		return
	}
	// Wait until pingWatcher() has de-initialized cleanly.
	<-pingDoneCh
}

// Watch for config/device changes.
func pingWatcher() {
	pingWatcherStarted.Store(true)
	defer close(pingDoneCh)

	prevPingEnabled := false
	pingDeviceSuccessfullyWorking.Store(false)

	for {
		time.Sleep(1 * time.Second)

		if shutdownPing.Load() {
			pingShutdown()
			return
		}
		// Autoreconnect the device
		if pingDeviceSuccessfullyWorking.Load() && globalSettings.Ping_Enabled && !globalStatus.Ping_connected {
			prevPingEnabled = false
		}

		if prevPingEnabled == globalSettings.Ping_Enabled {
			continue
		}

		// Global settings have changed, reconfig
		if globalSettings.Ping_Enabled && !globalStatus.Ping_connected {
			globalStatus.Ping_connected = initPingSerial()
			// This will retry next loop to connect again to the device
			if !globalStatus.Ping_connected {
				// Relaxed polling to wait for the device to be discovered
				for i := 0; i < 10 && !shutdownPing.Load(); i++ {
					time.Sleep(1 * time.Second)
				}
				continue
			}

			pingMu.Lock()
			port := pingSerialPort
			pingMu.Unlock()

			// pingEFB - 1090
			if globalStatus.Ping_connected && pingDeviceModel == 0 && port != nil {
				pingRepeater.start(func() {
					log.Println("Ping repeater failed to start")
				})
				pingReaderWG.Add(1)
				go pingSerialReader(port)
			}
			// pingUSB - MavLink
			if globalStatus.Ping_connected && pingDeviceModel == 1 && port != nil {
				pingReaderWG.Add(1)
				go pingUSBSerialReader(port)
			}
		} else if !globalSettings.Ping_Enabled {
			pingShutdown()
		}

		prevPingEnabled = globalSettings.Ping_Enabled
	}
}

func pingInit() {
	go pingWatcher()
}

type MavlinkTrafficMessageFormat struct {
	ICAO_address  uint32
	lat           int32
	lon           int32
	altitude      int32
	heading       uint16
	hor_velocity  uint16
	ver_velocity  int16
	validFlags    uint16
	squawk        uint16
	altitude_type uint8
	callsign      [9]byte
	emitter_type  uint8
	tslc          uint8
}

const (
	mavLinkMsgIDAdsbVehicle = 246
	mavLinkAdsbCRCExtra     = 184
)

func mavLinkCRCAccumulate(b byte, crc uint16) uint16 {
	tmp := b ^ byte(crc&0xFF)
	tmp ^= tmp << 4
	return (crc >> 8) ^ (uint16(tmp) << 8) ^ (uint16(tmp) << 3) ^ (uint16(tmp) >> 4)
}

func mavLinkVerifyCRC(frame []byte, crcExtra byte) bool {
	if len(frame) < 8 {
		return false
	}
	payloadLen := int(frame[1])
	if len(frame) != payloadLen+8 {
		return false
	}
	crc := uint16(0xFFFF)
	for i := 1; i < 6+payloadLen; i++ {
		crc = mavLinkCRCAccumulate(frame[i], crc)
	}
	crc = mavLinkCRCAccumulate(crcExtra, crc)
	expected := uint16(frame[6+payloadLen]) | (uint16(frame[7+payloadLen]) << 8)
	return crc == expected
}

func mavLinkFormat(x []byte) bool {
	if len(x) < 8 {
		return false
	}
	msglenMavLink := int(x[1])
	msgtypeMavLink := x[5]
	if msgtypeMavLink == mavLinkMsgIDAdsbVehicle && msglenMavLink >= 38 && len(x) >= 6+38+2 {
		if !mavLinkVerifyCRC(x, mavLinkAdsbCRCExtra) {
			if globalSettings.DEBUG {
				log.Printf("PingUSB: invalid MAVLink CRC for ADSB_VEHICLE frame")
			}
			return false
		}

		mavLink := MavlinkTrafficMessageFormat{}
		const hdr = 6
		mavLink.ICAO_address = uint32(x[hdr+3])<<24 | uint32(x[hdr+2])<<16 | uint32(x[hdr+1])<<8 | uint32(x[hdr+0])
		mavLink.lat = int32(uint32(x[hdr+7])<<24 | uint32(x[hdr+6])<<16 | uint32(x[hdr+5])<<8 | uint32(x[hdr+4]))
		mavLink.lon = int32(uint32(x[hdr+11])<<24 | uint32(x[hdr+10])<<16 | uint32(x[hdr+9])<<8 | uint32(x[hdr+8]))
		mavLink.altitude = int32(uint32(x[hdr+15])<<24 | uint32(x[hdr+14])<<16 | uint32(x[hdr+13])<<8 | uint32(x[hdr+12]))
		mavLink.heading = uint16(x[hdr+17])<<8 | uint16(x[hdr+16])
		mavLink.hor_velocity = uint16(x[hdr+19])<<8 | uint16(x[hdr+18])
		mavLink.ver_velocity = int16(uint16(x[hdr+21])<<8 | uint16(x[hdr+20]))
		mavLink.validFlags = uint16(x[hdr+23])<<8 | uint16(x[hdr+22])
		mavLink.squawk = uint16(x[hdr+25])<<8 | uint16(x[hdr+24])
		mavLink.altitude_type = x[hdr+26]

		for a, b := range x[hdr+27 : hdr+36] {
			if b == ' ' || b == 0 {
				break
			}
			mavLink.callsign[a] = b
		}
		mavLink.emitter_type = x[hdr+36]
		mavLink.tslc = x[hdr+37]
		if globalSettings.DEBUG {
			log.Printf("ICAO_address %06X lat: %d lon: %d alt: %d head: %d call: %s vspeed: %d speed: %d", mavLink.ICAO_address, mavLink.lat, mavLink.lon, mavLink.altitude, mavLink.heading, mavLink.callsign, mavLink.ver_velocity, mavLink.hor_velocity)
		}

		now := stratuxClock.GetTime()
		var ti TrafficInfo
		trafficMutex.Lock()
		defer trafficMutex.Unlock()

		signalLevelSimulated := -1
		if val, ok := traffic[mavLink.ICAO_address]; ok { // if we've already seen it, copy it in to do updates as it may contain some useful information like "tail" from 1090ES.
			ti = val
		} else {
			// New
			ti.Last_seen = now
		}
		if mavLink.heading != 0 {
			ti.Track = float32(mavLink.heading / 100)
		} else {
			signalLevelSimulated -= 5
		}
		ti.ReceivedMsgs += 1
		ti.Icao_addr = mavLink.ICAO_address
		ti.OnGround = false
		ti.Addr_type = 0
		if mavLink.squawk != 0 {
			ti.Squawk = int(mavLink.squawk)
		}
		ti.TargetType = TARGET_TYPE_ADSB
		if mavLink.emitter_type != 0 {
			ti.Emitter_category = uint8(mavLink.emitter_type)
		}
		if mavLink.lat != 0 && mavLink.lon != 0 {
			lat := float32(mavLink.lat) / 10000000.0
			lng := float32(mavLink.lon) / 10000000.0
			// Low signal may involve into a freeze location, update only if it really changes
			if lng != ti.Lng || lat != ti.Lat {
				ti.Lat = lat
				ti.Lng = lng
				if isGPSValid() {
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
				} else {
					ti.BearingDist_valid = false
				}
				ti.Position_valid = true
				ti.ExtrapolatedPosition = false
				ti.Last_seen = now
				ti.Timestamp = time.Now().UTC()
			}
		} else {
			ti.Position_valid = false
			signalLevelSimulated -= 5
		}
		altitudeFoot := int32(float32(mavLink.altitude) / 304.8)
		if altitudeFoot > -2000 && altitudeFoot < 70000 {
			// Low signal may involve into a freeze location, update only if it really changes
			if ti.Alt != altitudeFoot {
				ti.Alt = altitudeFoot
				ti.Last_alt = now
				ti.Last_seen = now
				ti.Timestamp = time.Now().UTC()
			}
		} else {
			signalLevelSimulated -= 5
		}
		if mavLink.altitude_type > 0 {
			ti.AltIsGNSS = true
		}
		if mavLink.hor_velocity > 0 {
			ti.Speed = uint16(float32(mavLink.hor_velocity) * 36 / 1852)
			ti.Speed_valid = true
			ti.Last_speed = now
		} else {
			ti.Speed_valid = false
			signalLevelSimulated -= 5
		}
		ti.Vvel = int16(float32(mavLink.ver_velocity) * 60.0 / 10.0 / 3.048)
		callsignLen := 0
		for a, b := range mavLink.callsign {
			if b == ' ' || b == '\u0000' {
				break
			}
			callsignLen = a + 1
		}
		thisReg, validReg := icao2reg(ti.Icao_addr)
		if validReg {
			ti.Reg = thisReg
		}
		if callsignLen > 0 {
			ti.Tail = string(mavLink.callsign[:callsignLen])
		} else {
			signalLevelSimulated -= 5
			if validReg {
				ti.Tail = thisReg
			}
		}
		// Timestamp and Last_seen are updated only if Location or Altitude changes
		ti.NACp = 8
		ti.NIC = 8
		ti.Last_source = TRAFFIC_SOURCE_1090ES
		ti.SignalLevel = float64(signalLevelSimulated)
		postProcessTraffic(&ti)
		traffic[ti.Icao_addr] = ti
		registerTrafficUpdate(ti)
		seenTraffic[ti.Icao_addr] = true
		return true
	}
	return false
}

func mavLinkParse(mavLinkFrame []byte) bool {
	if len(mavLinkFrame) == 0 || mavLinkFrame[0] != 0xfe || len(mavLinkFrame) > 1024 {
		return true
	}
	if len(mavLinkFrame) < 8 {
		return false
	}
	expectedLen := int(mavLinkFrame[1]) + 8
	if len(mavLinkFrame) < expectedLen {
		return false
	}
	if len(mavLinkFrame) > expectedLen {
		return true
	}
	if mavLinkFormat(mavLinkFrame) {
		pingDeviceSuccessfullyWorking.Store(true)
	}
	return true
}

func pingUSBSerialReader(port *serial.Port) {
	defer pingReaderWG.Done()
	defer func() {
		globalStatus.Ping_connected = false
		closePingSerialPort()
		log.Printf("Exiting PingUSB serial reader")
	}()

	log.Printf("Starting PingUSB serial reader")
	data := make([]byte, 4096)
	mavLinkFrame := make([]byte, 4096)
	mavLinkFrameLastIndex := 0

	for globalStatus.Ping_connected && globalSettings.Ping_Enabled && !shutdownPing.Load() {
		n, err := port.Read(data)
		if err != nil {
			break
		}
		for _, b := range data[:n] {
			if mavLinkFrameLastIndex == 0 {
				if b != 0xfe {
					continue
				}
			}
			if mavLinkFrameLastIndex >= len(mavLinkFrame) {
				// Prevent buffer overflow on corrupted stream; resync on next STX.
				mavLinkFrameLastIndex = 0
				if b != 0xfe {
					continue
				}
			}
			mavLinkFrame[mavLinkFrameLastIndex] = b
			mavLinkFrameLastIndex++

			if mavLinkFrameLastIndex >= 8 {
				expectedLen := int(mavLinkFrame[1]) + 8
				if mavLinkFrameLastIndex >= expectedLen {
					frame := mavLinkFrame[:expectedLen]
					valid := false
					if frame[5] == mavLinkMsgIDAdsbVehicle {
						valid = mavLinkFormat(frame)
						if valid {
							pingDeviceSuccessfullyWorking.Store(true)
						}
					} else {
						// Non-ADSB MAVLink frame (e.g. heartbeat); accept frame boundary.
						valid = true
					}
					if valid {
						mavLinkFrameLastIndex = 0
					} else {
						// Bad CRC: resync to the next 0xFE byte inside the buffered slice.
						nextStx := -1
						for i := 1; i < mavLinkFrameLastIndex; i++ {
							if mavLinkFrame[i] == 0xfe {
								nextStx = i
								break
							}
						}
						if nextStx > 0 {
							copy(mavLinkFrame, mavLinkFrame[nextStx:mavLinkFrameLastIndex])
							mavLinkFrameLastIndex -= nextStx
						} else {
							mavLinkFrameLastIndex = 0
						}
					}
				}
			}
		}
	}
}
