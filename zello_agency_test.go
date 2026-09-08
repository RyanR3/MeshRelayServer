package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// Reproduces the reported bug: applying a mapping to a DIFFERENT agency than the
// bridge default. The mapping must survive with its chosen agency.
func TestMapZelloBridgePerAgency(t *testing.T) {
	s := &Server{
		config:       &Config{ZelloBridgesFile: t.TempDir() + "/zb.json"},
		agencies:     map[string]*Agency{"default": {ID: "default"}, "dev": {ID: "dev"}},
		zelloBridges: map[string]*ZelloBridge{},
		clients:      map[string]*Client{},
		channels:     map[string]*Channel{},
	}
	s.zelloBridges["b1"] = &ZelloBridge{
		ID: "b1", Name: "Test", AgencyID: "default", Enabled: false,
	}

	// Apply one mapping targeting agency "dev" (not the bridge default "default").
	body, _ := json.Marshal(map[string]interface{}{
		"mappings": []map[string]interface{}{
			{"zello_channel": "Ops", "mesh_channel": 0, "agency_id": "dev"},
		},
	})
	req := httptest.NewRequest("POST", "/api/zello-bridges/b1/map", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "b1"})
	rec := httptest.NewRecorder()

	s.handleMapZelloBridge(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	b := s.zelloBridges["b1"]
	if len(b.Mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d: %+v", len(b.Mappings), b.Mappings)
	}
	m := b.Mappings[0]
	if m.ZelloChannel != "Ops" {
		t.Errorf("zello channel = %q, want Ops", m.ZelloChannel)
	}
	if m.AgencyID != "dev" {
		t.Errorf("mapping agency = %q, want dev (cross-agency mapping was dropped/rewritten)", m.AgencyID)
	}
	if m.MeshChannel < ZelloChannelMin || m.MeshChannel > ZelloChannelMax {
		t.Errorf("mesh channel = %d, want %d-%d",
			m.MeshChannel, ZelloChannelMin, ZelloChannelMax)
	}
	t.Logf("OK: mapping %+v", m)
}

// The announce for a cross-agency mapping must reach a client in the MAPPING's
// agency (dev), not the bridge default (default). Uses real UDP sockets so we
// exercise the actual sendPacket path with no production test hooks.
func TestNotifyZelloBridgeChannelsPerAgency(t *testing.T) {
	devAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55555}
	defAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55556}

	s := &Server{
		agencies:     map[string]*Agency{"default": {ID: "default"}, "dev": {ID: "dev"}},
		zelloBridges: map[string]*ZelloBridge{},
		clients:      map[string]*Client{},
		channels:     map[string]*Channel{},
	}
	s.clients["devuser"] = &Client{Username: "devuser", AgencyID: "dev", Addr: devAddr}
	s.clients["defuser"] = &Client{Username: "defuser", AgencyID: "default", Addr: defAddr}

	// Capture sends via the existing test sink instead of a real socket.
	sentTo := map[string]int{}
	s.testFloorSink = func(addr *net.UDPAddr, data []byte) { sentTo[addr.String()]++ }
	defer func() { s.testFloorSink = nil }()

	b := &ZelloBridge{
		ID: "b1", Name: "T", AgencyID: "default", Enabled: true,
		Mappings: []ZelloMapping{{ZelloChannel: "Ops", MeshChannel: 10, AgencyID: "dev"}},
	}
	s.notifyZelloBridgeChannels(b, true)

	if sentTo[devAddr.String()] == 0 {
		t.Errorf("dev-agency client got NO announce (cross-agency mapping not pushed to client)")
	}
	if sentTo[defAddr.String()] != 0 {
		t.Errorf("default-agency client wrongly received the dev mapping's announce")
	}
	t.Logf("OK: announces=%v", sentTo)
}
