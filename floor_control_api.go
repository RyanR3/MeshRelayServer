package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// --- talk floor control (dashboard toggle) ----------------------------------
//
// An agency admin controls their own agency; a super_admin may name one with
// ?agency= / "agency_id". The setting lives on the Agency record (agencies
// file), so it survives restarts and needs none.

func (s *Server) handleGetFloorControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	s.mu.RLock()
	a := s.agencies[agencyID]
	s.mu.RUnlock()
	if a == nil {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agency_id":     a.ID,
		"agency_name":   a.Name,
		"floor_control": s.floorEnabledForAgency(a.ID),
		"from_config":   !a.FloorControl && s.floorEnabledForAgency(a.ID),
	})
}

func (s *Server) handleUpdateFloorControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)

	var req struct {
		AgencyID     *string `json:"agency_id"` // super_admin only
		FloorControl *bool   `json:"floor_control"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FloorControl == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != nil && *req.AgencyID != "" {
		agencyID = *req.AgencyID
	}
	if err := s.setFloorControl(agencyID, *req.FloorControl); err != nil {
		code := http.StatusInternalServerError
		if err.Error() == "agency not found" {
			code = http.StatusNotFound
		}
		writeJSONError(w, code, err.Error())
		return
	}
	log.Printf("Floor control for agency %s: %v", agencyID, *req.FloorControl)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "ok",
		"agency_id":     agencyID,
		"floor_control": *req.FloorControl,
	})
}
