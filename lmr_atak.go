// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// LMR bridge markers on the ATAK map.
//
// An administrator gives an LMR bridge a location and switches on "Show in
// ATAK". The server then publishes one CoT marker for the bridge's channel on
// the TAK connection (tak.go) of every agency in the bridge. ATAK users with
// the MeshRelay plugin tap it to listen through their own MeshRelay login.
//
// The marker carries the channel number and name, nothing else: no stream
// address and no secret. Hearing the channel still needs a MeshRelay login
// with access to it; the marker only says where the radio is.
//
// Lifecycle:
//   - shown: sent at once when switched on, moved or renamed, then every 60 s
//     with a stale time 180 s ahead, and again right after a TAK reconnect;
//   - withdrawn (switched off, location removed, bridge paused or deleted, or
//     an agency removed from the bridge): the affected TAK connections get the
//     marker once more with a stale time of now, then the standard t-x-d-d
//     delete, so it leaves ATAK maps at once rather than after 180 s.
//
// Locking: the publisher reads the bridge store under s.mu (read lock) into a
// plain snapshot, releases it, and only then looks up TAK connections and
// writes. It never holds s.mu, the TAK manager's lock or a connection's lock
// while writing to the network (takConn.send writes with no lock held).

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

// Marker type and ATAK icon, agreed with the ATAK plugin. The plugin
// recognises the marker by its <__meshrelay kind="lmr"> detail; these two
// only decide how ATAK draws it: the radio-tower icon of ATAK's stock
// "Responder Icons" iconset, whose own type is a-n-G (a neutral ground point
// with the normal marker radial menu). An empty icon leaves out <usericon>.
const (
	LMRMarkerType = "a-n-G"
	LMRMarkerIcon = "de450cbf-2ffc-47fb-bd2b-ba2db89b035e/Incident/Communication-Ops-Generic.png"
)

const (
	lmrMarkerUIDPrefix = "MeshRelay-LMR-"
	lmrMarkerInterval  = 60 * time.Second  // re-send period
	lmrMarkerStale     = 180 * time.Second // stale = now + this
	// A withdrawal that cannot be delivered (TAK connection down) is retried
	// on each pass for this long, then dropped: by then the TAK server's copy
	// has long gone stale.
	lmrWithdrawKeep = time.Hour

	lmrMarkerRemarks = "Radio channel bridged by MeshRelay. Listen with the MeshRelay ATAK plugin."
)

// lmrMarker is everything a marker needs, copied out of the bridge store so
// no lock is held while it is built and sent.
type lmrMarker struct {
	BridgeID string
	Channel  int
	Name     string
	Lat, Lon float64
}

func lmrMarkerUID(bridgeID string) string { return lmrMarkerUIDPrefix + bridgeID }

// validLMRLocation checks a marker location: finite, latitude -90..90,
// longitude -180..180.
func validLMRLocation(lat, lon float64) error {
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return errors.New("latitude and longitude must be numbers")
	}
	if lat < -90 || lat > 90 {
		return errors.New("latitude must be between -90 and 90")
	}
	if lon < -180 || lon > 180 {
		return errors.New("longitude must be between -180 and 180")
	}
	return nil
}

// atakMarkerOn reports whether this bridge's marker should be on the map: the
// switch is on, it has a valid location, and the bridge is not paused (a
// paused bridge's channel is withdrawn from phones, so there is nothing to
// listen to).
func (b *LMRBridge) atakMarkerOn() bool {
	return b.Enabled && b.ShowInATAK && b.Lat != nil && b.Lon != nil &&
		validLMRLocation(*b.Lat, *b.Lon) == nil
}

// --- CoT builders -------------------------------------------------------------

func cotStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func cotCoord(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// lmrMarkerCoT builds the marker event. server is the MeshRelay server's
// identity (hex public key) or "" to leave the attribute out. A stale time of
// now (stale == 0) is the "withdraw" copy.
func lmrMarkerCoT(m lmrMarker, server string, now time.Time, stale time.Duration) string {
	icon := ""
	if LMRMarkerIcon != "" {
		icon = `<usericon iconsetpath="` + xmlEsc(LMRMarkerIcon) + `"/>`
	}
	srv := ""
	if server != "" {
		srv = ` server="` + xmlEsc(server) + `"`
	}
	ts := cotStamp(now)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<event version="2.0" uid="%s" type="%s" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="%s" lon="%s" hae="9999999.0" ce="9999999.0" le="9999999.0"/>`+
		`<detail><contact callsign="%s"/>%s`+
		`<__meshrelay kind="lmr" channel="%d" name="%s"%s/>`+
		`<remarks>%s</remarks></detail></event>`,
		xmlEsc(lmrMarkerUID(m.BridgeID)), xmlEsc(LMRMarkerType), ts, ts, cotStamp(now.Add(stale)),
		cotCoord(m.Lat), cotCoord(m.Lon),
		xmlEsc(m.Name), icon,
		m.Channel, xmlEsc(m.Name), srv,
		xmlEsc(lmrMarkerRemarks))
}

// lmrMarkerDeleteCoT is ATAK's own delete message (t-x-d-d): ATAK removes the
// map item named by the link uid. tak.go handles incoming deletes the same way.
func lmrMarkerDeleteCoT(bridgeID string, now time.Time) string {
	ts := cotStamp(now)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<event version="2.0" uid="%s" type="t-x-d-d" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="0" lon="0" hae="9999999.0" ce="9999999.0" le="9999999.0"/>`+
		`<detail><link uid="%s" relation="none" type="none"/><__forcedelete/></detail></event>`,
		newUUID(), ts, ts, cotStamp(now.Add(20*time.Second)), xmlEsc(lmrMarkerUID(bridgeID)))
}

// lmrMarkerWithdrawCoT: the marker once more with stale = now (so the TAK
// server's latest-copy cache no longer hands it to clients that connect
// later), followed by the delete that takes it off maps right away.
func lmrMarkerWithdrawCoT(m lmrMarker, server string, now time.Time) string {
	return lmrMarkerCoT(m, server, now, 0) + lmrMarkerDeleteCoT(m.BridgeID, now)
}

// --- publisher ------------------------------------------------------------------

type lmrPendingWithdraw struct {
	m     lmrMarker
	since time.Time
}

type lmrMarkerPublisher struct {
	s    *Server
	kick chan struct{}

	// deliver writes a CoT document on an agency's TAK connection and reports
	// whether it went out (false: no connection, not connected, or the write
	// failed). Tests may replace it.
	deliver func(agency, cot string) bool
	now     func() time.Time

	// Owned by the publishing goroutine (or a test calling pass directly).
	shown   map[string]map[string]lmrMarker          // agency -> bridge -> marker last delivered there
	pending map[string]map[string]lmrPendingWithdraw // agency -> bridge -> withdrawal not yet delivered
}

func (s *Server) lmrMarkers() *lmrMarkerPublisher {
	s.lmrPubOnce.Do(func() {
		s.lmrPub = &lmrMarkerPublisher{
			s:       s,
			kick:    make(chan struct{}, 1),
			deliver: s.takDeliver,
			now:     time.Now,
			shown:   map[string]map[string]lmrMarker{},
			pending: map[string]map[string]lmrPendingWithdraw{},
		}
	})
	return s.lmrPub
}

// lmrMarkersChanged asks the publisher for a pass now (a bridge changed, or a
// TAK connection came up). Never blocks; requests coalesce.
func (s *Server) lmrMarkersChanged() {
	select {
	case s.lmrMarkers().kick <- struct{}{}:
	default:
	}
}

// startLMRMarkers runs the publisher until the server shuts down.
func (s *Server) startLMRMarkers() {
	p := s.lmrMarkers()
	go p.run(s.shutdown)
}

func (p *lmrMarkerPublisher) run(stop <-chan struct{}) {
	p.pass()
	tick := time.NewTicker(lmrMarkerInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		case <-p.kick:
		}
		p.pass()
	}
}

// takDeliver sends on the agency's TAK connection if it is connected.
func (s *Server) takDeliver(agency, cot string) bool {
	tc := s.takConnFor(agency) // takes and releases the manager lock
	if tc == nil {
		return false
	}
	tc.mu.Lock()
	up := tc.status == "connected" && tc.conn != nil
	tc.mu.Unlock()
	if !up {
		return false
	}
	return tc.send(cot) // writes with no lock held
}

// lmrDesiredMarkers snapshots which markers belong on which agency's TAK
// connection. Takes s.mu for reading and returns plain values.
func (s *Server) lmrDesiredMarkers() map[string]map[string]lmrMarker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]lmrMarker{}
	for _, b := range s.lmrBridges {
		if !b.atakMarkerOn() {
			continue
		}
		m := lmrMarker{BridgeID: b.ID, Channel: b.Channel, Name: b.displayName(), Lat: *b.Lat, Lon: *b.Lon}
		for _, a := range b.Agencies {
			if out[a] == nil {
				out[a] = map[string]lmrMarker{}
			}
			out[a][b.ID] = m
		}
	}
	return out
}

// lmrServerID is the value of the marker's server attribute: the server's
// permanent identity public key in hex, which phones already pin from the
// enrollment QR ("spk") or their first secure sign-in. "" when there is none.
func (s *Server) lmrServerID() string {
	pub := s.identityPublic() // set once at start-up, never changed
	if len(pub) == 0 {
		return ""
	}
	return hex.EncodeToString(pub)
}

// pass brings every TAK connection up to date: withdraw what should no longer
// be shown, (re)send what should. No lock is held while sending.
func (p *lmrMarkerPublisher) pass() {
	now := p.now()
	want := p.s.lmrDesiredMarkers()
	server := p.s.lmrServerID()

	// Shown but no longer wanted -> withdraw.
	for agency, ms := range p.shown {
		for id, m := range ms {
			if _, ok := want[agency][id]; ok {
				continue
			}
			if p.pending[agency] == nil {
				p.pending[agency] = map[string]lmrPendingWithdraw{}
			}
			p.pending[agency][id] = lmrPendingWithdraw{m: m, since: now}
			delete(ms, id)
		}
		if len(ms) == 0 {
			delete(p.shown, agency)
		}
	}

	for agency, ws := range p.pending {
		for id, w := range ws {
			_, wanted := want[agency][id]
			switch {
			case wanted:
				delete(ws, id) // wanted again: the send below replaces it
			case p.deliver(agency, lmrMarkerWithdrawCoT(w.m, server, now)):
				log.Printf("LMR: ATAK marker for %q (ch %d) withdrawn from agency %s", w.m.Name, w.m.Channel, agency)
				delete(ws, id)
			case now.Sub(w.since) > lmrWithdrawKeep:
				delete(ws, id)
			}
		}
		if len(ws) == 0 {
			delete(p.pending, agency)
		}
	}

	for agency, ms := range want {
		for id, m := range ms {
			if !p.deliver(agency, lmrMarkerCoT(m, server, now, lmrMarkerStale)) {
				continue // not connected: a reconnect triggers a pass
			}
			if p.shown[agency] == nil {
				p.shown[agency] = map[string]lmrMarker{}
			}
			if prev, ok := p.shown[agency][id]; !ok || prev != m {
				log.Printf("LMR: ATAK marker for %q (ch %d) published to agency %s", m.Name, m.Channel, agency)
			}
			p.shown[agency][id] = m
		}
	}
}

// --- dashboard state ------------------------------------------------------------

// takLinkState says whether an agency's TAK connection can carry a marker:
// "connected", "down" (configured and switched on, not connected now),
// "off" (configured, switched off) or "none" (no TAK connection configured).
// Takes the TAK manager's lock and the connection's lock, one at a time; the
// caller must not hold s.mu.
func (s *Server) takLinkState(agency string) string {
	m := s.tak()
	m.mu.Lock()
	cfg, configured := m.cfgs[agency]
	tc := m.conns[agency]
	m.mu.Unlock()
	if !configured {
		return "none"
	}
	if !cfg.Enabled || tc == nil {
		return "off"
	}
	tc.mu.Lock()
	up := tc.status == "connected" && tc.conn != nil
	tc.mu.Unlock()
	if up {
		return "connected"
	}
	return "down"
}

// lmrAtakAgencyState is one row of "where is this marker being sent".
type lmrAtakAgencyState struct {
	AgencyID string `json:"agency_id"`
	Name     string `json:"name"`
	TAK      string `json:"tak"` // takLinkState
}

// --- API --------------------------------------------------------------------------

// handleLMRBridgeATAK sets a bridge's ATAK marker: the switch and the location,
// replaced as a whole. The owning agency or a super admin, like every other
// change to a bridge (pause, guests, sharing).
//
//	PUT {"show_in_atak": true, "lat": 41.65, "lon": -74.69}
//	PUT {"show_in_atak": false, "lat": null, "lon": null}   (clears the location)
func (s *Server) handleLMRBridgeATAK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	mine, isSuper := s.callerAgency(r)

	var req struct {
		ShowInATAK bool     `json:"show_in_atak"`
		Lat        *float64 `json:"lat"`
		Lon        *float64 `json:"lon"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if (req.Lat == nil) != (req.Lon == nil) {
		writeJSONError(w, http.StatusBadRequest, "enter both latitude and longitude, or neither")
		return
	}
	if req.Lat != nil {
		if err := validLMRLocation(*req.Lat, *req.Lon); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.ShowInATAK && req.Lat == nil {
		writeJSONError(w, http.StatusBadRequest, "set the bridge's location before showing it in ATAK")
		return
	}

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
	b.ShowInATAK = req.ShowInATAK
	b.Lat, b.Lon = nil, nil
	if req.Lat != nil {
		lat, lon := *req.Lat, *req.Lon
		b.Lat, b.Lon = &lat, &lon
	}
	agencies := append([]string(nil), b.Agencies...)
	name, enabled := b.displayName(), b.Enabled
	s.mu.Unlock()

	s.lmrMarkersChanged()
	if err := s.saveLMRBridges(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	log.Printf("LMR: bridge %s (%s) ATAK marker %s by %s", id, name,
		map[bool]string{true: "ON", false: "off"}[req.ShowInATAK], nonEmpty(mine, "super admin"))

	// An agency admin sees its own TAK link only; a super admin sees all.
	states := make([]lmrAtakAgencyState, 0, len(agencies))
	for _, a := range agencies {
		if isSuper || a == mine {
			states = append(states, lmrAtakAgencyState{AgencyID: a, TAK: s.takLinkState(a)})
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "show_in_atak": req.ShowInATAK, "lat": req.Lat, "lon": req.Lon,
		"enabled": enabled, "atak_marker": states,
	})
}

func copyFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
