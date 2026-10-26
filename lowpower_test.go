package main

import (
	"net"
	"testing"
	"time"
)

// A trailing flags byte is optional: old clients omit it, new ones set it.
func TestHeartbeatFlagsParse(t *testing.T) {
	plain, _ := CreateHeartbeatPacket("u1")
	hb, err := ParseHeartbeatPacket(plain[1:])
	if err != nil || hb.Username != "u1" || hb.Flags != 0 {
		t.Fatalf("legacy heartbeat: %+v %v", hb, err)
	}
	flagged := CreateHeartbeatPacketFlags("u1", HeartbeatFlagLowPower)
	hb, err = ParseHeartbeatPacket(flagged[1:])
	if err != nil || hb.Username != "u1" || hb.Flags != HeartbeatFlagLowPower {
		t.Fatalf("low-power heartbeat: %+v %v", hb, err)
	}
}

// The 10 s refresh reaches legacy clients every time but a low-power client
// only when the roster changed.
func TestPeriodicRosterSkipsUnchangedForLowPower(t *testing.T) {
	s := linkedServer(t)
	legacy := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 57001}
	lp := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 57002}
	s.clients["u1"] = &Client{Username: "u1", AgencyID: "a3", Addr: legacy}
	s.clients["u2"] = &Client{Username: "u2", AgencyID: "a3", Addr: lp, LowPower: true}

	sentTo := map[string]int{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) { sentTo[addr.String()]++ }
	defer func() { s.testFloorSink = nil }()

	s.broadcastAllRostersPeriodic() // first send: new to everyone
	s.broadcastAllRostersPeriodic() // unchanged
	s.broadcastAllRostersPeriodic() // unchanged
	if sentTo[legacy.String()] != 3 {
		t.Errorf("legacy client should get every refresh, got %d", sentTo[legacy.String()])
	}
	if sentTo[lp.String()] != 1 {
		t.Errorf("low-power client should get only the first, got %d", sentTo[lp.String()])
	}

	s.clients["u3"] = &Client{Username: "u3", AgencyID: "a3", Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 57003}}
	s.broadcastAllRostersPeriodic() // changed
	if sentTo[lp.String()] != 2 {
		t.Errorf("low-power client should get the changed roster, got %d", sentTo[lp.String()])
	}

	s.broadcastRoster("a3") // join/leave: always sent
	if sentTo[lp.String()] != 3 {
		t.Errorf("forced roster must reach low-power clients, got %d", sentTo[lp.String()])
	}
}

// A quiet low-power client is pinged, not dropped, until its longer timeout.
func TestLowPowerKeepaliveAndTimeout(t *testing.T) {
	s := linkedServer(t)
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	lp := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 57010}
	legacy := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 57011}
	now := time.Now()
	s.clients[userKey("a3", "lp")] = &Client{Username: "lp", AgencyID: "a3", Addr: lp, LowPower: true, LastHeartbeat: now.Add(-40 * time.Second)}
	s.clients[userKey("a3", "old")] = &Client{Username: "old", AgencyID: "a3", Addr: legacy, LastHeartbeat: now.Add(-40 * time.Second)}

	pings := map[string]int{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) {
		if len(data) > 0 && data[0] == PacketTypeHeartbeat {
			pings[addr.String()]++
		}
	}
	defer func() { s.testFloorSink = nil }()

	s.checkHeartbeats()
	if _, ok := s.clients[userKey("a3", "old")]; ok {
		t.Error("legacy client silent 40 s should time out")
	}
	if _, ok := s.clients[userKey("a3", "lp")]; !ok {
		t.Fatal("low-power client silent 40 s must not time out")
	}
	if pings[lp.String()] != 1 {
		t.Errorf("low-power client should be pinged once, got %d", pings[lp.String()])
	}
	s.checkHeartbeats() // within lowPowerPingAfter of the last ping
	if pings[lp.String()] != 1 {
		t.Errorf("ping must be rate-limited, got %d", pings[lp.String()])
	}

	s.clients[userKey("a3", "lp")].LastHeartbeat = now.Add(-80 * time.Second)
	s.checkHeartbeats()
	if _, ok := s.clients[userKey("a3", "lp")]; ok {
		t.Error("low-power client silent 80 s should time out")
	}
}
