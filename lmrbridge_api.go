package main

// HTTP API for LMR bridges (super_admin only), mirroring the mutual-aid
// endpoints: list, create, update agencies, toggle, delete.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/mux"
)

// lmrBridgeView is the API shape: the stored bridge plus agency display names,
// so the dashboard can render chips without a second lookup.
type lmrBridgeView struct {
	*LMRBridge
	AgencyNames []string `json:"agency_names"`
	Online      bool     `json:"online"`
}

// handleGetLMRBridges returns every bridge, newest config first.
func (s *Server) handleGetLMRBridges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	out := make([]lmrBridgeView, 0, len(s.lmrBridges))
	for _, b := range s.lmrBridges {
		names := make([]string, 0, len(b.Agencies))
		for _, aid := range b.Agencies {
			if a, ok := s.agencies[aid]; ok && a.Name != "" {
				names = append(names, a.Name)
			} else {
				names = append(names, aid)
			}
		}
		// The gateway is "online" when its account has a live client record.
		online := false
		if owner := b.ownerAgency(); owner != "" {
			if _, ok := s.clients[userKey(owner, b.Username)]; ok {
				online = true
			}
		}
		out = append(out, lmrBridgeView{LMRBridge: b, AgencyNames: names, Online: online})
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(out)
}

// handleCreateLMRBridge provisions a bridge: allocates a channel from the LMR
// block, creates the gateway user account, and announces the channel.
func (s *Server) handleCreateLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		Name        string   `json:"name"`
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Agencies    []string `json:"agencies"`
		ChannelName string   `json:"channel_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if req.Username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "gateway username and password are required")
		return
	}
	// One agency is enough here (unlike mutual aid, which patches agencies to
	// each other): a bridge with a single agency is a plain radio link.
	if len(req.Agencies) < 1 {
		writeJSONError(w, http.StatusBadRequest, "at least one agency is required")
		return
	}
	seen := map[string]bool{}
	for _, a := range req.Agencies {
		if seen[a] {
			writeJSONError(w, http.StatusBadRequest, "duplicate agency in list")
			return
		}
		seen[a] = true
	}

	channelName := req.ChannelName
	if channelName == "" {
		channelName = DefaultLMRChannelName
	}

	session := s.getSession(r)
	createdBy := ""
	if session != nil {
		createdBy = session.Username
	}

	s.mu.Lock()
	for _, aid := range req.Agencies {
		if _, ok := s.agencies[aid]; !ok {
			s.mu.Unlock()
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("agency %s not found", aid))
			return
		}
	}
	if s.lmrBridgeByUsername(req.Username) != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "a bridge already uses that gateway username")
		return
	}
	channel := s.pickLMRChannel()
	if channel == 0 {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict,
			fmt.Sprintf("no free LMR channel (block %d-%d is full)", LMRChannelMin, LMRChannelMax))
		return
	}

	b := &LMRBridge{
		ID:          newLMRBridgeID(),
		Name:        req.Name,
		Username:    req.Username,
		Agencies:    req.Agencies,
		Channel:     channel,
		ChannelName: channelName,
		Enabled:     true,
		CreatedAt:   nowRFC3339(),
		CreatedBy:   createdBy,
	}
	for _, aid := range b.Agencies {
		s.ensureLMRChannel(aid, channel, b.displayName())
	}
	s.lmrBridges[b.ID] = b
	owner := b.ownerAgency()
	s.mu.Unlock()

	// The gateway account is a normal PTT user, so the existing
	// challenge-response auth path works with no special case.
	if _, err := s.AddUser(req.Username, "LMR Gateway: "+b.displayName(), req.Password, owner, "user"); err != nil {
		// Roll the bridge back so a failed account doesn't leave a dead row.
		s.mu.Lock()
		delete(s.lmrBridges, b.ID)
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("failed to create gateway user: %s", err.Error()))
		return
	}
	if err := s.saveUsers(); err != nil {
		log.Printf("LMR: failed to save users after creating gateway account: %v", err)
	}

	go s.notifyLMRChannel(b.Agencies, b.Channel, b.displayName(), true)

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}

	log.Printf("LMR: created bridge %s user=%s by %s", describeLMRBridge(b), b.Username, createdBy)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "id": b.ID, "channel": b.Channel,
	})
}

// handleUpdateLMRBridge changes a bridge's agency set and/or display name.
// Agencies removed from the set have the channel withdrawn; agencies added get
// it announced.
func (s *Server) handleUpdateLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]

	var req struct {
		Name        *string   `json:"name"`
		ChannelName *string   `json:"channel_name"`
		Agencies    *[]string `json:"agencies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	s.mu.Lock()
	b, ok := s.lmrBridges[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}

	oldAgencies := append([]string(nil), b.Agencies...)
	oldName := b.displayName()

	if req.Name != nil {
		b.Name = *req.Name
	}
	if req.ChannelName != nil {
		b.ChannelName = *req.ChannelName
	}
	if req.Agencies != nil {
		if len(*req.Agencies) < 1 {
			s.mu.Unlock()
			writeJSONError(w, http.StatusBadRequest, "at least one agency is required")
			return
		}
		seen := map[string]bool{}
		for _, a := range *req.Agencies {
			if seen[a] {
				s.mu.Unlock()
				writeJSONError(w, http.StatusBadRequest, "duplicate agency in list")
				return
			}
			seen[a] = true
			if _, ok := s.agencies[a]; !ok {
				s.mu.Unlock()
				writeJSONError(w, http.StatusNotFound, fmt.Sprintf("agency %s not found", a))
				return
			}
		}
		b.Agencies = *req.Agencies
		for _, aid := range b.Agencies {
			s.ensureLMRChannel(aid, b.Channel, b.displayName())
		}
	}

	newAgencies := append([]string(nil), b.Agencies...)
	channel := b.Channel
	newName := b.displayName()
	enabled := b.Enabled
	s.mu.Unlock()

	// Withdraw from agencies that were dropped.
	nowSet := map[string]bool{}
	for _, a := range newAgencies {
		nowSet[a] = true
	}
	var removed []string
	for _, a := range oldAgencies {
		if !nowSet[a] {
			removed = append(removed, a)
		}
	}
	if len(removed) > 0 {
		go s.notifyLMRChannel(removed, channel, oldName, false)
	}
	if enabled {
		go s.notifyLMRChannel(newAgencies, channel, newName, true)
	}

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleToggleLMRBridge enables/disables a bridge, announcing or withdrawing
// its channel accordingly.
func (s *Server) handleToggleLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]

	s.mu.Lock()
	b, ok := s.lmrBridges[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	b.Enabled = !b.Enabled
	enabled := b.Enabled
	agencies := append([]string(nil), b.Agencies...)
	channel, name := b.Channel, b.displayName()
	s.mu.Unlock()

	go s.notifyLMRChannel(agencies, channel, name, enabled)

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}
	log.Printf("LMR: bridge %s enabled=%v", id, enabled)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "enabled": enabled})
}

// handleDeleteLMRBridge removes a bridge, withdraws its channel, and deletes
// the gateway account it owns.
func (s *Server) handleDeleteLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]

	s.mu.Lock()
	b, ok := s.lmrBridges[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	agencies := append([]string(nil), b.Agencies...)
	channel, name := b.Channel, b.displayName()
	username, owner := b.Username, b.ownerAgency()
	delete(s.lmrBridges, id)
	// Drop the agency-scoped channel records this bridge created.
	for _, a := range agencies {
		delete(s.channels, channelKey(a, channel))
	}

	// Tear the gateway account down the way a guest event does: remove the
	// account, revoke any enrollment tokens issued for it, and kick the session
	// if the gateway is connected. Leaving any of these behind would let a
	// deleted bridge's gateway keep talking or re-enrol.
	var kickAddrs []*net.UDPAddr
	var kickKeys []string
	if owner != "" && username != "" {
		delete(s.users, userKey(owner, username))
		for tok, et := range s.enrollmentTokens {
			if et.Username == username && et.AgencyID == owner {
				delete(s.enrollmentTokens, tok)
			}
		}
		ck := userKey(owner, username)
		if c, ok := s.clients[ck]; ok {
			kickAddrs = append(kickAddrs, c.Addr)
			kickKeys = append(kickKeys, ck)
		}
	}
	s.mu.Unlock()

	for i, addr := range kickAddrs {
		resp := SerializeAuthResponse(&AuthResponsePacket{
			Success: false,
			Revoked: true,
			Message: "LMR bridge removed",
		})
		s.sendPacket(resp, addr)
		s.mu.Lock()
		delete(s.clients, kickKeys[i])
		delete(s.authenticatedAddrs, addr.String())
		s.mu.Unlock()
	}

	go s.notifyLMRChannel(agencies, channel, name, false)

	if err := s.saveUsers(); err != nil {
		log.Printf("LMR: failed to save users after deleting gateway account: %v", err)
	}
	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}
	log.Printf("LMR: deleted bridge %s (ch %d, user %s)", id, channel, username)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// writeJSONError keeps the handlers above terse.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b)
}
