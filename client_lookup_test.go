// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"testing"
)

// The same username can exist in two agencies. Client records are keyed per
// agency, but audio/heartbeat used to resolve the record by USERNAME alone
// over a randomly-iterated map — so a packet from agency a2's "test" could be
// attributed to agency a1's "test" and relayed to the wrong channel. With the
// sender's authenticated agency known, resolution must be deterministic.
func TestFindClientForSenderIsAgencyScoped(t *testing.T) {
	s := linkedServer(t)
	if s.authenticatedAddrs == nil {
		s.authenticatedAddrs = map[string]string{}
	}
	addrA := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 40001}
	addrB := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 40002}
	s.authenticatedAddrs[addrA.String()] = "a1"
	s.authenticatedAddrs[addrB.String()] = "a2"
	s.clients[userKey("a1", "test")] = &Client{Username: "test", AgencyID: "a1", Addr: addrA, Channel: 5}
	s.clients[userKey("a2", "test")] = &Client{Username: "test", AgencyID: "a2", Addr: addrB, Channel: 100}

	for i := 0; i < 200; i++ { // map iteration is random; the answer must never be
		if c, _ := s.findClientForSender("test", addrB); c == nil || c.AgencyID != "a2" {
			t.Fatalf("iteration %d: packet from a2's address resolved to %+v, want agency a2", i, c)
		}
		if c, _ := s.findClientForSender("test", addrA); c == nil || c.AgencyID != "a1" {
			t.Fatalf("iteration %d: packet from a1's address resolved to %+v, want agency a1", i, c)
		}
	}
	// Roster-labelled name still resolves within the agency.
	if c, _ := s.findClientForSender("test (Fire Dept)", addrB); c == nil || c.AgencyID != "a2" {
		t.Errorf("labelled name from a2 resolved to %+v", c)
	}
	// Unauthenticated sender falls back to the username search (auth-disabled servers).
	unknown := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 1}
	if c, _ := s.findClientForSender("test", unknown); c == nil {
		t.Error("unauthenticated sender must still resolve by username")
	}
}

// A heartbeat carries only the username. When a client's record is gone
// (expired, or the server restarted) the old code re-registered it on channel
// 0, silently moving its audio off the channel it was using. It must go back
// to the channel of its last JOIN.
func TestHeartbeatReregistersOnLastChannel(t *testing.T) {
	s := linkedServer(t)
	if s.authenticatedAddrs == nil {
		s.authenticatedAddrs = map[string]string{}
	}
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 3), Port: 40003}
	s.authenticatedAddrs[addr.String()] = "a2"
	if got := s.lastChannelFor("test", addr); got != 0 {
		t.Fatalf("unknown user: want channel 0, got %d", got)
	}
	s.lastChannel = map[string]int{userKey("a2", "test"): 100}
	if got := s.lastChannelFor("test", addr); got != 100 {
		t.Fatalf("want last channel 100, got %d", got)
	}
	// Another agency's identical username must not leak its channel.
	other := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 4), Port: 40004}
	s.authenticatedAddrs[other.String()] = "a1"
	if got := s.lastChannelFor("test", other); got != 0 {
		t.Fatalf("a1's 'test' has never joined; want 0, got %d", got)
	}
}
