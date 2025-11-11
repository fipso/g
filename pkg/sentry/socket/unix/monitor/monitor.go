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

// Package monitor provides Unix domain socket traffic forwarding.
//
// Protocol:
// 1. On connection, a handshake line is sent: "CONTAINER_ID=<id>\n"
// 2. After handshake, packets are sent in this format:
//    [direction:1byte][length:4bytes][data:Nbytes]
//    - direction: 0 for send, 1 for recv
//    - length: uint32 big-endian, length of data
//    - data: actual packet payload
package monitor

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"

	"gvisor.dev/gvisor/pkg/log"
)

const (
	// Direction flags
	dirSend = 0
	dirRecv = 1

	// Queue size for async forwarding (buffered channel)
	queueSize = 4096
)

// packet represents a packet to be forwarded
type packet struct {
	direction byte
	data      []byte
}

var (
	mu         sync.RWMutex
	conn       net.Conn
	queue      chan *packet
	enabled    bool
	dropCount  uint64
)

// Init initializes the monitor with an already-connected file descriptor.
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
	file := os.NewFile(uintptr(fd), "monitor-socket")
	if file == nil {
		return fmt.Errorf("failed to create file from FD %d", fd)
	}

	// Convert file to net.Conn
	var err error
	conn, err = net.FileConn(file)
	file.Close() // Close the file, FileConn dups the FD
	if err != nil {
		log.Warningf("UDS monitor: failed to create connection from FD %d: %v", fd, err)
		return err
	}

	queue = make(chan *packet, queueSize)
	enabled = true

	// Start async worker
	go worker()

	log.Infof("UDS monitor: initialized with FD %d", fd)
	return nil
}

// worker drains the queue and writes to the UDS socket
func worker() {
	// Pre-allocate buffer for header (1 byte direction + 4 bytes length)
	header := make([]byte, 5)

	for pkt := range queue {
		if conn == nil {
			continue
		}

		// Build packet: [direction:1byte][length:4bytes][data:Nbytes]
		// If data is nil, only send header (for recv operations where we don't capture data)
		header[0] = pkt.direction
		dataLen := uint32(len(pkt.data))
		binary.BigEndian.PutUint32(header[1:5], dataLen)

		// Write header
		if _, err := conn.Write(header); err != nil {
			log.Warningf("UDS monitor: write header failed: %v", err)
			continue
		}

		// Write data only if present
		if dataLen > 0 {
			if _, err := conn.Write(pkt.data); err != nil {
				log.Warningf("UDS monitor: write data failed: %v", err)
				continue
			}
		}
	}
}

// Forward forwards a packet to the external socket asynchronously.
// direction: 0 for send, 1 for recv
// data: packet data (will be copied)
func Forward(direction byte, data []byte) {
	mu.RLock()
	if !enabled {
		mu.RUnlock()
		return
	}
	mu.RUnlock()

	// Copy data to avoid races
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	pkt := &packet{
		direction: direction,
		data:      dataCopy,
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

// ForwardOwned forwards a packet to the external socket asynchronously.
// Takes ownership of the data buffer (no copy). Caller must not modify buffer after call.
// direction: 0 for send, 1 for recv
func ForwardOwned(direction byte, data []byte) {
	mu.RLock()
	if !enabled {
		mu.RUnlock()
		return
	}
	mu.RUnlock()

	pkt := &packet{
		direction: direction,
		data:      data,
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
		log.Warningf("UDS monitor: dropped %d packets due to queue overflow", dropCount)
	}
}
