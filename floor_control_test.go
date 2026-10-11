package main

import (
	"os"
	"path/filepath"
	"testing"
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

	if err := s.setFloorControl("fire", true); err != nil {
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
	if err := s.setFloorControl("fire", false); err != nil {
		t.Fatal(err)
	}
	if s.floorEnabledForAgency("fire") {
		t.Fatal("toggle off not applied")
	}
	if s.floor.HasFloor("fire:1", "alice") {
		t.Fatal("held floor should be released when floor control is turned off")
	}

	if err := s.setFloorControl("nope", true); err == nil {
		t.Fatal("unknown agency should be an error")
	}
}
