// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"testing"
)

func linkedServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		agencies: map[string]*Agency{
			"a1": {ID: "a1"}, "a2": {ID: "a2"}, "a3": {ID: "a3"},
		},
		clients:        map[string]*Client{},
		channels:       map[string]*Channel{},
		mutualAidLinks: map[string]*MutualAidLink{},
		zelloBridges:   map[string]*ZelloBridge{},
	}
	s.mutualAidLinks["l1"] = &MutualAidLink{
		ID: "l1", Agencies: []string{"a1", "a2"}, Channel: 15, Active: true,
	}
	return s
}

// Linked agencies must see each other; unlinked ones must not.
func TestMutualAidAgenciesResolution(t *testing.T) {
	s := linkedServer(t)
	got := s.mutualAidAgencies("a1")
	if !got["a1"] || !got["a2"] {
		t.Errorf("a1 should see itself and a2, got %v", got)
	}
	if got["a3"] {
		t.Error("a3 is not linked and must not be visible")
	}
	if s.mutualAidAgencies("a3")["a1"] {
		t.Error("unlinked agency must not see a1")
	}
	t.Logf("OK: a1 sees %v", got)
}

// An INACTIVE link must not merge rosters.
func TestInactiveLinkDoesNotMerge(t *testing.T) {
	s := linkedServer(t)
	s.mutualAidLinks["l1"].Active = false
	if s.mutualAidAgencies("a1")["a2"] {
		t.Error("inactive link must not make a2 visible")
	}
	t.Log("OK: inactive link ignored")
}

// The roster sent to a1's clients must LIST partner users, but only be
// ADDRESSED to a1's own clients (a2 gets its own roster).
func TestRosterIncludesMutualAidPartners(t *testing.T) {
	s := linkedServer(t)
	a1Addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56001}
	a2Addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56002}
	a3Addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56003}
	s.clients["u1"] = &Client{Username: "u1", AgencyID: "a1", Addr: a1Addr}
	s.clients["u2"] = &Client{Username: "u2", AgencyID: "a2", Addr: a2Addr}
	s.clients["u3"] = &Client{Username: "u3", AgencyID: "a3", Addr: a3Addr}

	var payload []byte
	sentTo := map[string]int{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) {
		sentTo[addr.String()]++
		payload = append([]byte(nil), data...)
	}
	defer func() { s.testFloorSink = nil }()

	s.broadcastRoster("a1")

	if sentTo[a1Addr.String()] == 0 {
		t.Fatal("a1's own client got no roster")
	}
	if sentTo[a2Addr.String()] != 0 || sentTo[a3Addr.String()] != 0 {
		t.Error("roster for a1 must only be addressed to a1's clients")
	}
	body := string(payload)
	if !contains(body, "u1") || !contains(body, "u2") {
		t.Errorf("roster should list u1 and the partner u2; payload=%q", body)
	}
	if contains(body, "u3") {
		t.Error("unlinked agency user u3 must not appear")
	}
	t.Log("OK: roster lists partner users only")
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A group member invited across a mutual-aid link must receive group audio,
// even though the group lives under the creator's agency key.
func TestAdhocGroupAudioReachesPartnerAgencyMember(t *testing.T) {
	s := linkedServer(t)
	s.groupMgr = NewGroupManager(s)
	key := channelKey("a1", 10)
	s.groupMgr.CreateAdhoc(key, 10, "Task Force", GroupTypePreconfigured, "u1", 0)
	s.groupMgr.addMember(key, "u2") // u2 is in a2, a linked agency

	got := s.groupMgr.adhocMembersOf(key)
	found := false
	for _, m := range got {
		if m == "u2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("u2 not recorded as a member: %v", got)
	}
	t.Logf("OK: cross-agency member recorded, members=%v", got)
}

// Partner users must be labelled with their agency; own-agency users must not.
func TestRosterLabelsPartnerAgency(t *testing.T) {
	s := linkedServer(t)
	s.agencies["a2"].Name = "Fire Dept"
	a1Addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56011}
	a2Addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56012}
	s.clients["u1"] = &Client{Username: "medic1", AgencyID: "a1", Addr: a1Addr}
	s.clients["u2"] = &Client{Username: "engine7", AgencyID: "a2", Addr: a2Addr}

	var payload []byte
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) { payload = append([]byte(nil), data...) }
	defer func() { s.testFloorSink = nil }()
	s.broadcastRoster("a1")

	body := string(payload)
	if !contains(body, "engine7 (Fire Dept)") {
		t.Errorf("partner user should carry its agency label; got %q", body)
	}
	if contains(body, "medic1 (") {
		t.Errorf("own-agency user must NOT be labelled; got %q", body)
	}
	t.Log("OK: partner labelled, own-agency bare")
}

// A client echoing the DISPLAYED name back must still resolve to the real user,
// or cross-agency invites and calls would silently fail.
func TestFindClientTolerantOfRosterLabel(t *testing.T) {
	s := linkedServer(t)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56021}
	s.clients["u2"] = &Client{Username: "engine7", AgencyID: "a2", Addr: addr}

	if c, _ := s.findClientByUsername("engine7 (Fire Dept)"); c == nil {
		t.Error("decorated name must resolve to the real client")
	}
	if c, _ := s.findClientByUsername("engine7"); c == nil {
		t.Error("bare name must still resolve")
	}
	if c, _ := s.findClientByUsername("ghost (Fire Dept)"); c != nil {
		t.Error("unknown user must not resolve")
	}
	t.Log("OK: lookup tolerates the agency label")
}

// The group lives under the CREATOR's agency key. Both the creator and an
// invited partner must be able to locate it when they speak — otherwise audio
// flows one way only (the reported iOS->Android works, Android->iOS silent).
func TestFindAdhocForSpeakerBothDirections(t *testing.T) {
	s := linkedServer(t)
	s.groupMgr = NewGroupManager(s)
	key := channelKey("a1", 10) // created by u1 in agency a1
	s.groupMgr.CreateAdhoc(key, 10, "Task Force", GroupTypePreconfigured, "medic1", 0)
	s.groupMgr.addMember(key, "engine7") // invited from partner agency a2

	// Creator speaking, from its own agency.
	if got := s.findAdhocForSpeaker("medic1", "a1", 10); got != key {
		t.Errorf("creator lookup = %q, want %q", got, key)
	}
	// Partner speaking, from the OTHER agency — this is the direction that broke.
	if got := s.findAdhocForSpeaker("engine7", "a2", 10); got != key {
		t.Errorf("partner lookup = %q, want %q (one-way audio bug)", got, key)
	}
	// A stranger on the same channel must not match the group.
	if got := s.findAdhocForSpeaker("nobody", "a2", 10); got != "" {
		t.Errorf("non-member resolved to %q, want none", got)
	}
	// An unlinked agency must not reach it either.
	if got := s.findAdhocForSpeaker("engine7", "a3", 10); got != "" {
		t.Errorf("unlinked agency resolved to %q, want none", got)
	}
	t.Log("OK: group found from both the creator and the partner side")
}
