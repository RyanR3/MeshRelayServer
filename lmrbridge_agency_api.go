// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Agency-facing LMR bridges.
//
// An LMR bridge belongs to the agency that created it: their gateway hardware,
// their radio, their gateway login. It is private by default — no other agency
// knows it exists until the owner grants access, which makes the bridged
// channel appear for them exactly as it does for the owner.
//
// Granting is a direct act by the owner, not a negotiation, matching the Zello
// model. A recipient gets USE only: never the gateway credentials, the
// enrollment QR, or delete. They may leave at any time.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/mux"
)

// lmrAgencyView is what an agency admin sees. Owner-only fields are omitted for
// a recipient — notably the gateway login, which is a credential.
type lmrAgencyView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Channel     int    `json:"channel"`
	ChannelName string `json:"channel_name"`
	Enabled     bool   `json:"enabled"`
	AllowGuests bool   `json:"allow_guests"` // guests may transmit on this bridge
	Access      string `json:"access"`       // "owner" | "shared"

	// Owner-only.
	Username   string              `json:"username,omitempty"`
	Online     bool                `json:"online,omitempty"`
	SharedWith []map[string]string `json:"shared_with,omitempty"`

	// Recipient-facing.
	OwnerName string `json:"owner_name,omitempty"`

	// ATAK marker (lmr_atak.go), for the caller's own agency while the marker
	// is switched on: its own TAK link (takLinkState), the master TAK
	// connection (takMasterLinkState: states only, no address) and where the
	// marker goes now ("own", "master", "both", "none").
	ShowInATAK bool     `json:"show_in_atak"`
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	ATAKTAK    string   `json:"atak_tak,omitempty"`
	ATAKMaster string   `json:"atak_master,omitempty"`
	ATAKSent   string   `json:"atak_sent,omitempty"`
}

// handleGetMyLMRBridges lists the bridges this agency owns or has been granted.
func (s *Server) handleGetMyLMRBridges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, isSuper := s.callerAgency(r)

	s.mu.RLock()
	name := func(id string) string {
		if a, ok := s.agencies[id]; ok && a.Name != "" {
			return a.Name
		}
		return id
	}
	out := make([]lmrAgencyView, 0)
	for _, b := range s.lmrBridges {
		owner := b.ownedBy(mine)
		shared := b.sharedWith(mine)
		if !owner && !shared && !isSuper {
			continue
		}
		v := lmrAgencyView{
			ID: b.ID, Name: b.Name, Channel: b.Channel,
			ChannelName: b.displayName(), Enabled: b.Enabled, AllowGuests: b.AllowGuests,
			ShowInATAK: b.ShowInATAK, Lat: copyFloat(b.Lat), Lon: copyFloat(b.Lon),
		}
		if owner || isSuper {
			v.Access = "owner"
			v.Username = b.Username
			if o := b.ownerAgency(); o != "" {
				_, v.Online = s.clients[userKey(o, b.Username)]
			}
			v.SharedWith = make([]map[string]string, 0)
			for _, ag := range b.Agencies {
				if ag == b.ownerAgency() {
					continue
				}
				v.SharedWith = append(v.SharedWith,
					map[string]string{"id": ag, "name": name(ag)})
			}
		} else {
			// Recipient: the channel and who shared it. No gateway login.
			v.Access = "shared"
			v.OwnerName = name(b.ownerAgency())
		}
		out = append(out, v)
	}
	s.mu.RUnlock()

	// After s.mu is released (lmr_atak.go locking).
	if mine != "" {
		for i := range out {
			if out[i].ShowInATAK {
				st := s.lmrAtakDelivery(mine)
				out[i].ATAKTAK, out[i].ATAKMaster, out[i].ATAKSent = st.TAK, st.Master, st.Sent
			}
		}
	}

	json.NewEncoder(w).Encode(out)
}

// handleCreateMyLMRBridge creates a bridge owned by the caller's agency,
// provisioning its gateway account the same way the super_admin path does.
func (s *Server) handleCreateMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, _ := s.callerAgency(r)
	if mine == "" {
		writeJSONError(w, http.StatusForbidden, "no agency for this account")
		return
	}
	// getSession RLocks s.mu and Go's RWMutex is not reentrant, so resolve the
	// caller BEFORE taking the write lock.
	createdBy := ""
	if sess := s.getSession(r); sess != nil {
		createdBy = sess.Username
	}

	var req struct {
		Name        string `json:"name"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		ChannelName string `json:"channel_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "gateway username and password are required")
		return
	}
	if err := validateNewPassword(req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	channelName := req.ChannelName
	if channelName == "" {
		channelName = DefaultLMRChannelName
	}

	s.mu.Lock()
	if _, ok := s.agencies[mine]; !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	// Scoped to the owning agency: gateway accounts are per-agency, so another
	// agency using this name is not a conflict.
	if s.lmrBridgeByUsername(mine, req.Username) != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict,
			"a bridge in this agency already uses that gateway username")
		return
	}
	// A gateway account can outlive its bridge if an earlier delete failed
	// part-way. Say which it is, rather than letting AddUser fail later with a
	// message about users that does not mention bridges.
	if _, taken := s.users[userKey(mine, req.Username)]; taken {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict,
			"an account named \""+req.Username+"\" already exists in this agency — "+
				"pick another gateway username, or remove that user first")
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
		ID:            newLMRBridgeID(),
		Name:          req.Name,
		OwnerAgencyID: mine,
		Username:      req.Username,
		Agencies:      []string{mine},
		Channel:       channel,
		ChannelName:   channelName,
		Enabled:       true,
		CreatedAt:     nowStamp(),
		CreatedBy:     createdBy,
	}
	s.ensureLMRChannel(mine, channel, b.displayName())
	s.lmrBridges[b.ID] = b
	s.mu.Unlock()

	// The gateway account is a normal PTT user, so the existing
	// challenge-response auth path works with no special case.
	if _, err := s.AddUser(req.Username, "LMR Gateway: "+b.displayName(),
		req.Password, mine, "user"); err != nil {
		s.mu.Lock()
		delete(s.lmrBridges, b.ID)
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict,
			"failed to create gateway user: "+err.Error())
		return
	}
	if err := s.saveUsersUnlocked(); err != nil {
		log.Printf("LMR: failed to save users after creating gateway account: %v", err)
	}

	go s.notifyLMRChannel([]string{mine}, channel, b.displayName(), true)

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: agency %s created bridge %s on channel %d", mine, b.ID, channel)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "id": b.ID, "channel": channel,
	})
}

// handleShareMyLMRBridge grants another agency USE of a bridge the caller owns.
func (s *Server) handleShareMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	var req struct {
		AgencyID string `json:"agency_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgencyID == "" {
		writeJSONError(w, http.StatusBadRequest, "agency_id is required")
		return
	}

	// All checks and the grant happen in one critical section with a
	// deferred unlock: an early return used to leave s.mu held (the
	// "make your agency discoverable" refusal), freezing the whole server.
	var channel int
	var chName string
	var enabled bool
	code, msg := func() (int, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := s.lmrBridges[id]
		if b == nil {
			return http.StatusNotFound, "bridge not found"
		}
		if !b.ownedBy(mine) && !isSuper {
			return http.StatusForbidden, "only the owning agency can share this bridge"
		}
		if b.servesAgency(req.AgencyID) {
			return http.StatusConflict, "that agency already has access"
		}
		target := s.agencies[req.AgencyID]
		if target == nil || !target.Discoverable {
			// Refuse rather than confirm a hidden agency exists.
			return http.StatusNotFound, "agency not found"
		}
		if !isSuper {
			if me := s.agencies[mine]; me == nil || !me.Discoverable {
				return http.StatusForbidden, "make your agency discoverable before sharing a bridge"
			}
		}
		b.Agencies = append(b.Agencies, req.AgencyID)
		// The channel must exist in the new agency or its audio has nowhere to land.
		s.ensureLMRChannel(req.AgencyID, b.Channel, b.displayName())
		channel, chName, enabled = b.Channel, b.displayName(), b.Enabled
		return 0, ""
	}()
	if code != 0 {
		writeJSONError(w, code, msg)
		return
	}

	if enabled {
		go s.notifyLMRChannel([]string{req.AgencyID}, channel, chName, true)
		go s.broadcastRoster(req.AgencyID)
	}
	s.lmrMarkersChanged() // ATAK marker follows the bridge (lmr_atak.go)
	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: bridge %s shared with agency %q by %s", id, req.AgencyID, mine)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleUnshareMyLMRBridge revokes an agency's access. The owner may remove
// anyone; a recipient may remove only itself. The owner cannot be removed —
// that is what deleting the bridge is for.
func (s *Server) handleUnshareMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	var req struct {
		AgencyID string `json:"agency_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	b := s.lmrBridges[id]
	if b == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	target := req.AgencyID
	if target == "" {
		target = mine // no body: the caller is leaving
	}
	owner := b.ownedBy(mine) || isSuper
	if !owner && target != mine {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "you can only remove your own access")
		return
	}
	if target == b.ownerAgency() {
		s.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest,
			"the owning agency cannot leave its own bridge — delete it instead")
		return
	}

	kept := make([]string, 0, len(b.Agencies))
	removed := false
	for _, ag := range b.Agencies {
		if ag == target {
			removed = true
			continue
		}
		kept = append(kept, ag)
	}
	if !removed {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "that agency does not have access")
		return
	}
	b.Agencies = kept
	delete(s.channels, channelKey(target, b.Channel))
	channel, chName := b.Channel, b.displayName()
	s.mu.Unlock()

	go func() {
		s.announceChannelRemoval(target, channel, chName)
		s.broadcastRoster(target)
	}()
	s.lmrMarkersChanged() // ATAK marker follows the bridge (lmr_atak.go)

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: bridge %s access removed for agency %q (by %s)", id, target, mine)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleDeleteMyLMRBridge removes a bridge the caller's agency OWNS, tearing
// the gateway down completely: account, enrollment tokens, live session, and
// the channel in every agency that had it. Recipients cannot delete — they
// leave via unshare.
func (s *Server) handleDeleteMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	s.mu.Lock()
	b := s.lmrBridges[id]
	if b == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	if !b.ownedBy(mine) && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "only the owning agency can delete this bridge")
		return
	}

	agencies := append([]string(nil), b.Agencies...)
	channel, chName := b.Channel, b.displayName()
	username, owner := b.Username, b.ownerAgency()
	delete(s.lmrBridges, id)
	for _, a := range agencies {
		delete(s.channels, channelKey(a, channel))
	}

	// Same teardown as the super_admin path: leaving any of these behind lets a
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
		delete(s.authAccounts, addr.String())
		s.mu.Unlock()
	}

	go s.notifyLMRChannel(agencies, channel, chName, false)
	s.lmrMarkersChanged() // ATAK marker follows the bridge (lmr_atak.go)

	if err := s.saveUsersUnlocked(); err != nil {
		log.Printf("LMR: failed to save users after deleting gateway account: %v", err)
	}
	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: agency %s deleted bridge %s (ch %d, user %q)", mine, id, channel, username)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleToggleMyLMRBridge pauses or resumes a bridge the caller's agency owns.
// handleGuestsMyLMRBridge flips whether guest accounts may TRANSMIT on a
// bridge (bridge_tx.go). Owner agency or super admin.
func (s *Server) handleGuestsMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	s.mu.Lock()
	b := s.lmrBridges[id]
	if b == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	if !b.ownedBy(mine) && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "only the owning agency can change this bridge")
		return
	}
	b.AllowGuests = !b.AllowGuests
	allow := b.AllowGuests
	s.mu.Unlock()

	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: bridge %s guests %s by agency %s", id, map[bool]string{true: "ALLOWED to transmit", false: "blocked from transmitting"}[allow], mine)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "allow_guests": allow})
}

// Paused = disabled: the channel is withdrawn from phones, the gateway is told
// (it inhibits its transmitter), and the server relays nothing to the gateway,
// so the radio cannot be keyed even if the bridge host has gone wrong. This is
// the remote kill switch for an unattended bridge.
func (s *Server) handleToggleMyLMRBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	s.mu.Lock()
	b := s.lmrBridges[id]
	if b == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "bridge not found")
		return
	}
	if !b.ownedBy(mine) && !isSuper {
		s.mu.Unlock()
		writeJSONError(w, http.StatusForbidden, "only the owning agency can pause this bridge")
		return
	}
	b.Enabled = !b.Enabled
	enabled := b.Enabled
	agencies := append([]string(nil), b.Agencies...)
	channel, name := b.Channel, b.displayName()
	s.refreshPausedGateways()
	s.mu.Unlock()

	go s.notifyLMRChannel(agencies, channel, name, enabled)
	s.lmrMarkersChanged() // ATAK marker follows the bridge (lmr_atak.go)
	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: bridge %s %s by agency %s", id, map[bool]string{true: "resumed", false: "PAUSED"}[enabled], mine)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "enabled": enabled})
}
