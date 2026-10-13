package main

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The dashboard toggle is what decides floor control for an agency; the legacy
// config list still counts, and turning the toggle off clears any held floor.
func TestFloorControlToggle(t *testing.T) {
	s := newMARTestServer()
	s.config = &Config{AgenciesFile: filepath.Join(t.TempDir(), "agencies.json"),
		FloorControlAgencies: []string{"legacy"}}
	s.agencies["fire"] = &Agency{ID: "fire", Name: "Fire"}
	s.agencies["legacy"] = &Agency{ID: "legacy", Name: "Legacy"}
	s.floor = NewFloorController(s)

	if s.floorEnabledForAgency("fire") {
		t.Fatal("floor control should be off by default")
	}
	if !s.floorEnabledForAgency("legacy") {
		t.Fatal("legacy config list should still enable floor control")
	}

	if err := s.setFloorControl("fire", true, false); err != nil {
		t.Fatal(err)
	}
	if !s.floorEnabledForAgency("fire") || !s.agencies["fire"].FloorControl {
		t.Fatal("toggle on not applied")
	}
	if _, err := os.Stat(s.config.AgenciesFile); err != nil {
		t.Fatalf("toggle not persisted: %v", err)
	}

	// Someone holds the floor; switching the agency to open PTT clears it.
	s.floor.ch("fire:1").holder = &floorHolder{user: "alice"}
	if err := s.setFloorControl("fire", false, false); err != nil {
		t.Fatal(err)
	}
	if s.floorEnabledForAgency("fire") {
		t.Fatal("toggle off not applied")
	}
	if s.floor.HasFloor("fire:1", "alice") {
		t.Fatal("held floor should be released when floor control is turned off")
	}

	if err := s.setFloorControl("nope", true, false); err == nil {
		t.Fatal("unknown agency should be an error")
	}
}

// Queue mode: a second talker is granted a queued slot, their audio is
// recorded while the first talker holds the floor, and replayed to the channel
// in order once the floor is released. The replay holds the floor and the
// channel goes idle afterwards.
func TestFloorQueueRecordsAndReplays(t *testing.T) {
	s := newMARTestServer()
	s.config = &Config{AgenciesFile: filepath.Join(t.TempDir(), "agencies.json")}
	s.agencies["fire"] = &Agency{ID: "fire", Name: "Fire"}
	s.floor = NewFloorController(s)
	if err := s.setFloorControl("fire", true, true); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	floorPkts := map[string][][]byte{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) {}
	s.testFloorUserSink = func(user string, data []byte) {
		mu.Lock()
		floorPkts[user] = append(floorPkts[user], data)
		mu.Unlock()
	}
	var replayed []string
	s.testRelaySink = func(key string, channel int, user string, audioData []byte) {
		mu.Lock()
		replayed = append(replayed, user+":"+string(audioData))
		mu.Unlock()
	}

	key := channelKey("fire", 1)
	s.floor.Request(key, &FloorRequest{Channel: 1, User: "alice", Priority: 3}, 3)
	if !s.floor.HasFloor(key, "alice") {
		t.Fatal("alice should hold the floor")
	}
	// bob keys up while alice talks: granted a queued slot, not denied.
	s.floor.Request(key, &FloorRequest{Channel: 1, User: "bob", Priority: 3}, 3)
	mu.Lock()
	last := floorPkts["bob"][len(floorPkts["bob"])-1]
	mu.Unlock()
	if last[0] != PacketTypeFloorGranted {
		t.Fatalf("bob should get FLOOR_GRANTED (queued), got type 0x%02x", last[0])
	}
	if last[len(last)-1] != 1 {
		t.Fatalf("bob's queue position should be 1, got %d", last[len(last)-1])
	}
	if s.floor.HasFloor(key, "bob") {
		t.Fatal("bob must not hold the live floor")
	}
	// bob's frames are recorded, not relayed.
	for _, f := range []string{"b1", "b2", "b3"} {
		s.floor.RecordDeferred(key, "bob", &AudioPacket{Username: "bob"}, []byte(f))
	}
	s.floor.Release(key, 1, "bob") // bob done talking
	mu.Lock()
	n := len(replayed)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("nothing should replay while alice holds the floor, got %v", replayed)
	}

	// alice releases: bob's recording replays, then the channel goes idle.
	s.floor.Release(key, 1, "alice")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n = len(replayed)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), replayed...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "bob:b1" || got[2] != "bob:b3" {
		t.Fatalf("replay = %v, want bob's three frames in order", got)
	}
	time.Sleep(60 * time.Millisecond)
	if st := s.floor.FloorStatusFor(key, 1); st != nil {
		t.Fatalf("floor should be idle after replay, got %v", st)
	}
}
