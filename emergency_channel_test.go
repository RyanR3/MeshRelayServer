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

func rtpOpus(user string, seq uint16) *AudioPacket {
	return &AudioPacket{Username: user, AudioData: []byte{1, 2, 3, 4}, Codec: CodecOpus,
		RTP: &RTPHeader{PayloadType: 111, SequenceNumber: seq, SSRC: 77}}
}

func countAudio(sent map[string][][]byte, user string) (tagged99, untagged int) {
	for _, p := range sent[user] {
		switch p[0] {
		case PacketTypeAudioRtpChan:
			if int(int32(binary.BigEndian.Uint32(p[1:5]))) == EmergencyChannel {
				tagged99++
			}
		case PacketTypeAudioRtp, PacketTypeAudioOpus, PacketTypeAudio:
			untagged++
		}
	}
	return
}

// emergencyTestServer: medic, alice and old on channel 0, bob on channel 1.
// old is an app that doesn't know the EMERGENCY channel.
func emergencyTestServer(t *testing.T) (*Server, map[string][][]byte) {
	s, sent := newFloorTestServer(t, "medic", "alice", "bob", "old")
	s.ssrcToUser = map[uint32]string{}
	s.channels["ag:1"] = &Channel{Clients: make(map[string]*Client)}
	bob := s.clients[userKey("ag", "bob")]
	delete(s.channels["ag:0"].Clients, userKey("ag", "bob"))
	bob.Channel = 1
	s.channels["ag:1"].Clients[userKey("ag", "bob")] = bob
	for _, u := range []string{"medic", "alice", "bob"} {
		s.clients[userKey("ag", u)].EmergencyAware = true
	}
	return s, sent
}

func reset(sent map[string][][]byte) {
	for k := range sent {
		delete(sent, k)
	}
}

func TestEmergencyAudioGoesToEmergencyChannelOnly(t *testing.T) {
	s, sent := emergencyTestServer(t)
	medic := s.clients[userKey("ag", "medic")]

	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: true}, medic.Addr)
	// The alert reaches the whole agency, bob (other channel) and the caller too.
	for _, u := range []string{"medic", "alice", "bob", "old"} {
		if lastPacketType(sent, u) != PacketTypeEmergencyAlert && !hasType(sent[u], PacketTypeEmergencyAlert) {
			t.Fatalf("%s did not get the alert", u)
		}
	}
	if !s.floor.HasFloor("ag:99", "medic") {
		t.Fatal("the caller should hold the EMERGENCY channel")
	}
	if s.floor.HasFloor("ag:0", "medic") {
		t.Fatal("the working channel's floor must not be taken")
	}

	reset(sent)
	s.handleAudioPacket(rtpOpus("medic", 1), medic.Addr)
	for _, u := range []string{"alice", "bob"} {
		if tg, un := countAudio(sent, u); tg != 1 || un != 0 {
			t.Fatalf("%s: tagged=%d untagged=%d, want 1 tagged ch 99", u, tg, un)
		}
	}
	if tg, un := countAudio(sent, "old"); tg != 0 || un != 1 {
		t.Fatalf("old app: tagged=%d untagged=%d, want 1 untagged", tg, un)
	}
	if tg, un := countAudio(sent, "medic"); tg+un != 0 {
		t.Fatal("caller got its own audio back")
	}

	// Cleared: audio is back on the working channel only.
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: false}, medic.Addr)
	if s.emergencyActiveFor("ag", "medic") {
		t.Fatal("emergency still active after clear")
	}
	reset(sent)
	s.handleAudioPacket(rtpOpus("medic", 2), medic.Addr)
	if _, un := countAudio(sent, "alice"); un != 1 {
		t.Fatal("alice (same channel) should hear medic normally after the clear")
	}
	if tg, un := countAudio(sent, "bob"); tg+un != 0 {
		t.Fatal("bob (other channel) should not hear medic after the clear")
	}
}

func hasType(pk [][]byte, typ byte) bool {
	for _, p := range pk {
		if p[0] == typ {
			return true
		}
	}
	return false
}

func TestNobodyElseTalksOnEmergencyChannel(t *testing.T) {
	s, sent := emergencyTestServer(t)
	alice := s.clients[userKey("ag", "alice")]
	// alice puts herself on channel 99 and tries to talk.
	delete(s.channels["ag:0"].Clients, userKey("ag", "alice"))
	alice.Channel = EmergencyChannel
	s.ensureEmergencyChannel("ag")
	s.channels["ag:99"].Clients[userKey("ag", "alice")] = alice

	s.handleFloorRequest(&FloorRequest{Channel: EmergencyChannel, User: "alice", Priority: PriorityResponder}, alice.Addr)
	if lastPacketType(sent, "alice") != PacketTypeFloorDenied {
		t.Fatal("floor on the EMERGENCY channel must be denied")
	}
	reset(sent)
	s.handleAudioPacket(rtpOpus("alice", 1), alice.Addr)
	for _, u := range []string{"medic", "bob", "old"} {
		if tg, un := countAudio(sent, u); tg+un != 0 {
			t.Fatalf("%s heard alice on the EMERGENCY channel", u)
		}
	}
}

func TestSecondEmergencyIsQueuedThenReplayed(t *testing.T) {
	s, sent := emergencyTestServer(t)
	medic := s.clients[userKey("ag", "medic")]
	alice := s.clients[userKey("ag", "alice")]
	// The replay runs on its own goroutine: guard the capture.
	var mu sync.Mutex
	inner := s.testFloorSink
	s.testFloorSink = func(a *net.UDPAddr, d []byte) { mu.Lock(); inner(a, d); mu.Unlock() }
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: true}, medic.Addr)
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "alice", Active: true}, alice.Addr)

	if !s.floor.IsRecording("ag:99", "alice") {
		t.Fatal("second caller should be queued (recorded)")
	}
	mu.Lock()
	reset(sent)
	mu.Unlock()
	s.handleAudioPacket(rtpOpus("alice", 1), alice.Addr)
	mu.Lock()
	tg, un := countAudio(sent, "bob")
	mu.Unlock()
	if tg+un != 0 {
		t.Fatal("queued caller's audio was relayed live")
	}
	// The first caller clears: alice's recording plays out.
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: false}, medic.Addr)
	s.handleFloorRequest(&FloorRequest{Channel: 0, User: "alice", Priority: 0}, alice.Addr) // alice releases
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		tg, _ := countAudio(sent, "bob")
		mu.Unlock()
		if tg == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("queued emergency audio was never replayed")
}

func TestSupervisorClearAndIdleExpiry(t *testing.T) {
	s, sent := emergencyTestServer(t)
	medic := s.clients[userKey("ag", "medic")]
	s.raiseEmergency("ag", "medic", 0)
	if n := s.clearEmergenciesOnChannel("ag", 1, "sup"); n != 0 {
		t.Fatal("cleared an emergency on another channel")
	}
	if n := s.clearEmergenciesOnChannel("ag", 0, "sup"); n != 1 {
		t.Fatal("supervisor clear on the caller's channel failed")
	}
	got := sent["medic"][len(sent["medic"])-1]
	if got[0] != PacketTypeEmergencyAlert || got[len(got)-1] != 0 {
		t.Fatal("caller was not told its emergency was cleared")
	}

	s.raiseEmergency("ag", "medic", 0)
	s.emergMu.Lock()
	s.emergCalls["ag"]["medic"].LastActive = time.Now().Add(-emergencyIdleExpire - time.Minute)
	s.emergMu.Unlock()
	s.emergencyTick()
	if s.emergencyActiveFor("ag", "medic") {
		t.Fatal("idle emergency did not expire")
	}
	// Audio keeps it alive.
	s.raiseEmergency("ag", "medic", 0)
	s.emergMu.Lock()
	s.emergCalls["ag"]["medic"].LastActive = time.Now().Add(-emergencyIdleExpire - time.Minute)
	s.emergMu.Unlock()
	s.handleAudioPacket(rtpOpus("medic", 3), medic.Addr)
	s.emergencyTick()
	if !s.emergencyActiveFor("ag", "medic") {
		t.Fatal("an emergency with live audio expired")
	}
}

func TestEmergencyAudioGoesOutOnChosenLMRBridgeOnly(t *testing.T) {
	s, sent := emergencyTestServer(t)
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Ag", EmergencyLMRBridge: "b1"}}
	s.lmrBridges = map[string]*LMRBridge{
		"b1": {ID: "b1", Username: "gw1", OwnerAgencyID: "ag", Agencies: []string{"ag"}, Channel: 100, Enabled: true},
		"b2": {ID: "b2", Username: "gw2", OwnerAgencyID: "ag", Agencies: []string{"ag"}, Channel: 101, Enabled: true},
	}
	for i, gw := range []string{"gw1", "gw2"} {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 41000 + i}
		s.clients[userKey("ag", gw)] = &Client{Username: gw, AgencyID: "ag", Channel: 100 + i, Addr: addr, EmergencyAware: true}
		user := gw
		prev := s.testFloorSink
		s.testFloorSink = func(a *net.UDPAddr, d []byte) {
			if a.String() == addr.String() {
				sent[user] = append(sent[user], d)
				return
			}
			prev(a, d)
		}
	}
	medic := s.clients[userKey("ag", "medic")]
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: true}, medic.Addr)
	reset(sent)
	s.handleAudioPacket(rtpOpus("medic", 1), medic.Addr)
	if tg, un := countAudio(sent, "gw1"); tg != 0 || un != 1 {
		t.Fatalf("chosen bridge gateway: tagged=%d untagged=%d, want 1 untagged", tg, un)
	}
	if tg, un := countAudio(sent, "gw2"); tg+un != 0 {
		t.Fatal("another radio gateway received emergency audio")
	}
}

func TestJoinReplyCarriesEmergencyChannel(t *testing.T) {
	a := &Agency{ID: "ag", Name: "Ag"}
	pkt := buildEmergencyChannel(a)
	if pkt[0] != PacketTypeEmergencyChannel || int(binary.BigEndian.Uint32(pkt[1:5])) != EmergencyChannel {
		t.Fatalf("bad packet % x", pkt)
	}
	if n := int(binary.BigEndian.Uint16(pkt[5:7])); string(pkt[7:7+n]) != "EMERGENCY" {
		t.Fatalf("default name = %q", pkt[7:7+n])
	}
	a.EmergencyChannelName = "SOS"
	pkt = buildEmergencyChannel(a)
	if n := int(binary.BigEndian.Uint16(pkt[5:7])); string(pkt[7:7+n]) != "SOS" {
		t.Fatal("renamed channel not sent")
	}
}
