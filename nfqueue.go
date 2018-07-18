/**
 * @license
 * Copyright 2018 Telefónica Investigación y Desarrollo, S.A.U
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nfqueue

/*
#cgo pkg-config: libnetfilter_queue
#cgo CFLAGS: -Wall -Werror -I/usr/include
#cgo LDFLAGS: -L/usr/lib64/

#include "nfqueue.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"net"
	"unsafe"
)

// PacketHandler is an interface to handle a packet retrieved by netfilter.
type PacketHandler interface {
	Handle(p *Packet)
}

// PacketMeta contains metadata about a packet
type PacketMeta struct {
	HasUID     bool
	HasGID     bool
	UID        uint32
	GID        uint32
	InDev      uint32
	OutDev     uint32
	PhysInDev  uint32
	PhysOutDev uint32
	NFMark     uint32
	HWAddr     []byte
}

// InDevName returns the name of the input interface
func (m *PacketMeta) InDevName() string {
	iface, err := net.InterfaceByIndex(int(m.InDev))
	if err != nil {
		return ""
	}
	return iface.Name
}

// OutDevName returns the name of the output interface
func (m *PacketMeta) OutDevName() string {
	iface, err := net.InterfaceByIndex(int(m.OutDev))
	if err != nil {
		return ""
	}
	return iface.Name
}

// PhysInDevName returns the name of the physical input interface
func (m *PacketMeta) PhysInDevName() string {
	iface, err := net.InterfaceByIndex(int(m.PhysInDev))
	if err != nil {
		return ""
	}
	return iface.Name
}

// PhysOutDevName returns the name of the physical output interface
func (m *PacketMeta) PhysOutDevName() string {
	iface, err := net.InterfaceByIndex(int(m.PhysOutDev))
	if err != nil {
		return ""
	}
	return iface.Name
}

// MACAddr returns the human-readable value of the MAC address for the packet source
func (m *PacketMeta) MACAddr() string {
	return fmt.Sprintf(
		"%02X:%02X:%02X:%02X:%02X:%02X",
		m.HWAddr[0], m.HWAddr[1], m.HWAddr[2], m.HWAddr[3], m.HWAddr[4], m.HWAddr[5],
	)
}

// Packet struct provides the packet data and methods to accept, drop or modify the packet.
type Packet struct {
	Buffer []byte
	Meta   *PacketMeta
	id     uint32
	q      *Queue
}

// Accept the packet.
func (p *Packet) Accept() error {
	return p.setVerdict(C.NF_ACCEPT, 0, nil)
}

// Drop the packet.
func (p *Packet) Drop() error {
	return p.setVerdict(C.NF_DROP, 0, nil)
}

// Modify the packet with a new buffer.
func (p *Packet) Modify(buffer []byte) error {
	return p.setVerdict(C.NF_ACCEPT, C.u_int32_t(len(buffer)), (*C.uchar)(unsafe.Pointer(&buffer[0])))
}

func (p *Packet) setVerdict(verdict, len C.u_int32_t, buffer *C.uchar) error {
	if C.nfq_set_verdict(p.q.qh, C.u_int32_t(p.id), verdict, len, buffer) < 0 {
		return fmt.Errorf("Error setting verdict %d for packet %d", verdict, p.id)
	}
	return nil
}

// QueueFlag configures the kernel queue.
type QueueFlag C.uint32_t

const (
	// FailOpen (requires Linux kernel >= 3.6): the kernel will accept the packets if the kernel queue gets full.
	// If this flag is not set, the default action in this case is to drop packets.
	FailOpen QueueFlag = (1 << 0)
	// Conntrack (requires Linux kernel >= 3.6): the kernel will include the Connection Tracking system information.
	Conntrack QueueFlag = (1 << 1)
	// GSO (requires Linux kernel >= 3.10): the kernel will not normalize offload packets,
	// i.e. your application will need to be able to handle packets larger than the mtu.
	GSO QueueFlag = (1 << 2)
	// UIDGid makes the kernel dump UID and GID of the socket to which each packet belongs.
	UIDGid QueueFlag = (1 << 3)
	// Secctx makes the kernel dump security context of the socket to which each packet belongs.
	Secctx QueueFlag = (1 << 4)
)

// QueueConfig contains optional configuration parameters to initialize a queue.
type QueueConfig struct {
	MaxPackets uint32
	QueueFlags []QueueFlag
	BufferSize uint32
}

// Queue represents a netfilter queue with methods to start processing the packets (Run) and to stop
type Queue struct {
	ID      uint16
	handler PacketHandler
	cfg     *QueueConfig
	h       *C.struct_nfq_handle
	qh      *C.struct_nfq_q_handle
	fd      C.int
}

// NewQueue creates a Queue instance and registers it.
func NewQueue(queueID uint16, handler PacketHandler, cfg *QueueConfig) *Queue {
	if cfg == nil {
		cfg = &QueueConfig{}
	}
	q := &Queue{
		ID:      queueID,
		handler: handler,
		cfg:     cfg,
	}
	queueRegistry.Register(queueID, q)
	return q
}

// Start the processing of packets from the netfilter queue.
// This method initializes the netfilter queue and configures it.
// The thread is blocked until the queue is stopped externally.
func (q *Queue) Start() error {
	// Initialize the netfilter queue
	if q.h = C.nfq_open(); q.h == nil {
		return errors.New("Error in nfq_open")
	}

	// It is not possible to pass the queue as callback data due to error:
	// runtime error: cgo argument has Go pointer to Go pointer
	// As a result, we have to pass the queue ID and use the registry to retrieve the queue.
	if q.qh = C.nfqueue_create_queue(q.h, C.u_int16_t(q.ID)); q.qh == nil {
		return errors.New("Error in nfqueue_create_queue")
	}

	// Configure mode (packet copy) and the packet size. Note that this is not configurable on purpose.
	if C.nfq_set_mode(q.qh, C.NFQNL_COPY_PACKET, C.MAX_PACKET_SIZE) < 0 {
		return errors.New("Error in nfq_set_mode")
	}

	// Configure the max number of enqueued packets
	if q.cfg.MaxPackets > 0 {
		if ret := C.nfq_set_queue_maxlen(q.qh, C.u_int32_t(q.cfg.MaxPackets)); ret < 0 {
			return errors.New("Error in nfq_set_queue_maxlen")
		}
	}

	// Configure the flags (if any)
	if len(q.cfg.QueueFlags) > 0 {
		var flags C.uint32_t
		for _, flag := range q.cfg.QueueFlags {
			flags &= C.uint32_t(flag)
		}
		if ret := C.nfq_set_queue_flags(q.qh, flags, flags); ret < 0 {
			return errors.New("Error in nfq_set_queue_flags")
		}
	}

	if q.fd = C.nfq_fd(q.h); q.fd < 0 {
		return errors.New("Error in nfq_fd")
	}

	if q.cfg.BufferSize > 0 {
		C.nfnl_rcvbufsiz(C.nfq_nfnlh(q.h), C.uint(q.cfg.BufferSize))
	}

	if ret := C.nfqueue_loop(q.h, q.fd); ret < 0 {
		return errors.New("Error in nfqueue_loop")
	}

	return nil
}

// Stop the netfilter queue.
func (q *Queue) Stop() error {
	if C.close(q.fd) < 0 {
		return errors.New("Error closing fd")
	}
	if C.nfq_destroy_queue(q.qh) < 0 {
		return errors.New("Error in nfq_destroy_queue")
	}
	if C.nfq_close(q.h) < 0 {
		return errors.New("Error in nfq_close")
	}
	return nil
}
