// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// pipeTak is a TAK connection whose stream is an in-memory pipe: what it
// writes is collected.
type pipeTak struct {
	tc  *takConn
	mu  sync.Mutex
	out []string
}

func newPipeTak(t *testing.T, s *Server, cfg TakConfig, master bool) *pipeTak {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	p := &pipeTak{}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := theirs.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.out = append(p.out, string(buf[:n]))
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	p.tc = &takConn{s: s, cfg: cfg, master: master, conn: ours, status: "connected",
		units: map[string]*TakUnit{}, stop: make(chan struct{})}
	return p
}

// take returns (and forgets) everything written so far.
func (p *pipeTak) take(t *testing.T) string {
	t.Helper()
	marker := fmt.Sprintf("<sync n=%q/>", time.Now().String())
	p.tc.send(marker)
	waitFor(t, "pipe sync", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.out) > 0 && strings.Contains(p.out[len(p.out)-1], marker)
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	all := strings.ReplaceAll(strings.Join(p.out, ""), marker, "")
	p.out = nil
	return all
}

func (p *pipeTak) feed(t *testing.T, cot string) {
	t.Helper()
	var ev cotEvent
	if err := xml.Unmarshal([]byte(cot), &ev); err != nil {
		t.Fatalf("bad CoT: %v", err)
	}
	p.tc.handle(&ev)
}

const masterUID = "MeshRelay-Master-t1"

// masterRig: agencies alpha and bravo share with the master, charlie does
// not. Each has a console; the master's stream is a pipe.
type masterRig struct {
	s        *Server
	master   *pipeTak
	consoles map[string]*DispatchConn
}

func newMasterRig(t *testing.T) *masterRig {
	t.Helper()
	s, _ := newFloorTestServer(t)
	dir := t.TempDir()
	s.config.AgenciesFile = filepath.Join(dir, "agencies.json")
	s.activityLog = NewActivityLog(filepath.Join(dir, "activity.json"), 100)
	s.agencies = map[string]*Agency{
		"alpha":   {ID: "alpha", Name: "Alpha", ShareMasterTAK: true},
		"bravo":   {ID: "bravo", Name: "Bravo", ShareMasterTAK: true},
		"charlie": {ID: "charlie", Name: "Charlie"},
	}
	s.dispatchConns = map[string]*DispatchConn{}
	r := &masterRig{s: s, consoles: map[string]*DispatchConn{}}
	for i, a := range []string{"alpha", "bravo", "charlie"} {
		key := userKey(a, "desk")
		s.clients[key] = &Client{Username: "desk", AgencyID: a, Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 41000 + i}, LastHeartbeat: time.Now()}
		dc := &DispatchConn{Send: make(chan []byte, 64), Text: make(chan []byte, 256)}
		s.dispatchConns[key] = dc
		r.consoles[a] = dc
		medic := userKey(a, "medic")
		s.clients[medic] = &Client{Username: "medic", AgencyID: a, Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 3), Port: 42000 + i},
			Loc: &UnitLocation{Lat: 41.5, Lon: -74.5}}
	}
	t.Cleanup(func() {
		activeEmergMu.Lock()
		for k := range activeEmerg {
			for _, a := range []string{"alpha|", "bravo|", "charlie|"} {
				if strings.HasPrefix(k, a) {
					delete(activeEmerg, k)
				}
			}
		}
		activeEmergMu.Unlock()
	})
	cfg := TakConfig{Host: "tak.example.org", StreamPort: 8089, UID: masterUID, Callsign: "MeshRelay", Enabled: true}
	r.master = newPipeTak(t, s, cfg, true)
	m := s.tak()
	m.mu.Lock()
	m.master = r.master.tc
	m.masterCfg = &cfg
	m.mu.Unlock()
	return r
}

// ownConn gives agency its own TAK connection (a pipe) to host:port.
func (r *masterRig) ownConn(t *testing.T, agency, host string, port int) *pipeTak {
	cfg := TakConfig{AgencyID: agency, Host: host, StreamPort: port, UID: "MeshRelay-Dispatch-" + agency, Callsign: "Dispatch", Enabled: true}
	p := newPipeTak(t, r.s, cfg, false)
	m := r.s.tak()
	m.mu.Lock()
	m.conns[agency] = p.tc
	m.cfgs[agency] = cfg
	m.mu.Unlock()
	return p
}

// consoleMsgs drains what agency's console got.
func (r *masterRig) consoleMsgs(agency string) []map[string]interface{} {
	var out []map[string]interface{}
	dc := r.consoles[agency]
	for {
		select {
		case b := <-dc.Text:
			var m map[string]interface{}
			json.Unmarshal(b, &m)
			out = append(out, m)
		default:
			return out
		}
	}
}

func emergenciesFor(msgs []map[string]interface{}, user string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, m := range msgs {
		if m["type"] == "emergency" && m["user"] == user {
			out = append(out, m)
		}
	}
	return out
}

func pli(uid, callsign string) string {
	return `<event version="2.0" type="a-f-G-U-C" uid="` + uid + `" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.70" lon="-74.70"/><detail><contact callsign="` + callsign + `"/><__group name="Cyan" role="Team Member"/></detail></event>`
}

func geoChat(fromUID, fromCallsign, toUID, text string) string {
	return `<event version="2.0" type="b-t-f" uid="GeoChat.` + fromUID + `.x.1" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0"/><detail><__chat chatroom="x" id="` + toUID + `" senderCallsign="` + fromCallsign + `"><chatgrp uid0="` + fromUID + `" uid1="` + toUID + `"/></__chat><remarks>` + text + `</remarks></detail></event>`
}

func countIn(doc, sub string) int { return strings.Count(doc, sub) }

// --- inbound -------------------------------------------------------------------

func TestTakMasterUnitsOnlyForSharingAgencies(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, pli("ANDROID-m1", "Echo-1"))
	for _, a := range []string{"alpha", "bravo"} {
		u := r.s.takUnitsFor(a)
		if len(u) != 1 || u[0]["uid"] != "ANDROID-m1" || u[0]["source"] != "master" || u[0]["agency"] != a {
			t.Fatalf("%s units: %v", a, u)
		}
	}
	if u := r.s.takUnitsFor("charlie"); len(u) != 0 {
		t.Fatalf("non-sharing agency got master units: %v", u)
	}
	if u := r.s.takUnitsFor(""); len(u) != 1 {
		t.Fatalf("all-agency console should list a master unit once: %v", u)
	}
	// The master's own contact echoed back is not a TAK unit.
	r.master.feed(t, pli(masterUID, "MeshRelay"))
	if u := r.s.takUnitsFor("alpha"); len(u) != 1 {
		t.Fatalf("own contact listed as a unit: %v", u)
	}
	// Sharing off: gone at once.
	r.s.agencies["bravo"].ShareMasterTAK = false
	if u := r.s.takUnitsFor("bravo"); len(u) != 0 {
		t.Fatalf("units after sharing off: %v", u)
	}
}

func TestTakMasterEmergencyOnlyForSharingAgencies(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, atakAlert("ANDROID-0123456789abcdef", "Echo-1", true))
	for _, a := range []string{"alpha", "bravo"} {
		e := emergenciesFor(r.consoleMsgs(a), "Echo-1 (TAK)")
		if len(e) != 1 || e[0]["agency"] != a || e[0]["active"] != true || e[0]["tak_source"] != "master" {
			t.Fatalf("%s console: %v", a, e)
		}
	}
	if e := emergenciesFor(r.consoleMsgs("charlie"), "Echo-1 (TAK)"); len(e) != 0 {
		t.Fatalf("non-sharing agency got the master's alert: %v", e)
	}
	if len(activeEmergencies("charlie")) != 0 {
		t.Fatal("remembered for charlie")
	}
}

// sentChat: the one GeoChat in doc, as a TAK server reads it.
type sentChat struct {
	Type   string `xml:"type,attr"`
	Detail struct {
		Chat struct {
			Chatroom string `xml:"chatroom,attr"`
			ID       string `xml:"id,attr"`
		} `xml:"__chat"`
		Remarks string `xml:"remarks"`
		Marti   *struct {
			Dest []struct {
				UID      *string `xml:"uid,attr"`
				Callsign *string `xml:"callsign,attr"`
			} `xml:"dest"`
		} `xml:"marti"`
	} `xml:"detail"`
}

func oneChat(t *testing.T, doc string) sentChat {
	t.Helper()
	if n := countIn(doc, `type="b-t-f"`); n != 1 {
		t.Fatalf("%d GeoChats in %s", n, doc)
	}
	i := strings.LastIndex(doc[:strings.Index(doc, `type="b-t-f"`)], "<event ")
	var c sentChat
	if err := xml.Unmarshal([]byte(doc[i:i+strings.Index(doc[i:], "</event>")+8]), &c); err != nil {
		t.Fatalf("bad GeoChat %s: %v", doc, err)
	}
	return c
}

// wantDest: the chat goes to the TAK client uid only — one <marti><dest>
// by UID (not by callsign, which two users may share).
func wantDest(t *testing.T, c sentChat, uid, chatroom string) {
	t.Helper()
	m := c.Detail.Marti
	if m == nil || len(m.Dest) != 1 || m.Dest[0].UID == nil || *m.Dest[0].UID != uid || m.Dest[0].Callsign != nil {
		t.Fatalf("dest: %+v, want uid %q only", m, uid)
	}
	if c.Detail.Chat.ID != uid || c.Detail.Chat.Chatroom != chatroom {
		t.Fatalf("__chat: %+v", c.Detail.Chat)
	}
}

// A console's direct message reaches the addressee only: the agency's own
// connection and the master both address it with <marti><dest uid>, XML
// escaped (a TAK server without one hands it to the whole group, and every
// ATAK files it under the addressee's conversation).
func TestTakConsoleChatAddressedToRecipient(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak.alpha.example.org", 8089)
	const uid, callsign = `ANDROID-a&b"1`, `Echo <1> & "Co"`
	pliEsc := pli(xmlEsc(uid), xmlEsc(callsign))
	own.feed(t, pliEsc)
	r.master.feed(t, pliEsc)
	own.take(t)
	r.master.take(t)

	r.s.consoleSendTakMessage(r.s.clients[userKey("alpha", "desk")], "alpha", uid, "own path", "", "")
	c := oneChat(t, own.take(t))
	wantDest(t, c, uid, callsign)
	if c.Detail.Remarks != "own path" {
		t.Fatalf("remarks %q", c.Detail.Remarks)
	}

	r.write(t, "bravo", uid, "master path")
	c = oneChat(t, r.master.take(t))
	wantDest(t, c, uid, callsign)
	if !strings.HasSuffix(c.Detail.Remarks, "master path") {
		t.Fatalf("remarks %q", c.Detail.Remarks)
	}

	// A user the server does not list (callsign unknown) is still addressed
	// by UID.
	r.write(t, "bravo", "ANDROID-unlisted", "hi")
	wantDest(t, oneChat(t, r.master.take(t)), "ANDROID-unlisted", "ANDROID-unlisted")
}

// --- messages ---------------------------------------------------------------------
//
// In the rig the home agency is alpha (no "default": the first agency ID).

// write: agency's console messages the master TAK user uid.
func (r *masterRig) write(t *testing.T, agency, uid, text string) {
	t.Helper()
	r.s.consoleSendTakMessage(r.s.clients[userKey(agency, "desk")], agency, uid, text, "master", "")
}

// inbound: what agency's message history got from TAK.
func (r *masterRig) inbound(agency string) []dispatchMsg {
	var out []dispatchMsg
	for _, m := range r.s.messageHistory(agency) {
		if m.Dir == "in" {
			out = append(out, m)
		}
	}
	return out
}

func (r *masterRig) wantInbound(t *testing.T, want map[string]int) {
	t.Helper()
	for _, a := range []string{"alpha", "bravo", "charlie"} {
		if got := len(r.inbound(a)); got != want[a] {
			t.Fatalf("%s got %d messages from TAK, want %d: %+v", a, got, want[a], r.inbound(a))
		}
	}
}

func TestTakMasterReplyGoesToTheAgencyThatWrote(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, pli("ANDROID-m1", "Echo-1"))
	r.write(t, "bravo", "ANDROID-m1", "Status?")
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "Need water"))
	r.wantInbound(t, map[string]int{"bravo": 1})
	in := r.inbound("bravo")[0]
	if in.From != "Echo-1" || in.TakUID != "ANDROID-m1" || in.TakSource != "master" || in.Agency != "bravo" || in.Text != "Need water" {
		t.Fatalf("message: %+v", in)
	}
	// Another TAK user nobody wrote to: the home agency only.
	r.master.feed(t, geoChat("ANDROID-m2", "Echo-2", masterUID, "hello"))
	r.wantInbound(t, map[string]int{"alpha": 1, "bravo": 1})
}

func TestTakMasterReplyToBothAgenciesThatWrote(t *testing.T) {
	r := newMasterRig(t)
	r.write(t, "alpha", "ANDROID-m1", "from alpha")
	r.write(t, "bravo", "ANDROID-m1", "from bravo")
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "both"))
	r.wantInbound(t, map[string]int{"alpha": 1, "bravo": 1})
}

func TestTakMasterNobodyWroteGoesToHomeOnly(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "anyone?"))
	r.wantInbound(t, map[string]int{"alpha": 1})
	// The home agency chosen by the super admin.
	r.s.tak().masterCfg.HomeAgency = "bravo"
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "again"))
	r.wantInbound(t, map[string]int{"alpha": 1, "bravo": 1})
}

func TestTakMasterOldEntryGoesHome(t *testing.T) {
	r := newMasterRig(t)
	r.master.tc.noteChatPeer("ANDROID-m1", "bravo", time.Now().Add(-takMasterChatKeep-time.Minute))
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "late reply"))
	r.wantInbound(t, map[string]int{"alpha": 1})
	// Just inside the window: still bravo's.
	r.master.tc.noteChatPeer("ANDROID-m1", "bravo", time.Now().Add(-takMasterChatKeep+time.Minute))
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "reply"))
	r.wantInbound(t, map[string]int{"alpha": 1, "bravo": 1})
	// The 30 s prune forgets expired entries.
	r.master.tc.noteChatPeer("ANDROID-m9", "bravo", time.Now().Add(-takMasterChatKeep-time.Minute))
	r.master.tc.pruneStale()
	r.master.tc.mu.Lock()
	_, left := r.master.tc.chatPeers["ANDROID-m9"]
	r.master.tc.mu.Unlock()
	if left {
		t.Fatal("expired routing entry kept")
	}
}

func TestTakMasterStoppedSharingNotDelivered(t *testing.T) {
	r := newMasterRig(t)
	r.write(t, "bravo", "ANDROID-m1", "hi")
	r.write(t, "charlie", "ANDROID-m1", "not sharing") // not sent, not recorded
	r.s.agencies["bravo"].ShareMasterTAK = false
	r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", masterUID, "reply"))
	r.wantInbound(t, map[string]int{"alpha": 1})
	if p := r.master.tc.chatPeersOf("ANDROID-m1", time.Now()); strings.Join(p, ",") != "bravo" {
		t.Fatalf("routing record: %v", p)
	}
	// Switched off through the dashboard: dropped from the record too.
	r.s.agencies["bravo"].ShareMasterTAK = true
	addTestSession(r.s, "bravoadmin", "chief", "agency_admin", "bravo")
	if rec := doReq(r.s.webHandler(), "PUT", "/api/my-agency/tak/share-master", "bravoadmin", `{"share":false}`); rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if p := r.master.tc.chatPeersOf("ANDROID-m1", time.Now()); len(p) != 0 {
		t.Fatalf("withdrawn agency still in the record: %v", p)
	}
}

func TestTakMasterMessageToAnotherUIDDropped(t *testing.T) {
	r := newMasterRig(t)
	r.write(t, "bravo", "ANDROID-m1", "hi")
	for _, to := range []string{masterUID + ".alpha", masterUID + ".bravo", masterUID + "x", "All Chat Rooms", "MeshRelay-Dispatch-alpha", ""} {
		r.master.feed(t, geoChat("ANDROID-m1", "Echo-1", to, "hello "+to))
	}
	r.wantInbound(t, map[string]int{})
	// Not by the contact's name either (the master matches the UID only).
	r.master.feed(t, strings.Replace(geoChat("ANDROID-m1", "Echo-1", "", "by name"), `chatroom="x"`, `chatroom="MeshRelay"`, 1))
	r.wantInbound(t, map[string]int{})
}

// An agency connection whose contact has the master's name (same TAK
// server) does not take the master's messages.
func TestTakAgencyConnIgnoresMasterContactChats(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "bravo", "tak.example.org", 8089)
	own.tc.cfg.Callsign = "MeshRelay"
	chat := strings.Replace(geoChat("ANDROID-m1", "Echo-1", masterUID, "for the master"), `chatroom="x"`, `chatroom="MeshRelay"`, 1)
	own.feed(t, chat)
	r.master.feed(t, chat)
	r.wantInbound(t, map[string]int{"alpha": 1})
	// Its own name still works for a chat with no matching UID (as before).
	own.feed(t, strings.Replace(geoChat("ANDROID-m1", "Echo-1", "", "by name"), `chatroom="x"`, `chatroom="MeshRelay"`, 1))
	r.wantInbound(t, map[string]int{"alpha": 1, "bravo": 1})
}

func TestTakMasterConsoleMessageSentFromMasterContact(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, pli("ANDROID-m1", "Echo-1"))
	r.master.take(t)
	r.write(t, "bravo", "ANDROID-m1", "Water en route")
	got := r.master.take(t)
	if !strings.Contains(got, `uid0="`+masterUID+`"`) || !strings.Contains(got, `senderCallsign="MeshRelay"`) ||
		!strings.Contains(got, `uid1="ANDROID-m1"`) || !strings.Contains(got, ">[Bravo] Water en route</remarks>") {
		t.Fatalf("chat on the master: %s", got)
	}
	h := r.s.messageHistory("bravo")
	if len(h) != 1 || h[0].Sent != 1 || h[0].TakSource != "master" || h[0].To != "Echo-1 (TAK)" || h[0].Text != "Water en route" {
		t.Fatalf("history: %+v", h)
	}
	// A console that did not name the source still reaches a user who is
	// only on the master.
	r.s.consoleSendTakMessage(r.s.clients[userKey("bravo", "desk")], "bravo", "ANDROID-m1", "again", "", "")
	if got := r.master.take(t); !strings.Contains(got, "[Bravo] again") {
		t.Fatal("unlabelled message to a master-only user not sent")
	}
	// Another agency's console cannot send as bravo; a non-sharing agency
	// sends nothing to the master.
	r.s.consoleSendTakMessage(r.s.clients[userKey("alpha", "desk")], "bravo", "ANDROID-m1", "spoof", "master", "")
	r.write(t, "charlie", "ANDROID-m1", "not sharing")
	if got := r.master.take(t); strings.Contains(got, "spoof") || strings.Contains(got, "not sharing") {
		t.Fatalf("sent: %s", got)
	}
	if h := r.s.messageHistory("charlie"); len(h) != 1 || h[0].Sent != 0 {
		t.Fatalf("charlie's attempt should be recorded as not sent: %+v", h)
	}
	if p := r.master.tc.chatPeersOf("ANDROID-m1", time.Now()); strings.Join(p, ",") != "bravo" {
		t.Fatalf("routing record: %v", p)
	}
}

func TestTakMasterPrefixWithinMaxLength(t *testing.T) {
	long := strings.Repeat("é", maxMessageLen) // 2 bytes each
	out := takMasterPrefixed("Bravo", long)
	if len(out) > maxMessageLen || !strings.HasPrefix(out, "[Bravo] ") || !utf8.ValidString(out) {
		t.Fatalf("len %d, valid %v", len(out), utf8.ValidString(out))
	}
	if got := takMasterPrefixed("Bravo", "hi"); got != "[Bravo] hi" {
		t.Fatalf("short: %q", got)
	}
	r := newMasterRig(t)
	r.master.take(t)
	r.write(t, "bravo", "ANDROID-m1", strings.Repeat("x", maxMessageLen+50))
	got := r.master.take(t)
	if !strings.Contains(got, "[Bravo] "+strings.Repeat("x", maxMessageLen-len("[Bravo] "))+"</remarks>") {
		t.Fatal("long message not clipped to the limit with its prefix")
	}
	if h := r.s.messageHistory("bravo"); len(h) != 1 || len(h[0].Text) != maxMessageLen || strings.HasPrefix(h[0].Text, "[") {
		t.Fatalf("history keeps the text as typed: %d", len(h[0].Text))
	}
}

func TestTakMasterPresenceIsOneContact(t *testing.T) {
	r := newMasterRig(t)
	doc := r.master.tc.presenceCoT()
	if countIn(doc, "<event") != 1 || !strings.Contains(doc, `uid="`+masterUID+`"`) || !strings.Contains(doc, `callsign="MeshRelay"`) {
		t.Fatalf("presence: %s", doc)
	}
}

// On connect the master deletes the per-agency contacts an earlier version
// announced, for every agency that exists.
func TestTakMasterOldContactCleanupOnConnect(t *testing.T) {
	r := newMasterRig(t)
	r.master.tc.masterConnected()
	got := r.master.take(t)
	for _, a := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(got, `<link uid="`+masterUID+`.`+a+`" relation="none"`) {
			t.Fatalf("no delete for %s: %s", a, got)
		}
	}
	if countIn(got, `type="t-x-d-d"`) != 3 || strings.Contains(got, `<link uid="`+masterUID+`" `) {
		t.Fatalf("deletes: %s", got)
	}
}

// --- home agency ---------------------------------------------------------------------

func TestTakMasterHomeFallback(t *testing.T) {
	r := newMasterRig(t)
	cfg := r.s.tak().masterCfg
	check := func(want, wantHow string) {
		t.Helper()
		got, how := r.s.takMasterHomeWhy()
		st := r.s.takMasterStatusJSON()
		if got != want || how != wantHow || st["effective_home_agency"] != want {
			t.Fatalf("home %q (%s), status %v; want %q (%s)", got, how, st["effective_home_agency"], want, wantHow)
		}
	}
	check("alpha", "fallback") // no field (a tak_master.json from a v4.10.0 pre-release), no "default"
	cfg.HomeAgency = "charlie"
	check("charlie", "chosen")
	cfg.HomeAgency = "gone"
	check("alpha", "fallback")
	r.s.agencies["default"] = &Agency{ID: "default", Name: "Default"}
	check("default", "fallback")
	delete(r.s.agencies, "default")

	// The home agency takes part whatever its switch says: charlie, not
	// sharing, sees master units and its switch reads on, locked.
	cfg.HomeAgency = "charlie"
	r.master.feed(t, pli("ANDROID-m1", "Echo-1"))
	if u := r.s.takUnitsFor("charlie"); len(u) != 1 {
		t.Fatalf("home agency units: %v", u)
	}
	st := r.s.takStatusJSON("charlie")
	m, _ := st["master"].(map[string]interface{})
	if st["share_master"] != true || st["master_home"] != true || m["home_agency_name"] != "Charlie" {
		t.Fatalf("home status: %v", st)
	}
	if st := r.s.takStatusJSON("bravo"); st["master_home"] != false {
		t.Fatalf("bravo status: %v", st)
	}
	addTestSession(r.s, "charlieadmin", "chief", "agency_admin", "charlie")
	addTestSession(r.s, "super", "root", "super_admin", "")
	h := r.s.webHandler()
	if rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "charlieadmin", `{"share":false}`); rec.Code != http.StatusConflict {
		t.Fatalf("home switch off = %d %s", rec.Code, rec.Body)
	}
	if rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "super", `{"agency_id":"charlie","share":false}`); rec.Code != http.StatusConflict {
		t.Fatalf("home switch off by super = %d", rec.Code)
	}
	if u := r.s.takUnitsFor("charlie"); len(u) != 1 {
		t.Fatal("home agency dropped")
	}
	// Loaded from a file without the field: the fallback, file kept as is.
	var loaded TakConfig
	if err := json.Unmarshal([]byte(`{"host":"tak.example.org","uid":"`+masterUID+`","callsign":"MeshRelay","enabled":false}`), &loaded); err != nil || loaded.HomeAgency != "" {
		t.Fatalf("old file: %+v %v", loaded, err)
	}
}

// The super admin moves the home agency: one that took part only as the
// home agency leaves (its alerts cancelled), the new one joins.
func TestTakMasterHomeChange(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir) // tak_master.json is written to the working directory
	t.Cleanup(func() { os.Chdir(old) })
	r := newMasterRig(t)
	addTestSession(r.s, "super", "root", "super_admin", "")
	addTestSession(r.s, "alphaadmin", "chief", "agency_admin", "alpha")
	h := r.s.webHandler()
	r.s.agencies["alpha"].ShareMasterTAK = false // alpha takes part as the home agency only
	r.s.raiseEmergency("charlie", "medic", 0)
	if g := r.master.take(t); strings.Contains(g, "MeshRelay-911-charlie") {
		t.Fatal("non-sharing agency's alert sent")
	}
	if rec := doReq(h, "PUT", "/api/tak-master/home", "alphaadmin", `{"agency_id":"charlie"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("agency admin set the home agency: %d", rec.Code)
	}
	if rec := doReq(h, "PUT", "/api/tak-master/home", "super", `{"agency_id":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agency = %d", rec.Code)
	}
	r.s.raiseEmergency("alpha", "medic", 0)
	r.master.take(t)
	rec := doReq(h, "PUT", "/api/tak-master/home", "super", `{"agency_id":"charlie"}`)
	if rec.Code != 200 {
		t.Fatalf("PUT home = %d %s", rec.Code, rec.Body)
	}
	g := r.master.take(t)
	if alertCount(g, "charlie", "medic", true) != 1 || alertCount(g, "alpha", "medic", false) != 1 {
		t.Fatalf("home change on the master: %s", g)
	}
	var st map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st["home_agency"] != "charlie" || st["effective_home_agency"] != "charlie" {
		t.Fatalf("status: %v", st)
	}
	raw, _ := os.ReadFile(takMasterFile)
	if !strings.Contains(string(raw), `"home_agency": "charlie"`) && !strings.Contains(string(raw), `"home_agency":"charlie"`) {
		t.Fatalf("not saved: %s", raw)
	}
	r.s.clearEmergency("alpha", "medic", "medic")
	r.s.clearEmergency("charlie", "medic", "medic")
}

// --- outbound alerts and per-connection dedupe ----------------------------------------

func alertCount(doc, agency, user string, active bool) int {
	typ := "b-a-o-can"
	if active {
		typ = "b-a-o-tbl"
	}
	return countIn(doc, `type="`+typ+`" uid="MeshRelay-911-`+agency+`-`+user+`"`)
}

func TestTakMasterAlertsOnlyForSharingAgencies(t *testing.T) {
	r := newMasterRig(t)
	r.s.raiseEmergency("alpha", "medic", 0)
	r.s.raiseEmergency("charlie", "medic", 0)
	got := r.master.take(t)
	// One contact: the alert names the unit with its agency and links to
	// no map item (not the contact: ATAK would draw a line from it).
	if alertCount(got, "alpha", "medic", true) != 1 || strings.Contains(got, `<link uid="`+masterUID+`"`) ||
		!strings.Contains(got, `<link uid="MeshRelay-911-alpha-medic.unit"/>`) ||
		!strings.Contains(got, `callsign="medic (Alpha)-Alert"`) || !strings.Contains(got, "MeshRelay emergency: medic (Alpha)") {
		t.Fatalf("alpha's alert on the master: %s", got)
	}
	if strings.Contains(got, "MeshRelay-911-charlie") {
		t.Fatal("non-sharing agency's alert sent to the master")
	}
	r.s.clearEmergency("alpha", "medic", "medic")
	r.s.clearEmergency("charlie", "medic", "medic")
	got = r.master.take(t)
	if alertCount(got, "alpha", "medic", false) != 1 || strings.Contains(got, "charlie") {
		t.Fatalf("cancel: %s", got)
	}
}

// The agency connection and the master (different TAK servers) each keep
// their own one-alert-per-unit state: the phone's own alert seen on one does
// not withdraw the server's alert on the other.
func TestTakMasterAlertDedupeIsPerConnection(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak.alpha.example.org", 8089)
	r.s.raiseEmergency("alpha", "medic", 0)
	if g := own.take(t); alertCount(g, "alpha", "medic", true) != 1 {
		t.Fatalf("own connection: %s", g)
	}
	if g := r.master.take(t); alertCount(g, "alpha", "medic", true) != 1 {
		t.Fatalf("master: %s", g)
	}
	// The phone's own alert arrives on the agency's TAK server only.
	own.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	if g := own.take(t); alertCount(g, "alpha", "medic", false) != 1 {
		t.Fatalf("own connection should withdraw its alert: %s", g)
	}
	if g := r.master.take(t); g != "" {
		t.Fatalf("master touched by the other connection's phone alert: %s", g)
	}
	// End: the agency connection has nothing out (the phone held it), the
	// master cancels its own once.
	r.s.clearEmergency("alpha", "medic", "medic")
	if g := own.take(t); strings.Contains(g, "MeshRelay-911-") {
		t.Fatalf("own connection sent: %s", g)
	}
	if g := r.master.take(t); alertCount(g, "alpha", "medic", false) != 1 {
		t.Fatalf("master cancel: %s", g)
	}

	// And the other way round: the phone's alert on the master.
	r.s.raiseEmergency("alpha", "medic", 0)
	own.take(t)
	r.master.take(t)
	r.master.feed(t, atakAlert("MeshRelay-1a2b3c4d", "MEDIC", true))
	if g := r.master.take(t); alertCount(g, "alpha", "medic", false) != 1 {
		t.Fatalf("master should withdraw: %s", g)
	}
	if g := own.take(t); g != "" {
		t.Fatalf("own connection touched: %s", g)
	}
	// The phone's alert does not show as a second emergency on any console.
	for _, a := range []string{"alpha", "bravo"} {
		if e := emergenciesFor(r.consoleMsgs(a), "MEDIC (TAK)"); len(e) != 0 {
			t.Fatalf("%s shows the phone alert: %v", a, e)
		}
	}
	// Phone stops: the master's alert goes out again (hold), once.
	r.master.tc.alertTick(time.Now().Add(takPhoneAlertHold + time.Second))
	if g := r.master.take(t); alertCount(g, "alpha", "medic", true) != 1 {
		t.Fatalf("master resend: %s", g)
	}
	r.s.clearEmergency("alpha", "medic", "medic")
}

// The phone alert arrives on the master before the MeshRelay emergency: shown
// on the sharing consoles, then taken off; the master sends no alert of its own.
func TestTakMasterPhoneAlertBeforeEmergency(t *testing.T) {
	r := newMasterRig(t)
	r.master.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	if e := emergenciesFor(r.consoleMsgs("alpha"), "medic (TAK)"); len(e) != 1 || e[0]["active"] != true {
		t.Fatalf("phone alert not shown: %v", e)
	}
	r.consoleMsgs("bravo")
	r.s.raiseEmergency("alpha", "medic", 0)
	for _, a := range []string{"alpha", "bravo"} {
		e := emergenciesFor(r.consoleMsgs(a), "medic (TAK)")
		if len(e) != 1 || e[0]["active"] != false {
			t.Fatalf("%s: phone alert not taken off: %v", a, e)
		}
	}
	if g := r.master.take(t); strings.Contains(g, "MeshRelay-911-alpha-medic") {
		t.Fatalf("master sent its own alert while the phone's is on: %s", g)
	}
	r.s.clearEmergency("alpha", "medic", "medic")
}

// --- same TAK server twice ------------------------------------------------------------

func TestTakSameServerDetection(t *testing.T) {
	r := newMasterRig(t)
	mk := func(host string, port int, fp string) *takConn {
		return &takConn{cfg: TakConfig{Host: host, StreamPort: port}, peerFP: fp}
	}
	m := mk("tak.example.org", 8089, "")
	cases := []struct {
		c    *takConn
		want bool
	}{
		{mk("TAK.Example.ORG", 8089, ""), true},
		{mk(" tak.example.org", 8089, ""), true},
		{mk("tak.example.org", 8090, ""), false},
		{mk("tak2.example.org", 8089, ""), false},
		{mk("192.0.2.10", 8089, ""), false},
	}
	for _, c := range cases {
		if got := takSameServer(c.c, m); got != c.want {
			t.Errorf("%q:%d same=%v, want %v", c.c.cfg.Host, c.c.cfg.StreamPort, got, c.want)
		}
	}
	// Same streaming certificate, different address: the same server.
	a, b := mk("192.0.2.10", 8089, "ab12"), mk("tak.example.org", 8089, "ab12")
	if !takSameServer(a, b) {
		t.Error("equal certificate fingerprints not treated as the same server")
	}
	if takSameServer(mk("192.0.2.10", 8089, "ab12"), mk("192.0.2.11", 8089, "cd34")) {
		t.Error("different certificates and hosts treated as the same")
	}
	if takSameServer(mk("", 0, ""), mk("", 0, "")) {
		t.Error("two unconfigured connections are not a match")
	}

	// In the rig: alpha's own connection goes to the master's server.
	r.ownConn(t, "alpha", "TAK.example.org", 8089)
	st := r.s.takStatusJSON("alpha")
	if st["same_server_as_master"] != true || st["share_master"] != true {
		t.Fatalf("alpha status: %v", st)
	}
	if st := r.s.takStatusJSON("bravo"); st["same_server_as_master"] != false {
		t.Fatalf("bravo status: %v", st)
	}
	if got := r.s.takMasterOutboundAgencies(); strings.Join(got, ",") != "bravo" {
		t.Fatalf("outbound via master: %v", got)
	}
}

func TestTakSameServerSendsOnceAndListsOnce(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak.example.org", 8089)

	// Alerts: once, through the agency's own connection.
	r.s.raiseEmergency("alpha", "medic", 0)
	r.s.raiseEmergency("bravo", "medic", 0)
	if g := own.take(t); alertCount(g, "alpha", "medic", true) != 1 {
		t.Fatalf("own connection: %s", g)
	}
	g := r.master.take(t)
	if strings.Contains(g, "MeshRelay-911-alpha") || alertCount(g, "bravo", "medic", true) != 1 {
		t.Fatalf("master: %s", g)
	}
	r.s.clearEmergency("alpha", "medic", "medic")
	r.s.clearEmergency("bravo", "medic", "medic")
	if g := r.master.take(t); strings.Contains(g, "MeshRelay-911-alpha") {
		t.Fatalf("master cancel for alpha: %s", g)
	}
	own.take(t)

	// Units: the same TAK user on both connections is listed once (from the
	// agency's own); one only on the master is listed with its source.
	own.feed(t, pli("ANDROID-both", "Both"))
	r.master.feed(t, pli("ANDROID-both", "Both"))
	r.master.feed(t, pli("ANDROID-monly", "OnlyMaster"))
	u := r.s.takUnitsFor("alpha")
	if len(u) != 2 {
		t.Fatalf("alpha units: %v", u)
	}
	for _, x := range u {
		if x["uid"] == "ANDROID-both" && x["source"] != nil {
			t.Fatalf("duplicate should be the agency's own: %v", x)
		}
		if x["uid"] == "ANDROID-monly" && x["source"] != "master" {
			t.Fatalf("master-only unit not labelled: %v", x)
		}
	}
	if u := r.s.takUnitsFor("bravo"); len(u) != 2 {
		t.Fatalf("bravo units: %v", u)
	}

	// The master's TAK alerts: not to alpha (its own connection has them).
	r.consoleMsgs("alpha")
	r.consoleMsgs("bravo")
	r.master.feed(t, atakAlert("ANDROID-0123456789abcdef", "Echo-1", true))
	if e := emergenciesFor(r.consoleMsgs("alpha"), "Echo-1 (TAK)"); len(e) != 0 {
		t.Fatalf("alpha got the master copy: %v", e)
	}
	if e := emergenciesFor(r.consoleMsgs("bravo"), "Echo-1 (TAK)"); len(e) != 1 {
		t.Fatalf("bravo: %v", e)
	}
}

func TestTakMasterLMRMarkersRouting(t *testing.T) {
	r := newMasterRig(t)
	r.s.lmrBridges = map[string]*LMRBridge{
		"ba": {ID: "ba", Agencies: []string{"bravo"}, Channel: 100, Enabled: true, ShowInATAK: true, Lat: f64(41), Lon: f64(-74)},
		"bc": {ID: "bc", Agencies: []string{"charlie"}, Channel: 101, Enabled: true, ShowInATAK: true, Lat: f64(41), Lon: f64(-74)},
		"bo": {ID: "bo", Agencies: []string{"alpha"}, Channel: 102, Enabled: true, ShowInATAK: false, Lat: f64(41), Lon: f64(-74)},
	}
	want := r.s.lmrDesiredMarkers()[takMasterTarget]
	if _, ok := want["ba"]; !ok || len(want) != 1 {
		t.Fatalf("master markers: %v", want)
	}
	// Over the deliver seam: published on the master, withdrawn when bravo
	// stops sharing.
	p := r.s.lmrMarkers()
	p.now = func() time.Time { return lmrTestNow }
	p.pass()
	if sh, _ := countMarker(r.master.take(t), "ba"); sh != 1 {
		t.Fatal("marker not published on the master")
	}
	r.s.agencies["bravo"].ShareMasterTAK = false
	p.pass()
	if _, del := countMarker(r.master.take(t), "ba"); del != 1 {
		t.Fatal("marker not withdrawn from the master after sharing off")
	}
	// Same server: bravo's marker goes on its own connection only.
	r.s.agencies["bravo"].ShareMasterTAK = true
	r.ownConn(t, "bravo", "tak.example.org", 8089)
	if _, ok := r.s.lmrDesiredMarkers()[takMasterTarget]["ba"]; ok {
		t.Fatal("same-server agency's marker also on the master")
	}
}

// --- switching sharing off ----------------------------------------------------------

func TestTakShareSwitchOffWithdraws(t *testing.T) {
	r := newMasterRig(t)
	addTestSession(r.s, "bravoadmin", "chief", "agency_admin", "bravo")
	h := r.s.webHandler()
	r.s.raiseEmergency("bravo", "medic", 0)
	r.master.feed(t, atakAlert("ANDROID-0123456789abcdef", "Echo-1", true))
	r.master.take(t)
	rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "bravoadmin", `{"share":false}`)
	if rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	g := r.master.take(t)
	if alertCount(g, "bravo", "medic", false) != 1 || strings.Contains(g, "t-x-d-d") {
		t.Fatalf("withdrawal on the master: %s", g)
	}
	for _, e := range activeEmergencies("bravo") {
		if e["tak_source"] == "master" {
			t.Fatalf("master alert left on bravo's consoles: %v", e)
		}
	}
	// The end of the emergency no longer goes to the master.
	r.s.clearEmergency("bravo", "medic", "medic")
	if g := r.master.take(t); strings.Contains(g, "MeshRelay-911-bravo") {
		t.Fatalf("sent after sharing off: %s", g)
	}
	// Switched on again during an emergency: it is sent at once.
	r.s.raiseEmergency("bravo", "medic", 0)
	r.master.take(t)
	if rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "bravoadmin", `{"share":true}`); rec.Code != 200 {
		t.Fatalf("PUT on = %d", rec.Code)
	}
	if g := r.master.take(t); alertCount(g, "bravo", "medic", true) != 1 {
		t.Fatalf("joining: %s", g)
	}
	r.s.clearEmergency("bravo", "medic", "medic")
}

// --- authorization ------------------------------------------------------------------

func TestTakMasterAuthorization(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir) // tak_master.json is written to the working directory
	t.Cleanup(func() { os.Chdir(old) })
	s := newWebTestServer(t)
	addTestSession(s, "emsadmin", "boss", "agency_admin", "ems")
	h := s.webHandler()

	masterCalls := []struct{ method, path, body string }{
		{"GET", "/api/tak-master", ""},
		{"PUT", "/api/tak-master", `{"host":"tak.example.org","username":"x","password":"y","enabled":true}`},
		{"PUT", "/api/tak-master", `{"remove":true}`},
		{"GET", "/api/tak-master/groups", ""},
		{"PUT", "/api/tak-master/groups", `{"groups":["Cyan"]}`},
		{"PUT", "/api/tak-master/home", `{"agency_id":"ems"}`},
	}
	for _, tok := range []string{"fireadmin", "firedispatch"} {
		for _, c := range masterCalls {
			if rec := doReq(h, c.method, c.path, tok, c.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", tok, c.method, c.path, rec.Code)
			}
		}
	}
	for _, c := range masterCalls {
		if rec := doReq(h, c.method, c.path, "", c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if s.tak().masterCfg != nil {
		t.Fatal("master configured by a non-super caller")
	}
	// The handlers check again on their own.
	for _, c := range masterCalls[:2] {
		req := httpReq(c.method, c.path, c.body, "fireadmin")
		w := newRecorder()
		if c.method == "GET" {
			s.handleGetTakMaster(w, req)
		} else {
			s.handleUpdateTakMaster(w, req)
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("handler %s %s for agency admin = %d", c.method, c.path, w.Code)
		}
	}

	// The per-agency switch: an agency admin changes only their own agency.
	rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "fireadmin", `{"agency_id":"ems","share":true}`)
	if rec.Code != http.StatusForbidden || s.agencies["ems"].ShareMasterTAK {
		t.Fatalf("fire admin changed ems: %d %s", rec.Code, rec.Body)
	}
	rec = doReq(h, "PUT", "/api/my-agency/tak/share-master", "firedispatch", `{"share":true}`)
	if rec.Code != http.StatusForbidden || s.agencies["fire"].ShareMasterTAK {
		t.Fatalf("dispatch account changed the switch: %d", rec.Code)
	}
	rec = doReq(h, "PUT", "/api/my-agency/tak/share-master", "fireadmin", `{"share":true}`)
	if rec.Code != 200 || !s.agencies["fire"].ShareMasterTAK || s.agencies["ems"].ShareMasterTAK {
		t.Fatalf("own switch: %d %s", rec.Code, rec.Body)
	}
	rec = doReq(h, "PUT", "/api/my-agency/tak/share-master", "fireadmin", `{"agency_id":"fire","share":false}`)
	if rec.Code != 200 || s.agencies["fire"].ShareMasterTAK {
		t.Fatalf("own switch by id: %d", rec.Code)
	}
	if rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "fireadmin", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing value = %d", rec.Code)
	}
	// Super admin: any agency.
	rec = doReq(h, "PUT", "/api/my-agency/tak/share-master", "super", `{"agency_id":"ems","share":true}`)
	if rec.Code != 200 || !s.agencies["ems"].ShareMasterTAK {
		t.Fatalf("super admin: %d %s", rec.Code, rec.Body)
	}
	if rec := doReq(h, "PUT", "/api/my-agency/tak/share-master", "super", `{"agency_id":"nope","share":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agency = %d", rec.Code)
	}
	// ems's own admin sees its switch; fire's admin sees only fire's.
	var st map[string]interface{}
	json.Unmarshal(doReq(h, "GET", "/api/my-agency/tak?agency=ems", "fireadmin", "").Body.Bytes(), &st)
	if st["agency_id"] != "fire" || st["share_master"] != false {
		t.Fatalf("fire admin reading ems: %v", st)
	}
	json.Unmarshal(doReq(h, "GET", "/api/my-agency/tak", "emsadmin", "").Body.Bytes(), &st)
	if st["share_master"] != true {
		t.Fatalf("ems status: %v", st)
	}
	m, _ := st["master"].(map[string]interface{})
	if m["configured"] != false || st["host"] != nil {
		t.Fatalf("master summary for an agency admin: %v", st)
	}
	// Super admin reads the master page with the sharing list.
	rec = doReq(h, "GET", "/api/tak-master", "super", "")
	var ms struct {
		Configured bool             `json:"configured"`
		Sharing    []takMasterShare `json:"sharing"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ms)
	if rec.Code != 200 || ms.Configured || len(ms.Sharing) != 1 || ms.Sharing[0].Agency != "ems" {
		t.Fatalf("master status: %d %s", rec.Code, rec.Body)
	}
	// The role matrix: closed to agency admins (super admin by omission).
	for _, rt := range []string{"GET /api/tak-master", "PUT /api/tak-master", "GET /api/tak-master/groups", "PUT /api/tak-master/groups", "PUT /api/tak-master/home"} {
		p := strings.SplitN(rt, " ", 2)
		if routeAllowed("agency_admin", p[0], p[1]) || routeAllowed("dispatch", p[0], p[1]) || !routeAllowed("super_admin", p[0], p[1]) {
			t.Errorf("role matrix wrong for %s", rt)
		}
	}
	if !routeAllowed("agency_admin", "PUT", "/api/my-agency/tak/share-master") || routeAllowed("dispatch", "PUT", "/api/my-agency/tak/share-master") {
		t.Error("share switch role matrix wrong")
	}
}

// --- persistence / upgrade ------------------------------------------------------------

// An agencies.json and tak_connections.json written by v4.9.1 (no new
// fields): every agency starts with sharing off, no master, and the agency
// TAK status looks as before plus "not set up".
func TestTakMasterUpgradeFromOldConfig(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	s := newWebTestServer(t)
	v491 := `[{"id":"alpha","name":"Alpha","created_at":"2026-11-01T00:00:00Z","max_users":100,"max_channels":20,"discoverable":false,"floor_control":false,"queue_when_busy":false,"no_recording":true}]`
	if err := os.WriteFile(s.config.AgenciesFile, []byte(v491), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(takConnectionsFile, []byte(`[{"agency_id":"alpha","host":"tak.example.org","stream_port":8089,"enroll_port":8446,"api_port":8443,"username":"u","callsign":"Dispatch","uid":"MeshRelay-Dispatch-alpha-1","enabled":false}]`), 0o600)
	s.agencies = map[string]*Agency{}
	s.loadAgencies()
	s.loadTakConnections()
	s.loadTakMaster()
	if a := s.agencies["alpha"]; a == nil || a.ShareMasterTAK || !a.NoRecording {
		t.Fatalf("loaded agency: %+v", a)
	}
	if s.tak().masterCfg != nil || s.takMasterConn() != nil {
		t.Fatal("a master appeared from nowhere")
	}
	st := s.takStatusJSON("alpha")
	m, _ := st["master"].(map[string]interface{})
	if st["share_master"] != false || m["configured"] != false || st["same_server_as_master"] != false || st["host"] != "tak.example.org" {
		t.Fatalf("status: %v", st)
	}
	if out := s.takMasterOutboundAgencies(); len(out) != 0 {
		t.Fatalf("outbound: %v", out)
	}

	// Switched on: saved, and survives a restart.
	s.agencies["alpha"].ShareMasterTAK = true
	if err := s.saveAgencies(); err != nil {
		t.Fatal(err)
	}
	s2 := newWebTestServer(t)
	s2.config.AgenciesFile = s.config.AgenciesFile
	s2.agencies = map[string]*Agency{}
	s2.loadAgencies()
	if !s2.agencies["alpha"].ShareMasterTAK {
		t.Fatal("sharing switch lost on restart")
	}
	// Off again: the field is left out of the file, as in v4.9.1.
	s2.agencies["alpha"].ShareMasterTAK = false
	s2.saveAgencies()
	data, _ := os.ReadFile(s.config.AgenciesFile)
	if strings.Contains(string(data), "share_master_tak") {
		t.Fatalf("off written: %s", data)
	}
}

// An imported agency (user export from another server) starts with sharing
// off: the master TAK server is this server's setting.
func TestTakMasterShareNotImported(t *testing.T) {
	a, err := checkImportAgency(&Agency{ID: "alpha", Name: "Alpha", ShareMasterTAK: true})
	if err != nil || a.ShareMasterTAK {
		t.Fatalf("imported: %+v %v", a, err)
	}
}

// --- end to end over the fake TAK server ------------------------------------------------

// The super admin enrolls the master from the dashboard (certificate
// confirmation, then the password goes to that certificate only), it
// connects with one contact (and deletes the old per-agency ones), is saved
// owner-only without the password, carries a sharing agency's units, comes
// back after a restart, and removing it deletes the file.
func TestTakMasterEnrollConnectRemove(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	s := newWebTestServer(t)
	s.agencies["fire"].ShareMasterTAK = true
	f := startFakeTAK(t)
	t.Cleanup(s.stopTakMaster)
	h := s.webHandler()
	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	streamPort := f.stream.Addr().(*net.TCPAddr).Port
	body := map[string]interface{}{"host": "127.0.0.1", "stream_port": streamPort, "enroll_port": enrollPort,
		"api_port": f.api.Addr().(*net.TCPAddr).Port, "username": "dispatch", "password": "secret", "enabled": true}
	js, _ := json.Marshal(body)
	rec := doReq(h, "PUT", "/api/tak-master", "super", string(js))
	var needs struct {
		NeedsTrust  bool        `json:"needs_trust"`
		Certificate takCertInfo `json:"certificate"`
	}
	json.Unmarshal(rec.Body.Bytes(), &needs)
	if rec.Code != http.StatusConflict || !needs.NeedsTrust || len(needs.Certificate.Fingerprint) != 64 {
		t.Fatalf("first PUT = %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(takMasterFile); err == nil {
		t.Fatal("saved before enrollment")
	}
	body["trust_fingerprint"] = needs.Certificate.Fingerprint
	js, _ = json.Marshal(body)
	rec = doReq(h, "PUT", "/api/tak-master", "super", string(js))
	if rec.Code != 200 {
		t.Fatalf("enroll PUT = %d %s", rec.Code, rec.Body)
	}
	select {
	case <-f.connCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("master never connected: %v", s.takMasterStatusJSON())
	}
	waitFor(t, "master contact and old-contact cleanup", func() bool {
		return strings.Contains(f.got(), `callsign="MeshRelay"`) && strings.Contains(f.got(), `type="t-x-d-d"`) &&
			strings.Contains(f.got(), `.fire" relation="none"`) && strings.Contains(f.got(), `.ems" relation="none"`)
	})
	if strings.Contains(f.got(), `Dispatch" endpoint`) {
		t.Fatal("a per-agency contact was announced")
	}
	st := s.takMasterStatusJSON()
	if st["status"] != "connected" || st["enrolled"] != true || st["key_pem"] != nil {
		t.Fatalf("status: %v", st)
	}
	if mc := s.takMasterConn(); mc == nil || len(mc.peerFP) != 64 {
		t.Fatal("streaming certificate fingerprint not recorded")
	}
	fi, err := os.Stat(takMasterFile)
	raw, _ := os.ReadFile(takMasterFile)
	if err != nil || fi.Mode().Perm() != 0o600 || strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatalf("saved file: %v %v", err, fi.Mode())
	}

	f.send(pli("ANDROID-e2e", "Echo-1"))
	waitFor(t, "master unit on fire's console", func() bool {
		u := s.takUnitsFor("fire")
		return len(u) == 1 && u[0]["source"] == "master"
	})
	if len(s.takUnitsFor("ems")) != 0 {
		t.Fatal("non-sharing agency sees master units")
	}

	// Restart: loaded and connected again.
	s.stopTakMaster()
	s2 := newWebTestServer(t)
	s2.agencies["fire"].ShareMasterTAK = true
	s2.loadTakMaster()
	t.Cleanup(s2.stopTakMaster)
	select {
	case <-f.connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("master not reconnected after restart")
	}
	waitFor(t, "connected after restart", func() bool { return s2.takMasterStatusJSON()["status"] == "connected" })

	waitFor(t, "old-contact cleanup after restart", func() bool {
		return strings.Contains(f.got(), `<link uid="`+s2masterUIDFrom(raw)+`.fire"`)
	})

	// Removed: the file is gone.
	h2 := s2.webHandler()
	rec = doReq(h2, "PUT", "/api/tak-master", "super", `{"remove":true}`)
	if rec.Code != 200 {
		t.Fatalf("remove = %d", rec.Code)
	}
	if _, err := os.Stat(takMasterFile); !os.IsNotExist(err) {
		t.Fatal("tak_master.json left after removal")
	}
	if s2.takMasterConn() != nil || s2.takMasterStatusJSON()["configured"] != false {
		t.Fatal("still configured")
	}
}

func httpReq(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	return req
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func s2masterUIDFrom(raw []byte) string {
	var c TakConfig
	json.Unmarshal(raw, &c)
	return c.UID
}

// --- bench fixes: markers, recipient labels, not-sent ----------------------------

func TestTakMeshRelayMarkersAreNotUnits(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak2.example.org", 8089)
	now := time.Now()
	ours := lmrMarkerCoT(lmrMarker{BridgeID: "b1", Channel: 101, Name: "Fire Ops", Lat: 41.7, Lon: -74.7}, "abcd", now, time.Hour)
	// Another MeshRelay server's marker: known by the detail, not the uid.
	other := `<event version="2.0" type="a-n-G" uid="Other-Radio-7" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.71" lon="-74.71"/><detail><contact callsign="County Fire"/><__meshrelay kind="lmr" channel="3" name="County Fire"/></detail></event>`
	// An older marker without the detail: known by the uid prefix.
	bare := `<event version="2.0" type="a-n-G" uid="MeshRelay-LMR-x9" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.72" lon="-74.72"/><detail><contact callsign="Old Marker"/></detail></event>`
	for _, p := range []*pipeTak{r.master, own} {
		for _, cot := range []string{ours, other, bare} {
			p.feed(t, cot)
		}
		p.feed(t, pli("ANDROID-real", "Echo-1"))
	}
	for _, p := range []*pipeTak{r.master, own} {
		p.tc.mu.Lock()
		n, real := len(p.tc.units), p.tc.units["ANDROID-real"] != nil
		p.tc.mu.Unlock()
		if n != 1 || !real {
			t.Fatalf("master=%v: units %d (real listed %v)", p.tc.master, n, real)
		}
	}
	for _, u := range r.s.takUnitsFor("alpha") {
		if u["uid"] != "ANDROID-real" {
			t.Fatalf("marker listed as a TAK user: %v", u)
		}
	}
}

func TestTakMessageRecipientLabel(t *testing.T) {
	r := newMasterRig(t)
	// Unknown on the master (e.g. just after a restart): the console's
	// callsign, cleaned; never the uid.
	r.s.consoleSendTakMessage(r.s.clients[userKey("bravo", "desk")], "bravo", "ANDROID-new", "hello", "master", " Echo-2\x07 ")
	if got := r.master.take(t); !strings.Contains(got, `uid1="ANDROID-new"`) {
		t.Fatalf("not routed by uid: %s", got)
	}
	h := r.s.messageHistory("bravo")
	if len(h) != 1 || h[0].To != "Echo-2? (TAK)" || h[0].Sent != 1 || h[0].NotSent != "" {
		t.Fatalf("history: %+v", h)
	}
	// No hint: a neutral label.
	r.s.consoleSendTakMessage(r.s.clients[userKey("bravo", "desk")], "bravo", "ANDROID-new", "again", "master", "")
	if h := r.s.messageHistory("bravo"); h[1].To != "TAK user (TAK)" {
		t.Fatalf("no-hint label: %q", h[1].To)
	}
	// A known unit: the server's callsign wins over the hint.
	r.master.feed(t, pli("ANDROID-new", "Echo-2"))
	r.s.consoleSendTakMessage(r.s.clients[userKey("bravo", "desk")], "bravo", "ANDROID-new", "x", "master", "Spoofed")
	if h := r.s.messageHistory("bravo"); h[2].To != "Echo-2 (TAK)" {
		t.Fatalf("known label: %q", h[2].To)
	}
	// Agency connection: same rules.
	r.ownConn(t, "alpha", "tak2.example.org", 8089)
	r.s.consoleSendTakMessage(r.s.clients[userKey("alpha", "desk")], "alpha", "ANDROID-z", "hi", "", strings.Repeat("A", 200))
	if h := r.s.messageHistory("alpha"); len(h) != 1 || h[0].To != strings.Repeat("A", 64)+" (TAK)" || h[0].Sent != 1 {
		t.Fatalf("agency history: %+v", h)
	}
}

func TestTakMessageNotSentReason(t *testing.T) {
	r := newMasterRig(t)
	r.write(t, "charlie", "ANDROID-m1", "not sharing")
	if h := r.s.messageHistory("charlie"); len(h) != 1 || h[0].Sent != 0 || h[0].NotSent != "agency not sharing with the master" {
		t.Fatalf("not sharing: %+v", h)
	}
	r.master.tc.mu.Lock()
	r.master.tc.status = "connecting"
	r.master.tc.mu.Unlock()
	r.write(t, "bravo", "ANDROID-m1", "down")
	if h := r.s.messageHistory("bravo"); len(h) != 1 || h[0].Sent != 0 || h[0].NotSent != "master TAK not connected" {
		t.Fatalf("master down: %+v", h)
	}
	// The reason reaches the console with the message.
	var seen bool
	for _, m := range r.consoleMsgs("bravo") {
		if m["type"] == "message" && m["not_sent"] == "master TAK not connected" && m["sent"] == nil {
			seen = true
		}
	}
	if !seen {
		t.Fatal("console did not get the not-sent reason")
	}
	// No agency connection at all.
	r.s.consoleSendTakMessage(r.s.clients[userKey("alpha", "desk")], "alpha", "ANDROID-z", "hi", "", "Echo-3")
	if h := r.s.messageHistory("alpha"); len(h) != 1 || h[0].NotSent != "no TAK connection" || h[0].To != "Echo-3 (TAK)" {
		t.Fatalf("no connection: %+v", h)
	}
}

// --- the same person on MeshRelay and in TAK ----------------------------------

func listsUID(units []map[string]interface{}, uid string) bool {
	for _, u := range units {
		if u["uid"] == uid {
			return true
		}
	}
	return false
}

// A TAK unit whose callsign is a MeshRelay unit signed in to the agency (any
// case, outer spaces ignored) is not listed to that agency's console: the
// MeshRelay unit stands for it. Other agencies still list it, it comes back
// when the MeshRelay unit signs out, a console's own name does not hide a TAK
// unit, and the hidden unit's GeoChat still reaches the console.
func TestTakUnitHiddenWhileSameUserOnMeshRelay(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak.alpha.example.org", 8089)
	r.s.clients[userKey("alpha", "medic1")] = &Client{Username: "medic1", AgencyID: "alpha",
		Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 3), Port: 43000}, LastHeartbeat: time.Now()}
	signedInAs(r.s, r.s.clients[userKey("alpha", "medic1")], "android 4.0.4")
	own.feed(t, pli("ANDROID-own", " MEDIC1 "))
	r.master.feed(t, pli("ANDROID-mst", "Medic1"))
	r.master.feed(t, pli("ANDROID-desk", "desk")) // a console's name, not a unit
	r.master.feed(t, pli("ANDROID-other", "Echo-1"))

	alpha := r.s.takUnitsFor("alpha")
	if listsUID(alpha, "ANDROID-own") || listsUID(alpha, "ANDROID-mst") {
		t.Fatalf("alpha lists the TAK units of its signed-in medic1: %v", alpha)
	}
	if !listsUID(alpha, "ANDROID-other") || !listsUID(alpha, "ANDROID-desk") {
		t.Fatalf("alpha lost other TAK units: %v", alpha)
	}
	// Bravo has no medic1 signed in: the master unit is listed there.
	if b := r.s.takUnitsFor("bravo"); !listsUID(b, "ANDROID-mst") {
		t.Fatalf("bravo should list medic1's TAK unit: %v", b)
	}
	// The all-agency console shows alpha's medic1: not listed under bravo either.
	if all := r.s.takUnitsFor(""); listsUID(all, "ANDROID-mst") || listsUID(all, "ANDROID-own") || !listsUID(all, "ANDROID-other") {
		t.Fatalf("all-agency console: %v", all)
	}

	// The hidden units' messages still arrive (own connection: alpha; the
	// master: its home agency, alpha) with their TAK uid for the reply.
	own.feed(t, geoChat("ANDROID-own", " MEDIC1 ", "MeshRelay-Dispatch-alpha", "via own"))
	r.master.feed(t, geoChat("ANDROID-mst", "Medic1", masterUID, "via master"))
	got := map[string]string{}
	for _, m := range r.inbound("alpha") {
		got[m.TakUID] = m.Text
	}
	if got["ANDROID-own"] != "via own" || got["ANDROID-mst"] != "via master" {
		t.Fatalf("alpha inbound: %+v", r.inbound("alpha"))
	}

	// Signed out: listed again.
	delete(r.s.clients, userKey("alpha", "medic1"))
	alpha = r.s.takUnitsFor("alpha")
	if !listsUID(alpha, "ANDROID-own") || !listsUID(alpha, "ANDROID-mst") {
		t.Fatalf("after sign-out: %v", alpha)
	}
}

func TestCallsignKey(t *testing.T) {
	for in, want := range map[string]string{"Medic1": "medic1", "  MEDIC1\t": "medic1", "medic1 (Unit1)": "medic1 (unit1)", " ": ""} {
		if got := callsignKey(in); got != want {
			t.Errorf("callsignKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// signedInAs: c signed in with the secure sign-in, as client name.
func signedInAs(s *Server, c *Client, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authSessions == nil {
		s.authSessions = map[string]authSession{}
	}
	sess := s.authSessions[c.Addr.String()]
	sess.client = name
	s.authSessions[c.Addr.String()] = sess
}

// --- one entry per person: app and ATAK plugin users -------------------------

func (r *masterRig) unit(agency, user, client string, port int) *Client {
	c := &Client{Username: user, AgencyID: agency, Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 3), Port: port}, LastHeartbeat: time.Now()}
	r.s.mu.Lock()
	r.s.clients[userKey(agency, user)] = c
	r.s.mu.Unlock()
	if client != "" {
		signedInAs(r.s, c, client)
	}
	return c
}

func clientEntry(t *testing.T, s *Server, agency, user string) map[string]interface{} {
	t.Helper()
	for _, c := range s.GetClients(agency) {
		if c["username"] == user {
			return c
		}
	}
	t.Fatalf("%s not in %s's client list", user, agency)
	return nil
}

func lastOut(s *Server, agency string) dispatchMsg {
	var out dispatchMsg
	for _, m := range s.messageHistory(agency) {
		if m.Dir == "out" {
			out = m
		}
	}
	return out
}

// The app on both systems: one entry (the MeshRelay one, carrying the TAK
// uid), the fresher position, and texts over MeshRelay.
func TestOneEntryAppUser(t *testing.T) {
	r := newMasterRig(t)
	c := r.unit("alpha", "medic1", "android 4.0.4", 43001)
	c.Loc = &UnitLocation{Lat: 41.1, Lon: -74.1, At: time.Now().Add(-time.Hour)}
	r.master.feed(t, pli("ANDROID-m1", "MEDIC1"))
	r.master.tc.mu.Lock()
	r.master.tc.units["ANDROID-m1"].At = time.Now() // the TAK position is the fresher one
	r.master.tc.mu.Unlock()
	r.master.take(t)
	if listsUID(r.s.takUnitsFor("alpha"), "ANDROID-m1") {
		t.Fatal("TAK entry listed next to the app user")
	}
	e := clientEntry(t, r.s, "alpha", "medic1")
	tk, _ := e["tak"].(map[string]interface{})
	loc, _ := e["location"].(map[string]interface{})
	if tk["uid"] != "ANDROID-m1" || tk["source"] != "master" || loc["lat"] != 41.7 || loc["source"] != "tak" {
		t.Fatalf("entry: tak %v location %v", tk, loc)
	}
	r.s.consoleSendMessage(r.s.clients[userKey("alpha", "desk")], "hello", &messageTarget{Agency: "alpha", User: "medic1"}, "", "")
	if m := lastOut(r.s, "alpha"); m.Sent != 1 || m.TakUID != "" {
		t.Fatalf("message: %+v", m)
	}
	if g := r.master.take(t); strings.Contains(g, `type="b-t-f"`) {
		t.Fatalf("app user texted over TAK: %s", g)
	}
}

// The ATAK plugin on both systems (no texts, no 0x2E): one entry, located by
// its TAK unit; a console text to it goes as GeoChat to that TAK uid (dest
// uid, the agency prefix on the master), and the reply comes back to the
// agency that wrote (not the master's home agency). Other agencies still
// list the TAK unit; signed out, the TAK entry is back.
func TestOneEntryATAKPluginUser(t *testing.T) {
	r := newMasterRig(t)
	r.unit("bravo", "Engine3", "atak-plugin", 43002)
	r.master.feed(t, pli("ANDROID-engine3", "ENGINE3"))
	r.master.take(t)
	if listsUID(r.s.takUnitsFor("bravo"), "ANDROID-engine3") {
		t.Fatal("TAK entry listed next to the plugin user")
	}
	if !listsUID(r.s.takUnitsFor("alpha"), "ANDROID-engine3") {
		t.Fatal("other agency lost the TAK unit")
	}
	e := clientEntry(t, r.s, "bravo", "Engine3")
	tk, _ := e["tak"].(map[string]interface{})
	loc, _ := e["location"].(map[string]interface{})
	if tk["uid"] != "ANDROID-engine3" || loc["lat"] != 41.7 || loc["lon"] != -74.7 {
		t.Fatalf("entry: tak %v location %v", tk, loc)
	}

	r.s.consoleSendMessage(r.s.clients[userKey("bravo", "desk")], "status?", &messageTarget{Agency: "bravo", User: "Engine3"}, "", "")
	c := oneChat(t, r.master.take(t))
	wantDest(t, c, "ANDROID-engine3", "ENGINE3")
	if c.Detail.Remarks != takMasterPrefixed("Bravo", "status?") {
		t.Fatalf("remarks %q", c.Detail.Remarks)
	}
	if m := lastOut(r.s, "bravo"); m.Sent != 1 || m.TakUID != "ANDROID-engine3" || m.TakSource != "master" {
		t.Fatalf("history: %+v", m)
	}
	r.master.feed(t, geoChat("ANDROID-engine3", "ENGINE3", masterUID, "copy"))
	r.wantInbound(t, map[string]int{"bravo": 1})
	if in := r.inbound("bravo")[0]; in.TakUID != "ANDROID-engine3" || in.Text != "copy" {
		t.Fatalf("reply: %+v", in)
	}

	r.s.mu.Lock()
	delete(r.s.clients, userKey("bravo", "Engine3"))
	r.s.mu.Unlock()
	if !listsUID(r.s.takUnitsFor("bravo"), "ANDROID-engine3") {
		t.Fatal("TAK entry not back after sign-out")
	}
}

// The plugin user on the agency's own TAK connection: texted there.
func TestATAKPluginUserTextedOnOwnConnection(t *testing.T) {
	r := newMasterRig(t)
	own := r.ownConn(t, "alpha", "tak.alpha.example.org", 8089)
	r.unit("alpha", "engine3", "atak-plugin", 43003)
	own.feed(t, pli("ANDROID-e3", " Engine3 "))
	own.take(t)
	r.s.consoleSendMessage(r.s.clients[userKey("alpha", "desk")], "hi", &messageTarget{Agency: "alpha", User: "engine3"}, "", "")
	c := oneChat(t, own.take(t))
	wantDest(t, c, "ANDROID-e3", " Engine3 ")
	if c.Detail.Remarks != "hi" {
		t.Fatalf("remarks %q", c.Detail.Remarks)
	}
	own.feed(t, geoChat("ANDROID-e3", "Engine3", "MeshRelay-Dispatch-alpha", "ok"))
	if in := r.inbound("alpha"); len(in) != 1 || in[0].TakUID != "ANDROID-e3" {
		t.Fatalf("reply: %+v", in)
	}
}

// Not provable: a client whose kind is not known keeps its TAK entry and is
// texted over MeshRelay as before; a plugin user not on TAK is reported as
// not reached rather than texted into nothing.
func TestUnknownClientKeepsTakEntry(t *testing.T) {
	r := newMasterRig(t)
	r.unit("alpha", "old", "", 43004)
	r.master.feed(t, pli("ANDROID-old", "old"))
	if !listsUID(r.s.takUnitsFor("alpha"), "ANDROID-old") {
		t.Fatal("TAK entry hidden for a client of unknown kind")
	}
	if _, ok := clientEntry(t, r.s, "alpha", "old")["tak"]; ok {
		t.Fatal("unknown client merged with the TAK unit")
	}
	r.s.consoleSendMessage(r.s.clients[userKey("alpha", "desk")], "x", &messageTarget{Agency: "alpha", User: "old"}, "", "")
	if m := lastOut(r.s, "alpha"); m.Sent != 1 {
		t.Fatalf("message: %+v", m)
	}

	r.unit("alpha", "lone", "atak-plugin", 43005)
	r.master.take(t)
	r.s.consoleSendMessage(r.s.clients[userKey("alpha", "desk")], "y", &messageTarget{Agency: "alpha", User: "lone"}, "", "")
	if m := lastOut(r.s, "alpha"); m.Sent != 0 || m.NotSent == "" {
		t.Fatalf("plugin user not on TAK: %+v", m)
	}
	if g := r.master.take(t); strings.Contains(g, `type="b-t-f"`) {
		t.Fatalf("sent: %s", g)
	}
	// A unit that has sent dispatch a text receives them.
	c := r.unit("alpha", "texter", "", 43006)
	c.SentText = true
	r.master.feed(t, pli("ANDROID-tx", "texter"))
	if listsUID(r.s.takUnitsFor("alpha"), "ANDROID-tx") {
		t.Fatal("texting unit's TAK entry still listed")
	}
}
