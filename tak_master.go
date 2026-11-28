// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// The master TAK server: one server-wide TAK connection, set up by the super
// admin with the same enrollment, certificate checks and status as an
// agency's own connection (tak.go). Agencies take part only after their admin
// (or the super admin) switches on "Share with the master TAK server"
// (Agency.ShareMasterTAK, off by default), plus the home agency (below),
// which always takes part. For those agencies:
//
//   - out: their MeshRelay emergencies (raise and cancel, with the same
//     one-alert-per-unit logic, kept per connection) and the markers of their
//     LMR bridges with "Show in ATAK" on (lmr_atak.go);
//   - in: the master's TAK users and TAK alerts show on their consoles,
//     labelled as coming from the master;
//   - messages: the master announces one contact (its own UID and name; a
//     TAK server delivers direct GeoChat only to the first contact a
//     connection announces). A console's message to a master TAK user goes
//     out from that contact as "[<agency name>] <text>", and the connection
//     remembers (memory only) which agencies wrote to which TAK user. A TAK
//     user's direct message to the master's contact goes to every agency
//     that wrote to that user in the last 12 hours and still takes part; when
//     there is none, to the home agency only. Team chats and All Chat Rooms
//     reach no console.
//   - home agency (TakConfig.HomeAgency, chosen by the super admin): when
//     unset or no longer there, the agency "default" if it exists, otherwise
//     the first agency ID in sorted order.
//
// Same TAK server twice: when an agency's own connection and the master go to
// the same TAK server (host, any letter case, and streaming port; or the same
// streaming-port certificate), the agency's items go out once, on its own
// connection (no alert or marker of its own on the master), its
// console lists each TAK user once, and the master's TAK alerts are left to
// its own connection.
//
// Stored in tak_master.json (0600, like tak_connections.json): the client
// certificate and key the enrollment returned, never the password. Not part
// of the user export (migration.go), like the agency connections.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const takMasterFile = "tak_master.json"

// takMasterTarget: the master's key in the LMR marker publisher's per-target
// maps (agency IDs never start with a control character).
const takMasterTarget = "\x00master"

// --- persistence ------------------------------------------------------------

func (s *Server) loadTakMaster() {
	data, err := os.ReadFile(takMasterFile)
	if err != nil {
		return
	}
	var cfg TakConfig
	if json.Unmarshal(data, &cfg) != nil {
		log.Printf("TAK: could not parse %s", takMasterFile)
		return
	}
	cfg.AgencyID = ""
	m := s.tak()
	m.mu.Lock()
	m.masterCfg = &cfg
	m.mu.Unlock()
	if cfg.Enabled && cfg.CertPEM != "" {
		s.startTakMaster(cfg)
	}
	home, how := s.takMasterHomeWhy()
	log.Printf("TAK: master connection loaded (home agency %q, %s)", home, how)
}

func (s *Server) saveTakMaster() error {
	m := s.tak()
	m.mu.Lock()
	cfg := m.masterCfg
	m.mu.Unlock()
	if cfg == nil {
		if err := os.Remove(takMasterFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONFileAtomic(takMasterFile, cfg)
}

// --- connection ----------------------------------------------------------------

func (s *Server) startTakMaster(cfg TakConfig) {
	cfg.AgencyID = ""
	m := s.tak()
	m.mu.Lock()
	if old := m.master; old != nil {
		old.close()
	}
	tc := &takConn{s: s, cfg: cfg, master: true, status: "connecting", units: map[string]*TakUnit{}, stop: make(chan struct{})}
	m.master = tc
	c := cfg
	m.masterCfg = &c
	m.mu.Unlock()
	go tc.run()
}

func (s *Server) stopTakMaster() {
	m := s.tak()
	m.mu.Lock()
	if tc := m.master; tc != nil {
		tc.close()
		m.master = nil
	}
	m.mu.Unlock()
}

// takMasterConn: the running master connection, or nil.
func (s *Server) takMasterConn() *takConn {
	m := s.tak()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.master
}

// --- who takes part --------------------------------------------------------------

// takMasterShare is one agency sharing with the master.
type takMasterShare struct {
	Agency     string `json:"agency_id"`
	Name       string `json:"name"`
	SameServer bool   `json:"same_server"` // its own connection is to the master's TAK server
	Home       bool   `json:"home"`        // the home agency (takes part whatever its switch says)
}

// agencySharesMaster: whether agency takes part in the master: its switch is
// on, or it is the home agency. Takes the TAK manager's lock, then s.mu.
func (s *Server) agencySharesMaster(agency string) bool {
	home := s.takMasterHome()
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.agencies[agency]
	return a != nil && (a.ShareMasterTAK || agency == home)
}

// --- home agency ---------------------------------------------------------------

// takMasterHome: the effective home agency ("" while no master is set up, or
// when there are no agencies).
func (s *Server) takMasterHome() string {
	home, _ := s.takMasterHomeWhy()
	return home
}

// takMasterHomeWhy: the effective home agency and how it was chosen.
func (s *Server) takMasterHomeWhy() (string, string) {
	m := s.tak()
	m.mu.Lock()
	if m.masterCfg == nil {
		m.mu.Unlock()
		return "", "no master"
	}
	want := m.masterCfg.HomeAgency
	m.mu.Unlock()
	return s.resolveTakMasterHome(want)
}

// resolveTakMasterHome: want when that agency exists; otherwise "default"
// when it exists; otherwise the first agency ID in sorted order.
func (s *Server) resolveTakMasterHome(want string) (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if want != "" && s.agencies[want] != nil {
		return want, "chosen"
	}
	if s.agencies["default"] != nil {
		return "default", "fallback"
	}
	first := ""
	for id := range s.agencies {
		if first == "" || id < first {
			first = id
		}
	}
	if first == "" {
		return "", "no agencies"
	}
	return first, "fallback"
}

// takMasterSharing lists the agencies that take part (switched on, or the
// home agency; sorted), with same-server detection against the running
// master. Takes s.mu, the TAK manager's lock and connection locks one at a
// time; the caller must hold none of them.
func (s *Server) takMasterSharing() []takMasterShare {
	var list []takMasterShare
	home := s.takMasterHome()
	s.mu.RLock()
	for id, a := range s.agencies {
		if a.ShareMasterTAK || id == home {
			list = append(list, takMasterShare{Agency: id, Name: a.Name, Home: id == home})
		}
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Agency < list[j].Agency })
	m := s.tak()
	m.mu.Lock()
	mc := m.master
	own := make(map[string]*takConn, len(list))
	for _, sh := range list {
		if c := m.conns[sh.Agency]; c != nil {
			own[sh.Agency] = c
		}
	}
	m.mu.Unlock()
	if mc != nil {
		for i := range list {
			if c := own[list[i].Agency]; c != nil {
				list[i].SameServer = takSameServer(c, mc)
			}
		}
	}
	return list
}

// takMasterTargets: the sharing agencies, while a master connection runs.
func (s *Server) takMasterTargets() []takMasterShare {
	if s.takMasterConn() == nil {
		return nil
	}
	return s.takMasterSharing()
}

// takMasterOutboundAgencies: the agencies whose emergencies and markers go
// out on the master (sharing, and not already on that TAK server).
func (s *Server) takMasterOutboundAgencies() []string {
	var out []string
	for _, tg := range s.takMasterTargets() {
		if !tg.SameServer {
			out = append(out, tg.Agency)
		}
	}
	return out
}

// takMasterOutbound: the master connection, when agency's items go out on it.
func (s *Server) takMasterOutbound(agency string) *takConn {
	mc := s.takMasterConn()
	if mc == nil {
		return nil
	}
	for _, a := range s.takMasterOutboundAgencies() {
		if a == agency {
			return mc
		}
	}
	return nil
}

// takSameServer: whether two connections go to the same TAK server: the same
// host (any letter case) and streaming port, or the same certificate on the
// streaming port when both have connected.
func takSameServer(a, b *takConn) bool {
	if a == nil || b == nil || a == b {
		return false
	}
	a.mu.Lock()
	ac, afp := a.cfg, a.peerFP
	a.mu.Unlock()
	b.mu.Lock()
	bc, bfp := b.cfg, b.peerFP
	b.mu.Unlock()
	if strings.EqualFold(strings.TrimSpace(ac.Host), strings.TrimSpace(bc.Host)) && ac.Host != "" && ac.StreamPort == bc.StreamPort {
		return true
	}
	return afp != "" && afp == bfp
}

// takMasterOwnsUID: whether uid is the master's contact.
func (s *Server) takMasterOwnsUID(uid string) bool {
	m := s.tak()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.masterCfg != nil && m.masterCfg.UID != "" && uid == m.masterCfg.UID
}

// takMasterHasUnit: whether the master lists uid for a sharing agency.
func (s *Server) takMasterHasUnit(agency, uid string) bool {
	if !s.agencySharesMaster(agency) {
		return false
	}
	return s.takMasterConn().hasUnit(uid)
}

func (s *Server) agencyDisplayName(agency string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a := s.agencies[agency]; a != nil {
		return a.Name
	}
	return ""
}

// takMasterChatKeep: how long a console's message to a master TAK user
// routes that user's messages to the console's agency.
const takMasterChatKeep = 12 * time.Hour

// consoleSendMasterMessage: a console's message to a TAK user on the master,
// sent from the master's contact as "[<agency name>] <text>" (nothing goes
// out unless the agency takes part). The history keeps the text as typed.
func (s *Server) consoleSendMasterMessage(console *Client, agency, uid, text, hint string) {
	callsign, sent, why := "", 0, "master TAK not set up"
	mc := s.takMasterConn()
	switch {
	case mc == nil:
	case !s.agencySharesMaster(agency):
		why = "agency not sharing with the master"
	default:
		mc.mu.Lock()
		if u := mc.units[uid]; u != nil {
			callsign = u.Callsign
		}
		connected := mc.status == "connected"
		fromUID, fromName := mc.cfg.UID, mc.cfg.Callsign
		mc.mu.Unlock()
		why = "master TAK not connected"
		if connected {
			why = "master TAK write failed"
			lat, lon := s.agencyCentroid(agency)
			out := takMasterPrefixed(nonEmpty(strings.TrimSpace(s.agencyDisplayName(agency)), agency), text)
			if mc.send(chatCoTAs(fromUID, fromName, lat, lon, uid, nonEmpty(callsign, uid), out)) {
				sent, why = 1, ""
				mc.noteChatPeer(uid, agency, time.Now())
			}
		}
	}
	s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: agency, Dir: "out", From: console.Username,
		To: takRecipientLabel(callsign, hint) + " (TAK)", Text: text, Sent: sent, NotSent: why, TakUID: uid,
		TakSource: "master", unlisted: callsign == ""})
}

// takMasterPrefixed: "[<agency name>] <text>", within maxMessageLen (the
// text is shortened, never the name).
func takMasterPrefixed(agencyName, text string) string {
	p := "[" + agencyName + "] "
	if len(p) >= maxMessageLen {
		return clipUTF8(p+text, maxMessageLen)
	}
	return p + clipUTF8(text, maxMessageLen-len(p))
}

// clipUTF8: s cut to at most n bytes, not inside a character.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// --- message routing (master) ---------------------------------------------------

// noteChatPeer: agency's console wrote to the TAK user uid.
func (t *takConn) noteChatPeer(uid, agency string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.chatPeers == nil {
		t.chatPeers = map[string]map[string]time.Time{}
	}
	if t.chatPeers[uid] == nil {
		t.chatPeers[uid] = map[string]time.Time{}
	}
	t.chatPeers[uid][agency] = now
}

// chatPeersOf: the agencies that wrote to uid within takMasterChatKeep, sorted.
func (t *takConn) chatPeersOf(uid string, now time.Time) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for a, at := range t.chatPeers[uid] {
		if now.Sub(at) < takMasterChatKeep {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// forgetChatAgency drops agency from the routing record.
func (t *takConn) forgetChatAgency(agency string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for uid, peers := range t.chatPeers {
		delete(peers, agency)
		if len(peers) == 0 {
			delete(t.chatPeers, uid)
		}
	}
}

// pruneChatPeersLocked forgets entries older than takMasterChatKeep. t.mu held.
func (t *takConn) pruneChatPeersLocked(now time.Time) {
	for uid, peers := range t.chatPeers {
		for a, at := range peers {
			if now.Sub(at) >= takMasterChatKeep {
				delete(peers, a)
			}
		}
		if len(peers) == 0 {
			delete(t.chatPeers, uid)
		}
	}
}

// takMasterRoute: the agencies whose consoles get a direct message from the
// TAK user from: those that wrote to that user within takMasterChatKeep and
// still take part; when there are none, the home agency.
func (s *Server) takMasterRoute(mc *takConn, from string, now time.Time) ([]string, string) {
	var out []string
	for _, a := range mc.chatPeersOf(from, now) {
		if s.agencySharesMaster(a) {
			out = append(out, a)
		}
	}
	if len(out) > 0 {
		return out, "agencies that wrote to this user"
	}
	if home := s.takMasterHome(); home != "" {
		return []string{home}, "home agency, nobody wrote to this user"
	}
	return nil, "no home agency"
}

// deliverMasterChat: a TAK user's direct message to the master's contact.
func (t *takConn) deliverMasterChat(from, name, text string, now time.Time) {
	agencies, why := t.s.takMasterRoute(t, from, now)
	log.Printf("TAK [master]: message from TAK uid %q to agencies %q (%s)", from, agencies, why)
	for _, a := range agencies {
		t.s.recordMessage(dispatchMsg{At: now.UnixMilli(), Agency: a, Dir: "in",
			From: name, To: "Dispatch", Text: text, TakUID: from, TakSource: "master"})
	}
}

// masterConnected: once per connect, after the first presence. Deletes the
// per-agency contacts an earlier version announced (master UID + "." +
// agency ID), so ATAK drops them; harmless when they never existed.
func (t *takConn) masterConnected() {
	t.mu.Lock()
	uid := t.cfg.UID
	t.mu.Unlock()
	t.s.mu.RLock()
	ids := make([]string, 0, len(t.s.agencies))
	for id := range t.s.agencies {
		ids = append(ids, id)
	}
	t.s.mu.RUnlock()
	sort.Strings(ids)
	now := time.Now()
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(cotDeleteCoT(uid+"."+id, now))
	}
	if b.Len() > 0 && t.send(b.String()) {
		log.Printf("TAK [master]: sent deletes for %d old per-agency contacts", len(ids))
	}
	home, how := t.s.takMasterHomeWhy()
	log.Printf("TAK [master]: home agency %q (%s)", home, how)
}

// --- withdrawing --------------------------------------------------------------

// withdrawAgency takes an agency off the master: the server's alerts it has
// out there are cancelled and it leaves the message routing record. (Its LMR
// markers are withdrawn by the marker publisher on its next pass.)
func (t *takConn) withdrawAgency(agency string, now time.Time) {
	var cancel []string
	t.mu.Lock()
	for k, a := range t.alerts {
		if a.agency != agency {
			continue
		}
		if a.ownOut {
			cancel = append(cancel, a.user)
		}
		delete(t.alerts, k)
	}
	t.mu.Unlock()
	t.forgetChatAgency(agency)
	for _, u := range cancel {
		lat, lon := t.s.userFix(agency, u)
		if t.send(t.emergencyCoTFor(agency, u, lat, lon, false)) {
			log.Printf("TAK [%s]: 911 alert cancel for %q [%s] sent (agency left the master)", t.label(), u, agency)
		}
	}
}

// withdrawFromMaster, before the master connection is switched off or
// removed: LMR markers it carries are withdrawn and the server's alerts on it
// cancelled, so they leave ATAK maps now rather than when they go stale.
func (s *Server) withdrawFromMaster(mc *takConn) {
	now := time.Now()
	server := s.lmrServerID()
	for _, mk := range s.lmrDesiredMarkers()[takMasterTarget] {
		mc.send(lmrMarkerWithdrawCoT(mk, server, now))
	}
	agencies := map[string]bool{}
	for _, tg := range s.takMasterTargets() {
		agencies[tg.Agency] = true
	}
	mc.mu.Lock()
	for _, a := range mc.alerts {
		if a.agency != "" {
			agencies[a.agency] = true
		}
	}
	mc.mu.Unlock()
	for a := range agencies {
		mc.withdrawAgency(a, now)
	}
	s.clearMasterEmergencies("")
}

// clearMasterEmergencies takes the master's TAK alerts off the consoles of
// agency ("" = every agency): they no longer receive the master's cancel.
func (s *Server) clearMasterEmergencies(agency string) {
	var stale []map[string]interface{}
	activeEmergMu.Lock()
	for _, e := range activeEmerg {
		if e["tak_source"] == "master" && (agency == "" || e["agency"] == agency) {
			cp := map[string]interface{}{}
			for k, v := range e {
				cp[k] = v
			}
			cp["active"] = false
			delete(cp, "at")
			stale = append(stale, cp)
		}
	}
	activeEmergMu.Unlock()
	for _, e := range stale {
		a, _ := e["agency"].(string)
		s.sendToConsoles(a, e)
	}
}

// joinMaster: an agency started taking part. A MeshRelay emergency already
// on is sent to the master.
func (s *Server) joinMaster(agency string) {
	mc := s.takMasterOutbound(agency)
	if mc == nil {
		return
	}
	var users []string
	s.emergMu.Lock()
	for u := range s.emergCalls[agency] {
		users = append(users, u)
	}
	s.emergMu.Unlock()
	for _, u := range users {
		mc.ownEmergencyFor(agency, u, true, time.Now(), "agency joined the master")
	}
}

// leaveMaster: an agency stopped taking part (switch off, or no longer the
// home agency): its alerts are cancelled on the master, it leaves the
// routing record, and the master's TAK alerts leave its consoles.
func (s *Server) leaveMaster(agency string) {
	if mc := s.takMasterConn(); mc != nil {
		mc.withdrawAgency(agency, time.Now())
	}
	s.clearMasterEmergencies(agency)
}

// --- dashboard API -------------------------------------------------------------------

// takMasterShareStatus adds the agency's sharing switch and the master's
// state (status only: the master's address and account are not shown to
// agency admins) to the agency's TAK status.
func (s *Server) takMasterShareStatus(agency string, out map[string]interface{}) {
	m := s.tak()
	m.mu.Lock()
	mcfg, mc, own := m.masterCfg, m.master, m.conns[agency]
	m.mu.Unlock()
	out["share_master"] = s.agencySharesMaster(agency)
	master := map[string]interface{}{"configured": mcfg != nil}
	if mcfg != nil {
		home := s.takMasterHome()
		// The home agency's switch is shown on and locked; every sharing
		// agency is told whose console gets unaddressed messages.
		out["master_home"] = home != "" && home == agency
		master["home_agency_name"] = nonEmpty(strings.TrimSpace(s.agencyDisplayName(home)), home)
		master["status"] = "disabled"
		if mc != nil {
			mc.mu.Lock()
			master["status"] = mc.status
			mc.mu.Unlock()
		}
	}
	out["master"] = master
	out["same_server_as_master"] = own != nil && mc != nil && takSameServer(own, mc)
}

func (s *Server) takMasterStatusJSON() map[string]interface{} {
	m := s.tak()
	m.mu.Lock()
	mcfg, mc := m.masterCfg, m.master
	m.mu.Unlock()
	out := map[string]interface{}{"configured": mcfg != nil}
	if mcfg == nil {
		out["stream_port"], out["enroll_port"], out["api_port"], out["callsign"] = 8089, 8446, 8443, "MeshRelay"
	} else {
		takCfgStatus(out, *mcfg, mc)
	}
	sharing := s.takMasterSharing()
	if sharing == nil {
		sharing = []takMasterShare{}
	}
	out["sharing"] = sharing
	// The home agency: as chosen ("" = not chosen) and in effect, and the
	// agencies to choose from.
	home, how := s.takMasterHomeWhy()
	chosen := ""
	if mcfg != nil {
		chosen = mcfg.HomeAgency
	}
	out["home_agency"], out["effective_home_agency"], out["home_agency_how"] = chosen, home, how
	type agencyItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	agencies := []agencyItem{}
	s.mu.RLock()
	for id, a := range s.agencies {
		agencies = append(agencies, agencyItem{ID: id, Name: a.Name})
	}
	s.mu.RUnlock()
	sort.Slice(agencies, func(i, j int) bool { return agencies[i].ID < agencies[j].ID })
	out["agencies"] = agencies
	return out
}

// isSuperRequest re-checks the caller's role inside a handler (the routes
// are also wrapped in requireSuperAdmin).
func (s *Server) isSuperRequest(r *http.Request) bool {
	_, isSuper := s.callerAgency(r)
	return isSuper
}

func (s *Server) handleGetTakMaster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.isSuperRequest(r) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	json.NewEncoder(w).Encode(s.takMasterStatusJSON())
}

func (s *Server) handleUpdateTakMaster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.isSuperRequest(r) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	var req struct {
		Host             string `json:"host"`
		StreamPort       int    `json:"stream_port"`
		EnrollPort       int    `json:"enroll_port"`
		APIPort          int    `json:"api_port"`
		Username         string `json:"username"`
		Password         string `json:"password"`
		Callsign         string `json:"callsign"`
		Enabled          bool   `json:"enabled"`
		Remove           bool   `json:"remove"`
		TrustFingerprint string `json:"trust_fingerprint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	m := s.tak()
	if req.Remove {
		if mc := s.takMasterConn(); mc != nil {
			s.withdrawFromMaster(mc)
		}
		s.stopTakMaster()
		m.mu.Lock()
		m.masterCfg = nil
		m.mu.Unlock()
		if err := s.saveTakMaster(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.lmrMarkersChanged()
		log.Printf("TAK [master]: connection removed")
		json.NewEncoder(w).Encode(s.takMasterStatusJSON())
		return
	}

	m.mu.Lock()
	var cfg TakConfig
	existed := m.masterCfg != nil
	if existed {
		cfg = *m.masterCfg
	}
	m.mu.Unlock()
	host := strings.TrimSpace(req.Host)
	if host == "" {
		writeJSONError(w, http.StatusBadRequest, "TAK server address is required")
		return
	}
	username := strings.TrimSpace(req.Username)
	reEnroll := req.Password != "" || !existed || host != cfg.Host || username != cfg.Username || req.EnrollPort != cfg.EnrollPort
	cfg.AgencyID, cfg.Host, cfg.Enabled, cfg.Managed = "", host, req.Enabled, false
	cfg.StreamPort, cfg.EnrollPort, cfg.APIPort = req.StreamPort, req.EnrollPort, req.APIPort
	if cfg.StreamPort <= 0 {
		cfg.StreamPort = 8089
	}
	if cfg.EnrollPort <= 0 {
		cfg.EnrollPort = 8446
	}
	if cfg.APIPort <= 0 {
		cfg.APIPort = 8443
	}
	cfg.Username = username
	cfg.Callsign = nonEmpty(strings.TrimSpace(req.Callsign), "MeshRelay")
	if cfg.UID == "" {
		cfg.UID = "MeshRelay-Master-" + randomHex(3)
	}
	if err := takHostAllowed(cfg.Host, cfg.StreamPort, cfg.EnrollPort, cfg.APIPort); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if reEnroll {
		if req.Password == "" {
			writeJSONError(w, http.StatusBadRequest, "Enter the TAK password to enroll for a certificate")
			return
		}
		// Super admin only, so a TAK server on this machine is allowed, as
		// for a super admin's agency connection.
		if !takDashboardEnroll(w, "master", &cfg, req.Password, req.TrustFingerprint, true) {
			return
		}
	}
	m.mu.Lock()
	c := cfg
	m.masterCfg = &c
	m.mu.Unlock()
	if err := s.saveTakMaster(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.Enabled {
		s.startTakMaster(cfg)
		time.Sleep(1500 * time.Millisecond) // let the first connect attempt report
	} else {
		if mc := s.takMasterConn(); mc != nil {
			s.withdrawFromMaster(mc)
		}
		s.stopTakMaster()
		s.lmrMarkersChanged()
	}
	json.NewEncoder(w).Encode(s.takMasterStatusJSON())
}

func (s *Server) handleGetTakMasterGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.isSuperRequest(r) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	m := s.tak()
	m.mu.Lock()
	var cfg TakConfig
	ok := m.masterCfg != nil
	if ok {
		cfg = *m.masterCfg
	}
	m.mu.Unlock()
	if !ok || cfg.CertPEM == "" {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	groups, err := takListGroups(&cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}

func (s *Server) handleUpdateTakMasterGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.isSuperRequest(r) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	var req struct {
		Groups []string `json:"groups"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	m := s.tak()
	m.mu.Lock()
	var cfg TakConfig
	ok := m.masterCfg != nil && m.masterCfg.CertPEM != ""
	if ok && len(req.Groups) > 0 {
		m.masterCfg.Groups = req.Groups
		cfg = *m.masterCfg
		if tc := m.master; tc != nil {
			tc.mu.Lock()
			tc.cfg.Groups = req.Groups
			tc.mu.Unlock()
		}
	}
	m.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	if len(req.Groups) == 0 {
		writeJSONError(w, http.StatusBadRequest, "Pick at least one group")
		return
	}
	if err := takApplyGroups(&cfg); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.saveTakMaster()
	log.Printf("TAK [master]: active groups set to %q", req.Groups)
	groups, err := takListGroups(&cfg)
	if err != nil {
		groups = nil
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}

// handleUpdateTakShareMaster sets an agency's "Share with the master TAK
// server" switch: that agency's admin, or a super admin for any agency.
//
//	PUT {"share": true}                       (the caller's own agency)
//	PUT {"agency_id": "alpha", "share": true} (super admin)
func (s *Server) handleUpdateTakShareMaster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID string `json:"agency_id"`
		Share    *bool  `json:"share"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Share == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.AgencyID != "" && req.AgencyID != agencyID {
		if !isSuper {
			writeJSONError(w, http.StatusForbidden, "you can only change your own agency's setting")
			return
		}
		agencyID = req.AgencyID
	}
	home := s.takMasterHome()
	if !*req.Share && agencyID == home {
		writeJSONError(w, http.StatusConflict, "This agency is the master TAK server's home agency, so it always takes part. A super admin can choose another home agency first.")
		return
	}
	was := s.agencySharesMaster(agencyID)
	s.mu.Lock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	a.ShareMasterTAK = *req.Share
	s.mu.Unlock()
	s.mu.RLock()
	err := s.saveAgencies()
	s.mu.RUnlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	if now := s.agencySharesMaster(agencyID); was != now {
		if now {
			s.joinMaster(agencyID)
		} else {
			s.leaveMaster(agencyID)
		}
		s.lmrMarkersChanged()
	}
	log.Printf("TAK [master]: agency %q sharing %s (by %s)", agencyID,
		map[bool]string{true: "ON", false: "off"}[*req.Share], map[bool]string{true: "super admin", false: "agency admin"}[isSuper])
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

// handleUpdateTakMasterHome sets the master's home agency (super admin):
//
//	PUT {"agency_id": "alpha"}   ("" = the fallback)
//
// An agency that took part only as the home agency leaves; the new one joins.
func (s *Server) handleUpdateTakMasterHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.isSuperRequest(r) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	var req struct {
		AgencyID *string `json:"agency_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.AgencyID == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	want := strings.TrimSpace(*req.AgencyID)
	if want != "" {
		s.mu.RLock()
		ok := s.agencies[want] != nil
		s.mu.RUnlock()
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agency not found")
			return
		}
	}
	before := map[string]bool{}
	for _, sh := range s.takMasterSharing() {
		before[sh.Agency] = true
	}
	m := s.tak()
	m.mu.Lock()
	if m.masterCfg == nil {
		m.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest, "Set up the master TAK server first")
		return
	}
	c := *m.masterCfg // replaced, not changed in place: readers hold the old copy
	c.HomeAgency = want
	m.masterCfg = &c
	if tc := m.master; tc != nil {
		tc.mu.Lock()
		tc.cfg.HomeAgency = want
		tc.mu.Unlock()
	}
	m.mu.Unlock()
	if err := s.saveTakMaster(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	after := map[string]bool{}
	for _, sh := range s.takMasterSharing() {
		after[sh.Agency] = true
	}
	changed := false
	for a := range before {
		if !after[a] {
			s.leaveMaster(a)
			changed = true
		}
	}
	for a := range after {
		if !before[a] {
			s.joinMaster(a)
			changed = true
		}
	}
	if changed {
		s.lmrMarkersChanged()
	}
	home, how := s.takMasterHomeWhy()
	log.Printf("TAK [master]: home agency set to %q (in effect %q, %s)", want, home, how)
	json.NewEncoder(w).Encode(s.takMasterStatusJSON())
}
