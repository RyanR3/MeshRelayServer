// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// loginWith signs in with secret, sending flags, and returns success, the
// reply message and the challenge nonce.
func loginWith(t *testing.T, s *Server, sink *secSink, addr *net.UDPAddr, username, agency, secret string, flags byte) (bool, string, [32]byte) {
	t.Helper()
	sink.take(addr)
	s.handleAuthChallengeRequest(&AuthChallengeRequestPacket{Username: username, AgencyID: agency}, addr)
	var nonce [32]byte
	for _, p := range sink.take(addr) {
		if p[0] == PacketTypeAuthChallenge && len(p) == 33 {
			copy(nonce[:], p[1:])
		}
	}
	mac := hex.EncodeToString(computeHmacSha256([]byte(hashPassword(secret)), nonce[:]))
	s.handleAuthRequest(&AuthRequestPacket{Username: username, PasswordHash: mac, AgencyID: agency, Flags: flags}, addr)
	for _, p := range sink.take(addr) {
		if p[0] == PacketTypeAuthResponse {
			n := int(binary.BigEndian.Uint16(p[2:4]))
			return p[1] == 0x01, string(p[4 : 4+n]), nonce
		}
	}
	t.Fatalf("no auth response to %v", addr)
	return false, "", nonce
}

// unwrapDeviceKey is what the app does with "|devkey=".
func unwrapDeviceKey(t *testing.T, msg, token string, nonce [32]byte) string {
	t.Helper()
	w, err := hex.DecodeString(devkeySuffixFromMessage(msg))
	if err != nil || len(w) != 32 {
		t.Fatalf("no device key in reply %q", msg)
	}
	pad := deviceKeyWrap(token, nonce)
	out := make([]byte, 32)
	for i := range w {
		out[i] = w[i] ^ pad[i]
	}
	return hex.EncodeToString(out)
}

func join(s *Server, addr *net.UDPAddr, callsign string, ch int) {
	s.handleJoin(&JoinPacket{Username: callsign, Channel: ch}, addr)
}

// The QR works once. The phone gets its own key; a copy of the QR is dead.
func TestQRIsOneTimeAndIssuesDeviceKey(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	token := s.GenerateEnrollmentToken("bob", "a1", "")
	if len(token) != 32 {
		t.Fatalf("token should be 128-bit hex, got %d chars", len(token))
	}
	phone := udp(6001)
	ok, msg, nonce := loginWith(t, s, sink, phone, "bob", "a1", token, AuthFlagDeviceKey)
	if !ok {
		t.Fatal("first QR sign-in failed")
	}
	key := unwrapDeviceKey(t, msg, token, nonce)
	bob := s.users[userKey("a1", "bob")]
	if bob.PasswordHash != hashPassword("bob-password-123") {
		t.Fatal("the QR replaced the account password")
	}
	if len(bob.Devices) != 1 || bob.Devices[0].Legacy {
		t.Fatal("expected one (non-legacy) device key")
	}
	// The phone reconnects with its device key.
	if ok, _, _ := loginWith(t, s, sink, udp(6002), "bob", "a1", key, AuthFlagDeviceKey); !ok {
		t.Fatal("device key does not sign in")
	}
	// A copy of the QR from somewhere else: refused.
	if ok, _, _ := loginWith(t, s, sink, &net.UDPAddr{IP: net.IPv4(10, 9, 9, 9), Port: 7000}, "bob", "a1", token, AuthFlagDeviceKey); ok {
		t.Fatal("a copy of a used QR still signs in")
	}
	// After the retry grace the token is gone even from the same address.
	s.enrollmentTokens[token].UsedAt = time.Now().Add(-enrollRetryGrace - time.Second)
	s.pruneAuthLocked(time.Now())
	if ok, _, _ := loginWith(t, s, sink, phone, "bob", "a1", token, AuthFlagDeviceKey); ok {
		t.Fatal("used QR still signs in after the grace period")
	}
	// The password still works (app and dashboard).
	if ok, _, _ := loginWith(t, s, sink, udp(6003), "bob", "a1", "bob-password-123", 0); !ok {
		t.Fatal("password stopped working after QR enrollment")
	}
}

// A lost reply, or the app's two connections at startup: the same phone may
// use the QR again briefly and gets the SAME key; only one key exists.
func TestQRRetryFromSamePhoneReplacesKey(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	token := s.GenerateEnrollmentToken("bob", "a1", "")
	phone := udp(6101)
	_, m1, n1 := loginWith(t, s, sink, phone, "bob", "a1", token, AuthFlagDeviceKey)
	// The app's second connection (other port, same phone) within the grace.
	ok, m2, n2 := loginWith(t, s, sink, udp(6102), "bob", "a1", token, AuthFlagDeviceKey)
	if !ok {
		t.Fatal("retry from the same phone within the grace failed")
	}
	if k1, k2 := unwrapDeviceKey(t, m1, token, n1), unwrapDeviceKey(t, m2, token, n2); k1 != k2 {
		t.Fatal("two sign-ins with the same QR got different keys: one connection would hold a dead key")
	}
	if n := len(s.users[userKey("a1", "bob")].Devices); n != 1 {
		t.Fatalf("retry left %d device keys, want 1", n)
	}
}

// Showing the QR again never extends its life.
func TestQRRegenerateDoesNotExtend(t *testing.T) {
	s, _ := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	tok := s.GenerateEnrollmentToken("bob", "a1", "")
	exp := s.enrollmentTokens[tok].ExpiresAt
	if again := s.GenerateEnrollmentToken("bob", "a1", ""); again != tok || !s.enrollmentTokens[tok].ExpiresAt.Equal(exp) {
		t.Fatal("re-showing the QR changed the token or extended it")
	}
}

// Password reset ends the password's sessions; QR phones stay. Sign out
// phones ends everything and removes the keys.
func TestPasswordResetAndSignOutPhones(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	token := s.GenerateEnrollmentToken("bob", "a1", "")
	qrPhone, pwPhone := udp(6201), udp(6202)
	_, msg, nonce := loginWith(t, s, sink, qrPhone, "bob", "a1", token, AuthFlagDeviceKey)
	key := unwrapDeviceKey(t, msg, token, nonce)
	join(s, qrPhone, "engine1", 0)
	loginWith(t, s, sink, pwPhone, "bob", "a1", "bob-password-123", 0)
	join(s, pwPhone, "engine2", 0)

	if err := s.UpdatePassword("a1", "bob", "new-password-456"); err != nil {
		t.Fatal(err)
	}
	if s.isAuthenticated(pwPhone) || s.clients[userKey("a1", "engine2")] != nil {
		t.Fatal("password reset left the password phone signed in")
	}
	if !s.isAuthenticated(qrPhone) {
		t.Fatal("password reset signed out the QR phone")
	}
	if ok, _, _ := loginWith(t, s, sink, udp(6203), "bob", "a1", key, AuthFlagDeviceKey); !ok {
		t.Fatal("device key stopped working after a password reset")
	}

	if _, err := s.SignOutDevices("a1", "bob"); err != nil {
		t.Fatal(err)
	}
	if s.isAuthenticated(qrPhone) || s.isAuthenticated(udp(6203)) {
		t.Fatal("sign out phones left a session")
	}
	if ok, _, _ := loginWith(t, s, sink, udp(6204), "bob", "a1", key, AuthFlagDeviceKey); ok {
		t.Fatal("removed device key still signs in")
	}
}

// A phone that moves addresses leaves no signed-in address behind; a
// sign-in that never joins expires.
func TestMovedAndUnjoinedAddressesAreDropped(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	oldA, newA := udp(6301), udp(6302)
	loginWith(t, s, sink, oldA, "bob", "a1", "bob-password-123", 0)
	join(s, oldA, "bob", 0)
	loginWith(t, s, sink, newA, "bob", "a1", "bob-password-123", 0)
	join(s, newA, "bob", 0)
	if s.isAuthenticated(oldA) {
		t.Fatal("old address still signed in after the phone moved")
	}
	if !s.isAuthenticated(newA) {
		t.Fatal("new address not signed in")
	}

	lone := udp(6303)
	loginWith(t, s, sink, lone, "bob", "a1", "bob-password-123", 0)
	s.authSessions[lone.String()] = authSession{at: time.Now().Add(-unjoinedAuthLife - time.Second), cred: credPassword}
	s.pruneAuthLocked(time.Now())
	if s.isAuthenticated(lone) {
		t.Fatal("a sign-in that never joined did not expire")
	}
	if !s.isAuthenticated(newA) {
		t.Fatal("pruning dropped a joined phone")
	}
}

// Promoting an account to admin needs a dashboard password the admin types;
// the app password (possibly an old QR token) is never copied.
func TestPromoteNeedsDashboardPassword(t *testing.T) {
	s, _ := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	if err := s.UpdateUserRole("a1", "bob", "admin"); err != ErrNeedsDashboardPassword {
		t.Fatalf("promotion without a password: %v", err)
	}
	if s.users[userKey("a1", "bob")].Role != "user" || s.webAdmins[userKey("a1", "bob")] != nil {
		t.Fatal("refused promotion still changed the account")
	}
	if err := s.UpdateUserRole("a1", "bob", "admin", "short"); err == nil {
		t.Fatal("short dashboard password accepted")
	}
	if err := s.UpdateUserRole("a1", "bob", "admin", "dashboard-pass-789"); err != nil {
		t.Fatal(err)
	}
	wa := s.webAdmins[userKey("a1", "bob")]
	if wa == nil || !checkPassword("dashboard-pass-789", wa.PasswordHash) || checkPassword("bob-password-123", wa.PasswordHash) {
		t.Fatal("dashboard login should use the typed password only")
	}
}

// Deleting a guest event takes its phones out of the channels too.
func TestDeleteGuestEventRemovesPhonesFromChannels(t *testing.T) {
	s, sink := newSecTestServer(t)
	s.agencies["event-x"] = &Agency{ID: "event-x", Name: "Event", MaxUsers: 100}
	s.ensureAgencyChannels("event-x")
	s.users[userKey("event-x", "guest-x")] = &UserAccount{Username: "guest-x", AgencyID: "event-x", Role: "user"}
	s.enrollmentTokens["evtok"] = &EnrollmentToken{Token: "evtok", Username: "guest-x", AgencyID: "event-x", ExpiresAt: time.Now().Add(time.Hour), IsGuest: true}
	s.guestEvents["x"] = &GuestEvent{ID: "x", Name: "Event", AgencyID: "event-x", Username: "guest-x", Token: "evtok", OwnsAgency: true}
	phone := udp(6401)
	if ok, _, _ := loginWith(t, s, sink, phone, "guest-x", "event-x", "evtok", 0); !ok {
		t.Fatal("guest login failed")
	}
	join(s, phone, "visitor", 0)
	if err := s.DeleteGuestEvent("x"); err != nil {
		t.Fatal(err)
	}
	if s.isAuthenticated(phone) || len(s.clients) != 0 {
		t.Fatal("guest phone still connected after the event was deleted")
	}
	for k, ch := range s.channels {
		if len(ch.Clients) != 0 {
			t.Fatalf("guest phone still in channel %s", k)
		}
	}
	revoked := false
	for _, p := range sink.take(phone) {
		revoked = revoked || (p[0] == PacketTypeAuthResponse && p[1] == AuthStatusRevoked)
	}
	if !revoked {
		t.Fatal("guest phone was not told its session ended")
	}
}

// The auth request's optional flags byte follows the agency (empty or not).
func TestAuthRequestFlagsParse(t *testing.T) {
	mk := func(agency string, flags ...byte) []byte {
		var b []byte
		put := func(s string) {
			b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
			b = append(b, s...)
		}
		put("bob")
		put("abcd")
		put(agency)
		return append(b, flags...)
	}
	for _, c := range []struct {
		agency string
		flags  []byte
		want   byte
	}{{"a1", []byte{1}, 1}, {"", []byte{1}, 1}, {"a1", nil, 0}} {
		p, err := ParseAuthRequestPacket(mk(c.agency, c.flags...))
		if err != nil || p.Flags != c.want || p.AgencyID != c.agency {
			t.Fatalf("agency %q flags %v: got %+v %v", c.agency, c.flags, p, err)
		}
	}
}

// The device-key reply carries a tag only a holder of the QR token can make.
func TestDeviceKeyReplyIsTagged(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	token := s.GenerateEnrollmentToken("bob", "a1", "")
	_, msg, nonce := loginWith(t, s, sink, udp(6501), "bob", "a1", token, AuthFlagDeviceKey)
	key := unwrapDeviceKey(t, msg, token, nonce)
	secret, _ := hex.DecodeString(key)
	tag := ""
	for _, part := range strings.Split(msg, "|") {
		if strings.HasPrefix(part, "devtag=") {
			tag = strings.TrimPrefix(part, "devtag=")
		}
	}
	if tag == "" || tag != deviceKeyTag(token, nonce, secret) {
		t.Fatalf("missing or wrong device key tag in %q", msg)
	}
	if tag == deviceKeyTag("someone-elses-token", nonce, secret) {
		t.Fatal("tag does not depend on the token")
	}
}

func TestCallsignValidation(t *testing.T) {
	for _, ok := range []string{"Xcover", "Engine 12", "medic1 (Fire Dept)", "O'Brien", "Müller-7"} {
		if !validCallsign(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"", "x');alert(1);('<", "<img src=x>", "a\"b", "tab\there", "back\\slash", strings.Repeat("a", 65), "tick`"} {
		if validCallsign(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
	// And JOIN enforces it.
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "bob-password-123")
	loginWith(t, s, sink, udp(6601), "bob", "a1", "bob-password-123", 0)
	join(s, udp(6601), "<script>", 0)
	if len(s.clients) != 0 {
		t.Fatal("JOIN accepted a callsign with markup")
	}
}

// Leaving as "Unit1" in one agency must not release another agency's
// "Unit1" floor.
func TestDropUserStaysInItsAgency(t *testing.T) {
	s, _ := newFloorTestServer(t, "Unit1")
	s.floor.Request("ag:0", req(0, "Unit1"), PriorityResponder)
	s.floor.DropUser("other", "Unit1")
	if !s.floor.HasFloor("ag:0", "Unit1") {
		t.Fatal("a same-named user in another agency released this floor")
	}
	s.floor.DropUser("ag", "Unit1")
	if s.floor.HasFloor("ag:0", "Unit1") {
		t.Fatal("own-agency drop did not release the floor")
	}
}

// A guest event that patches in another agency never lands on a talkgroup
// number taken from the request.
func TestGuestEventChannelIsServerChosenWhenPatched(t *testing.T) {
	s, _ := newSecTestServer(t)
	s.config.UsersFile = t.TempDir() + "/users.json"
	ev, err := s.CreateGuestEvent("Parade", "a1", 3, nil, "chief", []string{"a2"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Channel < SharedChannelMin || ev.Channel > SharedChannelMax {
		t.Fatalf("patched event landed on channel %d (a partner's own talkgroup range)", ev.Channel)
	}
	if _, err := s.CreateGuestEvent("Bad", "a1", 150, nil, "chief", nil); err == nil {
		t.Fatal("event accepted a bridge-range channel")
	}
}

func TestLimiterEvictsOldestInOnePass(t *testing.T) {
	l := newFailLimiter(30, 10, time.Minute, time.Minute)
	base := time.Now()
	for i := 0; i < 1000; i++ {
		l.entries[fmt.Sprintf("ip:%d", i)] = &failEntry{first: base.Add(time.Duration(i) * time.Second)}
	}
	l.evictOldestLocked(100)
	if len(l.entries) != 900 {
		t.Fatalf("left %d entries, want 900", len(l.entries))
	}
	if _, ok := l.entries["ip:99"]; ok {
		t.Fatal("an old entry survived")
	}
	if _, ok := l.entries["ip:100"]; !ok {
		t.Fatal("a newer entry was evicted")
	}
}
