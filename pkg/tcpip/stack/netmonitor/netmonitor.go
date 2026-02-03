// Copyright 2025 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package netmonitor provides TCP/UDP traffic forwarding to an external collector.
//
// Protocol:
// 1. On connection, a handshake line is sent: "CONTAINER_ID=<id>\n"
// 2. After handshake, packets are sent in this format:
//    [dir:1][timestamp:8][ipver:1][proto:1][srcIP:4/16][dstIP:4/16][srcPort:2][dstPort:2][len:4][payload:N]
//    - dir: 0 for outbound, 1 for inbound
//    - timestamp: nanoseconds since epoch (big-endian)
//    - ipver: 4 or 6
//    - proto: 6=TCP, 17=UDP
//    - srcIP/dstIP: 4 bytes (IPv4) or 16 bytes (IPv6)
//    - srcPort/dstPort: big-endian uint16
//    - len: payload length, big-endian uint32
//    - payload: raw packet payload
package netmonitor

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/log"
)

const (
	// DirOutbound represents an outbound packet direction.
	DirOutbound = 0
	// DirInbound represents an inbound packet direction.
	DirInbound = 1

	// ProtoTCP is the protocol number for TCP.
	ProtoTCP = 6
	// ProtoUDP is the protocol number for UDP.
	ProtoUDP = 17

	// Queue size for async forwarding (buffered channel)
	queueSize = 4096

	// IPv4 header size for our protocol
	ipv4HeaderSize = 23 // 1+8+1+1+4+4+2+2

	// IPv6 header size for our protocol
	ipv6HeaderSize = 47 // 1+8+1+1+16+16+2+2
)

// packet represents a packet to be forwarded
type packet struct {
	data []byte
}

var (
	mu        sync.RWMutex
	conn      net.Conn
	queue     chan *packet
	enabled   bool
	dropCount uint64
)

// Init initializes the net monitor with an already-connected file descriptor.
// Must be called before any Forward calls.
// The FD should be a connected Unix domain socket.
func Init(fd int) error {
	mu.Lock()
	defer mu.Unlock()

	if enabled {
		return nil // Already initialized
	}

	if fd < 0 {
		// No monitor FD provided, monitoring is disabled
		return nil
	}

	// Create a file from the FD
	file := os.NewFile(uintptr(fd), "netmonitor-socket")
	if file == nil {
		return fmt.Errorf("failed to create file from FD %d", fd)
	}

	// Convert file to net.Conn
	var err error
	conn, err = net.FileConn(file)
	file.Close() // Close the file, FileConn dups the FD
	if err != nil {
		log.Warningf("Net monitor: failed to create connection from FD %d: %v", fd, err)
		return err
	}

	queue = make(chan *packet, queueSize)
	enabled = true

	// Start async worker
	go worker()

	log.Infof("Net monitor: initialized with FD %d", fd)
	return nil
}

// Enabled returns true if net monitoring is enabled.
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled
}

// worker drains the queue and writes to the UDS socket
func worker() {
	for pkt := range queue {
		if conn == nil {
			continue
		}

		// Write the pre-built packet data
		if _, err := conn.Write(pkt.data); err != nil {
			log.Warningf("Net monitor: write failed: %v", err)
			continue
		}
	}
}

// Forward forwards a packet to the external collector asynchronously.
// dir: DirOutbound (0) or DirInbound (1)
// proto: ProtoTCP (6) or ProtoUDP (17)
// srcIP, dstIP: source and destination IP addresses as byte slices (4 bytes for IPv4, 16 for IPv6)
// srcPort, dstPort: source and destination ports
// payload: packet payload data (will be copied)
func Forward(dir byte, proto byte, srcIP, dstIP []byte, srcPort, dstPort uint16, payload []byte) {
	mu.RLock()
	if !enabled {
		mu.RUnlock()
		return
	}
	mu.RUnlock()

	// Determine IP version
	ipver := byte(4)
	if len(srcIP) == 16 {
		ipver = 6
	}

	// Calculate header size
	headerSize := ipv4HeaderSize
	if ipver == 6 {
		headerSize = ipv6HeaderSize
	}

	// Build packet
	// [dir:1][timestamp:8][ipver:1][proto:1][srcIP:4/16][dstIP:4/16][srcPort:2][dstPort:2][len:4][payload:N]
	dataLen := uint32(len(payload))
	totalLen := headerSize + 4 + int(dataLen) // header + len field + payload
	data := make([]byte, totalLen)

	offset := 0

	// Direction
	data[offset] = dir
	offset++

	// Timestamp (nanoseconds since epoch)
	binary.BigEndian.PutUint64(data[offset:offset+8], uint64(time.Now().UnixNano()))
	offset += 8

	// IP version
	data[offset] = ipver
	offset++

	// Protocol
	data[offset] = proto
	offset++

	// Source IP
	copy(data[offset:], srcIP)
	offset += len(srcIP)

	// Destination IP
	copy(data[offset:], dstIP)
	offset += len(dstIP)

	// Source port
	binary.BigEndian.PutUint16(data[offset:offset+2], srcPort)
	offset += 2

	// Destination port
	binary.BigEndian.PutUint16(data[offset:offset+2], dstPort)
	offset += 2

	// Payload length
	binary.BigEndian.PutUint32(data[offset:offset+4], dataLen)
	offset += 4

	// Payload
	if dataLen > 0 {
		copy(data[offset:], payload)
	}

	pkt := &packet{
		data: data,
	}

	// Non-blocking send - drop if queue is full
	select {
	case queue <- pkt:
		// Success
	default:
		// Queue full, drop packet
		mu.Lock()
		dropCount++
		mu.Unlock()
	}
}

// Close closes the monitor connection
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if !enabled {
		return
	}

	enabled = false
	close(queue)

	if conn != nil {
		conn.Close()
		conn = nil
	}

	if dropCount > 0 {
		log.Warningf("Net monitor: dropped %d packets due to queue overflow", dropCount)
	}
}
