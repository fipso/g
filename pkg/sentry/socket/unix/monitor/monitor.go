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

// Package monitor provides Unix domain socket traffic monitoring with metrics.
package monitor

import (
	"strconv"
	"sync"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/metric"
)

// Annotation key for enabling UDS monitoring
const AnnotationUDSMonitor = "dev.gvisor.uds.monitor.path"

// Metrics for UDS monitoring
var (
	// Message sizes
	sendBytesTotal = metric.MustCreateNewUint64Metric("/uds/send_bytes_total", metric.Uint64Metadata{
		Cumulative:  true,
		Sync:        false,
		Description: "Total bytes sent over monitored UDS",
	})
	recvBytesTotal = metric.MustCreateNewUint64Metric("/uds/recv_bytes_total", metric.Uint64Metadata{
		Cumulative:  true,
		Sync:        false,
		Description: "Total bytes received over monitored UDS",
	})

	// Operation counts
	sendOpsTotal = metric.MustCreateNewUint64Metric("/uds/send_ops_total", metric.Uint64Metadata{
		Cumulative:  true,
		Sync:        false,
		Description: "Total send operations",
	})
	recvOpsTotal = metric.MustCreateNewUint64Metric("/uds/recv_ops_total", metric.Uint64Metadata{
		Cumulative:  true,
		Sync:        false,
		Description: "Total receive operations",
	})
)

const (
	// Ring buffer size for storing send data for async processing
	sendBufferSize = 1024
)

// SendData represents a captured send operation for async processing
type SendData struct {
	ContainerID string
	SocketPath  string
	Data        []byte
	Size        uint64
}

// Ring buffer for async processing of send data
type ringBuffer struct {
	mu     sync.Mutex
	buffer [sendBufferSize]*SendData
	head   int
	tail   int
	count  int
}

var sendBuffer = &ringBuffer{}

// Monitor tracks UDS traffic for registered containers and socket paths.
type Monitor struct {
	mu sync.RWMutex

	// config maps container_id -> monitored socket path
	config map[string]string

	// fdMap maps "container_id:hostfd" -> socket path (for host-backed sockets)
	fdMap map[string]string
}

var globalMonitor = &Monitor{
	config: make(map[string]string),
	fdMap:  make(map[string]string),
}

// RegisterContainer registers a container for UDS monitoring with the specified socket path.
func RegisterContainer(containerID, socketPath string) {
	globalMonitor.mu.Lock()
	defer globalMonitor.mu.Unlock()

	globalMonitor.config[containerID] = socketPath
	log.Infof("UDS monitor: registered container %q with socket path %q", containerID, socketPath)
}

// UnregisterContainer removes a container from monitoring.
func UnregisterContainer(containerID string) {
	globalMonitor.mu.Lock()
	defer globalMonitor.mu.Unlock()

	delete(globalMonitor.config, containerID)
	log.Infof("UDS monitor: unregistered container %q", containerID)
}

// RegisterHostFD registers a host FD mapping for monitoring.
// This is used for host-backed sockets accessed via --host-uds=open.
func RegisterHostFD(containerID string, hostFD int, socketPath string) {
	globalMonitor.mu.Lock()
	defer globalMonitor.mu.Unlock()

	key := fdKey(containerID, hostFD)
	globalMonitor.fdMap[key] = socketPath
	log.Infof("UDS monitor: registered host FD %d for container %q with path %q", hostFD, containerID, socketPath)
}

// UnregisterHostFD removes a host FD mapping.
func UnregisterHostFD(containerID string, hostFD int) {
	globalMonitor.mu.Lock()
	defer globalMonitor.mu.Unlock()

	key := fdKey(containerID, hostFD)
	delete(globalMonitor.fdMap, key)
}

// ShouldMonitorHostFD checks if a host FD should be monitored and returns the socket path.
func ShouldMonitorHostFD(containerID string, hostFD int) (string, bool) {
	globalMonitor.mu.RLock()
	defer globalMonitor.mu.RUnlock()

	key := fdKey(containerID, hostFD)
	path, ok := globalMonitor.fdMap[key]
	return path, ok
}

// ShouldMonitor checks if the given container and socket path should be monitored.
func ShouldMonitor(containerID, socketPath string) bool {
	globalMonitor.mu.RLock()
	defer globalMonitor.mu.RUnlock()

	monitoredPath, ok := globalMonitor.config[containerID]
	if !ok {
		return false
	}

	// Check if the socket path matches the configured path
	return socketPath == monitoredPath
}

func fdKey(containerID string, hostFD int) string {
	return containerID + ":" + strconv.Itoa(hostFD)
}

// RecordSend records a send operation by storing raw data for async processing.
// This is called from the hot path, so it must be fast.
func RecordSend(containerID, socketPath string, data []byte) {
	if !ShouldMonitor(containerID, socketPath) {
		return
	}

	size := uint64(len(data))

	// Update counters (fast atomic operations)
	sendOpsTotal.IncrementBy(1)
	sendBytesTotal.IncrementBy(size)

	// Store raw data in ring buffer for async processing (external JSON parsing)
	// Make a copy to avoid data races
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	sendBuffer.push(&SendData{
		ContainerID: containerID,
		SocketPath:  socketPath,
		Data:        dataCopy,
		Size:        size,
	})
}

// RecordRecv records a receive operation - just track bytes received.
func RecordRecv(containerID, socketPath string, size uint64) {
	if !ShouldMonitor(containerID, socketPath) {
		return
	}

	// Simple counter updates - no data capture for receive
	recvOpsTotal.IncrementBy(1)
	recvBytesTotal.IncrementBy(size)
}

// Ring buffer operations
func (rb *ringBuffer) push(data *SendData) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Overwrite oldest if full
	rb.buffer[rb.tail] = data
	rb.tail = (rb.tail + 1) % sendBufferSize

	if rb.count < sendBufferSize {
		rb.count++
	} else {
		// Buffer full, move head forward (drop oldest)
		rb.head = (rb.head + 1) % sendBufferSize
	}
}

// GetSendData retrieves all buffered send data for external processing.
// This can be called by the metrics server or external process.
func GetSendData() []*SendData {
	sendBuffer.mu.Lock()
	defer sendBuffer.mu.Unlock()

	if sendBuffer.count == 0 {
		return nil
	}

	result := make([]*SendData, 0, sendBuffer.count)

	idx := sendBuffer.head
	for i := 0; i < sendBuffer.count; i++ {
		if sendBuffer.buffer[idx] != nil {
			result = append(result, sendBuffer.buffer[idx])
		}
		idx = (idx + 1) % sendBufferSize
	}

	// Clear buffer after reading
	sendBuffer.head = 0
	sendBuffer.tail = 0
	sendBuffer.count = 0

	return result
}
