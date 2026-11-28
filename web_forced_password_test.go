// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// A session that must change its password (the first "admin" on a fresh
// install) gets the pages and the change-password call, and nothing the
// dashboard or the console would start: no /ws, no /ws/dispatch, no APIs.
func TestForcedPasswordChangeGate(t *testing.T) {
	s := newWebTestServer(t)
	addTestSession(s, "forced", "admin", "super_admin", "")
	s.webAdmins[userKey("", "admin")].MustChangePassword = true
	h := s.webHandler()

	for _, path := range []string{"/ws", "/ws/dispatch", "/api/users", "/api/agencies", "/api/version",
		"/api/server-identity", "/api/update/check", "/vendor/leaflet/leaflet.js"} {
		rec := doReq(h, "GET", path, "forced", "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "must_change_password") {
			t.Errorf("%s during a forced change: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/", "/app.js", "/command", "/dispatch.js"} {
		if rec := doReq(h, "GET", path, "forced", ""); rec.Code != http.StatusOK {
			t.Errorf("%s during a forced change: %d", path, rec.Code)
		}
	}
	rec := doReq(h, "GET", "/api/session", "forced", "")
	var sess struct {
		MustChange bool `json:"must_change_password"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &sess) != nil || !sess.MustChange {
		t.Fatalf("/api/session must report the forced change: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(h, "POST", "/api/admin/change-password", "forced", `{"new_password":"a long new passphrase 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(h, "GET", "/api/users", "forced", ""); rec.Code == http.StatusForbidden {
		t.Errorf("APIs still refused after the change: %s", rec.Body.String())
	}
}

// jsFunc returns the source of `function name(...) { ... }` in src (braces
// counted; good enough for the functions checked here).
func jsFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	open := strings.Index(src[start:], "{") + start
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("function %s not closed", name)
	return ""
}

// requireBefore fails unless a occurs in body, and before b when b occurs.
func requireBefore(t *testing.T, where, body, a, b string) {
	t.Helper()
	ia := strings.Index(body, a)
	if ia < 0 {
		t.Errorf("%s: %q missing", where, a)
		return
	}
	if ib := strings.Index(body, b); ib >= 0 && ib < ia {
		t.Errorf("%s: %q must come before %q", where, a, b)
	}
}

// Issue #2: the forced "Change Your Password" form reloaded after ~18 s
// because the dashboard started its WebSocket (refused until the change)
// and the reconnect loop reloads the page after 5 failures.
func TestDashboardForcedPasswordChangeStartsNothing(t *testing.T) {
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(raw)

	// checkSession: a must-change session shows the form and reports "not
	// ready", before the dispatch redirect (a dispatch account would bounce
	// between / and /command) and before any role set-up.
	cs := jsFunc(t, app, "checkSession")
	i := strings.Index(cs, "if (data.must_change_password)")
	if i < 0 {
		t.Fatal("checkSession: no must_change_password branch")
	}
	branch := cs[i:]
	branch = branch[:strings.Index(branch, "}")+1]
	if !strings.Contains(branch, "showForcePasswordChange()") || !strings.Contains(branch, "return false") {
		t.Errorf("checkSession must show the form and return false for a forced change:\n%s", branch)
	}
	requireBefore(t, "checkSession", cs, "if (data.must_change_password)", "data.role === 'dispatch'")
	requireBefore(t, "checkSession", cs, "if (data.must_change_password)", "applyRoleUI()")

	// The one-time start-up (WebSocket, fetches, handlers) runs only once.
	sd := jsFunc(t, app, "startDashboard")
	requireBefore(t, "startDashboard", sd, "if (!valid) return;", "connectWebSocket()")
	requireBefore(t, "startDashboard", sd, "if (dashboardStarted)", "dashboardStarted = true")
	requireBefore(t, "startDashboard", sd, "dashboardStarted = true", "fetchUsers()")
	if strings.Count(app, "fetchUsers();\n        fetchVersionInfo();") != 1 {
		t.Error("dashboard start-up should exist once, in startDashboard")
	}

	// After a successful change the start-up runs (not a bare checkSession).
	cpw := jsFunc(t, app, "changeAdminPassword")
	if !strings.Contains(cpw, "startDashboard()") || strings.Contains(cpw, "checkSession()") {
		t.Error("changeAdminPassword must continue with startDashboard()")
	}

	// Safety net: no reconnect and no reload while the form is up.
	ws := jsFunc(t, app, "connectWebSocket")
	oc := ws[strings.Index(ws, "ws.onclose"):]
	if strings.Count(oc, "forcedPasswordChangeShowing()") < 2 {
		t.Error("ws.onclose and its retry timer must both stop while the forced form is showing")
	}
	requireBefore(t, "ws.onclose", oc, "forcedPasswordChangeShowing()", "location.reload()")
	ir := oc[strings.Index(oc, "setInterval("):]
	requireBefore(t, "reconnect timer", ir, "forcedPasswordChangeShowing()", "reconnectAttempts++")

	// Re-showing an open form must not move the cursor or reset fields.
	sf := jsFunc(t, app, "showForcePasswordChange")
	requireBefore(t, "showForcePasswordChange", sf, "if (forcedPasswordChangeShowing()) return;", ".focus()")

	// The form's Enter key works without the rest of the start-up.
	if strings.Contains(sd, "adminConfirmPassword") {
		t.Error("the change-password Enter handler must not depend on startDashboard")
	}
}

// The /command console is open to a must-change session (page only): it must
// hand the session to the dashboard's password form before it connects.
func TestDispatchConsoleForcedPasswordChange(t *testing.T) {
	raw, err := os.ReadFile("web/dispatch.js")
	if err != nil {
		t.Fatal(err)
	}
	cs := jsFunc(t, string(raw), "checkSession")
	requireBefore(t, "dispatch checkSession", cs, "data.must_change_password", "connectDispatchWS()")
	requireBefore(t, "dispatch checkSession", cs, "data.must_change_password", "requestMic()")
	if !strings.Contains(cs, "location.replace('/')") {
		t.Error("dispatch checkSession must send a must-change session to the dashboard")
	}
}
