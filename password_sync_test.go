package main

import "testing"

func newPwTestServer() *Server {
	return &Server{
		users:     map[string]*UserAccount{},
		webAdmins: map[string]*WebAdmin{},
		agencies:  map[string]*Agency{"dev1": {ID: "dev1", Name: "Dev1"}},
		config:    &Config{UsersFile: "/dev/null", WebAdminsFile: "/dev/null"},
	}
}

// An agency_admin exists in BOTH stores. A password change must update both, or
// the account works in one place and not the other — and the stale file wins
// after a restart, which reads as the password "reverting".
func TestUpdatePasswordSyncsBothStores(t *testing.T) {
	s := newPwTestServer()
	key := userKey("dev1", "Dev3")
	s.users[key] = &UserAccount{Username: "Dev3", AgencyID: "dev1", Role: "admin",
		PasswordHash: hashPassword("old")}
	s.webAdmins[key] = &WebAdmin{Username: "Dev3", AgencyID: "dev1",
		Role: "agency_admin", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("dev1", "Dev3", "Dev3"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	if got := s.users[key].PasswordHash; got != hashPassword("Dev3") {
		t.Error("PTT account hash was not updated")
	}
	if !checkPassword("Dev3", s.webAdmins[key].PasswordHash) {
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
	key := userKey("dev1", "WebOnly")
	s.webAdmins[key] = &WebAdmin{Username: "WebOnly", AgencyID: "dev1",
		Role: "agency_admin", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("dev1", "WebOnly", "fresh"); err != nil {
		t.Fatalf("dashboard-only account should be updatable: %v", err)
	}
	if !checkPassword("fresh", s.webAdmins[key].PasswordHash) {
		t.Error("dashboard-only password was not updated")
	}
}

// A PTT-only user (no dashboard login) keeps working as before.
func TestUpdatePasswordWorksForPttOnlyUser(t *testing.T) {
	s := newPwTestServer()
	key := userKey("dev1", "Responder")
	s.users[key] = &UserAccount{Username: "Responder", AgencyID: "dev1",
		Role: "user", PasswordHash: hashPassword("old")}

	if err := s.UpdatePassword("dev1", "Responder", "fresh"); err != nil {
		t.Fatalf("PTT-only user should be updatable: %v", err)
	}
	if s.users[key].PasswordHash != hashPassword("fresh") {
		t.Error("PTT-only password was not updated")
	}
}

func TestUpdatePasswordRejectsUnknownAccount(t *testing.T) {
	s := newPwTestServer()
	if err := s.UpdatePassword("dev1", "ghost", "x"); err == nil {
		t.Fatal("expected an error for an account in neither store")
	}
}

// Re-saving a role must not touch the dashboard password. The users.json hash
// can be a QR enrollment token (unknown to any person); copying it over the
// bcrypt dashboard hash silently locked agency admins out of the dashboard
// until a super admin reset the password.
func TestUpdateUserRoleKeepsDashboardPassword(t *testing.T) {
	s := newPwTestServer()
	key := userKey("dev1", "Dev3")
	s.users[key] = &UserAccount{Username: "Dev3", AgencyID: "dev1", Role: "admin",
		PasswordHash: hashPassword("enrollment-token-nobody-knows")}
	bcryptHash, _ := hashPasswordSecure("dashboard-pass")
	s.webAdmins[key] = &WebAdmin{Username: "Dev3", AgencyID: "dev1", Role: "agency_admin", PasswordHash: bcryptHash}

	if err := s.UpdateUserRole("dev1", "Dev3", "admin"); err != nil {
		t.Fatal(err)
	}
	if !checkPassword("dashboard-pass", s.webAdmins[key].PasswordHash) {
		t.Fatal("dashboard password was overwritten by the PTT hash on a role save")
	}
	// Demote and re-promote: the entry is recreated from the PTT hash (all we
	// have), which is the documented behaviour — but the demotion must remove it.
	if err := s.UpdateUserRole("dev1", "Dev3", "user"); err != nil {
		t.Fatal(err)
	}
	if _, still := s.webAdmins[key]; still {
		t.Fatal("demoted user still has a dashboard entry")
	}
}
