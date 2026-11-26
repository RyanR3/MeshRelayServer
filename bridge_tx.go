// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Transmit safety on bridged channels.
//
// Audio a phone sends on an LMR bridge channel keys a real radio on a
// licensed frequency; audio on a Zello channel goes out to another network.
// Two rules apply to phones on those channels (never to the gateway itself,
// whose audio is what the radio RECEIVED):
//
//   - Guests (accounts invited from the app, and guest-event accounts) may not
//     transmit there unless the bridge's owner allows guests on that bridge.
//   - One transmission may last bridgeMaxTx. After that the unit's audio is
//     dropped until it has been silent for bridgeTxGap, so a stuck microphone
//     or a hostile client cannot hold the radio keyed. A normal user just
//     releases and presses again.
//
// Emergency audio forwarded to an agency's emergency LMR bridge is segmented
// instead: bridgeMaxTx on, bridgeTxGap off, repeating, so a long open-mic
// emergency still reaches the radio but the channel is never keyed without a
// break. Whether an agency's emergencies go to the radio at all is only the
// agency admin's setting (Agency.EmergencyLMRBridge).

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bridgeMaxTx = 60 * time.Second
	bridgeTxGap = 2 * time.Second
	// Silence longer than this starts a new transmission.
	bridgeTxNewAfter = 1 * time.Second
)

type bridgeTxState struct {
	start       time.Time // start of the current transmission
	last        time.Time // last frame seen
	lockedUntil time.Time // dropping until then
	told        bool      // unit told once for this lockout
}

type bridgeTxGate struct {
	mu sync.Mutex
	m  map[string]*bridgeTxState
}

// allow reports whether a frame at now may pass. extendWhileKeyed: a frame
// during the lockout pushes the lockout out (the unit must go quiet); false
// gives fixed on/off segments. firstDrop is true on the frame that starts a
// lockout.
func (g *bridgeTxGate) allow(key string, now time.Time, extendWhileKeyed bool) (ok, firstDrop bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*bridgeTxState{}
	}
	st := g.m[key]
	if st == nil {
		if len(g.m) > 4096 { // bounded: drop idle entries
			for k, v := range g.m {
				if now.Sub(v.last) > time.Minute {
					delete(g.m, k)
				}
			}
		}
		st = &bridgeTxState{start: now}
		g.m[key] = st
	}
	defer func() { st.last = now }()
	if now.Before(st.lockedUntil) {
		if extendWhileKeyed {
			st.lockedUntil = now.Add(bridgeTxGap)
		}
		return false, false
	}
	if !st.lockedUntil.IsZero() || now.Sub(st.last) > bridgeTxNewAfter {
		// Lockout served, or the unit was silent: a new transmission.
		st.start, st.lockedUntil, st.told = now, time.Time{}, false
	}
	if now.Sub(st.start) >= bridgeMaxTx {
		st.lockedUntil = now.Add(bridgeTxGap)
		first := !st.told
		st.told = true
		return false, first
	}
	return true, false
}

// bridgeOnChannel: is (agency, channel) served by an enabled LMR or Zello
// bridge, and does that bridge allow guests to transmit? Caller holds s.mu
// (read is enough).
func (s *Server) bridgeOnChannelLocked(agencyID string, channel int) (bridged, guestsOK bool, kind string) {
	if b := s.lmrBridgeForChannel(channel); b != nil && b.servesAgency(agencyID) {
		return true, b.AllowGuests, "LMR"
	}
	for _, b := range s.zelloBridges {
		if !b.Enabled {
			continue
		}
		for _, m := range b.effectiveMappings() {
			if m.servesAgency(agencyID) && m.MeshChannel == channel {
				return true, b.AllowGuests, "Zello"
			}
		}
	}
	return false, false, ""
}

// isGuestAccount: invited from the app, or a guest-event account.
func isGuestAccount(u *UserAccount, agencyID string) bool {
	if strings.HasPrefix(agencyID, "event-") {
		return true
	}
	return u != nil && u.GuestExpiresAt != nil
}

// bridgeTxAllowed gates a phone's audio frame on a bridged channel. It
// returns true for every other channel and for the bridge's own gateway.
func (s *Server) bridgeTxAllowed(client *Client, now time.Time) bool {
	s.mu.RLock()
	bridged, guestsOK, kind := s.bridgeOnChannelLocked(client.AgencyID, client.Channel)
	if !bridged || s.ownLMRBridge(client) != nil {
		s.mu.RUnlock()
		return true
	}
	guest := isGuestAccount(s.accountFor(client), client.AgencyID)
	s.mu.RUnlock()

	key := userKey(client.AgencyID, client.Username) + "#" + itoa(client.Channel)
	if guest && !guestsOK {
		// Told once per transmission, through the same gate bookkeeping.
		if s.bridgeGuestGate.note(key, now) {
			log.Printf("%s bridge ch %d [%s]: guest %q may not transmit here (guests not allowed on this bridge)",
				kind, client.Channel, client.AgencyID, client.Username)
			s.sendFloorToUserIn(channelKey(client.AgencyID, client.Channel), client.Username,
				buildFloorDenied(client.Channel, 1, 0, "Guests can't transmit on this bridge"))
		}
		return false
	}
	ok, first := s.bridgeTx.allow(key, now, true)
	if first {
		log.Printf("%s bridge ch %d [%s]: %q reached the %s transmit limit — dropped until it unkeys",
			kind, client.Channel, client.AgencyID, client.Username, bridgeMaxTx)
		s.activityLog.Add("bridge_tx_limit", client.Username,
			"Transmit limit reached on "+kind+" bridge channel "+itoa(client.Channel), client.AgencyID)
		fkey := channelKey(client.AgencyID, client.Channel)
		if s.floor != nil {
			s.floor.Release(fkey, client.Channel, client.Username)
		}
		s.sendFloorToUserIn(fkey, client.Username, buildFloorPreempted(client.Channel, "Transmit limit", 0))
	}
	return ok
}

// emergencyLMRAllowed segments emergency audio going to an agency's
// emergency LMR bridge: bridgeMaxTx on, bridgeTxGap off.
func (s *Server) emergencyLMRAllowed(agencyID string, now time.Time) bool {
	ok, first := s.bridgeTx.allow("emerg#"+agencyID, now, false)
	if first {
		log.Printf("Emergency [%s]: %s of continuous audio to the LMR bridge — pausing %s so the radio unkeys",
			agencyID, bridgeMaxTx, bridgeTxGap)
	}
	return ok
}

// noteGate remembers "told once per burst" for refusals that are not timed.
type noteGate struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// note returns true the first time key is seen after a quiet second.
func (g *noteGate) note(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]time.Time{}
	}
	last, seen := g.m[key]
	g.m[key] = now
	if len(g.m) > 4096 {
		for k, v := range g.m {
			if now.Sub(v) > time.Minute {
				delete(g.m, k)
			}
		}
	}
	return !seen || now.Sub(last) > bridgeTxNewAfter
}

func itoa(n int) string { return strconv.Itoa(n) }
