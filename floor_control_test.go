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

// A radio bridge never sends FLOOR_REQUEST; with floor control on, its audio
// must still get the floor automatically (idle channel) while a phone that
// never asked is still dropped.
func TestGatewayAudioAutoFloor(t *testing.T) {
	s := newMARTestServer()
	s.config = &Config{AgenciesFile: filepath.Join(t.TempDir(), "agencies.json")}
	s.agencies["a"] = &Agency{ID: "a", Name: "A"}
	s.lmrBridges["b1"] = &LMRBridge{ID: "b1", Username: "pi", OwnerAgencyID: "a", Agencies: []string{"a"}, Channel: 102, Enabled: true}
	s.floor = NewFloorController(s)
	s.testFloorUserSink = func(user string, data []byte) {}
	if err := s.setFloorControl("a", true, false); err != nil {
		t.Fatal(err)
	}
	key := channelKey("a", 102)
	gw := &Client{Username: "pi", AgencyID: "a", Channel: 102}
	if !s.autoFloorForGateway(gw, key) || !s.floor.HasFloor(key, "pi") {
		t.Fatal("gateway audio on an idle channel should be granted the floor automatically")
	}
	phone := &Client{Username: "phone", AgencyID: "a", Channel: 102}
	if s.autoFloorForGateway(phone, key) {
		t.Fatal("a phone that never requested the floor must not be auto-granted")
	}
	// Busy channel (live talker): after the throttle window the gateway asks
	// again and busy mode refuses it.
	s.floor.Release(key, 102, "pi")
	s.floor.Request(key, &FloorRequest{Channel: 102, User: "alice", Priority: 3}, 3)
	time.Sleep(1100 * time.Millisecond)
	if s.autoFloorForGateway(gw, key) {
		t.Fatal("gateway must not take the floor from a live talker in busy mode")
	}
}

// A returning client is put back on its last channel, and a bridge gateway
// with no history lands on its own bridge's channel rather than channel 0.
func TestLastChannelGatewayFallbackAndPersistence(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	s := newMARTestServer()
	s.config = &Config{AgenciesFile: filepath.Join(dir, "agencies.json")}
	s.users = map[string]*UserAccount{userKey("a", "pi"): {Username: "pi", AgencyID: "a"}}
	s.agencies["a"] = &Agency{ID: "a", Name: "A"}
	s.lmrBridges["b1"] = &LMRBridge{ID: "b1", Username: "pi", OwnerAgencyID: "a", Agencies: []string{"a"}, Channel: 102, Enabled: true}
	if ch := s.lastChannelFor("pi", nil); ch != 102 {
		t.Fatalf("gateway with no history should land on its bridge channel 102, got %d", ch)
	}

	s.mu.Lock()
	s.lastChannel = map[string]int{userKey("a", "pi"): 105}
	s.scheduleSaveLastChannels()
	s.mu.Unlock()
	time.Sleep(2500 * time.Millisecond)

	s2 := newMARTestServer()
	s2.users = s.users
	s2.loadLastChannels()
	if ch := s2.lastChannelFor("pi", nil); ch != 105 {
		t.Fatalf("last channel should survive a restart, got %d", ch)
	}
}
