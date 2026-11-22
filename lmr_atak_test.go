// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"encoding/xml"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

var lmrTestNow = time.Date(2026, 11, 21, 15, 4, 5, 123000000, time.UTC)

// --- builder ------------------------------------------------------------------

func TestLMRMarkerCoTExact(t *testing.T) {
	m := lmrMarker{BridgeID: "a1b2c3d4e5f60718", Channel: 100, Name: "County VHF", Lat: 41.6512, Lon: -74.6931}
	got := lmrMarkerCoT(m, "00ff", lmrTestNow, lmrMarkerStale)

	icon := ""
	if LMRMarkerIcon != "" {
		icon = `<usericon iconsetpath="` + LMRMarkerIcon + `"/>`
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<event version="2.0" uid="MeshRelay-LMR-a1b2c3d4e5f60718" type="` + LMRMarkerType + `" how="h-g-i-g-o" ` +
		`time="2026-11-21T15:04:05.123Z" start="2026-11-21T15:04:05.123Z" stale="2026-11-21T15:07:05.123Z">` +
		`<point lat="41.6512" lon="-74.6931" hae="9999999.0" ce="9999999.0" le="9999999.0"/>` +
		`<detail><contact callsign="County VHF"/>` + icon +
		`<__meshrelay kind="lmr" channel="100" name="County VHF" server="00ff"/>` +
		`<remarks>Radio channel bridged by MeshRelay. Listen with the MeshRelay ATAK plugin.</remarks>` +
		`</detail></event>`
	if got != want {
		t.Fatalf("marker XML\n got: %s\nwant: %s", got, want)
	}
	// The values agreed with the ATAK plugin, spelled out.
	if !strings.Contains(got, ` type="a-n-G" `) ||
		!strings.Contains(got, `<contact callsign="County VHF"/><usericon iconsetpath="de450cbf-2ffc-47fb-bd2b-ba2db89b035e/Incident/Communication-Ops-Generic.png"/><__meshrelay `) {
		t.Fatalf("type/icon differ from the plugin contract: %s", got)
	}

	// No server identity: the attribute is left out, not sent empty.
	if g := lmrMarkerCoT(m, "", lmrTestNow, lmrMarkerStale); strings.Contains(g, "server=") {
		t.Fatalf("empty server id should omit the attribute: %s", g)
	}
	// The withdraw copy is stale immediately.
	if g := lmrMarkerCoT(m, "", lmrTestNow, 0); !strings.Contains(g, `stale="2026-11-21T15:04:05.123Z"`) {
		t.Fatalf("withdraw copy not stale now: %s", g)
	}
}

// What a parser sees in the marker.
type parsedMarker struct {
	UID   string `xml:"uid,attr"`
	Type  string `xml:"type,attr"`
	Stale string `xml:"stale,attr"`
	Point struct {
		Lat string `xml:"lat,attr"`
		Lon string `xml:"lon,attr"`
	} `xml:"point"`
	Detail struct {
		Contact struct {
			Callsign string `xml:"callsign,attr"`
		} `xml:"contact"`
		MR struct {
			Kind    string `xml:"kind,attr"`
			Channel string `xml:"channel,attr"`
			Name    string `xml:"name,attr"`
			Server  string `xml:"server,attr"`
		} `xml:"__meshrelay"`
		Link struct {
			UID      string `xml:"uid,attr"`
			Relation string `xml:"relation,attr"`
		} `xml:"link"`
		Remarks string `xml:"remarks"`
		Extra   []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"detail"`
}

func parseEvents(t *testing.T, doc string) []parsedMarker {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	var out []parsedMarker
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "event" {
			var ev parsedMarker
			if err := dec.DecodeElement(&ev, &se); err != nil {
				t.Fatalf("event does not parse: %v\n%s", err, doc)
			}
			out = append(out, ev)
		}
	}
	return out
}

func TestLMRMarkerCoTEscapesHostileName(t *testing.T) {
	hostile := `Ops"/><__meshrelay kind="lmr" channel="1" server="evil"/></detail><x a='1'>&amp; <script>` + "\n\x00"
	m := lmrMarker{BridgeID: `id"<&>`, Channel: 101, Name: hostile, Lat: 1, Lon: 2}
	doc := lmrMarkerCoT(m, "", lmrTestNow, lmrMarkerStale)

	if strings.Contains(doc, "<script>") || strings.Contains(doc, `server="evil"`) || strings.Contains(doc, "<x ") {
		t.Fatalf("hostile name leaked markup: %s", doc)
	}
	evs := parseEvents(t, doc)
	if len(evs) != 1 {
		t.Fatalf("want exactly one event, got %d: %s", len(evs), doc)
	}
	ev := evs[0]
	// Only the elements we wrote: contact, (usericon), __meshrelay, remarks.
	for _, e := range ev.Detail.Extra {
		switch e.XMLName.Local {
		case "contact", "usericon", "__meshrelay", "remarks":
		default:
			t.Fatalf("unexpected element <%s> in detail: %s", e.XMLName.Local, doc)
		}
	}
	wantName := strings.ReplaceAll(hostile, "\x00", "�") // XML cannot carry NUL
	if ev.Detail.Contact.Callsign != wantName || ev.Detail.MR.Name != wantName {
		t.Fatalf("name did not survive escaping: callsign %q name %q", ev.Detail.Contact.Callsign, ev.Detail.MR.Name)
	}
	if ev.Detail.MR.Channel != "101" || ev.Detail.MR.Server != "" || ev.UID != `MeshRelay-LMR-id"<&>` {
		t.Fatalf("attributes: %+v", ev)
	}

	// The withdraw pair escapes too and parses as two events.
	w := parseEvents(t, lmrMarkerWithdrawCoT(m, "", lmrTestNow))
	if len(w) != 2 || w[1].Type != "t-x-d-d" || w[1].Detail.Link.UID != `MeshRelay-LMR-id"<&>` || w[1].Detail.Link.Relation != "none" {
		t.Fatalf("withdraw: %+v", w)
	}
}

func TestLMRMarkerCoordinatesAtBounds(t *testing.T) {
	for _, c := range []struct{ lat, lon float64 }{{90, 180}, {-90, -180}, {0, 0}, {41.123456789, -74.987654321}} {
		ev := parseEvents(t, lmrMarkerCoT(lmrMarker{BridgeID: "b", Channel: 100, Name: "n", Lat: c.lat, Lon: c.lon}, "", lmrTestNow, lmrMarkerStale))[0]
		lat, _ := strconv.ParseFloat(ev.Point.Lat, 64)
		lon, _ := strconv.ParseFloat(ev.Point.Lon, 64)
		if lat != c.lat || lon != c.lon {
			t.Fatalf("coords %v,%v came out as %s,%s", c.lat, c.lon, ev.Point.Lat, ev.Point.Lon)
		}
	}
}

func TestValidLMRLocation(t *testing.T) {
	ok := [][2]float64{{0, 0}, {90, 180}, {-90, -180}, {41.65, -74.69}}
	bad := [][2]float64{{90.0001, 0}, {-90.0001, 0}, {0, 180.0001}, {0, -180.0001},
		{math.NaN(), 0}, {0, math.NaN()}, {math.Inf(1), 0}, {0, math.Inf(-1)}}
	for _, c := range ok {
		if err := validLMRLocation(c[0], c[1]); err != nil {
			t.Errorf("%v rejected: %v", c, err)
		}
	}
	for _, c := range bad {
		if validLMRLocation(c[0], c[1]) == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

// --- persistence ----------------------------------------------------------------

func TestLMRBridgeATAKPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lmr.json")
	// A file written before the ATAK fields existed.
	old := `[{"id":"old1","name":"County","username":"gw","owner_agency_id":"fire","agencies":["fire"],` +
		`"channel":100,"channel_name":"County VHF","enabled":true,"created_at":"2026-01-01T00:00:00Z","created_by":"root"}]`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s := newLMRTestServer()
	s.config = &Config{LMRBridgeFile: path}
	s.loadLMRBridges()
	b := s.lmrBridges["old1"]
	if b == nil || b.ShowInATAK || b.Lat != nil || b.Lon != nil || b.ChannelName != "County VHF" || !b.Enabled {
		t.Fatalf("old file loaded as %+v", b)
	}

	// Saved unchanged: no new keys appear for a bridge that never used them.
	if err := s.saveLMRBridges(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, k := range []string{"show_in_atak", `"lat"`, `"lon"`} {
		if strings.Contains(string(data), k) {
			t.Fatalf("unset field %s written: %s", k, data)
		}
	}

	// Round trip with the marker on, including a location at 0,0.
	b.ShowInATAK, b.Lat, b.Lon = true, f64(0), f64(0)
	s.lmrBridges["new2"] = &LMRBridge{ID: "new2", OwnerAgencyID: "ems", Agencies: []string{"ems"}, Channel: 101,
		ShowInATAK: true, Lat: f64(-33.8688), Lon: f64(151.2093)}
	if err := s.saveLMRBridges(); err != nil {
		t.Fatal(err)
	}
	s2 := newLMRTestServer()
	s2.config = &Config{LMRBridgeFile: path}
	s2.loadLMRBridges()
	if b := s2.lmrBridges["old1"]; !b.ShowInATAK || b.Lat == nil || *b.Lat != 0 || *b.Lon != 0 {
		t.Fatalf("0,0 location lost: %+v", b)
	}
	if b := s2.lmrBridges["new2"]; !b.ShowInATAK || *b.Lat != -33.8688 || *b.Lon != 151.2093 {
		t.Fatalf("round trip: %+v", b)
	}
}

// --- API -----------------------------------------------------------------------

func TestLMRBridgeATAKAPI(t *testing.T) {
	s := newWebTestServer(t)
	addTestSession(s, "emsadmin", "medic", "agency_admin", "ems")
	addTestSession(s, "defaultadmin", "boss", "agency_admin", "default")
	s.lmrBridges["l1"] = &LMRBridge{ID: "l1", OwnerAgencyID: "fire", Agencies: []string{"fire", "ems"},
		Channel: 100, ChannelName: "County VHF", Enabled: true}
	h := s.webHandler()
	mine := "/api/my-agency/lmr-bridges/l1/atak"
	on := `{"show_in_atak":true,"lat":41.65,"lon":-74.69}`

	// Validation.
	for _, c := range []struct{ body, why string }{
		{`{"show_in_atak":true}`, "on without a location"},
		{`{"show_in_atak":true,"lat":null,"lon":null}`, "on with a null location"},
		{`{"show_in_atak":false,"lat":41.65}`, "latitude without longitude"},
		{`{"show_in_atak":true,"lat":91,"lon":0}`, "latitude out of range"},
		{`{"show_in_atak":true,"lat":0,"lon":-181}`, "longitude out of range"},
		{`{"show_in_atak":true,"lat":"x","lon":0}`, "not a number"},
		{`{"show_in_atak":true,"lat":NaN,"lon":0}`, "NaN"},
	} {
		if rec := doReq(h, "PUT", mine, "fireadmin", c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", c.why, rec.Code, rec.Body.String())
		}
	}
	if b := s.lmrBridges["l1"]; b.ShowInATAK || b.Lat != nil {
		t.Fatalf("a refused request changed the bridge: %+v", b)
	}

	// Who may change it: the owner and a super admin, as for pause and guests.
	// A recipient of the bridge, an unrelated agency and dispatch may not.
	for _, c := range []struct {
		tok  string
		path string
		code int
	}{
		{"emsadmin", mine, http.StatusForbidden},
		{"defaultadmin", mine, http.StatusForbidden},
		{"firedispatch", mine, http.StatusForbidden},
		{"", mine, http.StatusUnauthorized},
		{"fireadmin", "/api/lmr-bridges/l1/atak", http.StatusForbidden},
		{"fireadmin", "/api/my-agency/lmr-bridges/nope/atak", http.StatusNotFound},
	} {
		rec := doReq(h, "PUT", c.path, c.tok, on)
		if rec.Code != c.code && !(c.code == http.StatusUnauthorized && rec.Code == http.StatusForbidden) {
			t.Errorf("%s %s: %d %s, want %d", c.tok, c.path, rec.Code, rec.Body.String(), c.code)
		}
	}
	if s.lmrBridges["l1"].ShowInATAK {
		t.Fatal("a refused caller switched the marker on")
	}

	rec := doReq(h, "PUT", mine, "fireadmin", on)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Marker []lmrAtakAgencyState `json:"atak_marker"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Marker) != 1 || resp.Marker[0].AgencyID != "fire" || resp.Marker[0].TAK != "none" {
		t.Fatalf("owner sees only its own TAK link: %s", rec.Body.String())
	}
	if b := s.lmrBridges["l1"]; !b.ShowInATAK || *b.Lat != 41.65 || *b.Lon != -74.69 {
		t.Fatalf("not stored: %+v", b)
	}
	data, _ := os.ReadFile(s.lmrBridgesFile())
	var saved []LMRBridge
	if json.Unmarshal(data, &saved) != nil || len(saved) != 1 || !saved[0].ShowInATAK || saved[0].Lat == nil || *saved[0].Lat != 41.65 {
		t.Fatalf("not saved: %s", data)
	}

	// The owner's list shows the state and the TAK link; a recipient sees the
	// switch too (the marker reaches its TAK server).
	rec = doReq(h, "GET", "/api/my-agency/lmr-bridges", "fireadmin", "")
	if !strings.Contains(rec.Body.String(), `"show_in_atak":true`) || !strings.Contains(rec.Body.String(), `"atak_tak":"none"`) {
		t.Fatalf("owner list: %s", rec.Body.String())
	}
	rec = doReq(h, "GET", "/api/lmr-bridges", "super", "")
	if !strings.Contains(rec.Body.String(), `"atak_marker":[{"agency_id":"fire","name":"FIRE","tak":"none"},{"agency_id":"ems","name":"EMS","tak":"none"}]`) {
		t.Fatalf("super list: %s", rec.Body.String())
	}

	// Super admin, through the super admin route: off, location cleared.
	if rec := doReq(h, "PUT", "/api/lmr-bridges/l1/atak", "super", `{"show_in_atak":false,"lat":null,"lon":null}`); rec.Code != http.StatusOK {
		t.Fatalf("super: %d %s", rec.Code, rec.Body.String())
	}
	if b := s.lmrBridges["l1"]; b.ShowInATAK || b.Lat != nil || b.Lon != nil {
		t.Fatalf("not cleared: %+v", b)
	}
	data, _ = os.ReadFile(s.lmrBridgesFile())
	if strings.Contains(string(data), "show_in_atak") || strings.Contains(string(data), `"lat"`) {
		t.Fatalf("cleared fields still written: %s", data)
	}
}

// --- publisher -------------------------------------------------------------------

// fakeTakLinks stands in for the TAK connections at the deliver seam.
type fakeTakLinks struct {
	up   map[string]bool
	sent map[string][]string
}

func (f *fakeTakLinks) deliver(agency, cot string) bool {
	if !f.up[agency] {
		return false
	}
	f.sent[agency] = append(f.sent[agency], cot)
	return true
}

func (f *fakeTakLinks) take(agency string) string {
	out := strings.Join(f.sent[agency], "")
	f.sent[agency] = nil
	return out
}

func newMarkerTestServer() (*Server, *lmrMarkerPublisher, *fakeTakLinks) {
	s := newLMRTestServer()
	links := &fakeTakLinks{up: map[string]bool{"fire": true, "ems": true, "dpw": true}, sent: map[string][]string{}}
	p := s.lmrMarkers()
	p.deliver = links.deliver
	p.now = func() time.Time { return lmrTestNow }
	return s, p, links
}

func countMarker(doc, bridgeID string) (shows, deletes int) {
	uid := `uid="MeshRelay-LMR-` + bridgeID + `"`
	for _, ev := range strings.Split(doc, "<event ")[1:] {
		switch {
		case strings.Contains(ev, `type="t-x-d-d"`) && strings.Contains(ev, `<link `+uid):
			deletes++
		case strings.HasPrefix(ev, `version="2.0" `+uid):
			shows++
		}
	}
	return
}

func TestLMRMarkerPublishAndWithdraw(t *testing.T) {
	s, p, links := newMarkerTestServer()
	b := &LMRBridge{ID: "b1", OwnerAgencyID: "fire", Agencies: []string{"fire", "ems"}, Channel: 100,
		ChannelName: "County VHF", Enabled: true}
	s.lmrBridges["b1"] = b
	// Another bridge that never shows: nothing is ever sent for it.
	s.lmrBridges["b2"] = &LMRBridge{ID: "b2", OwnerAgencyID: "fire", Agencies: []string{"fire"}, Channel: 101,
		Enabled: true, Lat: f64(1), Lon: f64(1)}

	// Off by default: nothing sent, nothing withdrawn.
	p.pass()
	if len(links.sent) != 0 {
		t.Fatalf("sent while off: %v", links.sent)
	}

	// Switched on: both agencies get the marker; a pass is also the 60 s re-send.
	b.ShowInATAK, b.Lat, b.Lon = true, f64(41.65), f64(-74.69)
	p.pass()
	for _, a := range []string{"fire", "ems"} {
		got := links.take(a)
		if sh, del := countMarker(got, "b1"); sh != 1 || del != 0 || !strings.Contains(got, `stale="2026-11-21T15:07:05.123Z"`) {
			t.Fatalf("%s after switch-on: %s", a, got)
		}
	}
	if links.take("dpw") != "" {
		t.Fatal("an agency outside the bridge got the marker")
	}

	// Renamed: the next pass carries the new name.
	b.ChannelName = "Fire Dispatch"
	p.pass()
	if got := links.take("fire"); !strings.Contains(got, `callsign="Fire Dispatch"`) {
		t.Fatalf("rename not published: %s", got)
	}
	links.take("ems")

	// ems removed from the bridge: ems gets the withdrawal (stale copy +
	// delete), fire keeps the marker.
	b.Agencies = []string{"fire"}
	p.pass()
	got := links.take("ems")
	if sh, del := countMarker(got, "b1"); sh != 1 || del != 1 || !strings.Contains(got, `stale="2026-11-21T15:04:05.123Z"`) {
		t.Fatalf("ems withdrawal: %s", got)
	}
	if sh, del := countMarker(links.take("fire"), "b1"); sh != 1 || del != 0 {
		t.Fatal("fire lost the marker when ems was removed")
	}
	p.pass() // a withdrawal is sent once
	if links.take("ems") != "" {
		t.Fatal("ems withdrawal repeated")
	}
	links.take("fire")

	// Paused: withdrawn; resumed: back.
	b.Enabled = false
	p.pass()
	if _, del := countMarker(links.take("fire"), "b1"); del != 1 {
		t.Fatal("pausing did not withdraw the marker")
	}
	b.Enabled = true
	p.pass()
	if sh, _ := countMarker(links.take("fire"), "b1"); sh != 1 {
		t.Fatal("resuming did not restore the marker")
	}

	// Switched off: withdrawn.
	b.ShowInATAK = false
	p.pass()
	if _, del := countMarker(links.take("fire"), "b1"); del != 1 {
		t.Fatal("switching off did not withdraw the marker")
	}
	p.pass()
	if len(links.take("fire")) != 0 {
		t.Fatal("sent after switch-off")
	}

	// Deleted while shown: withdrawn.
	b.ShowInATAK = true
	p.pass()
	links.take("fire")
	delete(s.lmrBridges, "b1")
	p.pass()
	if _, del := countMarker(links.take("fire"), "b1"); del != 1 {
		t.Fatal("deleting the bridge did not withdraw the marker")
	}
}

// A withdrawal that cannot go out (TAK link down) is delivered when the link
// is back; a marker is not sent while down and is sent on the next pass after.
func TestLMRMarkerWithdrawWaitsForTAK(t *testing.T) {
	s, p, links := newMarkerTestServer()
	b := &LMRBridge{ID: "b1", Agencies: []string{"fire"}, Channel: 100, Enabled: true,
		ShowInATAK: true, Lat: f64(41), Lon: f64(-74)}
	s.lmrBridges["b1"] = b
	links.up["fire"] = false
	p.pass()
	if links.take("fire") != "" {
		t.Fatal("sent on a link that is down")
	}
	links.up["fire"] = true
	p.pass()
	if sh, _ := countMarker(links.take("fire"), "b1"); sh != 1 {
		t.Fatal("not sent once the link was up")
	}

	links.up["fire"] = false
	b.ShowInATAK = false
	p.pass()
	links.up["fire"] = true
	p.pass()
	if _, del := countMarker(links.take("fire"), "b1"); del != 1 {
		t.Fatal("pending withdrawal not delivered after reconnect")
	}

	// Switched off and on again while the link was down: no stray delete.
	b.ShowInATAK = true
	p.pass()
	links.take("fire")
	links.up["fire"] = false
	b.ShowInATAK = false
	p.pass()
	b.ShowInATAK = true
	links.up["fire"] = true
	p.pass()
	if sh, del := countMarker(links.take("fire"), "b1"); sh != 1 || del != 0 {
		t.Fatalf("off/on while down: shows %d deletes %d", sh, del)
	}
}

// End to end over a real TLS CoT stream (the fake TAK server in tak_test.go):
// the marker arrives, comes back promptly after a reconnect, and the delete
// arrives when it is switched off.
func TestLMRMarkerOverTakConnection(t *testing.T) {
	s := newLMRTestServer()
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Agency"}}
	f := startFakeTAK(t)
	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	cfg := TakConfig{AgencyID: "ag", Host: "127.0.0.1", StreamPort: f.stream.Addr().(*net.TCPAddr).Port,
		EnrollPort: enrollPort, Username: "dispatch", Callsign: "MeshRelay Dispatch", UID: "MeshRelay-Dispatch-ag-t", Enabled: true}
	if err := takEnroll(&cfg, "secret"); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	b := &LMRBridge{ID: "e2e", Agencies: []string{"ag"}, Channel: 102, ChannelName: "Mutual <Aid>", Enabled: true,
		ShowInATAK: true, Lat: f64(41.65), Lon: f64(-74.69)}
	s.lmrBridges["e2e"] = b
	stop := make(chan struct{})
	defer close(stop)
	go s.lmrMarkers().run(stop)

	s.startTak(cfg)
	defer s.stopTak("ag")
	select {
	case <-f.connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("connector never connected")
	}
	waitFor(t, "marker on the TAK stream", func() bool {
		sh, _ := countMarker(f.got(), "e2e")
		return sh >= 1 && strings.Contains(f.got(), `callsign="Mutual &lt;Aid&gt;"`)
	})
	if st := s.takLinkState("ag"); st != "connected" {
		t.Fatalf("link state %q", st)
	}

	// Drop the stream from the TAK side. The connector reconnects (2 s
	// back-off) and the marker is sent again right away, well inside 60 s.
	before, _ := countMarker(f.got(), "e2e")
	f.mu.Lock()
	f.conn.Close()
	f.mu.Unlock()
	select {
	case <-f.connCh:
	case <-time.After(10 * time.Second):
		t.Fatal("connector did not reconnect")
	}
	waitFor(t, "marker after reconnect", func() bool {
		sh, _ := countMarker(f.got(), "e2e")
		return sh > before
	})

	// Switched off: the delete goes out promptly.
	s.mu.Lock()
	b.ShowInATAK = false
	s.mu.Unlock()
	s.lmrMarkersChanged()
	waitFor(t, "delete on the TAK stream", func() bool {
		_, del := countMarker(f.got(), "e2e")
		return del == 1
	})
}
