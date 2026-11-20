// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"path/filepath"
	"testing"
)

// A phone signed in with account "Bunker" but joined as callsign
// "dmartyparty". Deleting the account must take that session off the server,
// not just the account file.
func TestRemoveUserEndsSessionUnderAnotherCallsign(t *testing.T) {
	s, sent := newFloorTestServer(t, "dmartyparty", "alice")
	s.config.UsersFile = filepath.Join(t.TempDir(), "users.json")
	s.users = map[string]*UserAccount{
		userKey("ag", "Bunker"): {Username: "Bunker", AgencyID: "ag", Role: "user"},
		userKey("ag", "alice"):  {Username: "alice", AgencyID: "ag", Role: "user"},
	}
	s.webAdmins = map[string]*WebAdmin{}
	s.activityLog = NewActivityLog(filepath.Join(t.TempDir(), "activity.json"), 100)
	s.authenticatedAddrs = map[string]string{}
	s.authAccounts = map[string]string{}
	phone := s.clients[userKey("ag", "dmartyparty")]
	phone.Account = userKey("ag", "Bunker")
	s.authenticatedAddrs[phone.Addr.String()] = "ag"
	s.authAccounts[phone.Addr.String()] = userKey("ag", "Bunker")
	alice := s.clients[userKey("ag", "alice")]
	s.authenticatedAddrs[alice.Addr.String()] = "ag"
	s.authAccounts[alice.Addr.String()] = userKey("ag", "alice")

	if err := s.RemoveUser("ag", "Bunker"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.clients[userKey("ag", "dmartyparty")]; ok {
		t.Fatal("the deleted account's session is still connected")
	}
	if s.isAuthenticated(phone.Addr) {
		t.Fatal("the deleted account's address is still authenticated")
	}
	revoked := false
	for _, p := range sent["dmartyparty"] {
		revoked = revoked || p[0] == PacketTypeAuthResponse
	}
	if !revoked {
		t.Fatal("the phone was not told its sign-in was revoked")
	}
	if _, ok := s.clients[userKey("ag", "alice")]; !ok || !s.isAuthenticated(alice.Addr) {
		t.Fatal("another account's session was ended")
	}
}
