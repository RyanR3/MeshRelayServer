// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Custom map layers for the Command console (like CloudTAK's ZXY basemaps).
//
// An admin pastes a tile URL template — https://host/{z}/{x}/{y}.png, with
// optional {s} (subdomains) and {-y} (TMS rows) — on the dashboard's Maps
// page. Agency admins add layers for their own agency; a super admin can add
// one for any agency or for every agency (Agency ""). Each console of the
// agency then offers it in the map's layer picker, as a base map or as an
// overlay drawn on top (radar, parcels…).
//
// Stored in map_layers.json next to the agencies file.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

type MapLayer struct {
	ID          string  `json:"id"`
	Agency      string  `json:"agency"` // "" = every agency
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Kind        string  `json:"kind"` // "basemap" | "overlay"
	MaxZoom     int     `json:"max_zoom"`
	Opacity     float64 `json:"opacity"`
	Subdomains  string  `json:"subdomains,omitempty"`
	Attribution string  `json:"attribution,omitempty"`
	CreatedBy   string  `json:"created_by,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
}

const maxMapLayersPerAgency = 30

var (
	mapLayersMu sync.RWMutex
	mapLayers   []*MapLayer

	tilePlaceholder = regexp.MustCompile(`\{[^}]*\}`)
	subdomainsOK    = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
)

func (s *Server) mapLayersFile() string {
	return filepath.Join(filepath.Dir(s.config.AgenciesFile), "map_layers.json")
}

func (s *Server) loadMapLayers() {
	data, err := os.ReadFile(s.mapLayersFile())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Map layers: %v", err)
		}
		return
	}
	var list []*MapLayer
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("Map layers: bad file: %v", err)
		return
	}
	mapLayersMu.Lock()
	mapLayers = list
	mapLayersMu.Unlock()
	log.Printf("Loaded %d map layer(s)", len(list))
}

// saveMapLayers writes the list. Caller holds mapLayersMu (read or write).
func (s *Server) saveMapLayers() error {
	return writeJSONFileAtomic(s.mapLayersFile(), mapLayers)
}

// mapLayersFor lists the layers a console or admin of agency sees: the
// agency's own plus the every-agency ones. agency "" (super admin) = all.
func mapLayersFor(agency string) []MapLayer {
	mapLayersMu.RLock()
	defer mapLayersMu.RUnlock()
	out := []MapLayer{}
	for _, l := range mapLayers {
		if agency == "" || l.Agency == "" || l.Agency == agency {
			out = append(out, *l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "basemap"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// validateTileURL checks a ZXY template: https, {z} {x} and {y} or {-y},
// only known placeholders, and a URL that parses once they are filled in.
func validateTileURL(u string) error {
	if len(u) > 500 {
		return fmt.Errorf("URL is too long")
	}
	if !strings.HasPrefix(strings.ToLower(u), "https://") {
		return fmt.Errorf("the URL must start with https:// (the console is served over HTTPS, so browsers block http:// tiles)")
	}
	if strings.ContainsAny(u, " \t\r\n\"'<>\\`") {
		return fmt.Errorf("the URL contains a space or a character that is not allowed")
	}
	for _, p := range tilePlaceholder.FindAllString(u, -1) {
		switch p {
		case "{z}", "{x}", "{y}", "{-y}", "{s}", "{r}":
		default:
			return fmt.Errorf("unknown placeholder %s (use {z}, {x}, {y}, {-y} or {s})", p)
		}
	}
	if !strings.Contains(u, "{z}") || !strings.Contains(u, "{x}") ||
		!(strings.Contains(u, "{y}") || strings.Contains(u, "{-y}")) {
		return fmt.Errorf("the URL needs {z}, {x} and {y} (or {-y}) placeholders")
	}
	filled := tilePlaceholder.ReplaceAllString(u, "0")
	pu, err := url.Parse(filled)
	if err != nil || pu.Host == "" {
		return fmt.Errorf("that is not a valid URL")
	}
	return nil
}

func newMapLayerID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- dashboard API --------------------------------------------------------------
//
// GET    /api/my-agency/maps        layers the caller manages / sees
// POST   /api/my-agency/maps        add {agency_id (super admin; "" = all), name, url, kind, max_zoom, opacity, attribution, subdomains}
// DELETE /api/my-agency/maps/{id}
// GET    /api/dispatch/maps         layers for the caller's console

func (s *Server) handleGetMapLayers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		agencyID = ""
	}
	json.NewEncoder(w).Encode(mapLayersFor(agencyID))
}

func (s *Server) handleDispatchMapLayers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	if session == nil {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	json.NewEncoder(w).Encode(mapLayersFor(session.AgencyID))
}

func (s *Server) handleAddMapLayer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID    *string `json:"agency_id"` // super admin only; "" = every agency
		Name        string  `json:"name"`
		URL         string  `json:"url"`
		Kind        string  `json:"kind"`
		MaxZoom     int     `json:"max_zoom"`
		Opacity     float64 `json:"opacity"`
		Subdomains  string  `json:"subdomains"`
		Attribution string  `json:"attribution"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != nil {
		agencyID = *req.AgencyID
	}
	if agencyID != "" {
		s.mu.RLock()
		_, ok := s.agencies[agencyID]
		s.mu.RUnlock()
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agency not found")
			return
		}
	} else if !isSuper {
		writeJSONError(w, http.StatusForbidden, "only a super admin can add a map for every agency")
		return
	}

	l := &MapLayer{
		ID:          newMapLayerID(),
		Agency:      agencyID,
		Name:        strings.TrimSpace(req.Name),
		URL:         strings.TrimSpace(req.URL),
		Kind:        req.Kind,
		MaxZoom:     req.MaxZoom,
		Opacity:     req.Opacity,
		Subdomains:  strings.ToLower(strings.TrimSpace(req.Subdomains)),
		Attribution: strings.TrimSpace(req.Attribution),
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	if sess := s.getSession(r); sess != nil {
		l.CreatedBy = sess.Username
	}
	if l.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "give the map a name")
		return
	}
	if err := checkNameText("map name", l.Name, 40, ""); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTileURL(l.URL); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if l.Kind != "overlay" {
		l.Kind = "basemap"
	}
	if l.MaxZoom <= 0 {
		l.MaxZoom = 19
	}
	if l.MaxZoom > 24 {
		l.MaxZoom = 24
	}
	if l.Opacity <= 0 || l.Opacity > 1 {
		l.Opacity = 1
	}
	if l.Opacity < 0.1 {
		l.Opacity = 0.1
	}
	if strings.Contains(l.URL, "{s}") {
		if l.Subdomains == "" {
			l.Subdomains = "abc"
		}
		if !subdomainsOK.MatchString(l.Subdomains) {
			writeJSONError(w, http.StatusBadRequest, "subdomains must be letters or digits, like abc")
			return
		}
	} else {
		l.Subdomains = ""
	}
	if len(l.Attribution) > 200 {
		writeJSONError(w, http.StatusBadRequest, "attribution is too long (200 characters max)")
		return
	}
	if err := checkNameText("attribution", l.Attribution, 200, ""); l.Attribution != "" && err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	mapLayersMu.Lock()
	n := 0
	for _, x := range mapLayers {
		if x.Agency == agencyID {
			n++
			if strings.EqualFold(x.Name, l.Name) {
				mapLayersMu.Unlock()
				writeJSONError(w, http.StatusConflict, fmt.Sprintf("there is already a map called %q", x.Name))
				return
			}
		}
	}
	if n >= maxMapLayersPerAgency {
		mapLayersMu.Unlock()
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("at most %d maps per agency", maxMapLayersPerAgency))
		return
	}
	mapLayers = append(mapLayers, l)
	err := s.saveMapLayers()
	if err != nil {
		mapLayers = mapLayers[:len(mapLayers)-1]
	}
	mapLayersMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Map layer added: %q (%s) for %s by %q", l.Name, l.Kind, nonEmpty(agencyID, "every agency"), l.CreatedBy)
	json.NewEncoder(w).Encode(l)
}

func (s *Server) handleDeleteMapLayer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	id := mux.Vars(r)["id"]
	mapLayersMu.Lock()
	idx := -1
	for i, l := range mapLayers {
		if l.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		mapLayersMu.Unlock()
		writeJSONError(w, http.StatusNotFound, "map not found")
		return
	}
	l := mapLayers[idx]
	if !isSuper && l.Agency != agencyID {
		mapLayersMu.Unlock()
		writeJSONError(w, http.StatusForbidden, "this map belongs to another agency or to every agency")
		return
	}
	mapLayers = append(mapLayers[:idx:idx], mapLayers[idx+1:]...)
	err := s.saveMapLayers()
	mapLayersMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Map layer deleted: %q", l.Name)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
