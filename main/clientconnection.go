/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	clientconnection.go: Client connection implementations (UDP, Serial, TCP, BLE).
*/

package main

import (
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/stratux/serial"
	"tinygo.org/x/bluetooth"
)

// Connection interface where we will write data - no matter if UDP, Serial, TCP, or BLE
type connection interface {
	GetConnectionKey() string // e.g. "192.168.10.22:12345" for udp, "TCP:192.168.10.22:12345" for TCP, "/dev/serialout0" for serialout
	MessageQueue() *MessageQueue
	Writer() io.Writer
	IsThrottled() bool
	IsSleeping() bool
	Capabilities() uint8
	GetDesiredPacketSize() int
	OnError(error)
	Close()
}

type networkConnection struct {
	Conn       *net.UDPConn
	Ip         string
	Port       uint32
	Capability uint8
	Interface  string
	Protocol   string
	Queue      *MessageQueue `json:"-"` // don't store in settings

	LastPingResponse time.Time // last time the client responded
	LastPongResponse time.Time // last time the client responded
	LastUnreachable  time.Time // Last time the device sent an ICMP Unreachable packet.
	/*
		Sleep mode/throttle variables. "sleep mode" is actually now just a very reduced packet rate, since we don't know positively
		 when a client is ready to accept packets - we just assume so if we don't receive ICMP Unreachable packets in 5 secs.
	*/
	SleepFlag   bool // Whether or not this client has been marked as sleeping - only used for debugging
	PacketsSent uint64
	BytesSent   uint64

	mu *sync.Mutex // guards LastPingResponse, LastPongResponse, LastUnreachable, SleepFlag, Interface, PacketsSent, BytesSent, Queue
}

func (conn *networkConnection) lock() {
	if conn.mu != nil {
		conn.mu.Lock()
	}
}

func (conn *networkConnection) unlock() {
	if conn.mu != nil {
		conn.mu.Unlock()
	}
}

func (conn *networkConnection) SetInterface(iface string) {
	conn.lock()
	conn.Interface = iface
	conn.unlock()
}

func (conn *networkConnection) RecordSent(packets, bytes uint64) {
	conn.lock()
	conn.PacketsSent += packets
	conn.BytesSent += bytes
	conn.unlock()
}

func (conn *networkConnection) MarshalJSON() ([]byte, error) {
	if conn == nil {
		return []byte("null"), nil
	}
	type networkConnectionAlias networkConnection
	conn.lock()
	snap := networkConnectionAlias(*conn)
	conn.unlock()
	if snap.Interface == "" && snap.Ip != "" {
		snap.Interface = detectClientInterface(snap.Ip)
	}
	if snap.Protocol == "" {
		snap.Protocol = detectClientProtocol(snap.Capability, "udp")
	}
	return json.Marshal(&struct {
		networkConnectionAlias
		Active bool
	}{
		networkConnectionAlias: snap,
		Active:                 !snap.SleepFlag,
	})
}

func (conn *networkConnection) SetPingPong(t time.Time) {
	conn.lock()
	conn.LastPingResponse = t
	conn.LastPongResponse = t
	conn.unlock()
}

func (conn *networkConnection) SetUnreachable(t time.Time) {
	conn.lock()
	conn.LastUnreachable = t
	conn.unlock()
}

func (conn *networkConnection) HasRecentPing(d time.Duration) bool {
	conn.lock()
	lastPing := conn.LastPingResponse
	lastPong := conn.LastPongResponse
	conn.unlock()
	if !lastPing.IsZero() && stratuxClock.Since(lastPing) < d {
		return true
	}
	if !lastPong.IsZero() && stratuxClock.Since(lastPong) < d {
		return true
	}
	return false
}

func (conn *networkConnection) MessageQueue() *MessageQueue {
	conn.lock()
	if conn.Queue == nil {
		conn.Queue = NewMessageQueue(1024)
	}
	q := conn.Queue
	conn.unlock()
	return q
}

func (conn *networkConnection) Writer() io.Writer {
	if conn.Conn == nil {
		return nil
	}
	return conn.Conn
}

/*
IsThrottled().

	Checks if a client identifier 'ip:port' is throttled.
	Throttle mode is for testing port open and giving some start-up time to the app.
	Throttling means that we only send important packets for first 15 seconds (location, status, very close traffic).
*/
func (conn *networkConnection) IsThrottled() bool {
	conn.lock()
	lastUnreachable := conn.LastUnreachable
	conn.unlock()
	return (rand.Int()%1000 != 0) && stratuxClock.Since(lastUnreachable) < (15*time.Second)
}

/*
IsSleeping().

	Check if a client identifier 'ip:port' is in either a sleep or active state.
*/
func (conn *networkConnection) IsSleeping() bool {
	// Unable to listen to ICMP without root - send to everything. Just for debugging.
	globalSettingsMutex.RLock()
	noSleep := globalSettings.NoSleep
	globalSettingsMutex.RUnlock()
	if isX86DebugMode() || noSleep {
		return false
	}

	conn.lock()
	defer conn.unlock()
	// No ping response. Assume disconnected/sleeping device.
	if conn.LastPongResponse.IsZero() || stratuxClock.Since(conn.LastPongResponse) > (10*time.Second) {
		conn.SleepFlag = true
	} else if conn.LastPingResponse.IsZero() || stratuxClock.Since(conn.LastPingResponse) > (10*time.Second) {
		conn.SleepFlag = true
	} else if stratuxClock.Since(conn.LastUnreachable) < (5 * time.Second) {
		conn.SleepFlag = true
	} else {
		conn.SleepFlag = false
	}
	return conn.SleepFlag
}

func (conn *networkConnection) Capabilities() uint8 {
	return conn.Capability
}

func (conn *networkConnection) GetDesiredPacketSize() int {
	if conn.Capabilities()&(NETWORK_POSITION_FFSIM|NETWORK_AHRS_FFSIM) > 0 {
		// Hack: some software doesn't handle X-Plane as a stream correctly, e.g. SkyDemon, and requires each message in a separate packet, or it will crash.
		return 1
	}
	return 1024
}

func (conn *networkConnection) OnError(err error) {
	// Ignore for UDP. We keep the socket always open and just try to push data
}

func (conn *networkConnection) Close() {
	// Ignore for UDP. We keep the socket always open and just try to push data
}

func (conn *networkConnection) GetConnectionKey() string {
	return conn.Ip + ":" + strconv.Itoa(int(conn.Port))
}

type serialConnection struct {
	DeviceString string
	Baud         int
	Capability   uint8
	Interface    string
	Protocol     string
	PacketsSent  uint64
	BytesSent    uint64
	serialPort   *serial.Port
	Queue        *MessageQueue `json:"-"` // don't store in settings
	mu           *sync.Mutex
}

func (conn *serialConnection) lock() {
	if conn.mu != nil {
		conn.mu.Lock()
	}
}

func (conn *serialConnection) unlock() {
	if conn.mu != nil {
		conn.mu.Unlock()
	}
}

func (conn *serialConnection) RecordSent(packets, bytes uint64) {
	conn.lock()
	conn.PacketsSent += packets
	conn.BytesSent += bytes
	conn.unlock()
}

func (conn *serialConnection) MarshalJSON() ([]byte, error) {
	if conn == nil {
		return []byte("null"), nil
	}
	conn.lock()
	dev := conn.DeviceString
	baud := conn.Baud
	capVal := conn.Capability
	iface := conn.Interface
	proto := conn.Protocol
	packets := conn.PacketsSent
	bytes := conn.BytesSent
	sleeping := conn.serialPort == nil
	conn.unlock()

	if iface == "" {
		iface = "serial"
	}
	if proto == "" {
		proto = detectClientProtocol(capVal, "serial")
	}
	return json.Marshal(&struct {
		DeviceString string
		Baud         int
		Capability   uint8
		Interface    string
		Protocol     string
		SleepFlag    bool
		Active       bool
		PacketsSent  uint64
		BytesSent    uint64
	}{
		DeviceString: dev,
		Baud:         baud,
		Capability:   capVal,
		Interface:    iface,
		Protocol:     proto,
		SleepFlag:    sleeping,
		Active:       !sleeping,
		PacketsSent:  packets,
		BytesSent:    bytes,
	})
}

func (conn *serialConnection) MessageQueue() *MessageQueue {
	conn.lock()
	if conn.Queue == nil {
		conn.Queue = NewMessageQueue(1024)
	}
	q := conn.Queue
	conn.unlock()
	return q
}

func (conn *serialConnection) Writer() io.Writer {
	conn.lock()
	sp := conn.serialPort
	conn.unlock()
	if sp == nil {
		return nil
	}
	return sp
}

func (conn *serialConnection) IsThrottled() bool {
	return false
}

func (conn *serialConnection) IsSleeping() bool {
	conn.lock()
	sleeping := conn.serialPort == nil
	conn.unlock()
	return sleeping
}

func (conn *serialConnection) Capabilities() uint8 {
	return conn.Capability
}

func (conn *serialConnection) GetDesiredPacketSize() int {
	return 128
}

func (conn *serialConnection) OnError(err error) {
	// Close connection and queue
	log.Printf("Serial connection %s closed: %s", conn.DeviceString, err.Error())
	conn.Close()
}

func (conn *serialConnection) Close() {
	conn.lock()
	sp := conn.serialPort
	q := conn.Queue
	conn.serialPort = nil
	conn.unlock()

	if sp != nil {
		_ = sp.Close()
		log.Printf("Closed serial port %s", conn.DeviceString)
		if q != nil {
			q.Close()
		}
		onConnectionClosed(conn)
	}
}

func (conn *serialConnection) GetConnectionKey() string {
	return conn.DeviceString
}

type tcpConnection struct {
	Conn        *net.TCPConn
	Queue       *MessageQueue `json:"-"`
	Capability  uint8
	Key         string
	Interface   string
	Protocol    string
	PacketsSent uint64
	BytesSent   uint64
	mu          sync.Mutex
}

func (conn *tcpConnection) RecordSent(packets, bytes uint64) {
	conn.mu.Lock()
	conn.PacketsSent += packets
	conn.BytesSent += bytes
	conn.mu.Unlock()
}

func (conn *tcpConnection) MarshalJSON() ([]byte, error) {
	if conn == nil {
		return []byte("null"), nil
	}
	conn.mu.Lock()
	key := conn.Key
	capVal := conn.Capability
	iface := conn.Interface
	proto := conn.Protocol
	packets := conn.PacketsSent
	bytes := conn.BytesSent
	sleeping := conn.Conn == nil
	remoteAddr := ""
	if conn.Conn != nil && conn.Conn.RemoteAddr() != nil {
		remoteAddr = conn.Conn.RemoteAddr().String()
	}
	conn.mu.Unlock()

	if iface == "" && remoteAddr != "" {
		iface = detectClientInterface(remoteAddr)
	}
	if proto == "" {
		proto = detectClientProtocol(capVal, "tcp")
	}
	return json.Marshal(&struct {
		Key         string
		RemoteAddr  string
		Capability  uint8
		Interface   string
		Protocol    string
		SleepFlag   bool
		Active      bool
		PacketsSent uint64
		BytesSent   uint64
	}{
		Key:         key,
		RemoteAddr:  remoteAddr,
		Capability:  capVal,
		Interface:   iface,
		Protocol:    proto,
		SleepFlag:   sleeping,
		Active:      !sleeping,
		PacketsSent: packets,
		BytesSent:   bytes,
	})
}

func (conn *tcpConnection) MessageQueue() *MessageQueue {
	conn.mu.Lock()
	if conn.Queue == nil {
		conn.Queue = NewMessageQueue(1024)
	}
	q := conn.Queue
	conn.mu.Unlock()
	return q
}

func (conn *tcpConnection) Writer() io.Writer {
	return conn
}

func (conn *tcpConnection) Write(p []byte) (int, error) {
	conn.mu.Lock()
	c := conn.Conn
	conn.mu.Unlock()
	if c == nil {
		return 0, io.ErrClosedPipe
	}
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	return c.Write(p)
}

func (conn *tcpConnection) IsThrottled() bool {
	return false
}

func (conn *tcpConnection) IsSleeping() bool {
	conn.mu.Lock()
	sleeping := conn.Conn == nil
	conn.mu.Unlock()
	return sleeping
}

func (conn *tcpConnection) Capabilities() uint8 {
	return conn.Capability
}

func (conn *tcpConnection) GetDesiredPacketSize() int {
	return 512
}

func (conn *tcpConnection) OnError(err error) {
	// Close connection and queue
	conn.mu.Lock()
	c := conn.Conn
	conn.mu.Unlock()
	if c != nil {
		log.Printf("TCP connection %s closed: %s", c.RemoteAddr(), err.Error())
		conn.Close()
	}
}

func (conn *tcpConnection) Close() {
	// Close connection and queue
	conn.mu.Lock()
	c := conn.Conn
	q := conn.Queue
	conn.Conn = nil
	conn.mu.Unlock()

	if c != nil {
		_ = c.Close()
		if q != nil {
			q.Close()
		}
		onConnectionClosed(conn)
	}
}

func (conn *tcpConnection) GetConnectionKey() string {
	return conn.Key
}

type bleConnection struct {
	Capability     uint8
	UUIDService    string // SoftRF: 0xFFE0
	UUIDGatt       string
	Interface      string
	Protocol       string
	PacketsSent    uint64
	BytesSent      uint64
	Characteristic bluetooth.Characteristic
	Queue          *MessageQueue `json:"-"` // don't store in settings
	mu             *sync.Mutex
}

func (conn *bleConnection) lock() {
	if conn.mu != nil {
		conn.mu.Lock()
	}
}

func (conn *bleConnection) unlock() {
	if conn.mu != nil {
		conn.mu.Unlock()
	}
}

func (conn *bleConnection) RecordSent(packets, bytes uint64) {
	conn.lock()
	conn.PacketsSent += packets
	conn.BytesSent += bytes
	conn.unlock()
}

func (conn *bleConnection) MarshalJSON() ([]byte, error) {
	if conn == nil {
		return []byte("null"), nil
	}
	conn.lock()
	capVal := conn.Capability
	uuidSvc := conn.UUIDService
	uuidGatt := conn.UUIDGatt
	iface := conn.Interface
	proto := conn.Protocol
	packets := conn.PacketsSent
	bytes := conn.BytesSent
	conn.unlock()

	if iface == "" {
		iface = "ble"
	}
	if proto == "" {
		proto = detectClientProtocol(capVal, "ble")
	}
	return json.Marshal(&struct {
		Capability  uint8
		UUIDService string
		UUIDGatt    string
		Interface   string
		Protocol    string
		SleepFlag   bool
		Active      bool
		PacketsSent uint64
		BytesSent   uint64
	}{
		Capability:  capVal,
		UUIDService: uuidSvc,
		UUIDGatt:    uuidGatt,
		Interface:   iface,
		Protocol:    proto,
		SleepFlag:   false,
		Active:      true,
		PacketsSent: packets,
		BytesSent:   bytes,
	})
}

func (conn *bleConnection) MessageQueue() *MessageQueue {
	conn.lock()
	if conn.Queue == nil {
		conn.Queue = NewMessageQueue(1024)
	}
	q := conn.Queue
	conn.unlock()
	return q
}

func (conn *bleConnection) Writer() io.Writer {
	return conn
}

const bleMaxChunkSize = 20 // BLE 4.0 default ATT MTU payload size

func (conn *bleConnection) Write(p []byte) (n int, err error) {
	for n < len(p) {
		end := n + bleMaxChunkSize
		if end > len(p) {
			end = len(p)
		}
		written, err := conn.Characteristic.Write(p[n:end])
		n += written
		if err != nil {
			return n, err
		}
		if written == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

func (conn *bleConnection) IsThrottled() bool {
	return false
}

func (conn *bleConnection) IsSleeping() bool {
	return false
}

func (conn *bleConnection) Capabilities() uint8 {
	return conn.Capability
}

func (conn *bleConnection) GetDesiredPacketSize() int {
	return bleMaxChunkSize
}

func (conn *bleConnection) OnError(err error) {
	log.Printf("BLE Error %s", err.Error())
}

func (conn *bleConnection) Close() {
}

func (conn *bleConnection) GetConnectionKey() string {
	return conn.UUIDService
}
