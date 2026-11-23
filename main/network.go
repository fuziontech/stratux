/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	network.go: Client networking routines, DHCP lease monitoring, queue management, ICMP monitoring.
*/

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratux/serial"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"tinygo.org/x/bluetooth"
)

var clientConnections map[string]connection // UDP out, TCP out, serial out

var dhcpLeases map[string]string
var networkGDL90Chan chan []byte // For gdl90 web socket
var netMutex *sync.Mutex         // netMutex needs to be locked before accessing dhcpLeases and clientConnections.

var totalNetworkMessagesSent uint32
var networkDataMessagesSent uint64
var networkDataBytesSent uint64
var nmeaInActiveClients int32

const (
	NETWORK_GDL90_STANDARD = 1
	NETWORK_AHRS_FFSIM     = 2
	NETWORK_AHRS_GDL90     = 4
	NETWORK_FLARM_NMEA     = 8
	NETWORK_POSITION_FFSIM = 16
	dhcp_lease_file        = "/var/lib/misc/dnsmasq.leases"
	dhcp_usb0_lease_file   = "/var/lib/misc/dnsmasq.usb0.leases"
	dhcp_lease_dir         = "/var/lib/misc/"
	extra_hosts_file       = "/etc/stratux-static-hosts.conf"
)

// Overridable paths for deterministic testing.
var (
	dhcpLeaseFile     = dhcp_lease_file
	dhcpUSB0LeaseFile = dhcp_usb0_lease_file
	arpTableFile      = "/proc/net/arp"
)

var dhcpLeaseDirectoryLastTest time.Time // Last time fsWriteTest() was run on the DHCP lease directory.

func parseDHCPLeaseFile(path string, ret map[string]string) {
	dat, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(dat), "\n")
	for _, line := range lines {
		spaced := strings.Fields(line)
		if len(spaced) >= 4 {
			ip := spaced[2]
			host := spaced[3]
			if host == "*" {
				host = ""
			}
			if net.ParseIP(ip) != nil {
				ret[ip] = host
			}
		}
	}
}

// detectClientInterface resolves the network interface name ("usb0", "ap0", "wlan0", etc.)
// for a client IP using /proc/net/arp first, then falling back to subnet conventions.
func detectClientInterface(ip string) string {
	host := strings.TrimSpace(ip)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return "wlan0"
	}

	if dat, err := os.ReadFile(arpTableFile); err == nil {
		lines := strings.Split(string(dat), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			// Format: IP address | HW type | Flags | HW address | Mask | Device
			if len(fields) >= 6 && fields[0] == host && fields[2] != "0x0" {
				dev := strings.TrimSpace(fields[5])
				if dev != "" {
					return dev
				}
			}
		}
	}

	if strings.HasPrefix(host, "192.168.11.") {
		return "usb0"
	}
	if strings.HasPrefix(host, "192.168.10.") {
		return "ap0"
	}
	return "wlan0"
}

// detectClientProtocol maps a connection capability bitmask and transport hint to a
// human-readable EFB protocol name ("GDL90", "FLARM/NMEA", "X-Plane", "BLE").
func detectClientProtocol(capability uint8, transport string) string {
	if transport == "ble" {
		return "BLE"
	}
	if capability&(NETWORK_GDL90_STANDARD|NETWORK_AHRS_GDL90) != 0 {
		return "GDL90"
	}
	if capability&NETWORK_FLARM_NMEA != 0 {
		return "FLARM/NMEA"
	}
	if capability&(NETWORK_POSITION_FFSIM|NETWORK_AHRS_FFSIM) != 0 {
		return "X-Plane"
	}
	return "Unknown"
}

// Read the "dnsmasq.leases" and "dnsmasq.usb0.leases" files and parse out IP/hostname.
func getDHCPLeases() (map[string]string, error) {
	// Do a write test. Even if we are able to read the file, it may be out of date because there's a fs write issue.
	// Only perform the test once every 5 minutes to minimize writes.
	if stratuxClock != nil && stratuxClock.Since(dhcpLeaseDirectoryLastTest) >= 5*time.Minute {
		err := fsWriteTest(dhcp_lease_dir)
		if err != nil {
			addSingleSystemErrorf("fs-write", "Write error on '%s', your EFB may have issues receiving weather and traffic.", dhcp_lease_dir)
		}
		dhcpLeaseDirectoryLastTest = stratuxClock.GetTime()
	}
	ret := make(map[string]string)

	parseDHCPLeaseFile(dhcpLeaseFile, ret)
	parseDHCPLeaseFile(dhcpUSB0LeaseFile, ret)

	// Add IP's set through the settings page
	globalSettingsMutex.RLock()
	staticIps := make([]string, len(globalSettings.StaticIps))
	copy(staticIps, globalSettings.StaticIps)
	globalSettingsMutex.RUnlock()
	for _, ip := range staticIps {
		if net.ParseIP(ip) != nil {
			ret[ip] = ""
		}
	}

	// Check kernel ARP table - useful when in client mode or usb0 tethering. We skip reverse hostname lookup since it can be very slow..
	if dat2, err := os.ReadFile(arpTableFile); err == nil {
		iplines := strings.Split(string(dat2), "\n")
		for _, ipline := range iplines {
			fields := strings.Fields(ipline)
			// Format: IP address | HW type | Flags | HW address | Mask | Device
			if len(fields) >= 4 {
				ip := fields[0]
				flags := fields[2]
				// Skip header and incomplete ARP entries (Flags == 0x0).
				if flags == "0x0" || net.ParseIP(ip) == nil {
					continue
				}
				if _, contained := ret[ip]; !contained {
					ret[ip] = ""
				}
			}
		}
	}

	// Added the ability to have static IP hosts stored in /etc/stratux-static-hosts.conf
	if dat3, err := os.ReadFile(extra_hosts_file); err == nil {
		iplines := strings.Split(string(dat3), "\n")
		for _, ipline := range iplines {
			fields := strings.Fields(ipline)
			if len(fields) >= 2 && net.ParseIP(fields[0]) != nil {
				ret[fields[0]] = fields[1]
			}
		}
	}

	return ret, nil
}

func networkOutWatcher() {
	for {
		ch := <-networkGDL90Chan
		gdl90Update.SendJSON(ch)
	}
}

// Monitor serial output channel, send to serial port.
func serialOutWatcher() {
	// Check every 10 seconds for a serial output device.
	serialTicker := time.NewTicker(10 * time.Second)
	defer serialTicker.Stop()

	serialDevs := make([]string, 0, 20)
	for i := 0; i < 10; i++ {
		serialDevs = append(serialDevs, fmt.Sprintf("/dev/serialout%d", i))
		serialDevs = append(serialDevs, fmt.Sprintf("/dev/serialout_nmea%d", i))
	}

	for range serialTicker.C {
		for _, serialDev := range serialDevs {
			if _, err := os.Stat(serialDev); !os.IsNotExist(err) { // Check if the device file exists.
				var config serialConnection
				needSave := false

				// Master is globalSettings.SerialOutputs. Once we connect to one, a fresh active connection is created in clientConnections.
				globalSettingsMutex.Lock()
				if val, ok := globalSettings.SerialOutputs[serialDev]; !ok {
					proto := uint8(NETWORK_GDL90_STANDARD)
					if strings.Contains(serialDev, "_nmea") {
						proto = NETWORK_FLARM_NMEA
					}
					if globalSettings.SerialOutputs == nil {
						globalSettings.SerialOutputs = make(map[string]serialConnection)
					}
					globalSettings.SerialOutputs[serialDev] = serialConnection{
						DeviceString: serialDev,
						Baud:         38400,
						Capability:   proto,
					}
					log.Printf("detected new serial output, setting up now: %s. Default baudrate 38400.\n", serialDev)
					config = globalSettings.SerialOutputs[serialDev]
					needSave = true
				} else {
					config = val
					if config.Capability == 0 {
						config.Capability = NETWORK_GDL90_STANDARD // Fix old serial conns that didn't have protocol set
					}
				}
				globalSettingsMutex.Unlock()

				if needSave {
					saveSettings()
				}

				netMutex.Lock()

				needsConnect := true
				if activeConn, ok := clientConnections[serialDev]; ok {
					if !activeConn.IsSleeping() {
						needsConnect = false
					} else {
						delete(clientConnections, serialDev)
						go activeConn.Close() // expired/unplugged? async because it might lock..
					}
				}

				if needsConnect {
					cfg := &serial.Config{Name: config.DeviceString, Baud: config.Baud}
					p, err := serial.OpenPort(cfg)
					if err != nil {
						log.Printf("serialout port (%s) err: %s\n", config.DeviceString, err.Error())
					} else {
						log.Printf("opened serialout: Name: %s, Baud: %d\n", config.DeviceString, config.Baud)
						// Allocate a fresh MessageQueue and mutex for each opened serial connection.
						active := &serialConnection{
							DeviceString: config.DeviceString,
							Baud:         config.Baud,
							Capability:   config.Capability,
							Interface:    "serial",
							Protocol:     detectClientProtocol(config.Capability, "serial"),
							serialPort:   p,
							Queue:        NewMessageQueue(1024),
							mu:           &sync.Mutex{},
						}
						clientConnections[serialDev] = active
						go connectionWriter(active)
					}
				}
				netMutex.Unlock()
			}
		}
	}
}

// TCP port 2000 for Airconnect-like NMEA-Out
func tcpNMEAOutListener() {
	ln, err := net.Listen("tcp", ":2000")
	if err != nil {
		log.Printf("tcpNMEAOutListener: %s", err.Error())
		return
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("tcpNMEAOutListener accept: %s", err.Error())
			continue
		}
		rawTCP, ok := conn.(*net.TCPConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		_ = rawTCP.SetKeepAlive(true)
		_ = rawTCP.SetKeepAlivePeriod(30 * time.Second)

		key := "TCP:" + conn.RemoteAddr().String()
		remoteHost, _, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
		if splitErr != nil {
			remoteHost = conn.RemoteAddr().String()
		}

		tcpConn := &tcpConnection{
			Conn:       rawTCP,
			Queue:      NewMessageQueue(1024),
			Capability: NETWORK_FLARM_NMEA,
			Key:        key,
			Interface:  detectClientInterface(remoteHost),
			Protocol:   detectClientProtocol(NETWORK_FLARM_NMEA, "tcp"),
		}
		netMutex.Lock()
		clientConnections[tcpConn.GetConnectionKey()] = tcpConn
		netMutex.Unlock()
		go connectionWriter(tcpConn)
	}
}

/* Server that can be used to feed NMEA data to, e.g. to connect OGN Tracker wirelessly */
func tcpNMEAInListener() {
	ln, err := net.Listen("tcp", ":30011")
	if err != nil {
		log.Printf("tcpNMEAInListener: %s", err.Error())
		return
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("tcpNMEAInListener accept: %s", err.Error())
			continue
		}
		go handleNmeaInConnection(conn)
	}
}

func handleNmeaInConnection(c net.Conn) {
	defer c.Close()

	atomic.AddInt32(&nmeaInActiveClients, 1)
	defer func() {
		if atomic.AddInt32(&nmeaInActiveClients, -1) == 0 {
			globalStatusMutex.Lock()
			globalStatus.GPS_connected = false
			globalStatus.GPS_detected_type = 0
			globalStatus.GPS_NetworkRemoteIp = ""
			globalStatusMutex.Unlock()
		}
	}()

	remoteHost, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		remoteHost = c.RemoteAddr().String()
	}

	// Set to fixed GPS_TYPE_NETWORK in the beginning, to override previous detected NMEA types
	globalStatusMutex.Lock()
	globalStatus.GPS_detected_type = GPS_TYPE_NETWORK
	globalStatus.GPS_NetworkRemoteIp = remoteHost
	globalStatus.GPS_connected = true
	globalStatusMutex.Unlock()

	const maxNmeaLineLen = 1024
	scanner := bufio.NewScanner(c)
	scanner.Buffer(make([]byte, maxNmeaLineLen), maxNmeaLineLen)

	for {
		if err := c.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			break
		}
		if !scanner.Scan() {
			break
		}
		globalStatusMutex.Lock()
		globalStatus.GPS_connected = true
		// Keep detected protocol, only ensure type=network
		globalStatus.GPS_detected_type = GPS_TYPE_NETWORK | (globalStatus.GPS_detected_type & 0xf0)
		globalStatusMutex.Unlock()

		processNMEALine(scanner.Text())
	}
}

// Returns the number of connected active clients and prints queue lengths.
func getNetworkStats() {
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()

	for range timer.C {
		globalSettingsMutex.RLock()
		debug := globalSettings.DEBUG
		globalSettingsMutex.RUnlock()

		netMutex.Lock()
		activeIPs := make(map[string]struct{})

		for _, conn := range clientConnections {
			// only use net conns
			netconn, ok := conn.(*networkConnection)
			if netconn == nil || !ok {
				continue
			}

			if debug {
				queueBytes := 0
				queueDump := netconn.MessageQueue().GetQueueDump(true)
				for _, msg := range queueDump {
					if b, ok := msg.([]byte); ok {
						queueBytes += len(b)
					}
				}
				log.Printf("On  %s:%d,  Queue length = %d messages / %d bytes\n", netconn.Ip, netconn.Port, len(queueDump), queueBytes)
			}
			// Deduplicate by client IP so multi-port outputs (4000, 2000, 49002) don't overcount Connected_Users.
			if netconn.HasRecentPing(15 * time.Minute) {
				activeIPs[netconn.Ip] = struct{}{}
			}
		}
		netMutex.Unlock()

		globalStatusMutex.Lock()
		globalStatus.Connected_Users = uint(len(activeIPs))
		globalStatusMutex.Unlock()
	}
}

// See who has a DHCP lease and make a UDP connection to each of them.
func refreshConnectedClients() {
	validConnections := make(map[string]bool)
	t, err := getDHCPLeases()
	if err != nil {
		log.Printf("getDHCPLeases(): %s\n", err.Error())
		return
	}

	globalSettingsMutex.RLock()
	networkOutputs := make([]networkConnection, len(globalSettings.NetworkOutputs))
	copy(networkOutputs, globalSettings.NetworkOutputs)
	globalSettingsMutex.RUnlock()

	clientIfaces := make(map[string]string, len(t))
	for ip := range t {
		clientIfaces[ip] = detectClientInterface(ip)
	}

	netMutex.Lock()
	defer netMutex.Unlock()

	dhcpLeases = t
	// Client connected that wasn't before.
	for ip, hostname := range dhcpLeases {
		iface := clientIfaces[ip]
		for _, networkOutput := range networkOutputs {
			ipAndPort := net.JoinHostPort(ip, strconv.Itoa(int(networkOutput.Port)))
			if existing, ok := clientConnections[ipAndPort]; !ok {
				log.Printf("client connected: %s:%d (%s) via %s.\n", ip, networkOutput.Port, hostname, iface)
				addr, err := net.ResolveUDPAddr("udp", ipAndPort)
				if err != nil {
					log.Printf("ResolveUDPAddr(%s): %s\n", ipAndPort, err.Error())
					continue
				}
				outConn, err := net.DialUDP("udp", nil, addr)
				if err != nil {
					log.Printf("DialUDP(%s): %s\n", ipAndPort, err.Error())
					continue
				}
				nc := &networkConnection{
					Conn:       outConn,
					Ip:         ip,
					Port:       networkOutput.Port,
					Capability: networkOutput.Capability,
					Interface:  iface,
					Protocol:   detectClientProtocol(networkOutput.Capability, "udp"),
					Queue:      NewMessageQueue(1024),
					mu:         &sync.Mutex{},
				}
				clientConnections[ipAndPort] = nc
				go connectionWriter(nc)
			} else if nc, ok := existing.(*networkConnection); ok && iface != "" {
				nc.SetInterface(iface)
			}
			validConnections[ipAndPort] = true
		}
	}
	// Client that was connected before that isn't.
	for ipAndPort, netconn := range clientConnections {
		if conn, ok := netconn.(*networkConnection); ok {
			if _, valid := validConnections[ipAndPort]; !valid {
				log.Printf("removed connection %s.\n", ipAndPort)
				conn.MessageQueue().Close()
				if conn.Conn != nil {
					_ = conn.Conn.Close()
				}
				delete(clientConnections, ipAndPort)
			}
		}
	}
}

func parseBleUuid(uuidStr string) (bluetooth.UUID, error) {
	if len(uuidStr) == 4 {
		// Assume hex 16 bit
		val, err := strconv.ParseUint(uuidStr, 16, 16)
		if err != nil {
			return bluetooth.UUID{}, err
		}
		return bluetooth.New16BitUUID(uint16(val)), nil
	}
	return bluetooth.ParseUUID(uuidStr)
}

var bleAdapter = bluetooth.DefaultAdapter

func initBluetooth() {
	globalSettingsMutex.RLock()
	bleConfigs := make([]bleConnection, len(globalSettings.BleOutputs))
	copy(bleConfigs, globalSettings.BleOutputs)
	localName := globalSettings.WiFiSSID
	globalSettingsMutex.RUnlock()

	if len(bleConfigs) == 0 {
		return
	}

	const maxEnableAttempts = 5
	backoff := 5 * time.Second
	enabled := false
	for attempt := 1; attempt <= maxEnableAttempts; attempt++ {
		if err := bleAdapter.Enable(); err != nil {
			log.Printf("Failed to init BLE adapter (attempt %d/%d): %s", attempt, maxEnableAttempts, err.Error())
			if attempt == maxEnableAttempts {
				log.Printf("Disabling BLE output after %d failed adapter init attempts", maxEnableAttempts)
				return
			}
			time.Sleep(backoff)
			if backoff < 60*time.Second {
				backoff *= 2
			}
		} else {
			enabled = true
			break
		}
	}
	if !enabled {
		return
	}

	services := make([]bluetooth.UUID, 0, len(bleConfigs))
	for _, cfg := range bleConfigs {
		svcUUID, err := parseBleUuid(cfg.UUIDService)
		if err != nil {
			log.Printf("Invalid BLE service UUID %q: %s", cfg.UUIDService, err.Error())
			continue
		}
		services = append(services, svcUUID)
	}
	if len(services) == 0 {
		return
	}

	adv := bleAdapter.DefaultAdvertisement()
	if err := adv.Configure(bluetooth.AdvertisementOptions{
		LocalName:    localName,
		ServiceUUIDs: services,
	}); err != nil {
		log.Printf("BLE Advertising configure failed: %s", err.Error())
	} else if err := adv.Start(); err != nil {
		addSingleSystemErrorf("BLE", "BLE Advertising failed to start: %s", err.Error())
	}

	// TODO: not working if we have multiple GATTs in one service
	for _, cfg := range bleConfigs {
		svcUUID, err := parseBleUuid(cfg.UUIDService)
		if err != nil {
			log.Printf("Invalid BLE service UUID %q: %s", cfg.UUIDService, err.Error())
			continue
		}
		gattUUID, err := parseBleUuid(cfg.UUIDGatt)
		if err != nil {
			log.Printf("Invalid BLE GATT UUID %q: %s", cfg.UUIDGatt, err.Error())
			continue
		}

		// Allocate a fresh live bleConnection instead of mutating globalSettings.BleOutputs directly.
		conn := &bleConnection{
			Capability:  cfg.Capability,
			UUIDService: cfg.UUIDService,
			UUIDGatt:    cfg.UUIDGatt,
			Interface:   "ble",
			Protocol:    detectClientProtocol(cfg.Capability, "ble"),
			Queue:       NewMessageQueue(1024),
			mu:          &sync.Mutex{},
		}
		err = bleAdapter.AddService(&bluetooth.Service{
			UUID: svcUUID,
			Characteristics: []bluetooth.CharacteristicConfig{
				{
					Handle: &conn.Characteristic,
					UUID:   gattUUID,
					Value:  []byte{},
					Flags:  bluetooth.CharacteristicNotifyPermission | bluetooth.CharacteristicReadPermission | bluetooth.CharacteristicWriteWithoutResponsePermission,
				},
			},
		})
		if err != nil {
			log.Printf("Failed to bring up BLE Gatt Service %s: %s", conn.UUIDService, err.Error())
			continue
		}
		netMutex.Lock()
		clientConnections[conn.GetConnectionKey()] = conn
		netMutex.Unlock()
		go connectionWriter(conn)
	}
}

func onConnectionClosed(conn connection) {
	if conn == nil {
		return
	}
	netMutex.Lock()
	key := conn.GetConnectionKey()
	if existing, ok := clientConnections[key]; ok && existing == conn {
		delete(clientConnections, key)
	}
	netMutex.Unlock()
}

func collectMessages(conn connection) []byte {
	data := make([]byte, 0)
	maxMsgLen := 0
	for {
		if conn.IsSleeping() || conn.IsThrottled() {
			_, prio := conn.MessageQueue().PeekFirst()
			if conn.IsSleeping() && prio > -10 {
				// If we are sleeping, only send heartbeats to detect a client becoming available
				return data
			}
			if conn.IsThrottled() && prio > 0 {
				// if throttled, only send important stuff (position, status, crucial traffic)
				return data
			}
		}

		newData, _ := conn.MessageQueue().PopFirst()
		if newData == nil {
			return data // no more data to send
		}
		msg, ok := newData.([]byte)
		if !ok {
			continue
		}
		data = append(data, msg...)
		// So we can estimate if another message fits in
		if len(msg) > maxMsgLen {
			maxMsgLen = len(msg)
		}
		if len(data)+maxMsgLen > conn.GetDesiredPacketSize() {
			// Probably can't fit in another message
			return data
		}
	}
}

func connectionWriter(connection connection) {
	for {
		queue := connection.MessageQueue()
		if queue.IsClosed() {
			return
		}
		<-queue.DataAvailable
		for {
			if queue.IsClosed() {
				return
			}

			// Try to send around 1kb of data per packet to reduce IOPS when queue is full
			msg := collectMessages(connection)
			if len(msg) == 0 {
				break // Wait for next time that the DataAvailable channel has more for us
			}

			w := connection.Writer()
			if w == nil {
				connection.OnError(io.ErrClosedPipe)
				break
			}

			written := 0
			for written < len(msg) {
				writtenNow, err := w.Write(msg[written:])
				written += writtenNow
				if err != nil {
					connection.OnError(err)
					break
				}
				if writtenNow == 0 {
					connection.OnError(io.ErrShortWrite)
					break
				}
			}
			if written > 0 {
				if statsConn, ok := connection.(interface{ RecordSent(uint64, uint64) }); ok {
					statsConn.RecordSent(1, uint64(written))
				}
			}
			atomic.AddUint32(&totalNetworkMessagesSent, 1)
			atomic.AddUint64(&networkDataMessagesSent, 1)
			atomic.AddUint64(&networkDataBytesSent, uint64(written))
		}
	}
}

func sendMsg(msg []byte, msgType uint8, maxAge time.Duration, priority int32) {
	if (msgType & NETWORK_GDL90_STANDARD) != 0 {
		// It's a GDL90 message - non-blocking UI broadcast so slow WS clients never stall GDL90 output.
		select {
		case networkGDL90Chan <- msg:
		default:
		}
	}

	netMutex.Lock()
	defer netMutex.Unlock()

	// Push to all UDP, TCP, Serial connections if they support the message
	for _, conn := range clientConnections {
		// Check if this port is able to accept the type of message we're sending.
		if (conn.Capabilities() & msgType) == 0 {
			continue
		}
		conn.MessageQueue().Put(priority, maxAge, msg)
	}
}

func sendGDL90(msg []byte, maxAge time.Duration, priority int32) {
	sendMsg(msg, NETWORK_GDL90_STANDARD, maxAge, priority)
}

func sendXPlane(msg []byte, maxAge time.Duration, priority int32) {
	sendMsg(msg, NETWORK_POSITION_FFSIM, maxAge, priority)
}

func sendNetFLARM(msg string, maxAge time.Duration, priority int32) {
	sendMsg([]byte(msg), NETWORK_FLARM_NMEA, maxAge, priority)
}

func monitorDHCPLeases() {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for range timer.C {
		refreshConnectedClients()
	}
}

func icmpEchoSender(c *icmp.PacketConn, done <-chan struct{}) {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}

		// Snapshot unique client IPs under netMutex, then release before network syscalls.
		netMutex.Lock()
		ips := make(map[string]struct{})
		for _, conn := range clientConnections {
			if netconn, ok := conn.(*networkConnection); ok && netconn.Ip != "" {
				ips[netconn.Ip] = struct{}{}
			}
		}
		netMutex.Unlock()

		// Send to all IPs without holding netMutex.
		for ip := range ips {
			parsedIP := net.ParseIP(ip)
			if parsedIP == nil {
				continue
			}
			wm := icmp.Message{
				Type: ipv4.ICMPTypeEcho, Code: 0,
				Body: &icmp.Echo{
					ID:   os.Getpid() & 0xffff,
					Seq:  1,
					Data: []byte("STRATUX"),
				},
			}
			wb, err := wm.Marshal(nil)
			if err != nil {
				log.Printf("couldn't send ICMP Echo: %s\n", err.Error())
				continue
			}
			if _, err := c.WriteTo(wb, &net.IPAddr{IP: parsedIP}); err != nil {
				log.Printf("couldn't send ICMP Echo: %s\n", err.Error())
				continue
			}
			atomic.AddUint32(&totalNetworkMessagesSent, 1)
		}
	}
}

func getNetworkConn(ipAndPort string) *networkConnection {
	netMutex.Lock()
	defer netMutex.Unlock()
	if strings.Contains(ipAndPort, ":") {
		if conn, ok := clientConnections[ipAndPort]; ok {
			if netconn, ok := conn.(*networkConnection); ok {
				return netconn
			}
		}
	}
	return nil
}

func getNetworkConnsByIp(ip string) []*networkConnection {
	netMutex.Lock()
	defer netMutex.Unlock()
	conns := make([]*networkConnection, 0)
	// Search for any connection with the exact same IP to match ping responses
	for _, conn := range clientConnections {
		if netconn, ok := conn.(*networkConnection); ok {
			if netconn.Ip == ip {
				conns = append(conns, netconn)
			}
		}
	}
	return conns
}

func getSerialConns() []*serialConnection {
	netMutex.Lock()
	defer netMutex.Unlock()
	conns := make([]*serialConnection, 0)
	for _, conn := range clientConnections {
		if s, ok := conn.(*serialConnection); ok {
			conns = append(conns, s)
		}
	}
	return conns
}

func closeSerial(dev string) {
	netMutex.Lock()
	var conn connection
	if c, ok := clientConnections[dev]; ok {
		conn = c
		delete(clientConnections, dev)
	}
	netMutex.Unlock()
	if conn != nil {
		go conn.Close()
	}
}

// Monitor clients going in/out of sleep mode via ICMP unreachable packets.
func sleepMonitor() {
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		log.Printf("error listening for icmp - sending data to all ports for all connected clients. err: %s", err)
		return
	}
	done := make(chan struct{})
	defer func() {
		close(done)
		_ = c.Close()
	}()
	go icmpEchoSender(c, done)

	buf := make([]byte, 1500)
	for {
		n, peer, err := c.ReadFrom(buf)
		if err != nil {
			log.Printf("sleepMonitor ReadFrom: %s\n", err.Error())
			continue
		}
		msg, err := icmp.ParseMessage(1, buf[:n])
		if err != nil {
			continue
		}

		ip := peer.String()

		// Look for echo replies, mark it as received.
		if msg.Type == ipv4.ICMPTypeEchoReply {
			now := stratuxClock.GetTime()
			for _, conn := range getNetworkConnsByIp(ip) {
				conn.SetPingPong(now)
			}
			continue // No further processing needed.
		}

		// Only deal with ICMP Unreachable packets (since that's what iOS and Android seem to be sending whenever the apps are not available).
		if msg.Type != ipv4.ICMPTypeDestinationUnreachable {
			continue
		}
		// Packet parsing.
		mb, err := msg.Body.Marshal(1)
		if err != nil {
			continue
		}
		// ICMP Destination Unreachable body has 4 bytes unused/next-hop MTU, followed by inner IPv4 header (min 20 bytes) + at least 8 bytes of UDP header.
		if len(mb) < 4+20+4 {
			continue
		}
		ihl := int(mb[4]&0x0f) * 4
		udpDstPortOffset := 4 + ihl + 2
		if ihl < 20 || len(mb) < udpDstPortOffset+2 {
			continue
		}

		// The unreachable UDP destination port.
		port := (uint16(mb[udpDstPortOffset]) << 8) | uint16(mb[udpDstPortOffset+1])
		ipAndPort := net.JoinHostPort(ip, strconv.Itoa(int(port)))
		conn := getNetworkConn(ipAndPort)
		if conn != nil {
			conn.SetUnreachable(stratuxClock.GetTime())
		}
	}
}

func networkStatsCounter() {
	timer := time.NewTicker(1 * time.Second)
	defer timer.Stop()
	var previousNetworkMessagesSent, previousNetworkBytesSent uint64

	for range timer.C {
		totalMsgs := atomic.LoadUint64(&networkDataMessagesSent)
		totalBytes := atomic.LoadUint64(&networkDataBytesSent)

		globalStatusMutex.Lock()
		globalStatus.NetworkDataMessagesSent = totalMsgs
		globalStatus.NetworkDataBytesSent = totalBytes
		globalStatus.NetworkDataMessagesSentLastSec = totalMsgs - previousNetworkMessagesSent
		globalStatus.NetworkDataBytesSentLastSec = totalBytes - previousNetworkBytesSent
		globalStatusMutex.Unlock()

		previousNetworkMessagesSent = totalMsgs
		previousNetworkBytesSent = totalBytes
	}
}

func initNetwork() {
	networkGDL90Chan = make(chan []byte, 1024)
	clientConnections = make(map[string]connection)

	netMutex = &sync.Mutex{}
	refreshConnectedClients()
	go monitorDHCPLeases() // Checks for new UDP connections
	go sleepMonitor()
	go networkStatsCounter()
	go serialOutWatcher()  // Check for new Serial connections
	go networkOutWatcher() // Pushes to websocket
	go tcpNMEAOutListener()
	go tcpNMEAInListener()
	go getNetworkStats()
	go initBluetooth()
}
