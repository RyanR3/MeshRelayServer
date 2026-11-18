// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestValidateTileURL(t *testing.T) {
	good := []string{
		"https://tile.openstreetmap.org/{z}/{x}/{y}.png",
		"https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png",
		"https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}",
		"https://example.com/tms/{z}/{x}/{-y}.png?key=abc123",
	}
	for _, u := range good {
		if err := validateTileURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://tile.example.com/{z}/{x}/{y}.png", // not https
		"https://tile.example.com/{z}/{x}.png",    // no y
		"https://tile.example.com/{zoom}/{x}/{y}", // unknown placeholder
		"javascript:alert(1)//{z}/{x}/{y}",
		"https://x.com/{z}/{x}/{y}\"><script>",
		"https:///{z}/{x}/{y}", // no host
	}
	for _, u := range bad {
		if validateTileURL(u) == nil {
			t.Errorf("accepted %s", u)
		}
	}
}

func mapReq(t *testing.T, s *Server, token, method, path string, body interface{}, h http.HandlerFunc, vars map[string]string) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// Agency admins manage their own agency's maps; every-agency maps are the
// super admin's; consoles see their agency's plus every-agency maps.
func TestMapLayersScoping(t *testing.T) {
	s, _ := newSecTestServer(t)
	s.webSessions = map[string]*WebSession{}
	mapLayersMu.Lock()
	mapLayers = nil
	mapLayersMu.Unlock()
	addTestSession(s, "root", "root", "super_admin", "")
	addTestSession(s, "adm1", "boss1", "agency_admin", "a1")
	addTestSession(s, "adm2", "boss2", "agency_admin", "a2")

	add := func(tok string, body map[string]interface{}) (int, MapLayer) {
		code, out := mapReq(t, s, tok, "POST", "/api/my-agency/maps", body, s.handleAddMapLayer, nil)
		var l MapLayer
		json.Unmarshal(out, &l)
		return code, l
	}
	if code, _ := add("root", map[string]interface{}{"agency_id": "", "name": "County Streets", "url": "https://t.example.com/{z}/{x}/{y}.png"}); code != 200 {
		t.Fatalf("super admin every-agency add = %d", code)
	}
	code, own := add("adm1", map[string]interface{}{"agency_id": "", "name": "Radar", "kind": "overlay", "opacity": 0.6, "url": "https://r.example.com/{z}/{x}/{y}.png"})
	if code != 200 || own.Agency != "a1" || own.Kind != "overlay" {
		t.Fatalf("agency admin add = %d %+v (agency_id must be ignored)", code, own)
	}
	if code, _ := add("adm1", map[string]interface{}{"name": "radar", "url": "https://r2.example.com/{z}/{x}/{y}.png"}); code != http.StatusConflict {
		t.Fatalf("duplicate name = %d", code)
	}

	if got := mapLayersFor("a1"); len(got) != 2 {
		t.Fatalf("a1 console sees %d maps, want 2", len(got))
	}
	if got := mapLayersFor("a2"); len(got) != 1 || got[0].Name != "County Streets" {
		t.Fatalf("a2 console sees %+v", got)
	}

	// a2's admin can't delete a1's map or the every-agency one.
	for _, id := range []string{own.ID, mapLayersFor("a2")[0].ID} {
		if code, _ := mapReq(t, s, "adm2", "DELETE", "/api/my-agency/maps/"+id, nil, s.handleDeleteMapLayer, map[string]string{"id": id}); code != http.StatusForbidden {
			t.Fatalf("cross-agency delete = %d", code)
		}
	}
	if code, _ := mapReq(t, s, "adm1", "DELETE", "/api/my-agency/maps/"+own.ID, nil, s.handleDeleteMapLayer, map[string]string{"id": own.ID}); code != 200 {
		t.Fatalf("own delete = %d", code)
	}
	if got := mapLayersFor("a1"); len(got) != 1 {
		t.Fatalf("after delete a1 sees %d", len(got))
	}
}
