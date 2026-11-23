package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/stratux/serial"
)

/*

#cgo LDFLAGS: -Wl,-rpath=${SRCDIR}/.. -Wl,-rpath=/opt/stratux/lib -L${SRCDIR}/.. -ldump978 -lm

#include <stdint.h>
#include "../dump978/fec.h"

*/
import "C"

var (
	radioSerialMu     sync.Mutex
	radioSerialConfig *serial.Config
	radioSerialPort   *serial.Port
	uatRadioShutdown  atomic.Bool
)

func closeUATRadioSerial() {
	radioSerialMu.Lock()
	p := radioSerialPort
	radioSerialPort = nil
	radioSerialMu.Unlock()
	if p != nil {
		p.Close()
	}
}

func initUATRadioSerial(isTraceReplayMode bool) error {
	// Init for FEC routines.
	C.init_fec()
	if isTraceReplayMode {
		return nil
	}
	go func() {
		watchTicker := time.NewTicker(1 * time.Second)
		defer watchTicker.Stop()
		for {
			<-watchTicker.C
			if uatRadioShutdown.Load() {
				closeUATRadioSerial()
				return
			}
			// If UAT was disabled while the radio is connected, close it so the reader exits cleanly.
			if !globalSettings.UAT_Enabled {
				if globalStatus.UATRadio_connected {
					log.Printf("UAT disabled, closing UATRadio serial port.\n")
					closeUATRadioSerial()
				}
				continue
			}
			if globalStatus.UATRadio_connected {
				// Radio already set up. Continue.
				continue
			}
			if _, err := os.Stat("/dev/uatradio"); err != nil {
				// Device not connected.
				continue
			}
			log.Printf("===== UAT Device Name  : UATRadio v1.0 =====\n")

			// Initialize port at 2Mbaud.
			radioSerialConfig = &serial.Config{Name: "/dev/uatradio", Baud: 2000000}
			p, err := serial.OpenPort(radioSerialConfig)
			if err != nil {
				log.Printf("\tUAT Open Failed: %s\n", err.Error())
				continue
			}

			log.Printf("\tUATRadio init success.\n")

			radioSerialMu.Lock()
			radioSerialPort = p
			radioSerialMu.Unlock()
			globalStatus.UATRadio_connected = true

			// Start a goroutine to watch the serial port.
			go radioSerialPortReader(p)
		}
	}()
	return nil
}

/*
radioSerialPortReader().

	Loop to read data from the radio serial port.
*/
var radioMagic = []byte{0x0a, 0xb0, 0xcd, 0xe0}

const radioMaxPayloadLen = 2048

func radioSerialPortReader(serialPort *serial.Port) {
	defer func() {
		globalStatus.UATRadio_connected = false
		radioSerialMu.Lock()
		if radioSerialPort == serialPort {
			radioSerialPort = nil
		}
		radioSerialMu.Unlock()
		serialPort.Close()
	}()
	tmpBuf := make([]byte, 1024) // Read buffer.
	var buf []byte               // Message buffer.
	for globalSettings.UAT_Enabled && !uatRadioShutdown.Load() {
		n, err := serialPort.Read(tmpBuf)
		if err != nil {
			log.Printf("serial port err, shutting down radio: %s\n", err.Error())
			return
		}
		buf = append(buf, tmpBuf[:n]...)
		bufLen := len(buf)
		consumed := 0
		waitingPartial := false

		// Search for a suitable message to extract.
		for i := 0; i <= bufLen-6; i++ {
			if buf[i] == radioMagic[0] && buf[i+1] == radioMagic[1] && buf[i+2] == radioMagic[2] && buf[i+3] == radioMagic[3] {
				// Found the magic sequence. Get the length.
				rawLen := int(uint16(buf[i+4]) | (uint16(buf[i+5]) << 8))
				if rawLen > radioMaxPayloadLen {
					// Implausible length; treat as false magic in noise.
					continue
				}
				msgLen := rawLen + 5 // 5 bytes for RSSI and TS.
				// Check if we've read enough to finish this message.
				if bufLen < i+6+msgLen {
					// Keep from the magic header onward and wait for more bytes.
					consumed = i
					waitingPartial = true
					break
				}
				// Message is long enough.
				processRadioMessage(buf[i+6 : i+6+msgLen])
				consumed = i + 6 + msgLen
				// Advance past the entire frame so decoded payload isn't re-scanned for radioMagic.
				i += 6 + msgLen - 1
			}
		}

		if consumed > 0 {
			rem := buf[consumed:]
			newBuf := make([]byte, len(rem))
			copy(newBuf, rem)
			buf = newBuf
		} else if !waitingPartial && len(buf) > 5 {
			// No magic found in scanned prefix; keep only trailing bytes that could start a split magic header.
			rem := buf[len(buf)-5:]
			newBuf := make([]byte, len(rem))
			copy(newBuf, rem)
			buf = newBuf
		}
	}
}

/*
	processRadioMessage().
	 Processes a single message from the radio. De-interleaves (when necessary), checks Reed-Solomon, passes to main process.
*/

func processRadioMessage(msg []byte) {
	TraceLog.Record(CONTEXT_LOWPOWERUAT, msg)
	if len(msg) < 5 {
		return
	}
	// RSSI and message timestamp are prepended to the actual packet.

	// RSSI
	rssiRaw := int8(msg[0])
	rssiDump978 := rssiRaw

	//_ := uint32(msg[1]) + (uint32(msg[2]) << 8) + (uint32(msg[3]) << 16) + (uint32(msg[4]) << 24) // Timestamp. Currently unused.

	msg = msg[5:]

	var toRelay string
	var rs_errors C.int
	switch len(msg) {
	case 552:
		to := make([]byte, 552)
		ret := int(C.correct_uplink_frame((*C.uint8_t)(unsafe.Pointer(&msg[0])), (*C.uint8_t)(unsafe.Pointer(&to[0])), &rs_errors))
		if ret >= 0 {
			toRelay = fmt.Sprintf("+%s;ss=%d;", hex.EncodeToString(to[:432]), rssiDump978)
		}
	case 48:
		to := make([]byte, 48)
		copy(to, msg)
		i := int(C.correct_adsb_frame((*C.uint8_t)(unsafe.Pointer(&to[0])), &rs_errors))
		if i == 1 {
			// Short ADS-B frame.
			toRelay = fmt.Sprintf("-%s;ss=%d;", hex.EncodeToString(to[:18]), rssiDump978)
		} else if i == 2 {
			// Long ADS-B frame.
			toRelay = fmt.Sprintf("-%s;ss=%d;", hex.EncodeToString(to[:34]), rssiDump978)
		}
	default:
		log.Printf("processRadioMessage(): unhandled message size %d\n", len(msg))
	}

	if len(toRelay) > 0 && rs_errors != 9999 {
		o, msgtype := parseInput(toRelay)
		if o != nil && msgtype != 0 {
			relayMessage(msgtype, o)
		}
	}
}
