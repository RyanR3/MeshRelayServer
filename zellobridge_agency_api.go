package main

// Agency-facing Zello bridges.
//
// A Zello bridge belongs to ONE agency: the one that created it and whose Zello
// Work account backs it. It is private by default — no other agency can see it
// exists. The owner may grant another (discoverable) agency access, which makes
// the bridged channel appear for them exactly as it does for the owner.
//
// Granting is a direct act by the owner, not a negotiation: it is their bridge
// and their Zello account, so there is nothing for the recipient to approve.
// The recipient may leave at any time.
//
// A recipient gets USE and nothing else. They never see the Zello network,
// username or credentials, cannot edit mappings, cannot re-share, and cannot
// delete. That asymmetry is the point of the feature.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/mux"
)

// zelloAgencyView is what an agency admin sees. For an owner it carries the
// configuration; for a recipient it deliberately omits everything about the
// Zello side.
type zelloAgencyView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	LastError string `json:"last_error,omitempty"`
	// Relationship: "owner" or "shared".
	Access string `json:"access"`

	// Owner-only fields. Left empty for a recipient.
	Network  string         `json:"network,omitempty"`
	Username string         `json:"username,omitempty"`
	HasAuth  bool           `json:"has_auth,omitempty"`
	Mappings []ZelloMapping `json:"mappings,omitempty"`
	// SharedWith lists the agencies this bridge has been granted to, with names.
	SharedWith []map[string]string `json:"shared_with,omitempty"`

	// Recipient-facing: which channels they actually get, and from whom.
	Channels  []map[string]interface{} `json:"channels,omitempty"`
	OwnerName string                   `json:"owner_name,omitempty"`
}

// handleGetMyZelloBridges lists the bridges this agency owns or has been
// granted. Everything else on the server stays invisible.
func (s *Server) handleGetMyZelloBridges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mine, isSuper := s.callerAgency(r)

	s.mu.RLock()
	name := func(id string) string {
		if a, ok := s.agencies[id]; ok && a.Name != "" {
			return a.Name
		}
		return id
	}
	out := make([]zelloAgencyView, 0)
	for _, b := range s.zelloBridges {
		owner := b.ownedBy(mine) || (isSuper && b.OwnerAgencyID == "")
		shared := b.sharedWith(mine)
		if !owner && !shared {
			continue
		}
		b.statusMu.Lock()
		status, lastErr := b.Status, b.LastError
		b.statusMu.Unlock()
		if status == "" {
			status = "disconnected"
		}

		v := zelloAgencyView{
			ID: b.ID, Name: b.Name, Enabled: b.Enabled,
			Status: status, LastError: lastErr,
		}
		if owner {
			v.Access = "owner"
			v.Network, v.Username = b.Network, b.Username
			v.HasAuth = b.AuthToken != "" || b.Password != ""
			v.Mappings = b.effectiveMappings()
			// Who this bridge has been granted to (excluding the owner).
			seen := map[string]bool{}
			v.SharedWith = make([]map[string]string, 0)
			for _, m := range b.effectiveMappings() {
				for _, ag := range m.ExtraAgencies {
					if ag == "" || ag == b.OwnerAgencyID || seen[ag] {
						continue
					}
					seen[ag] = true
					v.SharedWith = append(v.SharedWith,
						map[string]string{"id": ag, "name": name(ag)})
				}
			}
		} else {
			// Recipient: channels only. No Zello network, username or mappings.
			v.Access = "shared"
			v.OwnerName = name(b.OwnerAgencyID)
			v.Channels = make([]map[string]interface{}, 0)
			for _, m := range b.effectiveMappings() {
				if !m.servesAgency(mine) {
					continue
				}
				v.Channels = append(v.Channels, map[string]interface{}{
					"channel": m.MeshChannel,
					"name":    m.ZelloChannel,
				})
			}
		}
		out = append(out, v)
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(out)
}

// handleShareZelloBridge grants another agency USE of a bridge the caller owns.
// Access is expressed as ExtraAgencies on every mapping, which the audio relay
// and channel announcement already honour.
func (s *Server) handleShareZelloBridge(w http.ResponseWriter, r *http.Request) {
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
	b, enabled, code, msg := func() (*ZelloBridge, bool, int, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := s.zelloBridges[id]
		if b == nil {
			return nil, false, http.StatusNotFound, "bridge not found"
		}
		if !b.ownedBy(mine) && !isSuper {
			return nil, false, http.StatusForbidden, "only the owning agency can share this bridge"
		}
		if req.AgencyID == b.OwnerAgencyID {
			return nil, false, http.StatusBadRequest, "that agency already owns this bridge"
		}
		target := s.agencies[req.AgencyID]
		if target == nil || !target.Discoverable {
			// Refuse rather than confirm a hidden agency exists.
			return nil, false, http.StatusNotFound, "agency not found"
		}
		if !isSuper {
			if me := s.agencies[mine]; me == nil || !me.Discoverable {
				return nil, false, http.StatusForbidden, "make your agency discoverable before sharing a bridge"
			}
		}
		// A bridge with no channels mapped yet has nothing to grant: access
		// is expressed per-mapping, so there is nowhere to record it. Say
		// that plainly rather than reporting "already has access", which is
		// what an empty loop used to produce.
		if len(b.Mappings) == 0 {
			return nil, false, http.StatusConflict, "map a Zello channel to this bridge before sharing it"
		}
		added := false
		for i := range b.Mappings {
			if b.Mappings[i].servesAgency(req.AgencyID) {
				continue
			}
			b.Mappings[i].ExtraAgencies = append(b.Mappings[i].ExtraAgencies, req.AgencyID)
			added = true
		}
		if !added {
			return nil, false, http.StatusConflict, "that agency already has access"
		}
		return b, b.Enabled, 0, ""
	}()
	if code != 0 {
		writeJSONError(w, code, msg)
		return
	}

	// Push the channels so they appear for the new agency immediately.
	if enabled {
		go s.notifyZelloBridgeChannels(b, true)
	}
	if err := s.saveZelloBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Zello: bridge %s shared with agency %s by %s", id, req.AgencyID, mine)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleUnshareZelloBridge revokes an agency's access. The owner may remove
// anyone; a recipient may remove only itself (leaving).
func (s *Server) handleUnshareZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	var req struct {
		AgencyID string `json:"agency_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	b := s.zelloBridges[id]
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

	// Snapshot the channels being revoked so they can be withdrawn after the
	// mapping is edited — afterwards the bridge no longer knows about them.
	type chan1 struct {
		ch   int
		name string
	}
	var revoked []chan1
	removed := false
	for i := range b.Mappings {
		// Track per-mapping: a single `removed` flag would mark every mapping
		// after the first match as revoked, withdrawing channels the agency
		// never had.
		hit := false
		kept := b.Mappings[i].ExtraAgencies[:0]
		for _, ag := range b.Mappings[i].ExtraAgencies {
			if ag == target {
				hit = true
				continue
			}
			kept = append(kept, ag)
		}
		b.Mappings[i].ExtraAgencies = kept
		if hit {
			removed = true
			revoked = append(revoked, chan1{b.Mappings[i].MeshChannel, b.Mappings[i].ZelloChannel})
		}
	}
	if !removed {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "that agency does not have access")
		return
	}
	s.mu.Unlock()

	// Withdraw the channel from the agency that lost access, so it disappears
	// from their clients rather than lingering as a dead row.
	go func() {
		for _, c := range revoked {
			s.announceChannelRemoval(target, c.ch, c.name)
		}
		s.broadcastRoster(target)
	}()

	if err := s.saveZelloBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to save: %s", err.Error()))
		return
	}
	log.Printf("Zello: bridge %s access removed for agency %s (by %s)", id, target, mine)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleCreateMyZelloBridge lets an agency admin create a bridge owned by their
// own agency. Same shape as the super_admin creator, except the agency is the
// caller's rather than a parameter, and OwnerAgencyID is stamped so the bridge
// is private to them from the outset.
func (s *Server) handleCreateMyZelloBridge(w http.ResponseWriter, r *http.Request) {
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
		Name      string `json:"name"`
		Network   string `json:"network"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		AuthToken string `json:"auth_token"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Network == "" {
		writeJSONError(w, http.StatusBadRequest, "name and network are required")
		return
	}

	s.mu.Lock()
	if _, ok := s.agencies[mine]; !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	b := &ZelloBridge{
		ID:            newMutualAidRequestID(),
		Name:          req.Name,
		Network:       req.Network,
		Username:      req.Username,
		Password:      req.Password,
		AuthToken:     req.AuthToken,
		AgencyID:      mine,
		OwnerAgencyID: mine,
		Enabled:       req.Enabled,
		CreatedAt:     nowStamp(),
		CreatedBy:     createdBy,
	}
	s.zelloBridges[b.ID] = b
	enabled := b.Enabled
	s.mu.Unlock()

	if err := s.saveZelloBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	// "Connect immediately" — same behaviour as the super_admin path, so the
	// operator can pick channels on the card as soon as it is up.
	if enabled {
		go s.startZelloBridge(b)
	}
	log.Printf("Zello: agency %s created bridge %s (%s)", mine, b.ID, b.Name)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": b.ID})
}

// handleDeleteMyZelloBridge removes a bridge the caller's agency OWNS.
// Recipients cannot delete — they leave via unshare.
func (s *Server) handleDeleteMyZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	s.mu.Lock()
	b := s.zelloBridges[id]
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
	// Snapshot what to withdraw before the bridge goes away.
	type withdrawal struct {
		agency string
		ch     int
		name   string
	}
	var toWithdraw []withdrawal
	for _, m := range b.effectiveMappings() {
		for _, ag := range m.targetAgencies() {
			toWithdraw = append(toWithdraw, withdrawal{ag, m.MeshChannel, m.ZelloChannel})
		}
	}
	delete(s.zelloBridges, id)
	s.mu.Unlock()

	s.stopZelloBridge(b)
	go func() {
		for _, wd := range toWithdraw {
			s.announceChannelRemoval(wd.agency, wd.ch, wd.name)
		}
	}()

	if err := s.saveZelloBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Zello: agency %s deleted bridge %s", mine, id)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
