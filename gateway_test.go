// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// TestGatewayDisabledZeroImpact: with no sip_gateway config, the server has no
// gateway and nothing is started.
func TestGatewayDisabledZeroImpact(t *testing.T) {
	s := &Server{config: &Config{}}
	if s.sipGateway != nil {
		t.Fatal("gateway should be nil when unconfigured")
	}
	// Callback methods must be safe no-ops when floor/gateway are absent.
	if err := s.GrantFloor("ag:0", "x@d"); err != nil {
		t.Errorf("GrantFloor should be safe with nil floor: %v", err)
	}
}

// TestGatewayStartsWhenEnabled: an enabled gateway binds its SIP port.
func TestGatewayStartsWhenEnabled(t *testing.T) {
	cfg := &SipGatewayConfig{
		Enabled: true, SipPort: 0, // OS-assigned port (no test collisions)
		BridgeAgency: "ag", BridgeChannel: 0,
	}
	gw := NewSipGateway(cfg, nil)
	if err := gw.Start(); err != nil {
		t.Fatalf("gateway should start: %v", err)
	}
	defer gw.Stop()
	if !gw.running || gw.boundPort == 0 {
		t.Fatal("gateway should be running on a bound port")
	}
	if gw.bridgeKey != "ag:0" {
		t.Errorf("bridgeKey = %q, want ag:0", gw.bridgeKey)
	}
}

// fakeMesh captures MeshRelayCallback invocations for the SIP-side translation.
// The gateway calls it from its read goroutine while the test polls, so the
// fields are guarded (go test -race).
type fakeMesh struct {
	mu      sync.Mutex
	granted string
	audio   [][]byte
}

func (f *fakeMesh) GrantFloor(key, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted = user
	return nil
}
func (f *fakeMesh) DenyFloor(key, user string, r uint8) error { return nil }
func (f *fakeMesh) DeliverAudio(key string, rtp []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = append(f.audio, rtp)
	return nil
}
func (f *fakeMesh) BroadcastEmergency(key, user string) error { return nil }

func (f *fakeMesh) grantedUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.granted
}

// TestSipInviteTranslatesToFloor: an inbound SIP INVITE grants the floor to the
// SIP user on the bridged MeshRelay channel.
func TestSipInviteTranslatesToFloor(t *testing.T) {
	mesh := &fakeMesh{}
	cfg := &SipGatewayConfig{Enabled: true, SipPort: 0, BridgeAgency: "ag", BridgeChannel: 0}
	gw := NewSipGateway(cfg, mesh)
	if err := gw.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer gw.Stop()

	// Send a minimal SIP INVITE to the gateway's bound port.
	raddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", gw.boundPort))
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	invite := "INVITE sip:group@meshrelay SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:6000;branch=z9hG4bKtest\r\n" +
		"From: <sip:medic1@alpha.mcptt.example.com>;tag=abc\r\n" +
		"To: <sip:group@meshrelay>\r\n" +
		"Call-ID: test@meshrelay\r\n" +
		"CSeq: 1 INVITE\r\n\r\n"
	conn.Write([]byte(invite))

	// Give the gateway a moment to process and expect the floor grant.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if mesh.grantedUser() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := mesh.grantedUser(); got != "medic1" {
		t.Fatalf("INVITE should grant floor to medic1, got %q", got)
	}
}

// TestSipUserParse checks the From/To URI user extraction.
func TestSipUserParse(t *testing.T) {
	cases := map[string]string{
		"<sip:medic1@domain>;tag=x":   "medic1",
		"sip:medic3@example.net":      "medic3",
		"\"IC\" <sip:ic@example.org>": "ic",
	}
	for in, want := range cases {
		if got := sipUser(in); got != want {
			t.Errorf("sipUser(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRtpPassthroughStripsTypeByte: OnAudioPacket forwards pure RTP (drops the
// MeshRelay 0x13 type prefix).
func TestRtpPassthroughStripsTypeByte(t *testing.T) {
	// Use a peer that echoes into a buffer via a real UDP socket.
	peerConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer peerConn.Close()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	cfg := &SipGatewayConfig{Enabled: true, SipPort: 0, BridgeAgency: "ag", BridgeChannel: 0}
	gw := NewSipGateway(cfg, nil)
	gw.Start()
	defer gw.Stop()
	gw.peers[peerAddr.String()] = peerAddr

	// [0x13][rtp...] -> peer should receive [rtp...] without the type byte.
	pkt := append([]byte{PacketTypeAudioRtp}, []byte{0x80, 0x6f, 0x00, 0x01}...)
	gw.OnAudioPacket("ag:0", pkt)

	buf := make([]byte, 64)
	peerConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, _, err := peerConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if n != 4 || buf[0] != 0x80 {
		t.Fatalf("expected 4-byte pure RTP starting 0x80, got %d bytes starting 0x%02X", n, buf[0])
	}
}
