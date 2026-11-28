// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Where a unit in an emergency is.
//
// A phone reports its position with location packets (0x2E). A MeshRelayTAK
// plugin user does not: ATAK reports the position to a TAK server. The plugin
// therefore puts ATAK's position in the emergency alert itself (0x26 tail,
// ParseEmergencyAlert), and the server can also find the unit on a TAK
// connection of its agency (own or master) by callsign.
//
// unitPosition, in order:
//  1. the position the alert carried, or the unit's MeshRelay location report
//     when that one is newer;
//  2. the MeshRelay location report;
//  3. a fresh TAK unit, on the agency's own TAK connection or on the master
//     connection the agency shares, whose callsign is the MeshRelay username
//     (any case).
//
// That position goes to the consoles, to the phones (the 0x26 tail they read
// already) and into the server's TAK 911 alert. A TAK alert is never placed
// at 0,0: with no position at all it goes to the middle of the agency's
// MeshRelay units (agencyCentroid, as dispatch's own contact), else of the
// fresh TAK units on the agency's connections, and its remarks say the
// position is unknown.

import (
	"fmt"
	"log"
	"math"
	"time"
)

// emergencyFix: a position an emergency alert carried.
type emergencyFix struct {
	Lat, Lon float64
	At       time.Time
}

// fixMovedDeg: a re-sent alert this far (about 10 m) from the last position
// it carried updates consoles and TAK; closer re-sends do not.
const fixMovedDeg = 1e-4

// fixDistanceM: roughly how far apart two fixes are, in metres.
func fixDistanceM(a, b *emergencyFix) float64 {
	if a == nil || b == nil {
		return 0
	}
	const r = 6371000.0
	la1, la2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dLa, dLo := la2-la1, (b.Lon-a.Lon)*math.Pi/180
	h := math.Sin(dLa/2)*math.Sin(dLa/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLo/2)*math.Sin(dLo/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

func fixMoved(prev, next *emergencyFix) bool {
	if next == nil {
		return false
	}
	if prev == nil {
		return true
	}
	return math.Abs(prev.Lat-next.Lat) > fixMovedDeg || math.Abs(prev.Lon-next.Lon) > fixMovedDeg
}

// unitPosition: the best known position of agency's user (see above).
// Caller must not hold s.mu, s.emergMu or any TAK lock.
func (s *Server) unitPosition(agency, user string) (lat, lon float64, ok bool) {
	var alertFix *emergencyFix
	s.emergMu.Lock()
	if e := s.emergCalls[agency][user]; e != nil && e.Fix != nil {
		cp := *e.Fix
		alertFix = &cp
	}
	s.emergMu.Unlock()
	var loc *UnitLocation
	s.mu.RLock()
	if c := s.clients[userKey(agency, user)]; c != nil && c.Loc != nil {
		cp := *c.Loc
		loc = &cp
	}
	s.mu.RUnlock()
	switch {
	case alertFix != nil && (loc == nil || !loc.At.After(alertFix.At)):
		return alertFix.Lat, alertFix.Lon, true
	case loc != nil:
		return loc.Lat, loc.Lon, true
	}
	if u := s.takUnitByCallsign(agency, user, time.Now()); u != nil {
		return u.Lat, u.Lon, true
	}
	return 0, 0, false
}

// agencyTakConns: the TAK connections whose units agency's consoles see: its
// own, and the master while the agency shares it.
func (s *Server) agencyTakConns(agency string) []*takConn {
	var out []*takConn
	if tc := s.takConnFor(agency); tc != nil {
		out = append(out, tc)
	}
	if mc := s.takMasterConn(); mc != nil {
		for _, tg := range s.takMasterTargets() {
			if tg.Agency == agency {
				out = append(out, mc)
				break
			}
		}
	}
	return out
}

// takUnitByCallsign: the most recent fresh (not stale) TAK unit called
// callsign (any case) on agency's TAK connections, or nil.
func (s *Server) takUnitByCallsign(agency, callsign string, now time.Time) *TakUnit {
	u, _ := s.takUnitOf(agency, callsign, now, true)
	return u
}

// takUnitOf: the most recent TAK unit called callsign (callsignKey) on
// agency's TAK connections (fresh ones only when freshOnly), and where it
// is: source "" = the agency's own connection, "master" = the master.
func (s *Server) takUnitOf(agency, callsign string, now time.Time, freshOnly bool) (*TakUnit, string) {
	want := callsignKey(callsign)
	if want == "" {
		return nil, ""
	}
	var best *TakUnit
	source := ""
	for _, tc := range s.agencyTakConns(agency) {
		tc.mu.Lock()
		for _, u := range tc.units {
			if callsignKey(u.Callsign) != want || (freshOnly && !now.Before(u.Stale)) ||
				!validFix(u.Lat, u.Lon) {
				continue
			}
			if best == nil || u.At.After(best.At) {
				cp := *u
				best = &cp
				source = ""
				if tc.master {
					source = "master"
				}
			}
		}
		tc.mu.Unlock()
	}
	return best, source
}

// takUnitsCentroid: the middle of the fresh TAK units on agency's
// connections (0,0, false when none).
func (s *Server) takUnitsCentroid(agency string, now time.Time) (float64, float64, bool) {
	var lat, lon float64
	n := 0
	for _, tc := range s.agencyTakConns(agency) {
		tc.mu.Lock()
		for _, u := range tc.units {
			if now.Before(u.Stale) && validFix(u.Lat, u.Lon) {
				lat += u.Lat
				lon += u.Lon
				n++
			}
		}
		tc.mu.Unlock()
	}
	if n == 0 {
		return 0, 0, false
	}
	return lat / float64(n), lon / float64(n), true
}

// emergencyAlertFix: where the server's TAK alert for agency's user goes, and
// whether that is the unit's own position. Never 0,0 while anything is known.
func (s *Server) emergencyAlertFix(agency, user string) (lat, lon float64, known bool) {
	if lat, lon, ok := s.unitPosition(agency, user); ok {
		return lat, lon, true
	}
	if lat, lon := s.agencyCentroid(agency); validFix(lat, lon) {
		return lat, lon, false
	}
	if lat, lon, ok := s.takUnitsCentroid(agency, time.Now()); ok {
		return lat, lon, false
	}
	log.Printf("TAK: no position at all for %q [%s] or its agency: alert placed at 0,0", user, agency)
	return 0, 0, false
}

// emergencyPositionChanged: a re-sent alert gave a unit in an emergency a new
// position. Consoles move its card/marker (a repeat of an active emergency
// does not alarm again) and the server's TAK alert is sent again in place
// (same uid), unless the unit's own ATAK alert stands in for it.
func (s *Server) emergencyPositionChanged(agencyID string, channel int, user string, movedM float64) {
	lat, lon, ok := s.unitPosition(agencyID, user)
	if !ok {
		return
	}
	msg := map[string]interface{}{
		"type": "emergency", "agency": agencyID, "channel": channel, "user": user, "active": true,
		"lat": lat, "lon": lon,
	}
	// Keep when it was raised, for consoles that open later.
	activeEmergMu.Lock()
	if prev := activeEmerg[agencyID+"|"+user]; prev != nil && prev["at"] != nil {
		msg["at"] = prev["at"]
	}
	activeEmergMu.Unlock()
	s.sendToConsoles(agencyID, msg)
	s.takEmergency(agencyID, user, true, fmt.Sprintf("moved %.0f m", movedM))
}
