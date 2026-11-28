// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// takAlertRig: a test server with a TAK connector for agency "ag" whose
// stream is an in-memory pipe (no TAK server): what the connector writes is
// collected.
type takAlertRig struct {
	s   *Server
	tc  *takConn
	mu  sync.Mutex
	out []string
}

func newTakAlertRig(t *testing.T, withConnector bool) *takAlertRig {
	t.Helper()
	s, _ := newFloorTestServer(t, "alice", "medic")
	r := &takAlertRig{s: s}
	t.Cleanup(func() {
		activeEmergMu.Lock()
		for k := range activeEmerg {
			if strings.HasPrefix(k, "ag|") || strings.HasPrefix(k, "other|") {
				delete(activeEmerg, k)
			}
		}
		activeEmergMu.Unlock()
	})
	if !withConnector {
		return r
	}
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := theirs.Read(buf)
			if n > 0 {
				r.mu.Lock()
				r.out = append(r.out, string(buf[:n]))
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	r.tc = &takConn{s: s, cfg: TakConfig{AgencyID: "ag", UID: "dispatch-uid", Callsign: "Dispatch"},
		conn: ours, status: "connected", units: map[string]*TakUnit{}, stop: make(chan struct{})}
	m := s.tak()
	m.mu.Lock()
	m.conns["ag"] = r.tc
	m.mu.Unlock()
	return r
}

// sync makes sure everything written so far has been collected.
func (r *takAlertRig) sync(t *testing.T) {
	t.Helper()
	marker := fmt.Sprintf("<marker n=%q/>", time.Now().String())
	r.tc.send(marker)
	waitFor(t, "marker", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.out) > 0 && strings.Contains(r.out[len(r.out)-1], marker)
	})
}

// count: the server's own alerts (active) or cancels written for user.
func (r *takAlertRig) count(t *testing.T, user string, active bool) int {
	t.Helper()
	r.sync(t)
	typ := "b-a-o-can"
	if active {
		typ = "b-a-o-tbl"
	}
	want := `type="` + typ + `" uid="MeshRelay-911-ag-` + user + `"`
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.out {
		n += strings.Count(w, want)
	}
	return n
}

func (r *takAlertRig) emergency(user string, active bool) {
	c := r.s.clients[userKey("ag", user)]
	r.s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: user, Active: active}, c.Addr)
}

// feed hands the connector a CoT event as if it came from the TAK stream.
func (r *takAlertRig) feed(t *testing.T, cot string) {
	t.Helper()
	var ev cotEvent
	if err := xml.Unmarshal([]byte(cot), &ev); err != nil {
		t.Fatalf("bad CoT: %v", err)
	}
	r.tc.handle(&ev)
}

// atakAlert: the 911 alert ATAK (and the MeshRelay phone) sends.
func atakAlert(pliUID, callsign string, active bool) string {
	if !active {
		return `<event version="2.0" uid="` + pliUID + `-9-1-1" type="b-a-o-can" how="h-g-i-g-o" time="2026-11-22T12:00:00.000Z" start="2026-11-22T12:00:00.000Z" stale="2026-11-22T12:00:10.000Z"><point lat="41.6" lon="-74.7" hae="0" ce="9999999" le="9999999"/><detail><emergency cancel="true">` + callsign + `</emergency></detail></event>`
	}
	return `<event version="2.0" uid="` + pliUID + `-9-1-1" type="b-a-o-tbl" how="h-g-i-g-o" time="2026-11-22T12:00:00.000Z" start="2026-11-22T12:00:00.000Z" stale="2026-11-22T12:00:10.000Z"><point lat="41.6" lon="-74.7" hae="0" ce="9999999" le="9999999"/><detail><link uid="` + pliUID + `" type="a-f-G-U-C" relation="p-p"/><contact callsign="` + callsign + `-Alert"/><emergency type="911 Alert">` + callsign + `</emergency></detail></event>`
}

func consoleShows(user string) bool {
	for _, m := range activeEmergencies("ag") {
		if m["user"] == user {
			return true
		}
	}
	return false
}

// The phone's own alert for a unit in a MeshRelay emergency is not a second
// emergency on the console.
func TestTakPhoneAlertNotDuplicatedOnConsole(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	if !consoleShows("medic") {
		t.Fatal("MeshRelay emergency not on the console")
	}
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "MEDIC", true)) // any case
	if consoleShows("medic (TAK)") || consoleShows("MEDIC (TAK)") {
		t.Fatal("phone's own alert shown as a second emergency")
	}
	// Its cancel at the end does not show either, nor a repeat still on the way.
	r.emergency("medic", false)
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	if consoleShows("medic (TAK)") {
		t.Fatal("late repeat after the end shown")
	}
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", false))
	if consoleShows("medic") {
		t.Fatal("MeshRelay emergency not cleared")
	}
}

// The phone's alert arriving just before the MeshRelay emergency: shown for
// that moment, then taken off when the MeshRelay one arrives.
func TestTakPhoneAlertBeforeMeshRelayEmergency(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	if !consoleShows("medic (TAK)") {
		t.Fatal("phone alert without a MeshRelay emergency must show")
	}
	r.emergency("medic", true)
	if consoleShows("medic (TAK)") || !consoleShows("medic") {
		t.Fatal("console should show only the MeshRelay emergency")
	}
	if n := r.count(t, "medic", true); n != 0 {
		t.Fatalf("server alert sent %d times while the phone's is on TAK", n)
	}
	r.emergency("medic", false)
	if n := r.count(t, "medic", false); n != 0 {
		t.Fatalf("server never had an alert out but sent %d cancels", n)
	}
}

// A TAK user's own emergency shows when its callsign is not a MeshRelay unit
// in an emergency: a callsign like a MeshRelay unit's, or a MeshRelay-looking
// uid for a name that is not in a MeshRelay emergency. (An alert whose
// callsign IS such a unit is the same incident: TestTakATAKOwnAlert...)
func TestTakThirdPartyEmergencyStillShown(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	r.feed(t, atakAlert("ANDROID-fedcba9876543210", "Medic 2", true))
	if !consoleShows("Medic 2 (TAK)") {
		t.Fatal("similar callsign hidden")
	}
	r.feed(t, atakAlert("MeshRelay-99887766", "medic2", true))
	if !consoleShows("medic2 (TAK)") {
		t.Fatal("MeshRelay-looking uid for another name hidden")
	}
	r.feed(t, atakAlert("MeshRelay-55443322", "alice", true)) // alice: no MeshRelay emergency
	if !consoleShows("alice (TAK)") {
		t.Fatal("alert for a unit with no MeshRelay emergency hidden")
	}
	// None of these touch the server's alert for medic.
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 1 || c != 0 {
		t.Fatalf("server alert for medic: %d sent, %d cancelled; want 1, 0", n, c)
	}
}

// Server alert withheld while the phone's is fresh, sent again once it goes
// stale, and cancelled exactly once at the end.
func TestTakServerAlertYieldsToPhone(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	if n := r.count(t, "medic", true); n != 1 {
		t.Fatalf("server alert sent %d times, want 1", n)
	}
	// The phone's alert arrives: the server's is withdrawn (once).
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	if c := r.count(t, "medic", false); c != 1 {
		t.Fatalf("server alert cancelled %d times, want 1", c)
	}
	// Still fresh: nothing goes out again.
	r.tc.alertTick(time.Now().Add(10 * time.Second))
	if n := r.count(t, "medic", true); n != 1 {
		t.Fatalf("server alert resent while the phone's is fresh (%d)", n)
	}
	// Phone alert stopped (TAK link lost): the server's goes out again, once.
	r.tc.alertTick(time.Now().Add(takPhoneAlertHold + time.Second))
	r.tc.alertTick(time.Now().Add(takPhoneAlertHold + 6*time.Second))
	if n := r.count(t, "medic", true); n != 2 {
		t.Fatalf("server alert sent %d times after the phone's went stale, want 2", n)
	}
	// End: exactly one cancel for what the server had out.
	r.emergency("medic", false)
	if c := r.count(t, "medic", false); c != 2 {
		t.Fatalf("cancels %d, want 2 (takeover + end)", c)
	}
	r.tc.alertTick(time.Now().Add(time.Minute))
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 2 || c != 2 {
		t.Fatalf("after end: %d sent, %d cancels", n, c)
	}
}

// Phone took over and is still sending at the end: the server, with nothing
// out, sends no cancel.
func TestTakNoCancelWhenPhoneHeldTheAlert(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", true))
	r.emergency("medic", false)
	r.feed(t, atakAlert("MeshRelay-1a2b3c4d", "medic", false))
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 1 || c != 1 {
		t.Fatalf("server alerts %d, cancels %d; want 1, 1", n, c)
	}
}

// A unit whose phone sends no alert of its own: alert at the start, cancel at
// the end, exactly as before. And without a connector nothing happens.
func TestTakAlertUnchangedWithoutPhoneOrConnector(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	for i := 0; i < 3; i++ {
		r.tc.alertTick(time.Now().Add(time.Duration(i+1) * time.Minute))
	}
	r.emergency("medic", false)
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 1 || c != 1 {
		t.Fatalf("server alerts %d, cancels %d; want 1, 1", n, c)
	}
	if len(r.tc.alerts) != 0 {
		t.Fatalf("state left behind: %v", r.tc.alerts)
	}

	n := newTakAlertRig(t, false)
	n.emergency("medic", true)
	if n.s.takConnFor("ag") != nil || !consoleShows("medic") {
		t.Fatal("no connector: MeshRelay emergency should show, nothing else")
	}
	n.emergency("medic", false)
	if consoleShows("medic") {
		t.Fatal("not cleared")
	}
}

// alertEvent: a server alert as ATAK reads it.
type alertEvent struct {
	Type   string `xml:"type,attr"`
	UID    string `xml:"uid,attr"`
	Detail struct {
		Contact *struct {
			Callsign string `xml:"callsign,attr"`
		} `xml:"contact"`
		Link *struct {
			UID      *string `xml:"uid,attr"`
			Type     *string `xml:"type,attr"`
			Relation *string `xml:"relation,attr"`
		} `xml:"link"`
		Emergency *struct {
			Type   string `xml:"type,attr"`
			Cancel string `xml:"cancel,attr"`
			Text   string `xml:",chardata"`
		} `xml:"emergency"`
	} `xml:"detail"`
}

// serverAlerts: the server's own alerts (and cancels) written so far.
func (r *takAlertRig) serverAlerts(t *testing.T) []alertEvent {
	t.Helper()
	r.sync(t)
	r.mu.Lock()
	all := strings.Join(r.out, "")
	r.mu.Unlock()
	var out []alertEvent
	for _, doc := range strings.Split(all, `<?xml version="1.0" encoding="UTF-8"?>`) {
		if !strings.Contains(doc, `uid="MeshRelay-911-`) {
			continue
		}
		var ev alertEvent
		if err := xml.Unmarshal([]byte(doc[:strings.Index(doc, "</event>")+8]), &ev); err != nil {
			t.Fatalf("bad alert %q: %v", doc, err)
		}
		out = append(out, ev)
	}
	return out
}

// The server's alert as ATAK 5.6-5.8 handles it (EmergencyAlertReceiver,
// LinkDetailHandler): it has the <link> ATAK requires, but one that draws no
// line (no type, no "p-p" relation) and names no map item, so ATAK names the
// unit from the contact callsign less its last "-..." part. The cancel keeps
// the alert's uid.
func TestTakServerAlertForm(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	r.emergency("medic", false)
	evs := r.serverAlerts(t)
	if len(evs) != 2 {
		t.Fatalf("alerts %+v, want the alert and its cancel", evs)
	}
	a, c := evs[0], evs[1]
	const uid = "MeshRelay-911-ag-medic"
	if a.Type != "b-a-o-tbl" || a.UID != uid {
		t.Fatalf("alert: %+v", a)
	}
	e := a.Detail.Emergency
	if e == nil || e.Type != "911 Alert" || e.Text != "medic" || e.Cancel != "" {
		t.Fatalf("alert <emergency>: %+v", e)
	}
	l := a.Detail.Link
	if l == nil || l.UID == nil || *l.UID == "" {
		t.Fatalf("alert without the link (uid) ATAK needs: %+v", l)
	}
	if l.Type != nil || l.Relation != nil {
		t.Fatalf("link would draw a line in ATAK: type %v relation %v", l.Type, l.Relation)
	}
	if *l.UID == r.tc.cfg.UID || *l.UID == uid || *l.UID != emergencyAlertLink(uid) {
		t.Fatalf("link uid %q names a map item", *l.UID)
	}
	if a.Detail.Contact == nil {
		t.Fatal("alert without a contact callsign")
	}
	title := a.Detail.Contact.Callsign
	if title != "medic-Alert" || title[:strings.LastIndex(title, "-")] != "medic" {
		t.Fatalf("ATAK would name the unit from %q", title)
	}
	if c.Type != "b-a-o-can" || c.UID != uid || c.Detail.Emergency == nil || c.Detail.Emergency.Cancel != "true" {
		t.Fatalf("cancel: %+v", c)
	}
}

// A unit of another agency on this server whose phone reports into this
// agency's TAK server: its phone alert is not a second emergency either. The
// server sends no alert of its own for it here (its agency has no connector).
func TestTakPhoneAlertOtherAgencyNotDuplicated(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.s.raiseEmergency("other", "Medic4", 0)
	if len(activeEmergencies("other")) == 0 {
		t.Fatal("other agency's MeshRelay emergency missing")
	}
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "MEDIC4", true))
	if consoleShows("MEDIC4 (TAK)") {
		t.Fatal("other agency's unit shown twice")
	}
	// ATAK's own emergency tool for that callsign: the same incident too.
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic4", true))
	if consoleShows("Medic4 (TAK)") {
		t.Fatal("ATAK's own alert for the unit shown twice")
	}
	// End: a repeat still on the way and the phone's cancel stay hidden.
	r.s.clearEmergency("other", "Medic4", "Medic4")
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "MEDIC4", true))
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "MEDIC4", false))
	if consoleShows("MEDIC4 (TAK)") {
		t.Fatal("late repeat shown")
	}
	// Later, with no MeshRelay emergency, the phone's alert shows again.
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "MEDIC4", true))
	if !consoleShows("MEDIC4 (TAK)") {
		t.Fatal("phone alert with no MeshRelay emergency must show")
	}
	r.sync(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.out {
		if strings.Contains(w, "MeshRelay-911-") {
			t.Fatalf("connector sent a server alert for another agency's unit: %s", w)
		}
	}
}

// ATAK's own 911 tool (uid = ATAK device uid + "-9-1-1", the unit's callsign)
// used by a MeshRelay user who is in a MeshRelay emergency: one incident. The
// console shows the MeshRelay emergency only, and the server's TAK alert
// hands over to ATAK's, exactly as for the MeshRelay phone's own alert.
// Cancelling ATAK's alert first: the server's goes out again once the hold
// runs out; then the MeshRelay cancel leaves nothing behind.
func TestTakATAKOwnAlertSameIncident(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	if n := r.count(t, "medic", true); n != 1 {
		t.Fatalf("server alert sent %d times, want 1", n)
	}
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic", true))
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic", true))
	if consoleShows("Medic (TAK)") || !consoleShows("medic") {
		t.Fatal("console should list the unit once, as its MeshRelay emergency")
	}
	if c := r.count(t, "medic", false); c != 1 {
		t.Fatalf("server alert withdrawn %d times, want 1 (ATAK's alert stands for it)", c)
	}
	// ATAK's alert cancelled while the MeshRelay emergency is on: the cancel
	// is not shown, and the server's alert returns after the hold.
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic", false))
	if consoleShows("Medic (TAK)") || !consoleShows("medic") {
		t.Fatal("ATAK cancel changed the console")
	}
	r.tc.alertTick(time.Now().Add(takPhoneAlertHold + time.Second))
	if n := r.count(t, "medic", true); n != 2 {
		t.Fatalf("server alert after ATAK's was cancelled: %d sent, want 2", n)
	}
	r.emergency("medic", false)
	r.tc.alertTick(time.Now().Add(time.Minute))
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 2 || c != 2 {
		t.Fatalf("end: %d sent, %d cancels; want 2, 2", n, c)
	}
	if consoleShows("medic") || consoleShows("Medic (TAK)") {
		t.Fatal("ghost emergency left on the console")
	}
	r.tc.mu.Lock()
	left := len(r.tc.alerts)
	r.tc.mu.Unlock()
	if left != 0 {
		t.Fatalf("alert state left behind: %d", left)
	}
}

// ATAK's alert first, then the MeshRelay emergency, then the MeshRelay
// cancel while ATAK still sends: shown as the TAK alert, taken off while
// the MeshRelay one shows, and shown again (it is still live on TAK) once
// the hold runs out. The server never puts out an alert of its own.
func TestTakATAKOwnAlertMeshRelayCancelledFirst(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", true))
	if !consoleShows("medic (TAK)") {
		t.Fatal("ATAK alert with no MeshRelay emergency must show")
	}
	r.emergency("medic", true)
	if consoleShows("medic (TAK)") || !consoleShows("medic") {
		t.Fatal("console should show the MeshRelay emergency only")
	}
	r.emergency("medic", false)
	if consoleShows("medic") {
		t.Fatal("MeshRelay emergency not cleared")
	}
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", true)) // a repeat within the hold
	if consoleShows("medic (TAK)") {
		t.Fatal("repeat right after the MeshRelay cancel shown")
	}
	hidden := false
	r.tc.mu.Lock()
	for _, a := range r.tc.alerts {
		a.emergAt = a.emergAt.Add(-takPhoneAlertHold - time.Second) // the hold ran out
		hidden = hidden || a.hidden
	}
	r.tc.mu.Unlock()
	if !hidden {
		t.Fatal("state lost")
	}
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", true))
	if !consoleShows("medic (TAK)") {
		t.Fatal("ATAK's still-live alert should show once the MeshRelay emergency is over")
	}
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", false))
	if consoleShows("medic (TAK)") || consoleShows("medic") {
		t.Fatal("ghost after ATAK's cancel")
	}
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 0 || c != 0 {
		t.Fatalf("server sent %d alerts, %d cancels; want none", n, c)
	}
}

// Which uids count as a device's own alert.
func TestTakOwnAlertUID(t *testing.T) {
	for uid, want := range map[string]bool{
		"MeshRelay-1a2b3c4d-9-1-1":             true,
		"ANDROID-0123456789abcdef-9-1-1":       true,
		"S-1-5-21-1234-9-1-1":                  true,
		"-9-1-1":                               false,
		"MeshRelay-911-ag-medic":               false,
		"ANDROID-0123456789abcdef":             false,
		"ANDROID-0123456789abcdef-9-1-1-extra": false,
	} {
		if got := takOwnAlertUID(uid); got != want {
			t.Errorf("takOwnAlertUID(%q) = %v, want %v", uid, got, want)
		}
	}
}

// On the master connection too: ATAK's own alert for a sharing agency's unit
// in a MeshRelay emergency withdraws the master's alert, shows on no
// console, and the end leaves nothing out.
func TestTakMasterATAKOwnAlertSameIncident(t *testing.T) {
	r := newMasterRig(t)
	r.s.raiseEmergency("alpha", "medic", 0)
	if g := r.master.take(t); alertCount(g, "alpha", "medic", true) != 1 {
		t.Fatalf("master alert: %s", g)
	}
	r.consoleMsgs("alpha")
	r.consoleMsgs("bravo")
	r.master.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic", true))
	if g := r.master.take(t); alertCount(g, "alpha", "medic", false) != 1 {
		t.Fatalf("master should withdraw its alert: %s", g)
	}
	for _, a := range []string{"alpha", "bravo"} {
		if e := emergenciesFor(r.consoleMsgs(a), "Medic (TAK)"); len(e) != 0 {
			t.Fatalf("%s lists the unit twice: %v", a, e)
		}
	}
	r.s.clearEmergency("alpha", "medic", "medic")
	r.master.feed(t, atakAlert("ANDROID-0123456789abcdef", "Medic", false))
	if g := r.master.take(t); strings.Contains(g, "MeshRelay-911-") {
		t.Fatalf("master sent after the end (nothing was out): %s", g)
	}
	for _, a := range []string{"alpha", "bravo"} {
		if e := emergenciesFor(r.consoleMsgs(a), "Medic (TAK)"); len(e) != 0 {
			t.Fatalf("%s shows ATAK's cancel: %v", a, e)
		}
	}
}

// One alert CoT per real change: a phone's repeated emergency packets (no
// position, as the app sends them) write nothing more; while the emergency
// lasts the alert is refreshed every takAlertRefresh, well inside its 10 min
// stale time, and not after the end.
func TestTakServerAlertOnlyOnChangeAndRefresh(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	for i := 0; i < 5; i++ {
		r.emergency("medic", true) // re-sends of the active emergency
	}
	start := time.Now()
	r.tc.alertTick(start.Add(10 * time.Second))
	r.tc.alertTick(start.Add(time.Minute))
	if n := r.count(t, "medic", true); n != 1 {
		t.Fatalf("alerts %d after re-sends and early ticks, want 1", n)
	}
	r.tc.alertTick(start.Add(takAlertRefresh + time.Second))
	if n := r.count(t, "medic", true); n != 2 {
		t.Fatalf("alerts %d after the refresh time, want 2", n)
	}
	if takAlertRefresh >= 10*time.Minute/2 {
		t.Fatalf("refresh %v not well inside the 10 min stale time", takAlertRefresh)
	}
	r.emergency("medic", false)
	r.tc.alertTick(start.Add(3 * takAlertRefresh))
	if n, c := r.count(t, "medic", true), r.count(t, "medic", false); n != 2 || c != 1 {
		t.Fatalf("after the end: alerts %d cancels %d, want 2, 1", n, c)
	}
}
