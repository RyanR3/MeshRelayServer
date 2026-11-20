// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// A burst of packets from each source must be handled in arrival order, and
// sources must not wait for one another.
func TestDispatchKeepsOrderPerSource(t *testing.T) {
	s := &Server{}
	const sources, perSource = 8, 2000
	var mu sync.Mutex
	got := map[string][]uint32{}
	var wg sync.WaitGroup
	wg.Add(sources * perSource)
	s.dispatch.handle = func(p []byte, a *net.UDPAddr) {
		mu.Lock()
		got[a.String()] = append(got[a.String()], binary.BigEndian.Uint32(p[1:]))
		mu.Unlock()
		wg.Done()
	}
	addrs := make([]*net.UDPAddr, sources)
	for i := range addrs {
		addrs[i] = &net.UDPAddr{IP: net.IPv4(10, 0, 0, byte(i+1)), Port: 5000 + i}
	}
	// Types: plain audio (0x13) on even sources, secure data (0x44) on odd.
	for n := 0; n < perSource; n++ {
		for i, a := range addrs {
			p := make([]byte, 5)
			p[0] = PacketTypeAudioRtp
			if i%2 == 1 {
				p[0] = PacketTypeSecData
			}
			binary.BigEndian.PutUint32(p[1:], uint32(n))
			s.dispatchPacket(p, a)
			if n%srcQueueDepth == srcQueueDepth-1 {
				time.Sleep(time.Millisecond) // let the workers drain; never overflow
			}
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("packets were not all handled")
	}
	for a, seq := range got {
		if len(seq) != perSource {
			t.Fatalf("%s: handled %d of %d", a, len(seq), perSource)
		}
		for i, v := range seq {
			if v != uint32(i) {
				t.Fatalf("%s: packet %d handled at position %d", a, v, i)
			}
		}
	}
}

// A blocked source must not hold up another source.
func TestDispatchSourcesIndependent(t *testing.T) {
	s := &Server{}
	slow := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1}
	fast := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 2}
	release := make(chan struct{})
	fastDone := make(chan struct{})
	s.dispatch.handle = func(p []byte, a *net.UDPAddr) {
		if a.Port == 1 {
			<-release
			return
		}
		close(fastDone)
	}
	s.dispatchPacket([]byte{PacketTypeAudioRtp}, slow)
	s.dispatchPacket([]byte{PacketTypeAudioRtp}, fast)
	select {
	case <-fastDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked source delayed another source")
	}
	close(release)
}
