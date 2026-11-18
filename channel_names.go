// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Agency channel names.
//
// Each agency decides how many talkgroups it uses (5 unless an admin enables
// more) and what they are called. The names are pushed to every phone of the
// agency (CHANNEL_NAMES, 0x32) when it joins and whenever an admin changes
// them, and the command console shows the same names, so "channel 1" is called
// the same thing everywhere. Phones lock these channels against local renames;
// ad-hoc, direct, group and guest channels keep the names their creators give.
//
//	ChannelNames (0x32, server→client): u16 count | count × (u16 len | utf8 name)
//	  count = talkgroups the agency uses (wire channels 0..count-1);
//	  an empty name = not named ("Channel N").
//
// Older clients ignore the packet and keep their own names.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	PacketTypeChannelNames byte = 0x32

	// DefaultAgencyChannels is how many talkgroups an agency has until an
	// admin enables more.
	DefaultAgencyChannels = 5

	maxChannelNameLen = 32
)

// talkgroupCount is how many talkgroups the agency uses.
func (a *Agency) talkgroupCount() int {
	n := a.ChannelCount
	if n <= 0 {
		n = DefaultAgencyChannels
	}
	if lim := a.channelLimit(); n > lim {
		n = lim
	}
	return n
}

// channelLimit is the most talkgroups the agency may enable (the super admin's
// max_channels, at most MaxAgencyTalkgroups).
func (a *Agency) channelLimit() int {
	if a.MaxChannels > 0 && a.MaxChannels < MaxAgencyTalkgroups {
		return a.MaxChannels
	}
	return MaxAgencyTalkgroups
}

// talkgroupName is what everyone sees for talkgroup ch (0-based wire number).
func (a *Agency) talkgroupName(ch int) string {
	if a != nil {
		if n := a.ChannelNames[ch]; n != "" {
			return n
		}
	}
	return "Channel " + strconv.Itoa(ch+1)
}

// buildChannelNames builds the 0x32 packet for an agency. Caller holds s.mu.
func buildChannelNames(a *Agency) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeChannelNames)
	n := a.talkgroupCount()
	binary.Write(buf, binary.BigEndian, uint16(n))
	for ch := 0; ch < n; ch++ {
		putStr(buf, a.ChannelNames[ch]) // "" = unnamed
	}
	return buf.Bytes()
}

// broadcastChannelNames sends an agency's channel names to all its phones.
func (s *Server) broadcastChannelNames(agencyID string) {
	s.mu.RLock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.RUnlock()
		return
	}
	pkt := buildChannelNames(a)
	var addrs []*net.UDPAddr
	for _, c := range s.clients {
		if c.AgencyID == agencyID && c.Addr != nil && !isConsoleAddr(c.Addr) {
			addrs = append(addrs, c.Addr)
		}
	}
	s.mu.RUnlock()
	for _, addr := range addrs {
		if s.testFloorSink != nil {
			s.testFloorSink(addr, pkt)
			continue
		}
		s.sendPacket(pkt, addr)
	}
	log.Printf("Channel names for agency %s sent to %d client(s)", agencyID, len(addrs))
}

// --- dashboard API ------------------------------------------------------------
//
// GET  /api/my-agency/channels → [{agency_id, agency_name, count, limit, names[]}]
//      (an agency admin gets their own agency; a super admin gets every agency)
// PUT  /api/my-agency/channels {agency_id (super admin), count, names[]}

type agencyChannelsJSON struct {
	AgencyID   string   `json:"agency_id"`
	AgencyName string   `json:"agency_name"`
	Count      int      `json:"count"`
	Limit      int      `json:"limit"`
	Default    int      `json:"default"`
	Names      []string `json:"names"`
}

func agencyChannelsOf(a *Agency) agencyChannelsJSON {
	n := a.talkgroupCount()
	names := make([]string, n)
	for ch := 0; ch < n; ch++ {
		names[ch] = a.ChannelNames[ch]
	}
	return agencyChannelsJSON{a.ID, a.Name, n, a.channelLimit(), DefaultAgencyChannels, names}
}

func (s *Server) handleGetAgencyChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	s.mu.RLock()
	out := []agencyChannelsJSON{}
	for id, a := range s.agencies {
		if (isSuper && !strings.HasPrefix(id, "event-")) || id == agencyID {
			out = append(out, agencyChannelsOf(a))
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].AgencyName) < strings.ToLower(out[j].AgencyName)
	})
	json.NewEncoder(w).Encode(out)
}

// validateChannelName checks one channel name ("" = unnamed).
func validateChannelName(ch int, v string) error {
	if v == "" {
		return nil
	}
	if err := checkNameText("channel name", v, maxChannelNameLen, ""); err != nil {
		return fmt.Errorf("channel %d: %v", ch+1, err)
	}
	return nil
}

func (s *Server) handleUpdateAgencyChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID string   `json:"agency_id"` // super admin only
		Count    int      `json:"count"`
		Names    []string `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}

	s.mu.Lock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	limit := a.channelLimit()
	if req.Count < 1 || req.Count > limit {
		s.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("number of channels must be 1-%d", limit))
		return
	}
	names := map[int]string{}
	seen := map[string]int{}
	for ch, v := range req.Names {
		v = strings.TrimSpace(v)
		if ch >= req.Count || v == "" {
			continue
		}
		if err := validateChannelName(ch, v); err != nil {
			s.mu.Unlock()
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if prev, dup := seen[strings.ToLower(v)]; dup {
			s.mu.Unlock()
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("channels %d and %d are both named %q", prev+1, ch+1, v))
			return
		}
		seen[strings.ToLower(v)] = ch
		names[ch] = v
	}
	a.ChannelCount = req.Count
	a.ChannelNames = names // a new map: nothing else ever mutates it
	result := agencyChannelsOf(a)
	s.mu.Unlock()

	s.mu.RLock()
	err := s.saveAgencies()
	s.mu.RUnlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("Channels for agency %s: %d talkgroup(s), %d named", agencyID, req.Count, len(names))
	s.broadcastChannelNames(agencyID)
	json.NewEncoder(w).Encode(result)
}
