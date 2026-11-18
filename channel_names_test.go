// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// parseChannelNames decodes a 0x32 packet the way a phone does.
func parseChannelNames(t *testing.T, p []byte) []string {
	t.Helper()
	if len(p) < 3 || p[0] != PacketTypeChannelNames {
		t.Fatalf("not a channel-names packet: % x", p)
	}
	n := int(binary.BigEndian.Uint16(p[1:]))
	o := 3
	out := make([]string, n)
	for i := 0; i < n; i++ {
		l := int(binary.BigEndian.Uint16(p[o:]))
		o += 2
		out[i] = string(p[o : o+l])
		o += l
	}
	return out
}

func TestAgencyChannelDefaults(t *testing.T) {
	a := &Agency{ID: "x", MaxChannels: MaxAgencyTalkgroups}
	if a.talkgroupCount() != 5 {
		t.Fatalf("default count = %d, want 5", a.talkgroupCount())
	}
	if a.talkgroupName(0) != "Channel 1" {
		t.Fatalf("unnamed = %q", a.talkgroupName(0))
	}
	a.ChannelCount, a.MaxChannels = 30, 10
	if a.talkgroupCount() != 10 {
		t.Fatalf("count must be capped by max_channels, got %d", a.talkgroupCount())
	}
	got := parseChannelNames(t, buildChannelNames(&Agency{ChannelNames: map[int]string{1: "1-TG2"}}))
	if len(got) != 5 || got[0] != "" || got[1] != "1-TG2" {
		t.Fatalf("packet names = %q", got)
	}
}

func putChannels(t *testing.T, s *Server, token string, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/my-agency/channels", bytes.NewReader(b))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	rec := httptest.NewRecorder()
	s.handleUpdateAgencyChannels(rec, req)
	var out map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// An agency admin names their own channels; every phone of that agency (and
// only that agency) gets the new names at once.
func TestAgencyAdminRenamesOwnChannelsAndPhonesGetThem(t *testing.T) {
	s, sink := newSecTestServer(t)
	s.webSessions = map[string]*WebSession{}
	addTestSession(s, "tok-a1", "boss", "agency_admin", "a1")
	s.clients[userKey("a1", "u1")] = &Client{Username: "u1", AgencyID: "a1", Addr: udp(7001)}
	s.clients[userKey("a2", "u2")] = &Client{Username: "u2", AgencyID: "a2", Addr: udp(7002)}

	// agency_id is ignored for an agency admin: this renames a1, not a2.
	code, out := putChannels(t, s, "tok-a1", map[string]interface{}{
		"agency_id": "a2", "count": 6, "names": []string{"1-TG1", "", "Fire Ops"},
	})
	if code != 200 || out["agency_id"] != "a1" {
		t.Fatalf("PUT = %d %v", code, out)
	}
	if s.agencies["a2"].ChannelCount != 0 {
		t.Fatalf("agency admin changed another agency")
	}
	var got []string
	for _, p := range sink.take(udp(7001)) {
		if p[0] == PacketTypeChannelNames {
			got = parseChannelNames(t, p)
		}
	}
	if len(got) != 6 || got[0] != "1-TG1" || got[1] != "" || got[2] != "Fire Ops" {
		t.Fatalf("phone got %q", got)
	}
	for _, p := range sink.take(udp(7002)) {
		if p[0] == PacketTypeChannelNames {
			t.Fatalf("another agency's phone got the names")
		}
	}
	// The console directory uses the same names.
	_, chans := s.dispatchDirectory("a1")
	if len(chans) < 6 || chans[0].Name != "1-TG1" || chans[1].Name != "Channel 2" {
		t.Fatalf("console directory = %+v", chans)
	}
}

func TestChannelNameValidation(t *testing.T) {
	s, _ := newSecTestServer(t)
	s.webSessions = map[string]*WebSession{}
	addTestSession(s, "tok-root", "root", "super_admin", "")
	for _, tc := range []map[string]interface{}{
		{"agency_id": "a1", "count": 0},
		{"agency_id": "a1", "count": MaxAgencyTalkgroups + 1},
		{"agency_id": "a1", "count": 3, "names": []string{"Ops", "ops"}},                         // duplicate
		{"agency_id": "a1", "count": 3, "names": []string{"<b>x</b>"}},                           // markup
		{"agency_id": "a1", "count": 3, "names": []string{"0123456789012345678901234567890123"}}, // too long
	} {
		if code, out := putChannels(t, s, "tok-root", tc); code == 200 {
			t.Fatalf("accepted %v: %v", tc, out)
		}
	}
	// A super admin can rename any agency.
	if code, out := putChannels(t, s, "tok-root", map[string]interface{}{"agency_id": "a2", "count": 5, "names": []string{"North"}}); code != 200 {
		t.Fatalf("super admin rename = %d %v", code, out)
	}
	if s.agencies["a2"].talkgroupName(0) != "North" {
		t.Fatalf("name not saved")
	}
}
