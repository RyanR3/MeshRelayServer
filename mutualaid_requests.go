// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Agency-to-agency mutual-aid negotiation.
//
// Mutual-aid links used to be a super_admin-only construct: the server operator
// decided who was patched to whom. This lets the agencies arrange it themselves.
//
//   1. An agency opts into the directory (Agency.Discoverable).
//   2. Another agency admin finds it by name and requests a patch.
//   3. If the RECEIVING agency already trusts the sender (AutoAcceptFrom), the
//      link goes live immediately; otherwise the request waits for approval.
//   4. Approval creates an ordinary MutualAidLink — the same object the
//      super_admin path has always produced, so relay, channel allocation and
//      teardown are unchanged.
//
// Either side may leave a live patch, and super_admin retains an override.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"os"
	"time"
)

// Request lifecycle states.
const (
	MARequestPending  = "pending"
	MARequestApproved = "approved"
	MARequestDeclined = "declined"
)

// MutualAidRequest is one agency asking another for a patch.
type MutualAidRequest struct {
	ID          string `json:"id"`
	FromAgency  string `json:"from_agency"`
	ToAgency    string `json:"to_agency"`
	ChannelName string `json:"channel_name"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	// ResolvedAt/ResolvedBy record the approve or decline.
	ResolvedAt string `json:"resolved_at,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	// LinkID is the MutualAidLink this became, once approved.
	LinkID string `json:"link_id,omitempty"`
	// AutoAccepted marks a request that skipped approval via a standing
	// agreement, so the audit trail distinguishes it from a human approval.
	AutoAccepted bool `json:"auto_accepted,omitempty"`
}

func (s *Server) mutualAidRequestsFile() string {
	if s.config != nil && s.config.MutualAidRequestsFile != "" {
		return s.config.MutualAidRequestsFile
	}
	return "mutual_aid_requests.json"
}

// loadMutualAidRequests reads requests from disk. A missing file is not an error.
func (s *Server) loadMutualAidRequests() {
	path := s.mutualAidRequestsFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Mutual aid: failed to read %s: %v", path, err)
		}
		return
	}
	var list []*MutualAidRequest
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("Mutual aid: failed to parse %s: %v", path, err)
		return
	}
	for _, r := range list {
		if r.ID != "" {
			s.mutualAidRequests[r.ID] = r
		}
	}
	log.Printf("Loaded %d mutual aid request(s)", len(s.mutualAidRequests))
}

// saveMutualAidRequests writes requests to disk. Caller must NOT hold s.mu.
func (s *Server) saveMutualAidRequests() error {
	s.mu.RLock()
	list := make([]*MutualAidRequest, 0, len(s.mutualAidRequests))
	for _, r := range s.mutualAidRequests {
		list = append(list, r)
	}
	s.mu.RUnlock()

	return writeJSONFileAtomic(s.mutualAidRequestsFile(), list)
}

// newMutualAidRequestID mints a random request id.
func newMutualAidRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pendingRequestBetween returns an existing pending request between two
// agencies in EITHER direction, or nil. Stops both sides from stacking up
// duplicate asks while one is already open.
// Caller must hold s.mu (read is enough).
func (s *Server) pendingRequestBetween(a, b string) *MutualAidRequest {
	for _, r := range s.mutualAidRequests {
		if r.Status != MARequestPending {
			continue
		}
		if (r.FromAgency == a && r.ToAgency == b) ||
			(r.FromAgency == b && r.ToAgency == a) {
			return r
		}
	}
	return nil
}

// linkBetween returns an ACTIVE mutual-aid link covering both agencies, or nil.
// Caller must hold s.mu (read is enough).
func (s *Server) linkBetween(a, b string) *MutualAidLink {
	for _, l := range s.mutualAidLinks {
		if !l.Active {
			continue
		}
		var hasA, hasB bool
		for _, id := range l.Agencies {
			if id == a {
				hasA = true
			}
			if id == b {
				hasB = true
			}
		}
		if hasA && hasB {
			return l
		}
	}
	return nil
}

// nowStamp is the timestamp format used across the request records.
func nowStamp() string { return time.Now().Format(time.RFC3339) }

// createMutualAidLink allocates a channel, stores the link, and announces it to
// every member agency. Shared by the super_admin create path and by request
// approval, so both produce an identical link.
//
// Caller must hold s.mu (write). Returns nil when the shared channel block is
// full. Announcement and persistence happen on the caller's side, outside the
// lock, via finishMutualAidLink.
func (s *Server) createMutualAidLinkLocked(agencies []string, channelName, createdBy string) *MutualAidLink {
	channel := s.pickMutualAidChannel()
	if channel == 0 {
		return nil
	}
	link := &MutualAidLink{
		ID:          newMutualAidRequestID(),
		Agencies:    agencies,
		Channel:     channel,
		ChannelName: channelName,
		CreatedAt:   nowStamp(),
		CreatedBy:   createdBy,
		Active:      true,
	}
	s.mutualAidLinks[link.ID] = link
	return link
}

// finishMutualAidLink announces a new link and refreshes each member's roster.
// Call AFTER releasing s.mu.
func (s *Server) finishMutualAidLink(link *MutualAidLink) {
	agencies := append([]string(nil), link.Agencies...)
	go s.notifyMutualAidChannel(agencies, link.Channel, link.ChannelName, true)
	// Linked agencies can now see each other, so push a fresh roster to each —
	// otherwise their merged user list stays stale until the next join/leave.
	go func() {
		for _, a := range agencies {
			s.broadcastRoster(a)
		}
	}()
}

// mutualAidRevision is a cheap fingerprint of everything this agency's Mutual
// Aid page displays: its pending requests and its active patches. The dashboard
// socket carries it every tick, and the page reloads only when the value
// changes — so a new request or a partner leaving shows up on its own, without
// polling the four endpoints on a timer.
//
// It is a count-and-id digest rather than a hash of the full records: the page
// cares that the SET changed, and a stable value must not depend on map
// iteration order. Summing ids is order-independent; adding the state makes an
// approve or decline of an existing request register as a change.
func (s *Server) mutualAidRevision(agencyID string) uint64 {
	if agencyID == "" {
		return 0 // super_admin: no per-agency view to keep fresh
	}
	var rev uint64
	s.mu.RLock()
	for _, r := range s.mutualAidRequests {
		if r.FromAgency != agencyID && r.ToAgency != agencyID {
			continue
		}
		rev += fnv64(r.ID) ^ fnv64(r.Status)
	}
	for _, l := range s.mutualAidLinks {
		if !l.Active {
			continue
		}
		for _, a := range l.Agencies {
			if a == agencyID {
				rev += fnv64(l.ID)
				break
			}
		}
	}
	s.mu.RUnlock()
	return rev
}

// agencyRevision fingerprints everything the agency dashboard displays: its
// mutual-aid state plus the Zello and LMR bridges it owns or has been granted.
// The pages reload only when this changes, so a bridge appearing, being shared,
// or being deleted shows up without the operator clicking away and back.
//
// Like mutualAidRevision it sums per-record hashes, so the value does not
// depend on map iteration order — a whole-set hash would appear to change on
// every tick and reload the page constantly.
func (s *Server) agencyRevision(agencyID string) uint64 {
	if agencyID == "" {
		return 0
	}
	rev := s.mutualAidRevision(agencyID)

	s.mu.RLock()
	for _, b := range s.zelloBridges {
		if !b.visibleTo(agencyID) {
			continue
		}
		// Fold in the things the card shows, so an enable, a status change or a
		// re-share registers — not just the bridge existing.
		h := fnv64(b.ID) ^ fnv64(b.Name)
		if b.Enabled {
			h ^= fnv64("enabled")
		}
		for _, m := range b.effectiveMappings() {
			h ^= fnv64(m.ZelloChannel) ^ uint64(m.MeshChannel)
			for _, ag := range m.ExtraAgencies {
				h ^= fnv64(ag)
			}
		}
		rev += h
	}
	for _, b := range s.lmrBridges {
		if !b.visibleTo(agencyID) {
			continue
		}
		h := fnv64(b.ID) ^ fnv64(b.displayName()) ^ uint64(b.Channel)
		if b.Enabled {
			h ^= fnv64("enabled")
		}
		if b.ShowInATAK {
			h ^= fnv64("atak")
		}
		if b.Lat != nil && b.Lon != nil {
			h ^= fnv64("loc") ^ math.Float64bits(*b.Lat) ^ (math.Float64bits(*b.Lon) * 31)
		}
		for _, ag := range b.Agencies {
			h ^= fnv64(ag)
		}
		rev += h
	}
	s.mu.RUnlock()
	return rev
}

// fnv64 is the FNV-1a hash of a short string.
func fnv64(v string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(v); i++ {
		h ^= uint64(v[i])
		h *= prime64
	}
	return h
}
