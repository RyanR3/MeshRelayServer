package main

// HTTP API for agency-driven mutual aid: discovery settings, the directory,
// and the request/approve flow. Every route here is agency_admin-capable and
// scoped to the caller's own agency; super_admin sees and may act on everything.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/mux"
)

// callerAgency returns the agency this request acts on behalf of, and whether
// the caller is a super_admin (who is not bound to a single agency).
func (s *Server) callerAgency(r *http.Request) (string, bool) {
	session := s.getSession(r)
	if session == nil {
		return "", false
	}
	return session.AgencyID, session.Role == "super_admin"
}

// --- discovery settings -----------------------------------------------------

// handleGetMyDiscovery returns the caller's own discovery settings.
func (s *Server) handleGetMyDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, _ := s.callerAgency(r)
	// A super_admin may inspect a specific agency with ?agency=
	if q := r.URL.Query().Get("agency"); q != "" {
		if _, isSuper := s.callerAgency(r); isSuper {
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

	trusted := make([]map[string]string, 0, len(a.AutoAcceptFrom))
	s.mu.RLock()
	for _, id := range a.AutoAcceptFrom {
		name := id
		if other, ok := s.agencies[id]; ok && other.Name != "" {
			name = other.Name
		}
		trusted = append(trusted, map[string]string{"id": id, "name": name})
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"agency_id":    a.ID,
		"agency_name":  a.Name,
		"discoverable": a.Discoverable,
		"auto_accept":  trusted,
	})
}

// handleUpdateMyDiscovery sets the caller's discoverable flag and trust list.
func (s *Server) handleUpdateMyDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)

	var req struct {
		AgencyID     *string   `json:"agency_id"` // super_admin only
		Discoverable *bool     `json:"discoverable"`
		AutoAccept   *[]string `json:"auto_accept_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != nil && *req.AgencyID != "" {
		agencyID = *req.AgencyID
	}

	s.mu.Lock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if req.Discoverable != nil {
		a.Discoverable = *req.Discoverable
	}
	if req.AutoAccept != nil {
		// Keep only agencies that exist, and never trust yourself.
		clean := make([]string, 0, len(*req.AutoAccept))
		seen := map[string]bool{}
		for _, id := range *req.AutoAccept {
			if id == agencyID || seen[id] {
				continue
			}
			if _, ok := s.agencies[id]; !ok {
				continue
			}
			seen[id] = true
			clean = append(clean, id)
		}
		a.AutoAcceptFrom = clean
	}
	discoverable := a.Discoverable
	trustedCount := len(a.AutoAcceptFrom)
	s.mu.Unlock()

	if err := s.saveAgencies(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Mutual aid: agency %s discoverable=%v trusted=%d",
		agencyID, discoverable, trustedCount)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- directory --------------------------------------------------------------

// handleGetDiscoverableAgencies lists agencies that opted into the directory.
// NAMES ONLY: no users, channels or activity — see Agency.Discoverable.
func (s *Server) handleGetDiscoverableAgencies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, _ := s.callerAgency(r)

	s.mu.RLock()
	self := s.agencies[mine]
	out := make([]map[string]interface{}, 0)
	for _, a := range s.agencies {
		if !a.Discoverable || a.ID == mine {
			continue // your own agency is not a partner
		}
		// Surface the state of the relationship so the UI can show the right
		// action without a second round trip.
		state := "none"
		if s.linkBetween(mine, a.ID) != nil {
			state = "linked"
		} else if req := s.pendingRequestBetween(mine, a.ID); req != nil {
			if req.FromAgency == mine {
				state = "requested"
			} else {
				state = "awaiting_you"
			}
		}
		out = append(out, map[string]interface{}{
			"id":    a.ID,
			"name":  a.Name,
			"state": state,
			// Whether THEY auto-accept US — i.e. would a request link instantly.
			"auto_accepts_us": a.trustsAgency(mine),
			// Whether WE auto-accept THEM.
			"we_trust_them": self != nil && self.trustsAgency(a.ID),
		})
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(out)
}

// --- requests ---------------------------------------------------------------

// maRequestView adds agency display names to a stored request.
type maRequestView struct {
	*MutualAidRequest
	FromName  string `json:"from_name"`
	ToName    string `json:"to_name"`
	Direction string `json:"direction"` // "inbound" | "outbound"
}

// handleGetMutualAidRequests returns requests involving the caller's agency.
func (s *Server) handleGetMutualAidRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, isSuper := s.callerAgency(r)

	s.mu.RLock()
	name := func(id string) string {
		if a, ok := s.agencies[id]; ok && a.Name != "" {
			return a.Name
		}
		return id
	}
	out := make([]maRequestView, 0)
	for _, req := range s.mutualAidRequests {
		involved := req.FromAgency == mine || req.ToAgency == mine
		if !involved && !isSuper {
			continue
		}
		dir := "outbound"
		if req.ToAgency == mine {
			dir = "inbound"
		}
		out = append(out, maRequestView{
			MutualAidRequest: req,
			FromName:         name(req.FromAgency),
			ToName:           name(req.ToAgency),
			Direction:        dir,
		})
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(out)
}

// handleCreateMutualAidRequest asks another agency for a patch. If that agency
// already trusts the caller, the link is created immediately.
func (s *Server) handleCreateMutualAidRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, _ := s.callerAgency(r)
	session := s.getSession(r)
	createdBy := ""
	if session != nil {
		createdBy = session.Username
	}

	var req struct {
		ToAgency    string `json:"to_agency"`
		ChannelName string `json:"channel_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.ToAgency == "" {
		writeJSONError(w, http.StatusBadRequest, "to_agency is required")
		return
	}
	if req.ToAgency == mine {
		writeJSONError(w, http.StatusBadRequest, "cannot request a patch with your own agency")
		return
	}

	s.mu.Lock()
	target := s.agencies[req.ToAgency]
	if target == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if !target.Discoverable {
		// Not in the directory: refuse rather than leak that it exists.
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if s.linkBetween(mine, req.ToAgency) != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "these agencies are already patched")
		return
	}
	if existing := s.pendingRequestBetween(mine, req.ToAgency); existing != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "a request between these agencies is already pending")
		return
	}

	channelName := req.ChannelName
	if channelName == "" {
		channelName = "Mutual Aid"
	}

	mar := &MutualAidRequest{
		ID:          newMutualAidRequestID(),
		FromAgency:  mine,
		ToAgency:    req.ToAgency,
		ChannelName: channelName,
		Status:      MARequestPending,
		CreatedAt:   nowStamp(),
		CreatedBy:   createdBy,
	}

	// Standing agreement: the RECEIVING agency trusts us, so skip approval.
	var link *MutualAidLink
	if target.trustsAgency(mine) {
		link = s.createMutualAidLinkLocked([]string{mine, req.ToAgency}, channelName, createdBy)
		if link == nil {
			s.mu.Unlock()
			writeJSONError(w, http.StatusConflict,
				"no free mutual-aid channel — the shared block is full")
			return
		}
		mar.Status = MARequestApproved
		mar.AutoAccepted = true
		mar.ResolvedAt = nowStamp()
		mar.ResolvedBy = "auto (standing agreement)"
		mar.LinkID = link.ID
	}
	s.mutualAidRequests[mar.ID] = mar
	s.mu.Unlock()

	if link != nil {
		s.finishMutualAidLink(link)
		log.Printf("Mutual aid: %s -> %s auto-accepted, channel %d (%s)",
			mine, req.ToAgency, link.Channel, channelName)
		if err := s.saveMutualAid(); err != nil {
			log.Printf("Mutual aid: failed to save links: %v", err)
		}
	} else {
		log.Printf("Mutual aid: %s requested a patch with %s (%s), pending approval",
			mine, req.ToAgency, channelName)
	}

	if err := s.saveMutualAidRequests(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}

	resp := map[string]interface{}{"status": "ok", "id": mar.ID, "request_status": mar.Status}
	if link != nil {
		resp["channel"] = link.Channel
		resp["auto_accepted"] = true
	}
	json.NewEncoder(w).Encode(resp)
}

// handleApproveMutualAidRequest accepts an inbound request and creates the link.
func (s *Server) handleApproveMutualAidRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)
	session := s.getSession(r)
	by := ""
	if session != nil {
		by = session.Username
	}

	s.mu.Lock()
	mar := s.mutualAidRequests[id]
	if mar == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "request not found")
		return
	}
	// Only the RECEIVING agency may approve — you cannot approve your own ask.
	if mar.ToAgency != mine && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "only the receiving agency can approve this request")
		return
	}
	if mar.Status != MARequestPending {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "request is already "+mar.Status)
		return
	}
	if s.linkBetween(mar.FromAgency, mar.ToAgency) != nil {
		mar.Status = MARequestApproved
		mar.ResolvedAt, mar.ResolvedBy = nowStamp(), by
		s.mu.Unlock()
		_ = s.saveMutualAidRequests()
		writeJSONError(w, http.StatusConflict, "these agencies are already patched")
		return
	}

	link := s.createMutualAidLinkLocked(
		[]string{mar.FromAgency, mar.ToAgency}, mar.ChannelName, by)
	if link == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict,
			"no free mutual-aid channel — the shared block is full")
		return
	}
	mar.Status = MARequestApproved
	mar.ResolvedAt, mar.ResolvedBy, mar.LinkID = nowStamp(), by, link.ID
	s.mu.Unlock()

	s.finishMutualAidLink(link)
	log.Printf("Mutual aid: %s approved %s's request, channel %d (%s)",
		mar.ToAgency, mar.FromAgency, link.Channel, link.ChannelName)

	if err := s.saveMutualAid(); err != nil {
		log.Printf("Mutual aid: failed to save links: %v", err)
	}
	if err := s.saveMutualAidRequests(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "channel": link.Channel, "link_id": link.ID,
	})
}

// handleDeclineMutualAidRequest rejects an inbound request.
func (s *Server) handleDeclineMutualAidRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)
	session := s.getSession(r)
	by := ""
	if session != nil {
		by = session.Username
	}

	s.mu.Lock()
	mar := s.mutualAidRequests[id]
	if mar == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "request not found")
		return
	}
	// The receiver declines; the sender may also withdraw their own request.
	if mar.ToAgency != mine && mar.FromAgency != mine && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "not your request")
		return
	}
	if mar.Status != MARequestPending {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "request is already "+mar.Status)
		return
	}
	mar.Status = MARequestDeclined
	mar.ResolvedAt, mar.ResolvedBy = nowStamp(), by
	from, to := mar.FromAgency, mar.ToAgency
	s.mu.Unlock()

	log.Printf("Mutual aid: request %s -> %s declined by %s", from, to, by)
	if err := s.saveMutualAidRequests(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleLeaveMutualAid removes the caller's agency from a live patch. Either
// side may leave; super_admin may remove any link outright.
func (s *Server) handleLeaveMutualAid(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	s.mu.Lock()
	link := s.mutualAidLinks[id]
	if link == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "patch not found")
		return
	}
	member := false
	for _, a := range link.Agencies {
		if a == mine {
			member = true
			break
		}
	}
	if !member && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "your agency is not part of this patch")
		return
	}

	agencies := append([]string(nil), link.Agencies...)
	channel, name := link.Channel, link.ChannelName
	// A two-agency patch has no meaning with one member, and every patch here is
	// created as a pair, so leaving dissolves it for both sides.
	delete(s.mutualAidLinks, id)
	s.mu.Unlock()

	go s.notifyMutualAidChannel(agencies, channel, name, false)
	go func() {
		for _, a := range agencies {
			s.broadcastRoster(a)
		}
	}()

	log.Printf("Mutual aid: patch %s (ch %d) removed by %s", id, channel, mine)
	if err := s.saveMutualAid(); err != nil {
		writeJSONError(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
