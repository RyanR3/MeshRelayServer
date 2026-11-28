// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

func newPwTestServer() *Server {
	return &Server{
		users:     map[string]*UserAccount{},
		webAdmins: map[string]*WebAdmin{},
		agencies:  map[string]*Agency{"alpha": {ID: "alpha", Name: "Alpha"}},
		config:    &Config{UsersFile: "/dev/null", WebAdminsFile: "/dev/null"},
	}
}

// An agency_admin exists in BOTH stores. A password change must update both, or
// the account works in one place and not the other — and the stale file wins
// after a restart, which reads as the password "reverting".
func TestUpdatePasswordSyncsBothStores(t *testing.T) {
	s := newPwTestServer()
	key := userKey("alpha", "Unit3")
	s.users[key] = &UserAccount{Username: "Unit3", AgencyID: "alpha", Role: "admin",
		PasswordHash: hashPassword("old")}
	s.webAdmins[key] = &WebAdmin{Username: "Unit3", AgencyID: "alpha",
		Role: "agency_admin", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("alpha", "Unit3", "Unit3"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	if got := s.users[key].PasswordHash; got != hashPassword("Unit3") {
		t.Error("PTT account hash was not updated")
	}
	if !checkPassword("Unit3", s.webAdmins[key].PasswordHash) {
		t.Error("dashboard account does not accept the new password")
	}
	if checkPassword("old", s.webAdmins[key].PasswordHash) {
		t.Error("dashboard account still accepts the OLD password")
	}
}

// A dashboard-only account (no users.json entry) must still be updatable —
// the old code returned "user not found" and changed nothing.
func TestUpdatePasswordWorksForDashboardOnlyAccount(t *testing.T) {
	s := newPwTestServer()
	key := userKey("alpha", "WebOnly")
	s.webAdmins[key] = &WebAdmin{Username: "WebOnly", AgencyID: "alpha",
		Role: "agency_admin", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("alpha", "WebOnly", "fresh"); err != nil {
		t.Fatalf("dashboard-only account should be updatable: %v", err)
	}
	if !checkPassword("fresh", s.webAdmins[key].PasswordHash) {
		t.Error("dashboard-only password was not updated")
	}
}

// A PTT-only user (no dashboard login) keeps working as before.
func TestUpdatePasswordWorksForPttOnlyUser(t *testing.T) {
	s := newPwTestServer()
	key := userKey("alpha", "Responder")
	s.users[key] = &UserAccount{Username: "Responder", AgencyID: "alpha",
		Role: "user", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("alpha", "Responder", "fresh"); err != nil {
		t.Fatalf("PTT-only user should be updatable: %v", err)
	}
	if s.users[key].PasswordHash != hashPassword("fresh") {
		t.Error("PTT-only password was not updated")
	}
}

func TestUpdatePasswordRejectsUnknownAccount(t *testing.T) {
	s := newPwTestServer()
	if err := s.UpdatePassword("alpha", "ghost", "x"); err == nil {
		t.Fatal("expected an error for an account in neither store")
	}
}

// Re-saving a role must not touch the dashboard password. The users.json hash
// can be a QR enrollment token (unknown to any person); copying it over the
// bcrypt dashboard hash silently locked agency admins out of the dashboard
// until a super admin reset the password.
func TestUpdateUserRoleKeepsDashboardPassword(t *testing.T) {
	s := newPwTestServer()
	key := userKey("alpha", "Unit3")
	s.users[key] = &UserAccount{Username: "Unit3", AgencyID: "alpha", Role: "admin",
		PasswordHash: hashPassword("enrollment-token-nobody-knows")}
	bcryptHash, _ := hashPasswordSecure("dashboard-pass")
	s.webAdmins[key] = &WebAdmin{Username: "Unit3", AgencyID: "alpha", Role: "agency_admin", PasswordHash: bcryptHash}

	if err := s.UpdateUserRole("alpha", "Unit3", "admin"); err != nil {
		t.Fatal(err)
	}
	if !checkPassword("dashboard-pass", s.webAdmins[key].PasswordHash) {
		t.Fatal("dashboard password was overwritten by the PTT hash on a role save")
	}
	// Demote and re-promote: the entry is recreated from the PTT hash (all we
	// have), which is the documented behaviour — but the demotion must remove it.
	if err := s.UpdateUserRole("alpha", "Unit3", "user"); err != nil {
		t.Fatal(err)
	}
	if _, still := s.webAdmins[key]; still {
		t.Fatal("demoted user still has a dashboard entry")
	}
}

// The Edit User dialog's password reset must reach web_admins.json. It used
// to update the dashboard hash only in memory (the store was saved only on a
// rename), so the reset worked until the next restart reloaded the old hash.
func TestEditUserPasswordResetPersistsDashboardStore(t *testing.T) {
	dir := t.TempDir()
	s := newPwTestServer()
	s.config.WebAdminsFile = dir + "/web_admins.json"
	s.config.UsersFile = dir + "/users.json"
	s.webSessions = map[string]*WebSession{}
	addTestSession(s, "tok", "admin", "super_admin", "")
	s.activityLog = NewActivityLog(dir+"/activity.json", 100)
	key := userKey("dev", "devadmin")
	s.users[key] = &UserAccount{Username: "devadmin", AgencyID: "dev", Role: "admin", PasswordHash: hashPassword("wrong")}
	s.agencies["dev"] = &Agency{ID: "dev", Name: "Dev"}
	oldHash, _ := hashPasswordSecure("wrong")
	s.webAdmins[key] = &WebAdmin{Username: "devadmin", AgencyID: "dev", Role: "agency_admin", PasswordHash: oldHash}
	if err := s.saveWebAdmins(); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"password":"right-one-password"}`)
	req := httptest.NewRequest("PUT", "/api/users/devadmin?agency=dev", body)
	req = mux.SetURLVars(req, map[string]string{"username": "devadmin"})
	req.AddCookie(&http.Cookie{Name: "session", Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, req)
	if rec.Code != 200 {
		t.Fatalf("edit failed: %d %s", rec.Code, rec.Body.String())
	}

	// Simulate the restart: reload the store from disk.
	fresh := newPwTestServer()
	fresh.config.WebAdminsFile = s.config.WebAdminsFile
	fresh.loadWebAdmins()
	a := fresh.webAdmins[key]
	if a == nil || !checkPassword("right-one-password", a.PasswordHash) {
		t.Fatal("password reset did not survive a restart — web_admins.json was not saved")
	}
}
