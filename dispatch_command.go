// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Command console support: what a dispatch console needs beyond audio.
//
//   - Unit locations. Apps send a LOCATION_REPORT (0x2E) when signed in to the
//     server; the last fix is kept in memory on the Client (never written to
//     disk) and shown only to consoles of that unit's agency. Positions the
//     apps exchange with each other stay end-to-end encrypted; this report is
//     the separate, server-visible one ("share my location with dispatch").
//   - A channel directory per agency: talkgroups plus the mutual-aid, LMR,
//     Zello and ad-hoc channels that agency can hear, with unit counts.
//   - Multi-channel monitoring, and (for a super admin) choosing which agency's
//     channel to transmit on.
//   - Activity: "X is talking on agency/channel" for every channel the console's
//     agency owns, monitored or not — it drives the radio log and the map.
//   - Emergencies reach every console of the agency, with the unit's position.
//
// JSON text frames to the console (in addition to the 2 s "update"):
//   {"type":"talk","agency","channel","user"}            ~2/s while someone talks
//   {"type":"emergency","agency","channel","user","active","lat","lon"}

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PacketTypeLocationReport: client -> server
//
//	[f64 lat][f64 lon][f32 accuracy m][f32 speed m/s][f32 heading deg][i8 battery %, -1 unknown]
const PacketTypeLocationReport byte = 0x2E

type LocationReport struct {
	Lat, Lon                 float64
	Accuracy, Speed, Heading float32
	Battery                  int8
}

func ParseLocationReport(data []byte) (*LocationReport, error) {
	r := bytes.NewReader(data)
	var p LocationReport
	if err := binary.Read(r, binary.BigEndian, &p.Lat); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.BigEndian, &p.Lon); err != nil {
		return nil, err
	}
	// Optional tail (older/minimal senders may stop after lat/lon).
	p.Battery = -1
	binary.Read(r, binary.BigEndian, &p.Accuracy)
	binary.Read(r, binary.BigEndian, &p.Speed)
	binary.Read(r, binary.BigEndian, &p.Heading)
	binary.Read(r, binary.BigEndian, &p.Battery)
	return &p, nil
}

func validFix(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 &&
		!(lat == 0 && lon == 0)
}

func (s *Server) handleLocationReport(p *LocationReport, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	if !validFix(p.Lat, p.Lon) {
		return
	}
	s.mu.Lock()
	c, _ := s.findClientByAddr(addr)
	if c != nil {
		c.Loc = &UnitLocation{Lat: p.Lat, Lon: p.Lon, Accuracy: p.Accuracy, Speed: p.Speed,
			Heading: p.Heading, Battery: p.Battery, At: time.Now()}
		c.LastHeartbeat = time.Now()
	}
	s.mu.Unlock()
}

// UnitLocation is a unit's last reported position (memory only).
type UnitLocation struct {
	Lat, Lon                 float64
	Accuracy, Speed, Heading float32
	Battery                  int8
	At                       time.Time
}

func (l *UnitLocation) json() map[string]interface{} {
	m := map[string]interface{}{
		"lat": l.Lat, "lon": l.Lon, "at": l.At.Format(time.RFC3339),
	}
	if l.Accuracy > 0 {
		m["acc"] = l.Accuracy
	}
	if l.Speed > 0 {
		m["speed"] = l.Speed
		m["heading"] = l.Heading
	}
	if l.Battery >= 0 {
		m["battery"] = l.Battery
	}
	return m
}

// --- channel directory ------------------------------------------------------

type dispatchChannel struct {
	Agency     string `json:"agency"`
	AgencyName string `json:"agency_name"`
	Channel    int    `json:"channel"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // talkgroup | mutual_aid | lmr | zello | adhoc
	Units      int    `json:"units"`
}

// dispatchDirectory lists the channels a console can see: every agency for a
// super admin (agencyFilter ""), else just the console's agency.
func (s *Server) dispatchDirectory(agencyFilter string) ([]map[string]string, []dispatchChannel) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var agencyIDs []string
	for id := range s.agencies {
		if agencyFilter == "" || id == agencyFilter {
			agencyIDs = append(agencyIDs, id)
		}
	}
	sort.Slice(agencyIDs, func(i, j int) bool {
		return strings.ToLower(s.agencies[agencyIDs[i]].Name) < strings.ToLower(s.agencies[agencyIDs[j]].Name)
	})

	units := func(agency string, ch int) int {
		c := s.channels[channelKey(agency, ch)]
		if c == nil {
			return 0
		}
		c.mu.RLock()
		defer c.mu.RUnlock()
		return len(c.Clients)
	}

	agencies := make([]map[string]string, 0, len(agencyIDs))
	var out []dispatchChannel
	for _, id := range agencyIDs {
		a := s.agencies[id]
		agencies = append(agencies, map[string]string{"id": id, "name": a.Name})
		n := a.talkgroupCount()
		for ch := 0; ch < n; ch++ {
			out = append(out, dispatchChannel{id, a.Name, ch, a.talkgroupName(ch), "talkgroup", units(id, ch)})
		}
		// Listen-only, always heard (emergency_channel.go).
		out = append(out, dispatchChannel{id, a.Name, EmergencyChannel, a.emergencyChannelName(), "emergency", 0})
		seen := map[int]bool{EmergencyChannel: true}
		add := func(ch int, name, kind string) {
			if seen[ch] {
				return
			}
			seen[ch] = true
			out = append(out, dispatchChannel{id, a.Name, ch, name, kind, units(id, ch)})
		}
		for _, l := range s.mutualAidLinks {
			if l.Active && containsString(l.Agencies, id) {
				add(l.Channel, nonEmpty(l.ChannelName, "Mutual aid"), "mutual_aid")
			}
		}
		for _, b := range s.lmrBridges {
			if b.Enabled && containsString(b.Agencies, id) {
				add(b.Channel, nonEmpty(b.ChannelName, nonEmpty(b.Name, "LMR bridge")), "lmr")
			}
		}
		for _, b := range s.zelloBridges {
			for _, m := range b.effectiveMappings() {
				if containsString(m.targetAgencies(), id) {
					add(m.MeshChannel, "Zello: "+m.ZelloChannel, "zello")
				}
			}
		}
		if s.groupMgr != nil {
			s.groupMgr.mu.Lock()
			for key, g := range s.groupMgr.adhoc {
				if strings.HasPrefix(key, id+":") {
					add(g.channel, nonEmpty(g.name, "Group"), "adhoc")
				}
			}
			s.groupMgr.mu.Unlock()
		}
	}
	return agencies, out
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// --- console tuning: transmit target + monitored channels -------------------

// consoleHears reports whether a console hears agency/channel: its transmit
// channel, or one it monitors.
func consoleHears(c *Client, agencyID string, channel int) bool {
	if c.AgencyID != "" && c.AgencyID != agencyID {
		return false
	}
	if channel == EmergencyChannel {
		return true // every console of the agency hears the EMERGENCY channel
	}
	if c.Channel == channel && (c.DispatchTxAgency == "" || c.DispatchTxAgency == agencyID) {
		return true
	}
	return c.DispatchMonitor[channelKey(agencyID, channel)]
}

// consoleTxAgencies: the agencies a console's transmission goes to.
func (s *Server) consoleTxAgencies(c *Client) []string {
	if c.AgencyID != "" {
		return []string{c.AgencyID}
	}
	if c.DispatchTxAgency != "" {
		return []string{c.DispatchTxAgency}
	}
	var all []string
	for id := range s.agencies {
		all = append(all, id)
	}
	return all
}

// --- activity + emergencies to consoles -------------------------------------

var (
	talkNoteMu sync.Mutex
	talkNoteAt = map[string]time.Time{}
)

// noteDispatchTalk tells consoles of agencyID that user is talking on channel,
// at most twice a second per talker (the console ends the talk after ~1.2 s of
// silence).
func (s *Server) noteDispatchTalk(agencyID string, channel int, user string) {
	k := agencyID + "\x00" + strconv.Itoa(channel) + "\x00" + user
	now := time.Now()
	talkNoteMu.Lock()
	if now.Sub(talkNoteAt[k]) < 500*time.Millisecond {
		talkNoteMu.Unlock()
		return
	}
	talkNoteAt[k] = now
	if len(talkNoteAt) > 5000 { // forget old talkers
		for kk, t := range talkNoteAt {
			if now.Sub(t) > time.Minute {
				delete(talkNoteAt, kk)
			}
		}
	}
	talkNoteMu.Unlock()
	s.sendToConsoles(agencyID, map[string]interface{}{
		"type": "talk", "agency": agencyID, "channel": channel, "user": user,
	})
}

// notifyDispatchEmergency puts an emergency (set or cleared) on every console
// of the agency, with the unit's last position when known.
func (s *Server) notifyDispatchEmergency(agencyID string, channel int, user string, active bool) {
	msg := map[string]interface{}{
		"type": "emergency", "agency": agencyID, "channel": channel, "user": user, "active": active,
	}
	s.mu.RLock()
	if c := s.clients[userKey(agencyID, user)]; c != nil && c.Loc != nil {
		msg["lat"], msg["lon"] = c.Loc.Lat, c.Loc.Lon
	}
	s.mu.RUnlock()
	log.Printf("Dispatch: emergency %v by %s [%s] ch %d", active, user, agencyID, channel)
	s.sendToConsoles(agencyID, msg)
	s.takEmergency(agencyID, user, active) // and into the agency's TAK server
}

// Active emergencies, so a console that opens later still sees them.
var (
	activeEmergMu sync.Mutex
	activeEmerg   = map[string]map[string]interface{}{} // agency|user -> last "emergency" message
)

func rememberEmergency(msg map[string]interface{}) {
	agency, _ := msg["agency"].(string)
	user, _ := msg["user"].(string)
	k := agency + "|" + user
	activeEmergMu.Lock()
	if a, _ := msg["active"].(bool); a {
		activeEmerg[k] = msg
	} else {
		delete(activeEmerg, k)
	}
	activeEmergMu.Unlock()
}

// activeEmergencies for a console of agencyFilter ("" = all agencies).
func activeEmergencies(agencyFilter string) []map[string]interface{} {
	activeEmergMu.Lock()
	defer activeEmergMu.Unlock()
	var out []map[string]interface{}
	for _, m := range activeEmerg {
		if agencyFilter == "" || m["agency"] == agencyFilter {
			out = append(out, m)
		}
	}
	return out
}

// sendToConsoles queues a JSON message for every console that covers agencyID.
func (s *Server) sendToConsoles(agencyID string, msg map[string]interface{}) {
	if msg["type"] == "emergency" {
		if _, ok := msg["at"]; !ok {
			msg["at"] = time.Now().UnixMilli() // when it happened, for consoles that open later
		}
		rememberEmergency(msg)
	}
	var data []byte
	s.mu.RLock()
	for key, dc := range s.dispatchConns {
		c := s.clients[key]
		if c == nil || (c.AgencyID != "" && c.AgencyID != agencyID) {
			continue
		}
		if data == nil {
			data, _ = json.Marshal(msg)
		}
		select {
		case dc.Text <- data:
		default:
		}
	}
	s.mu.RUnlock()
}
