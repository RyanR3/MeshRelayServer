// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"testing"
)

// The relayed text packet starts with exactly ONE type byte, then the sender
// length — what both the Android and iOS parsers expect.
func TestTextRelayFramingHasOneTypeByte(t *testing.T) {
	p := &TextMessagePacket{Sender: "alice", Recipient: "bob", Content: "hi", Timestamp: 42}
	b := SerializeTextPacket(p)
	if b[0] != PacketTypeText {
		t.Fatalf("first byte = %#x, want type %#x", b[0], PacketTypeText)
	}
	if b[1] != 0 || b[2] != byte(len("alice")) {
		t.Fatalf("bytes 1-2 = % x, want the sender length 00 05", b[1:3])
	}
	back, err := ParseTextPacket(b[1:])
	if err != nil || back.Sender != "alice" || back.Recipient != "bob" || back.Content != "hi" || back.Timestamp != 42 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

// A private message reaches its recipient only, not the rest of the channel.
func TestPrivateTextGoesToRecipientOnly(t *testing.T) {
	s := newMARTestServer()
	s.config = &Config{}
	s.agencies["a"] = &Agency{ID: "a"}
	addr := func(p int) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p} }
	alice := &Client{Username: "alice", AgencyID: "a", Channel: 1, Addr: addr(1001)}
	bob := &Client{Username: "bob", AgencyID: "a", Channel: 1, Addr: addr(1002)}
	carol := &Client{Username: "carol", AgencyID: "a", Channel: 1, Addr: addr(1003)}
	ch := &Channel{Clients: map[string]*Client{}}
	for _, c := range []*Client{alice, bob, carol} {
		s.clients[userKey("a", c.Username)] = c
		ch.Clients[userKey("a", c.Username)] = c
	}
	s.channels[channelKey("a", 1)] = ch
	var got []int
	s.testFloorSink = func(to *net.UDPAddr, data []byte) { got = append(got, to.Port) }

	s.handleTextPacket(&TextMessagePacket{Sender: "alice", Recipient: "bob", Content: "secret", Timestamp: 1}, addr(1001))
	if len(got) != 1 || got[0] != 1002 {
		t.Fatalf("private message delivered to %v, want only bob (1002)", got)
	}
	got = nil
	s.handleTextPacket(&TextMessagePacket{Sender: "alice", Recipient: "", Content: "all", Timestamp: 2}, addr(1001))
	if len(got) != 3 {
		t.Fatalf("channel message delivered to %v, want all 3 members", got)
	}
}
