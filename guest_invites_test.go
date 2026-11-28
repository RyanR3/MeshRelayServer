// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// guestTestServer: agency "ag" with signed-in user alice, auth on, files in a
// temp dir.
func guestTestServer(t *testing.T) (*Server, *net.UDPAddr, map[string][][]byte) {
	t.Helper()
	s, sent := newFloorTestServer(t, "alice")
	dir := t.TempDir()
	s.config = &Config{UsersFile: dir + "/users.json", AgenciesFile: dir + "/agencies.json"}
	s.activityLog = NewActivityLog(dir+"/activity.json", 100)
	s.users = map[string]*UserAccount{
		userKey("ag", "alice"): {Username: "alice", AgencyID: "ag", Role: "user", PasswordHash: hashPassword("pw")},
	}
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Agency", MaxUsers: 100}}
	s.authenticatedAddrs = map[string]string{}
	addr := s.clients[userKey("ag", "alice")].Addr
	s.authenticatedAddrs[addr.String()] = "ag"
	s.authAccounts = map[string]string{addr.String(): userKey("ag", "alice")}
	return s, addr, sent
}

func decodeInviteResponse(t *testing.T, p []byte) *GuestInviteResponse {
	t.Helper()
	if p[0] != PacketTypeGuestInviteResponse {
		t.Fatalf("packet type %#x, want guest invite response", p[0])
	}
	r := bytes.NewReader(p[1:])
	ok, _ := r.ReadByte()
	out := &GuestInviteResponse{OK: ok == 1}
	out.Username, _ = getStr(r)
	out.Password, _ = getStr(r)
	out.AgencyID, _ = getStr(r)
	var exp int64
	binary.Read(r, binary.BigEndian, &exp)
	out.ExpiresAt = time.Unix(exp, 0)
	out.Message, _ = getStr(r)
	return out
}

func inviteRequest(hours int, name string) *GuestInviteRequest {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(hours))
	if name != "" {
		putStr(&buf, name)
	}
	req, err := ParseGuestInviteRequest(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return req
}

func TestGuestInviteCreatesLoginInInvitersAgency(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	s.handleGuestInviteRequest(inviteRequest(8, "John Smith"), addr)
	resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1])
	if !resp.OK {
		t.Fatalf("invite refused: %s", resp.Message)
	}
	if resp.Username != "guest-john-smith" || resp.AgencyID != "ag" {
		t.Fatalf("got %s in %s", resp.Username, resp.AgencyID)
	}
	u := s.users[userKey("ag", resp.Username)]
	if u == nil || u.InvitedBy != "alice" || u.GuestExpiresAt == nil {
		t.Fatalf("guest account not stored properly: %+v", u)
	}
	// The QR password logs in the same way an enrollment token does.
	if u.PasswordHash != hashPassword(resp.Password) {
		t.Fatalf("QR password does not match the stored hash")
	}
	if d := time.Until(*u.GuestExpiresAt); d < 7*time.Hour || d > 8*time.Hour {
		t.Fatalf("expiry %v, want ~8h", d)
	}

	// Same name again gets a distinct callsign.
	s.handleGuestInviteRequest(inviteRequest(8, "John Smith"), addr)
	if again := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1]); again.Username != "guest-john-smith-2" {
		t.Fatalf("second guest got %q", again.Username)
	}
}

func TestGuestInviteNoNameAndHourLimits(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	s.handleGuestInviteRequest(inviteRequest(9999, ""), addr)
	resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1])
	if !resp.OK || !strings.HasPrefix(resp.Username, "guest-") {
		t.Fatalf("unnamed invite: %+v", resp)
	}
	if d := time.Until(resp.ExpiresAt); d > time.Duration(guestInviteMaxHours)*time.Hour {
		t.Fatalf("expiry %v beyond the %dh cap", d, guestInviteMaxHours)
	}
}

// An address that has not signed in gets no answer at all: the refusal was
// 15 times the size of the request, an amplifier for a forged source.
func TestGuestInviteNotSignedInGetsNoReply(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	s.authenticatedAddrs = map[string]string{}
	s.handleGuestInviteRequest(inviteRequest(1, "x"), addr)
	if len(sent["alice"]) != 0 {
		t.Fatalf("an unauthenticated invite request was answered (%d packets)", len(sent["alice"]))
	}
	if len(s.users) != 1 {
		t.Fatalf("a refused invite must not create an account")
	}
}

func TestGuestInviteRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *Server)
		want  string
	}{
		{"agency switched it off", func(s *Server) { s.agencies["ag"].NoGuestInvites = true }, "turned off"},
		{"inviter is a guest", func(s *Server) {
			exp := time.Now().Add(time.Hour)
			s.users[userKey("ag", "alice")].GuestExpiresAt = &exp
		}, "Guests can't"},
		{"agency full", func(s *Server) { s.agencies["ag"].MaxUsers = 1 }, "user limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, addr, sent := guestTestServer(t)
			c.setup(s)
			s.handleGuestInviteRequest(inviteRequest(1, "x"), addr)
			resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1])
			if resp.OK || !strings.Contains(resp.Message, c.want) {
				t.Fatalf("got ok=%v %q, want refusal containing %q", resp.OK, resp.Message, c.want)
			}
			if len(s.users) != 1 {
				t.Fatalf("a refused invite must not create an account")
			}
		})
	}
}

func TestGuestInvitePerInviterCap(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	for i := 0; i < guestInvitesPerInviter; i++ {
		s.handleGuestInviteRequest(inviteRequest(1, ""), addr)
	}
	s.handleGuestInviteRequest(inviteRequest(1, ""), addr)
	if resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1]); resp.OK {
		t.Fatalf("invite %d should be refused", guestInvitesPerInviter+1)
	}
}

func TestExpiredGuestIsRemovedAndKicked(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	s.handleGuestInviteRequest(inviteRequest(1, "temp"), addr)
	resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1])

	// The guest signs in, then their time runs out.
	gAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000}
	gk := userKey("ag", resp.Username)
	s.clients[gk] = &Client{Username: resp.Username, AgencyID: "ag", Addr: gAddr}
	s.authenticatedAddrs[gAddr.String()] = "ag"
	past := time.Now().Add(-time.Minute)
	s.users[gk].GuestExpiresAt = &past
	var kickedWith []byte
	s.testFloorSink = func(a *net.UDPAddr, data []byte) {
		if a.String() == gAddr.String() && data[0] == PacketTypeAuthResponse {
			kickedWith = data
		}
	}

	finishesInTime(t, "guest expiry", s.cleanupExpiredTokens)
	if s.users[gk] != nil || s.clients[gk] != nil || s.authenticatedAddrs[gAddr.String()] != "" {
		t.Fatalf("expired guest still present")
	}
	if kickedWith == nil {
		t.Fatalf("expired guest's session was not told it ended")
	}
	if s.users[userKey("ag", "alice")] == nil {
		t.Fatalf("the inviter must not be touched")
	}
}

// The app signs in with an account but joins under its callsign ("Unit1" vs
// "Medic4"); the inviter is the account, found via the auth.
func TestGuestInviteWhenCallsignDiffersFromAccount(t *testing.T) {
	s, addr, sent := guestTestServer(t)
	c := s.clients[userKey("ag", "alice")]
	delete(s.clients, userKey("ag", "alice"))
	c.Username = "Medic4"
	s.clients[userKey("ag", "Medic4")] = c
	sent["Medic4"] = nil

	s.handleGuestInviteRequest(inviteRequest(1, "pat"), addr)
	resp := decodeInviteResponse(t, sent["alice"][len(sent["alice"])-1])
	if !resp.OK {
		t.Fatalf("invite refused: %s", resp.Message)
	}
	if u := s.users[userKey("ag", resp.Username)]; u == nil || u.InvitedBy != "alice" {
		t.Fatalf("guest should be credited to the account alice: %+v", u)
	}
}
