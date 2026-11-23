/*
	pong.go: uAvionix Pong ADS-B monitoring and management.
	Added 1/2026
*/

package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratux/serial"
)

// pong device data
var (
	pongMu                        sync.Mutex
	pongSerialConfig              *serial.Config
	pongSerialPort                *serial.Port
	pongReaderWG                  sync.WaitGroup
	pongRepeater                  = &dump1090Repeater{name: "Pong"}
	dump1090ConnectionPong        net.Conn
	connectionErrorPong           error
	pongUpdateMode                atomic.Bool
	pongDeviceSuccessfullyWorking atomic.Bool
	shutdownPong                  atomic.Bool
	pongWatcherStarted            atomic.Bool
	pongDoneCh                    = make(chan struct{})
)

type PongTermMessage struct {
	Text   string
	Source string
}

func initPongSerial() bool {
	var device string
	baudrate := 3000000

	log.Printf("Configuring Pong ADS-B\n")

	if _, err := os.Stat("/dev/pong"); err == nil {
		device = "/dev/pong"
	} else {
		log.Printf("No suitable Pong device found.\n")
		return false
	}
	log.Printf("Using %s for Pong\n", device)

	// Open port
	// No timeout specified as Pong does not heartbeat
	pongSerialConfig = &serial.Config{Name: device, Baud: baudrate}
	p, err := serial.OpenPort(pongSerialConfig)
	if err != nil {
		log.Printf("Error opening serial port: %s\n", err.Error())
		return false
	}
	errRts := p.ClearRTS()
	if errRts != nil {
		log.Printf("Clearing RTS returned %s\n", errRts.Error())
	}

	log.Printf("Pong opened serial port at %d baud\n", baudrate)

	// No device configuration is needed, we should be ready
	globalStatus.Pong_Heartbeats = 0

	pongMu.Lock()
	pongSerialPort = p
	pongMu.Unlock()
	return true
}

func closePongSerialPort() {
	pongMu.Lock()
	p := pongSerialPort
	pongSerialPort = nil
	pongMu.Unlock()
	if p != nil {
		p.Close()
	}
}

func closePongDump1090Conn() {
	pongMu.Lock()
	conn := dump1090ConnectionPong
	dump1090ConnectionPong = nil
	pongMu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func pongNetworkConnection() net.Conn {
	// Send to dump1090 on port 30001
	dump1090Addr := "127.0.0.1:30001"
	conn, err := net.DialTimeout("tcp", dump1090Addr, 2*time.Second)
	pongMu.Lock()
	dump1090ConnectionPong = conn
	connectionErrorPong = err
	pongMu.Unlock()
	return conn
}

func pongSerialReader(port *serial.Port) {
	defer pongReaderWG.Done()
	defer func() {
		globalStatus.Pong_connected = false
		closePongDump1090Conn()
		closePongSerialPort()
		log.Printf("Exiting Pong serial reader")
	}()

	log.Printf("Starting Pong serial reader")

	scanner := bufio.NewScanner(port)
	for scanner.Scan() && globalStatus.Pong_connected && globalSettings.Pong_Enabled && !shutdownPong.Load() {
		pongDeviceSuccessfullyWorking.Store(true)
		s := strings.TrimSpace(scanner.Text())
		if len(s) == 0 {
			continue
		}

		if s[0] == '\'' {
			report := strings.Split(s, "'")
			if len(report) >= 2 {
				log.Printf("Pong ASCII: %s\n", report[1])
			}
		} else if s[0] == '.' {
			globalStatus.Pong_Heartbeats++
		} else if s[0] == '*' {
			// 1090ES report
			// Pong appends a signal strength at the end of the message
			// e.g. *8DC01C2860C37797E9732E555B23;ss=049D;
			// Remove this before forwarding to dump1090
			report := strings.Split(s, ";")
			pongMu.Lock()
			conn := dump1090ConnectionPong
			pongMu.Unlock()
			if conn == nil {
				log.Println("Starting dump1090 network connection")
				conn = pongNetworkConnection()
			}
			if len(report[0]) != 0 && conn != nil {
				globalStatus.ES_messages_total++
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, err := conn.Write([]byte(report[0] + ";\r\n")); err != nil {
					log.Printf("Pong: error writing to dump1090: %s\n", err)
					closePongDump1090Conn()
				}
			}
		} else if s[0] == '+' || s[0] == '-' {
			// UAT report
			o, msgtype := parseInput(s)
			if o != nil && msgtype != 0 {
				if globalSettings.DEBUG {
					log.Printf("Relaying UAT, type=%d: %s\n", msgtype, s)
				}
				relayMessage(msgtype, o)
			} else if globalSettings.DEBUG {
				if o == nil {
					log.Println("Not relaying message, o == nil")
				} else {
					log.Println("Not relaying message, msgtype == 0")
				}
			}
		} else {
			log.Printf("Pong ASCII: %s", s)
			if strings.Contains(s, "ERROR SPI") {
				log.Printf("SPI error sent from Pong. Restarting Pong")
			}
		}
	}
}

func pongShutdown() {
	log.Println("Entered Pong shutdown() ...")
	globalStatus.Pong_connected = false
	closePongSerialPort()
	pongReaderWG.Wait()
	closePongDump1090Conn()
	pongRepeater.stop()
	log.Println("Pong shutdown() complete ...")
}

func pongSetUpdateMode() {
	pongUpdateMode.Store(true)
}

func pongRunUpdateWithOutput() error {
	cmd := exec.Command("/bin/bash", "-c", `"/tmp/updpong/util/updatepong.sh"`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Error creating stdout pipe for updatepong.sh: %s\n", err)
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		log.Printf("Error executing updatepong.sh: %s\n", err)
		return err
	}
	log.Println("Executed " + cmd.String() + " successfully...")

	scanStdout := bufio.NewScanner(stdout)
	for scanStdout.Scan() {
		m := PongTermMessage{Text: scanStdout.Text(), Source: "stdout"}
		logPongTermMessage(m)
		log.Printf("Pong Updater: %s\n", m.Text)
	}

	if err := scanStdout.Err(); err != nil {
		log.Printf("Pong Updater error: %s\n", err)
	}

	if err := cmd.Wait(); err != nil {
		return err
	}
	fmt.Printf("Update pong completed with success\n")
	return nil
}

func pongKill() {
	// Send signal to shutdown to pongWatcher().
	shutdownPong.Store(true)
	if !pongWatcherStarted.Load() {
		return
	}
	// Wait until pongWatcher() has de-initialized cleanly.
	<-pongDoneCh
}

// Watch for config/device changes.
func pongWatcher() {
	pongWatcherStarted.Store(true)
	defer close(pongDoneCh)

	pongDownCount := 0
	prevPongEnabled := false
	// Clear pong update mode
	pongUpdateMode.Store(false)
	pongDeviceSuccessfullyWorking.Store(false)

	for {
		time.Sleep(1 * time.Second)
		if shutdownPong.Load() {
			pongShutdown()
			return
		}
		// Autoreconnect the device
		if pongDeviceSuccessfullyWorking.Load() && globalSettings.Pong_Enabled && !globalStatus.Pong_connected {
			prevPongEnabled = false
		}

		if pongUpdateMode.Swap(false) {
			log.Printf("PONG UPDATE MODE SET run the steps we need to update it here!")
			// File should be in /tmp/update_pong.zip
			// Shut down the pong reader & repeater before running the update script.
			pongShutdown()
			time.Sleep(5 * time.Second)
			log.Printf("Run update process\n")
			_ = os.RemoveAll("/tmp/updpong")
			_, err := exec.Command("/usr/bin/unzip", "-o", "/tmp/update_pong.zip", "-d", "/tmp/updpong").Output()
			if err == nil {
				err2 := pongRunUpdateWithOutput()
				if err2 != nil {
					log.Printf("Failed to run pongRunUpdateWithOutput(): %s\n", err2.Error())
				}
			} else {
				log.Printf("Could not unpack the update file /tmp/update_pong.zip: %s\n", err.Error())
			}

			time.Sleep(1 * time.Second)
			log.Printf("update process complete - continue\n")
			prevPongEnabled = false
		}
		// Keep a counter of how long its been down
		if !prevPongEnabled && !globalSettings.Pong_Enabled {
			pongDownCount++
		} else {
			pongDownCount = 0
		}
		// Autodetect Pong
		if !globalSettings.Pong_Enabled && !prevPongEnabled && pongDownCount > 10 {
			if _, err := os.Stat("/dev/pong"); err == nil {
				log.Printf("Pong device file detected - Enabling the pong radio\n")
				globalSettings.Pong_Enabled = true
				saveSettings()
			}
		}

		if prevPongEnabled == globalSettings.Pong_Enabled {
			continue
		}

		// Global settings have changed, reconfig
		if globalSettings.Pong_Enabled && !globalStatus.Pong_connected {
			globalStatus.Pong_connected = initPongSerial()
			// This will retry next loop to connect again to the device
			if !globalStatus.Pong_connected {
				// Relaxed polling to wait for the device to be discovered
				for i := 0; i < 10 && !shutdownPong.Load(); i++ {
					time.Sleep(1 * time.Second)
				}
				continue
			}

			pongMu.Lock()
			port := pongSerialPort
			pongMu.Unlock()

			if globalStatus.Pong_connected && port != nil {
				pongRepeater.start(func() {
					log.Println("Pong repeater failed to start")
				})
				pongReaderWG.Add(1)
				go pongSerialReader(port)
			}
		} else if !globalSettings.Pong_Enabled {
			pongShutdown()
		}
		prevPongEnabled = globalSettings.Pong_Enabled
	}
}

func pongInit() {
	go pongWatcher()
}
