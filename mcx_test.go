// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import "testing"

func TestUserMCXID(t *testing.T) {
	cases := []struct {
		callsign, agency, domain, want string
	}{
		{"RAPTOR7", "sullivan", "meshrelayptt.com", "raptor7@sullivan.meshrelayptt.com"},
		{"Ryan F.", "default", "meshrelayptt.com", "ryan-f@default.meshrelayptt.com"},
		{"unit 12", "sc911", "r3systems.com", "unit-12@sc911.r3systems.com"},
		{"nodomain", "default", "", "nodomain@default"}, // unconfigured domain
	}
	for _, c := range cases {
		got := userMCXID(c.callsign, c.agency, c.domain)
		if got != c.want {
			t.Errorf("userMCXID(%q,%q,%q) = %q, want %q", c.callsign, c.agency, c.domain, got, c.want)
		}
	}
}

func TestGroupMCXID(t *testing.T) {
	got := groupMCXID("Tactical 1", "sullivan", "meshrelayptt.com")
	want := "tactical-1@sullivan.meshrelayptt.com"
	if got != want {
		t.Errorf("groupMCXID = %q, want %q", got, want)
	}
}

func TestValidMCXID(t *testing.T) {
	valid := []string{"raptor7@sullivan.meshrelayptt.com", "a@b.c", "unit-12@sc911.r3systems.com"}
	invalid := []string{"", "noatsign", "@domain.com", "user@", "user@@domain"}
	for _, v := range valid {
		if !validMCXID(v) {
			t.Errorf("validMCXID(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if validMCXID(v) {
			t.Errorf("validMCXID(%q) = true, want false", v)
		}
	}
}

func TestMCXDomainMatches(t *testing.T) {
	dom := "meshrelayptt.com"
	if !mcxDomainMatches("raptor7@sullivan.meshrelayptt.com", dom) {
		t.Error("agency subdomain should match configured domain")
	}
	if !mcxDomainMatches("x@meshrelayptt.com", dom) {
		t.Error("exact domain should match")
	}
	if mcxDomainMatches("intruder@evil.com", dom) {
		t.Error("foreign domain must NOT match")
	}
	if !mcxDomainMatches("anything@whatever", "") {
		t.Error("blank configured domain accepts anything (local mode)")
	}
}
