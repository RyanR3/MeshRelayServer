// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

// The MeshRelayTAK plugin's own test vectors (DispatchPacketsTest):
// emergency(3, "Medic 2", true, 41.65, -74.69) and emergency(3, "Medic 2", false).
const (
	pluginEmergSet = "260000000300074d656469632032014044d33333333333c052ac28f5c28f5c"
	pluginEmergClr = "260000000300074d65646963203200"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseEmergencyHex(t *testing.T, s string) *EmergencyAlert {
	t.Helper()
	typ, pkt, err := ParsePacket(mustHex(t, s))
	if err != nil || typ != PacketTypeEmergencyAlert {
		t.Fatalf("%s: type %#x err %v", s, typ, err)
	}
	return pkt.(*EmergencyAlert)
}

func withTail(body string, lat, lon float64) string {
	b := binary.BigEndian.AppendUint64(nil, math.Float64bits(lat))
	b = binary.BigEndian.AppendUint64(b, math.Float64bits(lon))
	return body + hex.EncodeToString(b)
}

func TestParseEmergencyAlertPositionTail(t *testing.T) {
	a := parseEmergencyHex(t, pluginEmergSet)
	if a.Channel != 3 || a.User != "Medic 2" || !a.Active || !a.HasFix || a.Lat != 41.65 || a.Lon != -74.69 {
		t.Fatalf("plugin set: %+v", a)
	}
	a = parseEmergencyHex(t, pluginEmergClr)
	if a.Channel != 3 || a.User != "Medic 2" || a.Active || a.HasFix {
		t.Fatalf("plugin clear: %+v", a)
	}
	// Without the tail: exactly as before.
	noTail := pluginEmergSet[:30]
	if a = parseEmergencyHex(t, noTail); *a != (EmergencyAlert{Channel: 3, User: "Medic 2", Active: true}) {
		t.Fatalf("no tail: %+v", a)
	}
	// A short or invalid tail is ignored, the rest still parses.
	for name, s := range map[string]string{
		"short tail": noTail + "4044d33333333333",
		"NaN lat":    withTail(noTail, math.NaN(), -74.69),
		"Inf lon":    withTail(noTail, 41.65, math.Inf(1)),
		"lat 91":     withTail(noTail, 91, -74.69),
		"lon -181":   withTail(noTail, 41.65, -181),
		"0,0":        withTail(noTail, 0, 0),
	} {
		if a = parseEmergencyHex(t, s); *a != (EmergencyAlert{Channel: 3, User: "Medic 2", Active: true}) {
			t.Fatalf("%s: %+v", name, a)
		}
	}
	// The server's own build of that alert with that position is the
	// plugin's vector (what other phones are sent).
	pkt := buildEmergencyAlert(3, "Medic 2", true)
	pkt = binary.BigEndian.AppendUint64(pkt, math.Float64bits(41.65))
	pkt = binary.BigEndian.AppendUint64(pkt, math.Float64bits(-74.69))
	if !bytes.Equal(pkt, mustHex(t, pluginEmergSet)) {
		t.Fatalf("server form % x differs from the plugin's", pkt)
	}
}

// emergencyPosRig: the TAK alert rig, with what phones are sent collected.
type emergencyPosRig struct {
	*takAlertRig
	phones map[string][][]byte
}

func newEmergencyPosRig(t *testing.T) *emergencyPosRig {
	r := &emergencyPosRig{takAlertRig: newTakAlertRig(t, true), phones: map[string][][]byte{}}
	byAddr := map[string]string{}
	for _, c := range r.s.clients {
		byAddr[c.Addr.String()] = c.Username
	}
	r.s.testFloorSink = func(a *net.UDPAddr, data []byte) {
		u := byAddr[a.String()]
		r.phones[u] = append(r.phones[u], data)
	}
	return r
}

// alertFrom sends medic's 0x26 exactly as the bytes given (hex).
func (r *emergencyPosRig) alertFrom(t *testing.T, user, hexPkt string) {
	t.Helper()
	_, pkt, err := ParsePacket(mustHex(t, hexPkt))
	if err != nil {
		t.Fatal(err)
	}
	a := pkt.(*EmergencyAlert)
	a.User = user
	r.s.handleEmergencyAlert(a, r.s.clients[userKey("ag", user)].Addr)
}

// phoneFix: the position in the last 0x26 set alice was sent.
func (r *emergencyPosRig) phoneFix(t *testing.T) (lat, lon float64, ok bool) {
	t.Helper()
	for i := len(r.phones["alice"]) - 1; i >= 0; i-- {
		p := r.phones["alice"][i]
		if p[0] != PacketTypeEmergencyAlert {
			continue
		}
		a, err := ParseEmergencyAlert(p[1:])
		if err != nil || !a.Active {
			t.Fatalf("0x26 to alice: % x (%v)", p, err)
		}
		return a.Lat, a.Lon, a.HasFix
	}
	t.Fatal("alice got no emergency alert")
	return
}

func consoleFix(t *testing.T, user string) (lat, lon float64, ok bool) {
	t.Helper()
	for _, m := range activeEmergencies("ag") {
		if m["user"] == user {
			lat, ok1 := m["lat"].(float64)
			lon, ok2 := m["lon"].(float64)
			return lat, lon, ok1 && ok2
		}
	}
	t.Fatalf("console does not show %s", user)
	return
}

// takAlertPoint: the point and remarks of the last server alert for user.
func (r *emergencyPosRig) takAlertPoint(t *testing.T, user string) (point, remarks string) {
	t.Helper()
	r.sync(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	all := strings.Join(r.out, "")
	i := strings.LastIndex(all, `type="b-a-o-tbl" uid="MeshRelay-911-ag-`+user+`"`)
	if i < 0 {
		t.Fatalf("no server alert for %s in %s", user, all)
	}
	ev := all[i:]
	p := ev[strings.Index(ev, "<point "):strings.Index(ev, ` hae=`)]
	rm := ev[strings.Index(ev, "<remarks>")+9 : strings.Index(ev, "</remarks>")]
	return p, rm
}

// A plugin user (no MeshRelay location reports): the position in its alert
// reaches the consoles, the other phones and the server's TAK alert.
func TestEmergencyPositionFromAlertTail(t *testing.T) {
	r := newEmergencyPosRig(t)
	r.alertFrom(t, "medic", pluginEmergSet)
	if lat, lon, ok := consoleFix(t, "medic"); !ok || lat != 41.65 || lon != -74.69 {
		t.Fatalf("console: %v %v %v", lat, lon, ok)
	}
	if lat, lon, ok := r.phoneFix(t); !ok || lat != 41.65 || lon != -74.69 {
		t.Fatalf("phones: %v %v %v", lat, lon, ok)
	}
	if p, rm := r.takAlertPoint(t, "medic"); p != `<point lat="41.650000" lon="-74.690000"` || strings.Contains(rm, "unknown") {
		t.Fatalf("TAK alert: %s / %s", p, rm)
	}
	// A re-send from another place moves the unit on the consoles and in
	// TAK, without raising it again.
	r.alertFrom(t, "medic", withTail(pluginEmergSet[:30], 41.70, -74.60))
	if lat, lon, _ := consoleFix(t, "medic"); lat != 41.70 || lon != -74.60 {
		t.Fatalf("console after move: %v %v", lat, lon)
	}
	if p, _ := r.takAlertPoint(t, "medic"); p != `<point lat="41.700000" lon="-74.600000"` {
		t.Fatalf("TAK alert after move: %s", p)
	}
	if n := r.count(t, "medic", true); n != 2 {
		t.Fatalf("server alerts %d, want 2 (raise + move)", n)
	}
	// The same place again changes nothing.
	r.alertFrom(t, "medic", withTail(pluginEmergSet[:30], 41.70, -74.60))
	if n := r.count(t, "medic", true); n != 2 {
		t.Fatalf("server alerts %d after an unmoved re-send, want 2", n)
	}
	r.alertFrom(t, "medic", pluginEmergClr)
	if consoleShows("medic") {
		t.Fatal("not cleared")
	}
}

// The newer of the alert's position and a MeshRelay location report wins.
func TestEmergencyPositionNewestOfAlertAndReport(t *testing.T) {
	r := newEmergencyPosRig(t)
	medic := r.s.clients[userKey("ag", "medic")]
	medic.Loc = &UnitLocation{Lat: 41.5, Lon: -74.5, At: time.Now().Add(-time.Minute)}
	r.alertFrom(t, "medic", pluginEmergSet)
	if lat, _, _ := consoleFix(t, "medic"); lat != 41.65 {
		t.Fatalf("older report used over the alert's position: %v", lat)
	}
	r.s.mu.Lock()
	medic.Loc = &UnitLocation{Lat: 41.4, Lon: -74.4, At: time.Now().Add(time.Second)}
	r.s.mu.Unlock()
	if lat, lon, ok := r.s.unitPosition("ag", "medic"); !ok || lat != 41.4 || lon != -74.4 {
		t.Fatalf("newer report not used: %v %v", lat, lon)
	}
	r.alertFrom(t, "medic", pluginEmergClr)
}

// No position in the alert and no location report: the unit's TAK position,
// found by callsign (any case) on the agency's TAK connection, if fresh.
func TestEmergencyPositionFromTakUnit(t *testing.T) {
	r := newEmergencyPosRig(t)
	r.feed(t, pli("ANDROID-0123456789abcdef", "MEDIC")) // 41.70, -74.70
	r.feed(t, pli("ANDROID-fedcba9876543210", "medic 2"))
	r.alertFrom(t, "medic", pluginEmergSet[:30])
	if lat, lon, ok := consoleFix(t, "medic"); !ok || lat != 41.70 || lon != -74.70 {
		t.Fatalf("console: %v %v %v", lat, lon, ok)
	}
	if lat, lon, ok := r.phoneFix(t); !ok || lat != 41.70 || lon != -74.70 {
		t.Fatalf("phones: %v %v %v", lat, lon, ok)
	}
	if p, rm := r.takAlertPoint(t, "medic"); p != `<point lat="41.700000" lon="-74.700000"` || strings.Contains(rm, "unknown") {
		t.Fatalf("TAK alert: %s / %s", p, rm)
	}
	r.alertFrom(t, "medic", pluginEmergClr)

	// A stale TAK unit does not count.
	r.tc.mu.Lock()
	for _, u := range r.tc.units {
		u.Stale = time.Now().Add(-time.Second)
	}
	r.tc.mu.Unlock()
	if _, _, ok := r.s.unitPosition("ag", "medic"); ok {
		t.Fatal("stale TAK unit used")
	}
}

// Truly no position: the TAK alert is not put at 0,0 but at the middle of
// the agency's units, and says the position is unknown. Consoles and phones
// are given no made-up position.
func TestEmergencyTakAlertNeverAtZero(t *testing.T) {
	r := newEmergencyPosRig(t)
	r.s.clients[userKey("ag", "alice")].Loc = &UnitLocation{Lat: 41.5, Lon: -74.5, At: time.Now()}
	r.alertFrom(t, "medic", pluginEmergSet[:30])
	if _, _, ok := consoleFix(t, "medic"); ok {
		t.Fatal("console given a position for a unit with none")
	}
	if _, _, ok := r.phoneFix(t); ok {
		t.Fatal("phones given a position for a unit with none")
	}
	p, rm := r.takAlertPoint(t, "medic")
	if p != `<point lat="41.500000" lon="-74.500000"` || !strings.Contains(rm, "position unknown") {
		t.Fatalf("TAK alert: %s / %s", p, rm)
	}
	r.alertFrom(t, "medic", pluginEmergClr)

	// Nobody in the agency has a position: the middle of the TAK units.
	r.s.clients[userKey("ag", "alice")].Loc = nil
	r.feed(t, pli("ANDROID-0123456789abcdef", "Engine 1"))
	r.alertFrom(t, "medic", pluginEmergSet[:30])
	p, rm = r.takAlertPoint(t, "medic")
	if p != `<point lat="41.700000" lon="-74.700000"` || !strings.Contains(rm, "position unknown") {
		t.Fatalf("TAK alert: %s / %s", p, rm)
	}
	r.alertFrom(t, "medic", pluginEmergClr)
}
