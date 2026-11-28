// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net/http"
	"testing"
	"time"
)

// An ordinary session (not in the forced first-login state) must give the
// current password to set a new one: a stolen cookie alone is not enough.
func TestChangePasswordNeedsCurrentPassword(t *testing.T) {
	s := newWebTestServer(t)
	s.loginLimit = newSignInGuard(10, 50, 15*time.Minute, 15*time.Minute)
	addTestSession(s, "fire2", "chief", "agency_admin", "fire") // a second session of the same account
	key := userKey("fire", "chief")
	s.users[key] = &UserAccount{Username: "chief", AgencyID: "fire", PasswordHash: hashPassword("initial-password-chief")}
	h := s.webHandler()
	const path = "/api/admin/change-password"
	oldHash := s.webAdmins[key].PasswordHash

	for name, body := range map[string]string{
		"no current password":    `{"new_password":"a long new passphrase 1"}`,
		"empty current password": `{"current_password":"","new_password":"a long new passphrase 1"}`,
		"wrong current password": `{"current_password":"not-the-password","new_password":"a long new passphrase 1"}`,
	} {
		if rec := doReq(h, "POST", path, "fireadmin", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	if s.webAdmins[key].PasswordHash != oldHash {
		t.Fatal("a refused change altered the stored password")
	}
	if s.webSessions["fire2"] == nil {
		t.Fatal("a refused change ended the account's other session")
	}

	rec := doReq(h, "POST", path, "fireadmin",
		`{"current_password":"initial-password-chief","new_password":"a long new passphrase 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("change with the right current password: %d %s", rec.Code, rec.Body.String())
	}
	if !checkPassword("a long new passphrase 1", s.webAdmins[key].PasswordHash) {
		t.Error("dashboard store does not hold the new password")
	}
	if !checkPassword("a long new passphrase 1", s.users[key].PasswordHash) {
		t.Error("PTT store (users.json) was not kept in step")
	}
	if s.webSessions["fire2"] != nil {
		t.Error("the account's other session survived the change")
	}
	if rec := doReq(h, "GET", "/api/session", "fireadmin", ""); rec.Code != http.StatusOK {
		t.Errorf("the session that made the change was ended: %d", rec.Code)
	}
}

// Wrong current passwords count against the login limits: after ten the
// address is refused, even with the right password, like the login page.
func TestChangePasswordWrongCurrentIsRateLimited(t *testing.T) {
	s := newWebTestServer(t)
	s.loginLimit = newSignInGuard(10, 50, 15*time.Minute, 15*time.Minute)
	h := s.webHandler()
	const path = "/api/admin/change-password"
	for i := 0; i < 10; i++ {
		rec := doReq(h, "POST", path, "fireadmin", `{"current_password":"guess","new_password":"a long new passphrase 1"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("guess %d: %d, want 403", i+1, rec.Code)
		}
	}
	rec := doReq(h, "POST", path, "fireadmin",
		`{"current_password":"initial-password-chief","new_password":"a long new passphrase 1"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 10 wrong guesses: %d %s, want 429", rec.Code, rec.Body.String())
	}
	if !checkPassword("initial-password-chief", s.webAdmins[userKey("fire", "chief")].PasswordHash) {
		t.Error("password changed while locked out")
	}
}

// A session in the forced must-change state needs only the new password, and
// the change still ends the account's other sessions.
func TestForcedChangeNeedsNoCurrentPassword(t *testing.T) {
	s := newWebTestServer(t)
	s.loginLimit = newSignInGuard(10, 50, 15*time.Minute, 15*time.Minute)
	addTestSession(s, "forced", "admin", "super_admin", "")
	addTestSession(s, "forced2", "admin", "super_admin", "")
	key := userKey("", "admin")
	s.webAdmins[key].MustChangePassword = true
	h := s.webHandler()

	rec := doReq(h, "POST", "/api/admin/change-password", "forced", `{"new_password":"a long new passphrase 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced change: %d %s", rec.Code, rec.Body.String())
	}
	if s.webAdmins[key].MustChangePassword {
		t.Error("must-change flag not cleared")
	}
	if s.webSessions["forced2"] != nil {
		t.Error("the account's other session survived the forced change")
	}
	// Once the flag is cleared, the next change needs the current password.
	rec = doReq(h, "POST", "/api/admin/change-password", "forced", `{"new_password":"another long passphrase 2"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("second change without the current password: %d, want 403", rec.Code)
	}
}
