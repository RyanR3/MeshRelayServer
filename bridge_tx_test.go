// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"testing"
	"time"
)

func TestBridgeGateLimitsAndRequiresSilence(t *testing.T) {
	var g bridgeTxGate
	t0 := time.Now()
	// 60 s of continuous frames pass.
	for d := time.Duration(0); d < bridgeMaxTx; d += 500 * time.Millisecond {
		if ok, _ := g.allow("u", t0.Add(d), true); !ok {
			t.Fatalf("frame at %v dropped before the limit", d)
		}
	}
	ok, first := g.allow("u", t0.Add(bridgeMaxTx), true)
	if ok || !first {
		t.Fatal("limit not enforced, or the unit was not told")
	}
	// Still keyed: stays dropped, however long it goes on.
	for d := bridgeMaxTx + 500*time.Millisecond; d < bridgeMaxTx+30*time.Second; d += 500 * time.Millisecond {
		if ok, first := g.allow("u", t0.Add(d), true); ok || first {
			t.Fatalf("a stuck mic got through (or was told twice) at %v", d)
		}
	}
	// Unkeyed for the gap: a new transmission is allowed.
	resume := t0.Add(bridgeMaxTx + 30*time.Second + bridgeTxGap + 100*time.Millisecond)
	if ok, _ := g.allow("u", resume, true); !ok {
		t.Fatal("unit still locked out after unkeying for the gap")
	}
	// Normal use: short transmissions with pauses never hit the limit.
	var n bridgeTxGate
	at := t0
	for i := 0; i < 40; i++ {
		for d := time.Duration(0); d < 20*time.Second; d += 500 * time.Millisecond {
			if ok, _ := n.allow("u", at.Add(d), true); !ok {
				t.Fatal("ordinary 20 s transmissions were limited")
			}
		}
		at = at.Add(23 * time.Second)
	}
}

func TestEmergencySegmentsToRadio(t *testing.T) {
	var g bridgeTxGate
	t0 := time.Now()
	passed, dropped := 0, 0
	for d := time.Duration(0); d < 5*time.Minute; d += 20 * time.Millisecond {
		if ok, _ := g.allow("emerg#ag", t0.Add(d), false); ok {
			passed++
		} else {
			dropped++
		}
	}
	// 60 s on / 2 s off, repeating: ~97% through, and at least four breaks.
	if dropped < 4*int(bridgeTxGap/(20*time.Millisecond))-4 {
		t.Fatalf("radio never got a break: %d frames dropped", dropped)
	}
	if float64(passed)/float64(passed+dropped) < 0.95 {
		t.Fatalf("too much emergency audio withheld from the radio: %d of %d", dropped, passed+dropped)
	}
}

// Guests listen on a bridge channel but cannot key the radio unless the
// bridge allows it; the gateway itself is never limited.
func TestGuestsCannotKeyBridgeUnlessAllowed(t *testing.T) {
	s, sent := newFloorTestServer(t, "member", "visitor", "gw")
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Ag"}}
	exp := time.Now().Add(time.Hour)
	s.users = map[string]*UserAccount{
		userKey("ag", "member"):  {Username: "member", AgencyID: "ag", Role: "user"},
		userKey("ag", "visitor"): {Username: "visitor", AgencyID: "ag", Role: "user", GuestExpiresAt: &exp},
		userKey("ag", "gw"):      {Username: "gw", AgencyID: "ag", Role: "user"},
	}
	s.authAccounts = map[string]string{}
	bridge := &LMRBridge{ID: "b1", Username: "gw", OwnerAgencyID: "ag", Agencies: []string{"ag"}, Channel: 100, Enabled: true}
	s.lmrBridges = map[string]*LMRBridge{"b1": bridge}
	s.ssrcToUser = map[uint32]string{}
	s.channels["ag:100"] = &Channel{Clients: make(map[string]*Client)}
	for _, u := range []string{"member", "visitor", "gw"} {
		c := s.clients[userKey("ag", u)]
		delete(s.channels["ag:0"].Clients, userKey("ag", u))
		c.Channel = 100
		c.Account = userKey("ag", u)
		s.authAccounts[c.Addr.String()] = userKey("ag", u)
		s.channels["ag:100"].Clients[userKey("ag", u)] = c
	}
	gwGot := func() int { tg, un := countAudio(sent, "gw"); return tg + un }
	send := func(u string, seq uint16) {
		s.handleAudioPacket(rtpOpus(u, seq), s.clients[userKey("ag", u)].Addr)
	}

	send("member", 1)
	if gwGot() != 1 {
		t.Fatal("a member's audio should reach the gateway")
	}
	send("visitor", 2)
	if gwGot() != 1 {
		t.Fatal("a guest keyed the radio on a bridge that does not allow guests")
	}
	if !hasType(sent["visitor"], PacketTypeFloorDenied) {
		t.Fatal("the guest was not told")
	}
	bridge.AllowGuests = true
	send("visitor", 3)
	if gwGot() != 2 {
		t.Fatal("guest still blocked after the bridge allowed guests")
	}
	// The gateway's own audio (what the radio received) is never gated.
	if tg, un := countAudio(sent, "member"); tg+un != 1 {
		t.Fatalf("member should have heard only the guest so far, got %d", tg+un)
	}
	send("gw", 4)
	if tg, un := countAudio(sent, "member"); tg+un != 2 {
		t.Fatal("radio audio from the gateway did not reach the phones")
	}
}

// A callsign that is an account's name belongs to that account; a gateway's
// name cannot be taken while it is offline.
func TestCallsignOfAnotherAccountIsRefused(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	addUser(s, "a1", "Chief1", "chief-password-123")
	addUser(s, "a1", "gw1", "gateway-password-1")
	s.lmrBridges["b1"] = &LMRBridge{ID: "b1", Username: "gw1", OwnerAgencyID: "a1", Agencies: []string{"a1"}, Channel: 100, Enabled: true}

	phone := udp(7001)
	loginWith(t, s, sink, phone, "bob", "a1", "bob-password-123", 0)
	for _, squat := range []string{"Chief1", "chief1", "gw1"} {
		join(s, phone, squat, 0)
		if len(s.clients) != 0 {
			t.Fatalf("bob joined as %q, another account's name", squat)
		}
	}
	join(s, phone, "Engine 7", 0) // a free callsign is fine
	join(s, phone, "bob", 0)      // and so is its own account name
	if s.clients[userKey("a1", "bob")] == nil {
		t.Fatal("an account could not join under its own name")
	}
	// The gateway joins as itself and is recognised as the gateway.
	gwAddr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 7002}
	loginWith(t, s, sink, gwAddr, "gw1", "a1", "gateway-password-1", 0)
	join(s, gwAddr, "gw1", 100)
	gw := s.clients[userKey("a1", "gw1")]
	if gw == nil || s.ownLMRBridge(gw) == nil {
		t.Fatal("the real gateway was not accepted as the gateway")
	}
}
