package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// With floor control off (the default "Open" mode) an emergency still reaches
// every unit on the channel, with the caller's position appended.
func TestEmergencyReachesChannelWithoutFloorControl(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice", "medic")
	medic := s.clients[userKey("ag", "medic")]
	medic.Loc = &UnitLocation{Lat: 41.65, Lon: -74.69}

	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: true}, medic.Addr)

	var got []byte
	for _, p := range sent["alice"] {
		if p[0] == PacketTypeEmergencyAlert {
			got = p
		}
	}
	if got == nil {
		t.Fatalf("alice did not receive the emergency")
	}
	base := len(buildEmergencyAlert(0, "medic", true))
	if len(got) != base+16 {
		t.Fatalf("expected position appended, packet len %d (base %d)", len(got), base)
	}
	lat := math.Float64frombits(binary.BigEndian.Uint64(got[base:]))
	lon := math.Float64frombits(binary.BigEndian.Uint64(got[base+8:]))
	if lat != 41.65 || lon != -74.69 {
		t.Fatalf("position = %v,%v", lat, lon)
	}
	if s.floor.HasFloor("ag:0", "medic") {
		t.Fatalf("floor control is off: no floor should be granted")
	}

	// Cancel goes out without a position.
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: false}, medic.Addr)
	last := sent["alice"][len(sent["alice"])-1]
	if last[0] != PacketTypeEmergencyAlert || len(last) != base || last[len(last)-1] != 0 {
		t.Fatalf("bad cancel packet % x", last)
	}
}
