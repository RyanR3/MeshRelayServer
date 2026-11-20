// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"testing"
	"time"
)

// newFloorTestServer builds a minimal Server with a channel + clients so the
// FloorController's send/broadcast helpers resolve, and captures packets sent to
// each user for assertions.
func newFloorTestServer(t *testing.T, users ...string) (*Server, map[string][][]byte) {
	t.Helper()
	s := &Server{
		clients:            make(map[string]*Client),
		channels:           make(map[string]*Channel),
		authenticatedAddrs: make(map[string]string),
		config:             &Config{},
	}
	sent := map[string][][]byte{}
	// Fake UDP addrs, one per user, and a reverse map addr->user for capture.
	addrToUser := map[string]string{}
	ch := &Channel{Clients: make(map[string]*Client)}
	s.channels["ag:0"] = ch
	for i, u := range users {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000 + i}
		cl := &Client{Username: u, AgencyID: "ag", Channel: 0, Addr: addr}
		s.clients[userKey("ag", u)] = cl
		ch.Clients[userKey("ag", u)] = cl
		addrToUser[addr.String()] = u
		// Sign-in is always required: each test client is signed in.
		s.recordAuthLocked(addr.String(), "ag", userKey("ag", u), credPassword)
	}
	// Override sendPacket path by capturing in a custom test hook: we can't easily
	// swap sendPacket, so capture via a wrapper the tests call. Instead, we record
	// through a package-level test sink set on the server.
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) {
		u := addrToUser[addr.String()]
		sent[u] = append(sent[u], data)
	}
	s.floor = NewFloorController(s)
	return s, sent
}

func lastPacketType(sent map[string][][]byte, user string) byte {
	pk := sent[user]
	if len(pk) == 0 {
		return 0
	}
	return pk[len(pk)-1][0]
}

func req(ch int, user string) *FloorRequest {
	return &FloorRequest{Channel: ch, User: user, Priority: PriorityResponder}
}

func TestFloorGrantOnIdle(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice")
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	if lastPacketType(sent, "alice") != PacketTypeFloorGranted {
		t.Fatalf("alice should get FLOOR_GRANTED on idle channel")
	}
	if !s.floor.HasFloor("ag:0", "alice") {
		t.Fatalf("alice should hold the floor")
	}
}

func TestFloorDenyEqualPriority(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice", "bob")
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	s.floor.Request("ag:0", req(0, "bob"), PriorityResponder)
	if lastPacketType(sent, "bob") != PacketTypeFloorDenied {
		t.Fatalf("bob (equal priority) should be DENIED")
	}
	if !s.floor.HasFloor("ag:0", "alice") {
		t.Fatalf("alice should still hold the floor")
	}
}

func TestFloorPreemptByHigherPriority(t *testing.T) {
	s, sent := newFloorTestServer(t, "resp", "cmd")
	s.floor.Request("ag:0", req(0, "resp"), PriorityResponder)
	s.floor.Request("ag:0", req(0, "cmd"), PriorityCommand) // higher (lower number)
	if lastPacketType(sent, "resp") != PacketTypeFloorPreempted {
		t.Fatalf("resp should be PREEMPTED by command priority")
	}
	if lastPacketType(sent, "cmd") != PacketTypeFloorGranted {
		t.Fatalf("cmd should be GRANTED the floor")
	}
	if !s.floor.HasFloor("ag:0", "cmd") {
		t.Fatalf("cmd should hold the floor")
	}
}

func TestFloorQueueAndAdvance(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice", "bob")
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	s.floor.Request("ag:0", req(0, "bob"), PriorityResponder) // denied -> queued pos 1
	// bob's denied packet should carry queue position 1.
	bobPkts := sent["bob"]
	last := bobPkts[len(bobPkts)-1]
	// FloorDenied: type|i32 ch|u8 reason|u8 queuePos|...
	if last[0] != PacketTypeFloorDenied || last[6] != 1 {
		t.Fatalf("bob should be queued at position 1, got type=0x%02X pos=%d", last[0], last[6])
	}
	// alice releases -> bob should be auto-granted.
	s.floor.Release("ag:0", 0, "alice")
	if !s.floor.HasFloor("ag:0", "bob") {
		t.Fatalf("bob should be granted after alice releases")
	}
	if lastPacketType(sent, "bob") != PacketTypeFloorGranted {
		t.Fatalf("bob should receive FLOOR_GRANTED on advance")
	}
}

func TestFloorMonitorCannotRequest(t *testing.T) {
	s, sent := newFloorTestServer(t, "watcher")
	s.floor.Request("ag:0", req(0, "watcher"), PriorityMonitor)
	if lastPacketType(sent, "watcher") != PacketTypeFloorDenied {
		t.Fatalf("monitor priority must be DENIED")
	}
	if s.floor.HasFloor("ag:0", "watcher") {
		t.Fatalf("monitor must not hold the floor")
	}
}

func TestFloorDeadManTimer(t *testing.T) {
	s, _ := newFloorTestServer(t, "alice")
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	// Force the holder's lastAudio into the past beyond the dead-man timeout.
	s.floor.mu.Lock()
	s.floor.channels["ag:0"].holder.lastAudio = time.Now().Add(-4 * time.Second)
	s.floor.mu.Unlock()
	s.floor.Tick()
	if s.floor.HasFloor("ag:0", "alice") {
		t.Fatalf("dead-man timer should have released alice's floor")
	}
}

func TestFloorEmergencyPreempts(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice", "medic")
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	s.floor.Emergency("ag:0", 0, "medic", true, false)
	if !s.floor.HasFloor("ag:0", "medic") {
		t.Fatalf("emergency should grant the floor to the alerting user")
	}
	// alice preempted; both should have received an emergency alert broadcast.
	foundAlert := false
	for _, p := range sent["alice"] {
		if p[0] == PacketTypeEmergencyAlert {
			foundAlert = true
		}
	}
	if !foundAlert {
		t.Fatalf("channel members should receive EMERGENCY_ALERT broadcast")
	}
}

// signInAll marks every test client signed in as its own account (sign-in
// is always required).
func signInAll(s *Server) {
	for k, c := range s.clients {
		if c.Addr != nil {
			s.recordAuthLocked(c.Addr.String(), c.AgencyID, k, credPassword)
		}
	}
}
