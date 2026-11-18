// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// One Zello channel shared with SEVERAL MeshRelay agencies must persist all of
// them, and announce the bridged channel into each.
func TestZelloMappingSharedWithMultipleAgencies(t *testing.T) {
	s := &Server{
		config: &Config{ZelloBridgesFile: t.TempDir() + "/zb.json"},
		agencies: map[string]*Agency{
			"default": {ID: "default"}, "dev": {ID: "dev"}, "dev1": {ID: "dev1"},
		},
		zelloBridges: map[string]*ZelloBridge{},
		clients:      map[string]*Client{},
		channels:     map[string]*Channel{},
	}
	s.zelloBridges["b1"] = &ZelloBridge{ID: "b1", Name: "Test", AgencyID: "default"}

	body, _ := json.Marshal(map[string]interface{}{
		"mappings": []map[string]interface{}{{
			"zello_channel":  "Ops",
			"mesh_channel":   11,
			"agency_id":      "default",
			"extra_agencies": []string{"dev", "dev1"},
		}},
	})
	req := httptest.NewRequest("POST", "/api/zello-bridges/b1/map", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "b1"})
	rec := httptest.NewRecorder()
	s.handleMapZelloBridge(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	m := s.zelloBridges["b1"].Mappings[0]
	if m.AgencyID != "default" {
		t.Errorf("primary agency = %q, want default", m.AgencyID)
	}
	if len(m.ExtraAgencies) != 2 {
		t.Fatalf("extra agencies = %v, want [dev dev1]", m.ExtraAgencies)
	}
	got := m.targetAgencies()
	if len(got) != 3 || got[0] != "default" {
		t.Errorf("targetAgencies = %v, want default first then dev, dev1", got)
	}
	t.Logf("OK: mapping shared with %v on ch %d", got, m.MeshChannel)
}

// Unknown agencies must be dropped, not stored.
func TestZelloExtraAgenciesRejectsUnknown(t *testing.T) {
	s := &Server{
		config:       &Config{ZelloBridgesFile: t.TempDir() + "/zb.json"},
		agencies:     map[string]*Agency{"default": {ID: "default"}, "dev": {ID: "dev"}},
		zelloBridges: map[string]*ZelloBridge{},
		clients:      map[string]*Client{},
		channels:     map[string]*Channel{},
	}
	s.zelloBridges["b1"] = &ZelloBridge{ID: "b1", Name: "T", AgencyID: "default"}

	body, _ := json.Marshal(map[string]interface{}{
		"mappings": []map[string]interface{}{{
			"zello_channel":  "Ops",
			"mesh_channel":   12,
			"agency_id":      "default",
			"extra_agencies": []string{"dev", "ghost", "default"},
		}},
	})
	req := httptest.NewRequest("POST", "/api/zello-bridges/b1/map", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "b1"})
	rec := httptest.NewRecorder()
	s.handleMapZelloBridge(rec, req)

	m := s.zelloBridges["b1"].Mappings[0]
	for _, a := range m.ExtraAgencies {
		if a == "ghost" {
			t.Error("unknown agency 'ghost' was stored")
		}
		if a == "default" {
			t.Error("primary agency duplicated into extras")
		}
	}
	t.Logf("OK: extras = %v", m.ExtraAgencies)
}

// The announce must reach clients in EVERY shared agency.
func TestNotifySharedChannelReachesAllAgencies(t *testing.T) {
	defAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55561}
	devAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55562}
	otherAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55563}

	s := &Server{
		agencies: map[string]*Agency{
			"default": {ID: "default"}, "dev": {ID: "dev"}, "unrelated": {ID: "unrelated"},
		},
		zelloBridges: map[string]*ZelloBridge{},
		clients:      map[string]*Client{},
		channels:     map[string]*Channel{},
	}
	s.clients["a"] = &Client{Username: "a", AgencyID: "default", Addr: defAddr}
	s.clients["b"] = &Client{Username: "b", AgencyID: "dev", Addr: devAddr}
	s.clients["c"] = &Client{Username: "c", AgencyID: "unrelated", Addr: otherAddr}

	sent := map[string]int{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) { sent[addr.String()]++ }
	defer func() { s.testFloorSink = nil }()

	b := &ZelloBridge{
		ID: "b1", Name: "T", AgencyID: "default", Enabled: true,
		Mappings: []ZelloMapping{{
			ZelloChannel: "Ops", MeshChannel: 11,
			AgencyID: "default", ExtraAgencies: []string{"dev"},
		}},
	}
	s.notifyZelloBridgeChannels(b, true)

	if sent[defAddr.String()] == 0 {
		t.Error("primary agency client got no announce")
	}
	if sent[devAddr.String()] == 0 {
		t.Error("SHARED agency client got no announce")
	}
	if sent[otherAddr.String()] != 0 {
		t.Error("unrelated agency wrongly received the announce")
	}
	t.Logf("OK: announces = %v", sent)
}
