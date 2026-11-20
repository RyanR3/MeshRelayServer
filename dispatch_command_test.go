// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func locationPayload(lat, lon float64) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, lat)
	binary.Write(&b, binary.BigEndian, lon)
	binary.Write(&b, binary.BigEndian, float32(8))
	binary.Write(&b, binary.BigEndian, float32(0))
	binary.Write(&b, binary.BigEndian, float32(0))
	binary.Write(&b, binary.BigEndian, int8(77))
	return b.Bytes()
}

func TestLocationReportShowsOnConsole(t *testing.T) {
	s, _, _, _ := dispatchTestServer(t)
	signInAll(s)
	alice := s.clients[userKey("ag", "alice")]

	p, err := ParseLocationReport(locationPayload(41.65, -74.69))
	if err != nil {
		t.Fatal(err)
	}
	s.handleLocationReport(p, alice.Addr)
	var loc map[string]interface{}
	for _, c := range s.GetClients("ag") {
		if c["username"] == "alice" {
			loc, _ = c["location"].(map[string]interface{})
		}
	}
	if loc == nil || loc["lat"] != 41.65 || loc["battery"] != int8(77) {
		t.Fatalf("alice's location not reported: %v", loc)
	}

	// Junk fixes are ignored.
	p0, _ := ParseLocationReport(locationPayload(0, 0))
	s.handleLocationReport(p0, alice.Addr)
	if alice.Loc.Lat != 41.65 {
		t.Fatalf("a 0,0 fix must not replace a real one")
	}
}

func TestConsoleMonitorsExtraChannels(t *testing.T) {
	s, dc, key, _ := dispatchTestServer(t)
	opus := []byte{0xF8, 1}
	if s.fanOutToDispatch("ag", 5, "alice", opus) != 0 {
		t.Fatalf("console should not hear an unmonitored channel")
	}
	s.clients[key].DispatchMonitor = map[string]bool{"ag:5": true}
	if s.fanOutToDispatch("ag", 5, "alice", opus) != 1 {
		t.Fatalf("console should hear a monitored channel")
	}
	drain(dc)
}

func TestEmergencyAndTalkReachConsole(t *testing.T) {
	s, dc, _, _ := dispatchTestServer(t)
	dc.Text = make(chan []byte, 8)
	s.clients[userKey("ag", "alice")].Loc = &UnitLocation{Lat: 1.5, Lon: 2.5}

	s.notifyDispatchEmergency("ag", 0, "alice", true)
	s.noteDispatchTalk("ag", 3, "alice")
	s.notifyDispatchEmergency("other", 0, "bob", true) // another agency: not ours

	var got []map[string]interface{}
	for len(dc.Text) > 0 {
		var m map[string]interface{}
		json.Unmarshal(<-dc.Text, &m)
		got = append(got, m)
	}
	if len(got) != 2 || got[0]["type"] != "emergency" || got[0]["lat"] != 1.5 || got[1]["type"] != "talk" {
		t.Fatalf("console got %v", got)
	}
}

func TestDirectoryListsAgencyChannels(t *testing.T) {
	s, _, _, _ := dispatchTestServer(t)
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Agency", MaxChannels: 3}, "fire": {ID: "fire", Name: "Fire"}}
	s.mutualAidLinks = map[string]*MutualAidLink{"l": {Agencies: []string{"ag", "fire"}, Channel: 60, ChannelName: "Fire assist", Active: true}}
	agencies, chans := s.dispatchDirectory("ag")
	if len(agencies) != 1 || agencies[0]["id"] != "ag" {
		t.Fatalf("agency console should see only its agency: %v", agencies)
	}
	var tg, ma int
	for _, c := range chans {
		if c.Kind == "talkgroup" {
			tg++
		}
		if c.Kind == "mutual_aid" && c.Channel == 60 && c.Name == "Fire assist" {
			ma++
		}
	}
	if tg != 3 || ma != 1 {
		t.Fatalf("talkgroups %d mutual aid %d: %+v", tg, ma, chans)
	}
	if a, _ := s.dispatchDirectory(""); len(a) != 2 {
		t.Fatalf("super admin console should see every agency")
	}
}
