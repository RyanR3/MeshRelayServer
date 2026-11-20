// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Packets from one source are handled in the order they arrived.
//
// The read loop used to start one goroutine per datagram. When a phone's
// audio reached the server in a bunch (Wi-Fi power save delivers 20 ms frames
// several at a time), those goroutines ran in no particular order and the
// frames were relayed shuffled. Receiving apps drop any frame older than the
// newest one they have played, so most of a bunched transmission was thrown
// away: steady stutter with missing audio.
//
// Each source address now has its own queue and one worker, so a source's
// packets keep their order while different sources still run side by side (a
// slow request from one unit never delays another unit's audio). A worker
// ends after a short idle time. Secure sign-in packets are answered
// statelessly and keep the one-goroutine-per-packet path, so a flood of
// hellos from made-up addresses creates no queues.

import (
	"net"
	"sync"
	"time"
)

const (
	srcQueueDepth = 512             // ~10 s of audio from one source
	srcQueueIdle  = 5 * time.Second // worker ends after this long with nothing to do
	srcQueueMax   = 8192            // sources with a live queue; beyond it, unordered
)

type srcQueue struct {
	ch chan []byte
}

type packetDispatcher struct {
	mu sync.Mutex
	q  map[string]*srcQueue
	// handle processes one packet (Server.handlePacket; replaced in tests).
	handle func([]byte, *net.UDPAddr)
}

// dispatchPacket hands one received datagram to its source's worker.
func (s *Server) dispatchPacket(pkt []byte, addr *net.UDPAddr) {
	d := &s.dispatch
	handle := d.handle
	if handle == nil {
		handle = s.handlePacket
	}
	if len(pkt) == 0 || addr == nil {
		return
	}
	if t := pkt[0]; t >= PacketTypeSecHello && t <= PacketTypeSecRetry && t != PacketTypeSecData {
		go handle(pkt, addr)
		return
	}
	key := addr.String()
	d.mu.Lock()
	if d.q == nil {
		d.q = map[string]*srcQueue{}
	}
	q := d.q[key]
	if q == nil {
		if len(d.q) >= srcQueueMax {
			d.mu.Unlock()
			go handle(pkt, addr)
			return
		}
		q = &srcQueue{ch: make(chan []byte, srcQueueDepth)}
		d.q[key] = q
		go d.run(key, q, addr, handle)
	}
	// Sent under the lock so the worker's "empty, so end" check cannot miss it.
	select {
	case q.ch <- pkt:
	default: // this source is far behind: drop rather than block the read loop
	}
	d.mu.Unlock()
}

func (d *packetDispatcher) run(key string, q *srcQueue, addr *net.UDPAddr, handle func([]byte, *net.UDPAddr)) {
	idle := time.NewTimer(srcQueueIdle)
	defer idle.Stop()
	for {
		select {
		case pkt := <-q.ch:
			handle(pkt, addr)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(srcQueueIdle)
		case <-idle.C:
			d.mu.Lock()
			if len(q.ch) == 0 {
				delete(d.q, key)
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
			idle.Reset(srcQueueIdle)
		}
	}
}
