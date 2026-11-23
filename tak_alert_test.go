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

// A TAK user's own emergency always shows, even with a callsign like (or
// equal to) a MeshRelay unit's, or a MeshRelay-looking uid for a name that is
// not in a MeshRelay emergency.
func TestTakThirdPartyEmergencyStillShown(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.emergency("medic", true)
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", true))
	if !consoleShows("medic (TAK)") {
		t.Fatal("ATAK user's emergency (same callsign, ATAK uid) hidden")
	}
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "medic", false))
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

// A unit of another agency on this server whose phone reports into this
// agency's TAK server: its phone alert is not a second emergency either. The
// server sends no alert of its own for it here (its agency has no connector).
func TestTakPhoneAlertOtherAgencyNotDuplicated(t *testing.T) {
	r := newTakAlertRig(t, true)
	r.s.raiseEmergency("other", "Xcover", 0)
	if len(activeEmergencies("other")) == 0 {
		t.Fatal("other agency's MeshRelay emergency missing")
	}
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "XCOVER", true))
	if consoleShows("XCOVER (TAK)") {
		t.Fatal("other agency's unit shown twice")
	}
	// A TAK user with that callsign but an ATAK uid still shows.
	r.feed(t, atakAlert("ANDROID-0123456789abcdef", "Xcover", true))
	if !consoleShows("Xcover (TAK)") {
		t.Fatal("ATAK user's emergency hidden")
	}
	// End: a repeat still on the way and the phone's cancel stay hidden.
	r.s.clearEmergency("other", "Xcover", "Xcover")
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "XCOVER", true))
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "XCOVER", false))
	if consoleShows("XCOVER (TAK)") {
		t.Fatal("late repeat shown")
	}
	// Later, with no MeshRelay emergency, the phone's alert shows again.
	r.feed(t, atakAlert("MeshRelay-0a1b2c3d", "XCOVER", true))
	if !consoleShows("XCOVER (TAK)") {
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
