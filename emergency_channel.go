// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// The EMERGENCY channel.
//
// Every agency has one listen-only channel, EmergencyChannel (99), that every
// phone and console of the agency always hears. Nobody can talk on it — not
// units, not dispatch — except a unit with an active emergency alert: from
// the moment it raises the alert until it is cleared, ALL of that unit's
// audio is routed to the EMERGENCY channel instead of the channel it is on.
// The working channel stays clear for everyone else, and an emergency open
// mic never keys a bridged radio or Zello channel by accident.
//
// Routing happens on the server, so the caller's phone needs no change: an
// older app with its open mic on its working channel is moved over as well.
//
// Two emergencies at once queue: the first caller holds the channel, the
// second is recorded and replayed after the first is cleared (queue-and-
// replay, whatever the agency's floor-control setting).
//
// An agency admin can bridge the channel to one of the agency's LMR bridges
// (dashboard → Emergency Alerts): emergency audio is then also sent out over
// that radio. It is one-way: radio traffic does not come back onto the
// EMERGENCY channel.
//
// Wire:
//
//	EMERGENCY_CHANNEL (0x37, server→client): i32 channel | str name
//	  sent in every JOIN reply and when an admin renames it. Older apps
//	  ignore it.
//	Heartbeat flag 0x02 (client→server): "I know the EMERGENCY channel".
//	  Those clients get its audio channel-tagged (0x14); older clients get it
//	  untagged, so they still hear it (as if on their own channel).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	EmergencyChannel                 = 99
	DefaultEmergencyChannelName      = "EMERGENCY"
	PacketTypeEmergencyChannel  byte = 0x37

	// An emergency nobody talks on or re-raises for this long is cleared by
	// the server (a phone that went away for good). Audio keeps it alive.
	emergencyIdleExpire = 30 * time.Minute

	// Queue mode on the EMERGENCY channel records up to 10 minutes per
	// caller (open mics run long), not the 60 s of an ordinary channel.
	emergencyDeferredMaxFrames = 30000
)

// emergencyCall is one unit's active emergency.
type emergencyCall struct {
	User           string
	WorkingChannel int // the channel the unit was on when it raised it
	Since          time.Time
	LastActive     time.Time
	Fix            *emergencyFix // position sent with the alert (nil: none)
}

func (a *Agency) emergencyChannelName() string {
	if a != nil && strings.TrimSpace(a.EmergencyChannelName) != "" {
		return a.EmergencyChannelName
	}
	return DefaultEmergencyChannelName
}

func isEmergencyKey(key string) bool {
	return channelNumFromKey(key) == EmergencyChannel && strings.Contains(key, ":")
}

// buildEmergencyChannel builds the 0x37 packet. Caller holds s.mu (read).
func buildEmergencyChannel(a *Agency) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeEmergencyChannel)
	binary.Write(buf, binary.BigEndian, int32(EmergencyChannel))
	putStr(buf, a.emergencyChannelName())
	return buf.Bytes()
}

// agencyPhoneAddrs: every phone of the agency (not consoles). Caller holds s.mu.
func (s *Server) agencyPhoneAddrs(agencyID string) []*net.UDPAddr {
	var addrs []*net.UDPAddr
	for _, c := range s.clients {
		if c.AgencyID == agencyID && c.Addr != nil && !isConsoleAddr(c.Addr) {
			addrs = append(addrs, c.Addr)
		}
	}
	return addrs
}

func (s *Server) sendToAgencyPhones(agencyID string, pkt []byte) int {
	s.mu.RLock()
	addrs := s.agencyPhoneAddrs(agencyID)
	s.mu.RUnlock()
	for _, addr := range addrs {
		if s.testFloorSink != nil {
			s.testFloorSink(addr, pkt)
			continue
		}
		s.sendPacket(pkt, addr)
	}
	return len(addrs)
}

// broadcastEmergencyChannel tells every phone of the agency the channel's name.
func (s *Server) broadcastEmergencyChannel(agencyID string) {
	s.mu.RLock()
	a := s.agencies[agencyID]
	s.mu.RUnlock()
	if a == nil {
		return
	}
	s.mu.RLock()
	pkt := buildEmergencyChannel(a)
	s.mu.RUnlock()
	n := s.sendToAgencyPhones(agencyID, pkt)
	log.Printf("Emergency channel for agency %s sent to %d client(s)", agencyID, n)
}

// ---- emergency state ----------------------------------------------------------

// emergencyFor returns a copy of a unit's active emergency, or nil.
func (s *Server) emergencyFor(agencyID, user string) *emergencyCall {
	s.emergMu.Lock()
	defer s.emergMu.Unlock()
	if e := s.emergCalls[agencyID][user]; e != nil {
		cp := *e
		return &cp
	}
	return nil
}

func (s *Server) emergencyActiveFor(agencyID, user string) bool {
	return s.emergencyFor(agencyID, user) != nil
}

func (s *Server) touchEmergency(agencyID, user string) {
	s.emergMu.Lock()
	if e := s.emergCalls[agencyID][user]; e != nil {
		e.LastActive = time.Now()
	}
	s.emergMu.Unlock()
}

// activeEmergencyCalls lists an agency's active emergencies, oldest first.
func (s *Server) activeEmergencyCalls(agencyID string) []emergencyCall {
	s.emergMu.Lock()
	defer s.emergMu.Unlock()
	var out []emergencyCall
	for _, e := range s.emergCalls[agencyID] {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// ensureEmergencyChannel makes sure the agency has its channel-99 record.
func (s *Server) ensureEmergencyChannel(agencyID string) {
	key := channelKey(agencyID, EmergencyChannel)
	s.mu.Lock()
	if _, ok := s.channels[key]; !ok {
		s.channels[key] = &Channel{ID: EmergencyChannel, Name: DefaultEmergencyChannelName, Clients: make(map[string]*Client)}
	}
	s.mu.Unlock()
}

// raiseEmergency handles an alert set (first raise or a re-send).
func (s *Server) raiseEmergency(agencyID, user string, workingChannel int) {
	s.raiseEmergencyAt(agencyID, user, workingChannel, nil)
}

// raiseEmergencyAt is raiseEmergency with the position the alert carried
// (nil: none). A re-send that moves the unit (or gives the first position)
// updates the consoles and the server's TAK alert.
func (s *Server) raiseEmergencyAt(agencyID, user string, workingChannel int, fix *emergencyFix) {
	now := time.Now()
	s.emergMu.Lock()
	if s.emergCalls == nil {
		s.emergCalls = map[string]map[string]*emergencyCall{}
	}
	if s.emergCalls[agencyID] == nil {
		s.emergCalls[agencyID] = map[string]*emergencyCall{}
	}
	e := s.emergCalls[agencyID][user]
	first := e == nil
	if first {
		e = &emergencyCall{User: user, WorkingChannel: workingChannel, Since: now}
		s.emergCalls[agencyID][user] = e
	}
	e.LastActive = now
	moved, movedM := false, 0.0
	if fix != nil {
		moved = !first && fixMoved(e.Fix, fix)
		movedM = fixDistanceM(e.Fix, fix)
		cp := *fix
		e.Fix = &cp
	}
	working := e.WorkingChannel
	s.emergMu.Unlock()

	s.ensureEmergencyChannel(agencyID)
	// The alert (with the caller's last position) goes to the whole agency:
	// everyone hears the EMERGENCY channel, not only the caller's channel.
	// The caller's own copy is the confirmation its phone waits for.
	s.sendToAgencyPhones(agencyID, s.emergencyAlertPacket(channelKey(agencyID, working), working, user, true))
	if first {
		s.notifyDispatchEmergency(agencyID, working, user, true)
		if s.activityLog != nil {
			s.activityLog.Add("emergency", user, "Emergency raised", agencyID)
		}
		log.Printf("EMERGENCY raised by %q [%s] (was on channel %d) — audio now on the EMERGENCY channel", user, agencyID, working)
	} else if moved {
		s.emergencyPositionChanged(agencyID, working, user, movedM)
	}
	// Give the caller the EMERGENCY channel (or a place in its queue).
	if s.floor != nil {
		ekey := channelKey(agencyID, EmergencyChannel)
		if !s.floor.HasFloor(ekey, user) && !s.floor.IsRecording(ekey, user) {
			s.floor.Request(ekey, &FloorRequest{Channel: EmergencyChannel, User: user, Priority: PriorityEmergency}, PriorityEmergency)
		}
	}
}

// clearEmergency ends a unit's emergency. Returns false if it had none.
func (s *Server) clearEmergency(agencyID, user, by string) bool {
	s.emergMu.Lock()
	e := s.emergCalls[agencyID][user]
	if e != nil {
		delete(s.emergCalls[agencyID], user)
	}
	s.emergMu.Unlock()
	if e == nil {
		return false
	}
	if s.floor != nil {
		// Frees the channel; a queued caller's recording plays next.
		s.floor.Release(channelKey(agencyID, EmergencyChannel), EmergencyChannel, user)
	}
	s.sendToAgencyPhones(agencyID, buildEmergencyAlert(e.WorkingChannel, user, false))
	s.notifyDispatchEmergency(agencyID, e.WorkingChannel, user, false)
	who := "by " + by
	if by == user {
		who = "by the unit"
	}
	if s.activityLog != nil {
		s.activityLog.Add("emergency", user, "Emergency cleared "+who, agencyID)
	}
	log.Printf("Emergency of %q [%s] cleared %q", user, agencyID, who)
	return true
}

// clearEmergenciesOnChannel: a supervisor's cancel from a phone clears the
// emergencies raised on their channel (the old one-emergency-per-channel rule).
func (s *Server) clearEmergenciesOnChannel(agencyID string, channel int, by string) int {
	n := 0
	for _, e := range s.activeEmergencyCalls(agencyID) {
		if e.WorkingChannel == channel && s.clearEmergency(agencyID, e.User, by) {
			n++
		}
	}
	return n
}

// emergencyTick clears emergencies that have gone quiet for good.
func (s *Server) emergencyTick() {
	now := time.Now()
	type stale struct{ agency, user string }
	var drop []stale
	s.emergMu.Lock()
	for agency, m := range s.emergCalls {
		for user, e := range m {
			if now.Sub(e.LastActive) > emergencyIdleExpire {
				drop = append(drop, stale{agency, user})
			}
		}
	}
	s.emergMu.Unlock()
	for _, d := range drop {
		s.clearEmergency(d.agency, d.user, "the server (no activity for 30 minutes)")
	}
}

// ---- audio --------------------------------------------------------------------

// routeEmergencyAudio sends a unit's audio to the EMERGENCY channel while its
// emergency is active. Returns false (audio handled normally) otherwise.
func (s *Server) routeEmergencyAudio(client *Client, packet *AudioPacket, audioData []byte) bool {
	if !s.emergencyActiveFor(client.AgencyID, packet.Username) {
		return false
	}
	s.touchEmergency(client.AgencyID, packet.Username)
	ekey := channelKey(client.AgencyID, EmergencyChannel)
	if s.floor != nil && !s.floor.HasFloor(ekey, packet.Username) {
		if !s.floor.IsRecording(ekey, packet.Username) {
			// No floor request seen yet (e.g. after a server restart): ask now.
			s.floor.Request(ekey, &FloorRequest{Channel: EmergencyChannel, User: packet.Username, Priority: PriorityEmergency}, PriorityEmergency)
		}
		if !s.floor.HasFloor(ekey, packet.Username) {
			s.floor.RecordDeferred(ekey, packet.Username, packet, audioData)
			return true
		}
	}
	if s.floor != nil {
		s.floor.NoteAudio(ekey, packet.Username)
	}
	s.relayEmergencyAudio(client.AgencyID, client.Username, client.Addr, packet, audioData)
	return true
}

// relayEmergencyAudio fans one frame out on the EMERGENCY channel (live or a
// queued replay) and to the agency's emergency LMR bridge.
func (s *Server) relayEmergencyAudio(agencyID, user string, addr *net.UDPAddr, packet *AudioPacket, audioData []byte) {
	s.ensureEmergencyChannel(agencyID)
	src := &Client{Username: user, AgencyID: agencyID, Channel: EmergencyChannel, Addr: addr}
	s.relayAudioData(src, packet, audioData)
	s.forwardEmergencyToLMR(agencyID, audioData)
}

// forwardEmergencyToLMR sends EMERGENCY channel audio out over the LMR bridge
// the agency admin picked (if any): straight to the bridge's gateway, which
// keys its radio. One-way.
func (s *Server) forwardEmergencyToLMR(agencyID string, audioData []byte) {
	s.mu.RLock()
	var addr *net.UDPAddr
	var gw *Client
	if a := s.agencies[agencyID]; a != nil && a.EmergencyLMRBridge != "" {
		if b := s.lmrBridges[a.EmergencyLMRBridge]; b != nil && b.Enabled && b.servesAgency(agencyID) {
			// Only the bridge's own gateway account, never a client that
			// merely holds its callsign.
			if c := s.clients[userKey(b.ownerAgency(), b.Username)]; c != nil && c.Addr != nil && s.ownLMRBridge(c) != nil {
				gw, addr = c, c.Addr
			}
		}
	}
	paused := gw != nil && s.gatewayPaused(gw)
	s.mu.RUnlock()
	if addr == nil || paused {
		return
	}
	// Segmented: the radio is never keyed without a break (bridge_tx.go).
	if !s.emergencyLMRAllowed(agencyID, time.Now()) {
		return
	}
	s.sendPacket(audioData, addr)
}

// emergencyListener: should this client hear the EMERGENCY channel? Every
// phone of the agency except radio gateways (only the bridge picked for it
// gets it, via forwardEmergencyToLMR) and consoles (fanOutToDispatch).
// Caller holds s.mu (read).
func (s *Server) emergencyListener(mc *Client) bool {
	if mc.Addr == nil || isConsoleAddr(mc.Addr) {
		return false
	}
	return !s.isGatewayLogin(mc)
}

// ---- dashboard API --------------------------------------------------------------
//
// GET  /api/my-agency/emergency        → [{agency_id, agency_name, channel_name, lmr_bridge, bridges[], active[]}]
// PUT  /api/my-agency/emergency        {agency_id (super admin), channel_name, lmr_bridge}
// POST /api/my-agency/emergency/clear  {agency_id (super admin), user}

type emergencyBridgeJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Channel int    `json:"channel"`
	Enabled bool   `json:"enabled"`
	Shared  bool   `json:"shared"` // owned by another agency, shared with this one
}

type emergencyActiveJSON struct {
	User        string `json:"user"`
	Channel     int    `json:"channel"`
	ChannelName string `json:"channel_name"`
	Since       string `json:"since"`
}

type agencyEmergencyJSON struct {
	AgencyID    string                `json:"agency_id"`
	AgencyName  string                `json:"agency_name"`
	Channel     int                   `json:"channel"`
	ChannelName string                `json:"channel_name"`
	LMRBridge   string                `json:"lmr_bridge"`
	Bridges     []emergencyBridgeJSON `json:"bridges"`
	Active      []emergencyActiveJSON `json:"active"`
}

func (s *Server) agencyEmergencyOf(agencyID string) agencyEmergencyJSON {
	s.mu.RLock()
	a := s.agencies[agencyID]
	out := agencyEmergencyJSON{AgencyID: agencyID, Channel: EmergencyChannel, Bridges: []emergencyBridgeJSON{}, Active: []emergencyActiveJSON{}}
	if a != nil {
		out.AgencyName = a.Name
		out.ChannelName = a.emergencyChannelName()
		out.LMRBridge = a.EmergencyLMRBridge
	}
	for _, b := range s.lmrBridges {
		if b.visibleTo(agencyID) {
			out.Bridges = append(out.Bridges, emergencyBridgeJSON{
				ID: b.ID, Name: b.displayName(), Channel: b.Channel, Enabled: b.Enabled, Shared: !b.ownedBy(agencyID),
			})
		}
	}
	s.mu.RUnlock()
	sort.Slice(out.Bridges, func(i, j int) bool { return out.Bridges[i].Channel < out.Bridges[j].Channel })
	for _, e := range s.activeEmergencyCalls(agencyID) {
		s.mu.RLock()
		name := a.talkgroupName(e.WorkingChannel)
		s.mu.RUnlock()
		out.Active = append(out.Active, emergencyActiveJSON{
			User: e.User, Channel: e.WorkingChannel, ChannelName: name, Since: e.Since.Format(time.RFC3339),
		})
	}
	return out
}

func (s *Server) handleGetAgencyEmergency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var ids []string
	s.mu.RLock()
	if isSuper {
		for id := range s.agencies {
			if !strings.HasPrefix(id, "event-") {
				ids = append(ids, id)
			}
		}
	} else if s.agencies[agencyID] != nil {
		ids = []string{agencyID}
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(s.agencies[ids[i]].Name) < strings.ToLower(s.agencies[ids[j]].Name)
	})
	s.mu.RUnlock()
	out := make([]agencyEmergencyJSON, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.agencyEmergencyOf(id))
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleUpdateAgencyEmergency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID    string `json:"agency_id"`
		ChannelName string `json:"channel_name"`
		LMRBridge   string `json:"lmr_bridge"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	name := strings.TrimSpace(req.ChannelName)
	if err := validateChannelName(EmergencyChannel, name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.EqualFold(name, DefaultEmergencyChannelName) {
		name = "" // stored as "not renamed"
	}

	s.mu.Lock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if req.LMRBridge != "" {
		b := s.lmrBridges[req.LMRBridge]
		if b == nil || !b.visibleTo(agencyID) {
			s.mu.Unlock()
			writeJSONError(w, http.StatusBadRequest, "that LMR bridge is not available to this agency")
			return
		}
	}
	renamed := a.EmergencyChannelName != name
	a.EmergencyChannelName = name
	a.EmergencyLMRBridge = req.LMRBridge
	s.mu.Unlock()

	s.mu.RLock()
	err := s.saveAgencies()
	s.mu.RUnlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Emergency channel for agency %s: name %q, LMR bridge %q", agencyID, name, req.LMRBridge)
	if renamed {
		s.broadcastEmergencyChannel(agencyID)
	}
	json.NewEncoder(w).Encode(s.agencyEmergencyOf(agencyID))
}

func (s *Server) handleClearAgencyEmergency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID string `json:"agency_id"`
		User     string `json:"user"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.User == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	by := "an admin"
	if sess := s.getSession(r); sess != nil && sess.Username != "" {
		by = sess.Username + " (dashboard)"
	}
	if !s.clearEmergency(agencyID, req.User, by) {
		writeJSONError(w, http.StatusNotFound, "no active emergency for that unit")
		return
	}
	json.NewEncoder(w).Encode(s.agencyEmergencyOf(agencyID))
}
