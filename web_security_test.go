// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// addTestSession creates (if needed) a dashboard account and a live session
// for it under the given token.
func addTestSession(s *Server, token, username, role, agency string) {
	if s.webAdmins == nil {
		s.webAdmins = map[string]*WebAdmin{}
	}
	if s.webSessions == nil {
		s.webSessions = map[string]*WebSession{}
	}
	key := userKey(agency, username)
	admin := s.webAdmins[key]
	if admin == nil {
		// SHA-256 legacy hash: bcrypt is slow under -race and not under test here.
		admin = &WebAdmin{Username: username, Role: role, AgencyID: agency, PasswordHash: hashPassword("initial-password-" + username)}
		s.webAdmins[key] = admin
	}
	now := time.Now()
	sess := &WebSession{Username: admin.Username, Role: admin.Role, AgencyID: admin.AgencyID,
		adminKey: key, pwHash: admin.PasswordHash, createdAt: now}
	sess.lastSeen.Store(now.UnixNano())
	s.webSessions[token] = sess
}

func newWebTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s := &Server{
		config: &Config{
			UsersFile:             dir + "/users.json",
			WebAdminsFile:         dir + "/web_admins.json",
			AgenciesFile:          dir + "/agencies.json",
			MutualAidFile:         dir + "/mutual_aid.json",
			LMRBridgeFile:         dir + "/lmr.json",
			MutualAidRequestsFile: dir + "/mar.json",
			ZelloBridgesFile:      dir + "/zello.json",
			LogLevel:              "INFO",
		},
		clients:            map[string]*Client{},
		channels:           map[string]*Channel{},
		groups:             map[string]*GroupPacket{},
		users:              map[string]*UserAccount{},
		agencies:           map[string]*Agency{},
		authenticatedAddrs: map[string]string{},
		authAccounts:       map[string]string{},
		enrollmentTokens:   map[string]*EnrollmentToken{},
		guestEvents:        map[string]*GuestEvent{},
		webAdmins:          map[string]*WebAdmin{},
		dispatchConns:      map[string]*DispatchConn{},
		webSessions:        map[string]*WebSession{},
		mutualAidLinks:     map[string]*MutualAidLink{},
		lmrBridges:         map[string]*LMRBridge{},
		mutualAidRequests:  map[string]*MutualAidRequest{},
		zelloBridges:       map[string]*ZelloBridge{},
		shutdown:           make(chan struct{}),
		stats:              &Stats{StartTime: time.Now()},
		updateStatus:       &UpdateStatus{},
	}
	s.activityLog = NewActivityLog(dir+"/activity.json", 100)
	s.networkTimeSeries = NewNetworkTimeSeries()
	for _, id := range []string{"default", "fire", "ems"} {
		s.agencies[id] = &Agency{ID: id, Name: strings.ToUpper(id)}
	}
	addTestSession(s, "super", "root", "super_admin", "")
	addTestSession(s, "fireadmin", "chief", "agency_admin", "fire")
	addTestSession(s, "firedispatch", "desk1", "dispatch", "fire")
	return s
}

func doReq(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: token})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// W1: /api/config is super_admin only — it carries server-wide settings.
func TestConfigIsSuperAdminOnly(t *testing.T) {
	s := newWebTestServer(t)
	h := s.webHandler()

	for _, tok := range []string{"fireadmin", "firedispatch"} {
		if rec := doReq(h, "GET", "/api/config", tok, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s GET /api/config = %d, want 403", tok, rec.Code)
		}
		rec := doReq(h, "POST", "/api/config", tok, `{"log_level":"DEBUG"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s POST /api/config = %d, want 403", tok, rec.Code)
		}
	}
	if s.config.LogLevel != "INFO" {
		t.Fatal("config was replaced by a non-super caller")
	}
	if rec := doReq(h, "GET", "/api/config", "super", ""); rec.Code != http.StatusOK {
		t.Fatalf("super GET /api/config = %d", rec.Code)
	}
	if rec := doReq(h, "GET", "/api/config", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/config = %d, want 401", rec.Code)
	}
}

// W2: a dispatch account gets only what the /command console uses.
func TestDispatchDeniedUsersAgenciesConfig(t *testing.T) {
	s := newWebTestServer(t)
	s.users[userKey("ems", "medic1")] = &UserAccount{Username: "medic1", AgencyID: "ems", Role: "user", PasswordHash: hashPassword("x")}
	h := s.webHandler()

	denied := []struct{ method, path, body string }{
		{"GET", "/api/users", ""},
		{"POST", "/api/users", `{"username":"evil","password":"pw123456789abc","role":"admin","agency_id":"ems"}`},
		{"POST", "/api/users/bulk", "username,password,role\nevil,pw,admin\n"},
		{"PUT", "/api/users/medic1/password?agency=ems", `{"password":"owned"}`},
		{"PUT", "/api/users/medic1/role?agency=ems", `{"role":"admin"}`},
		{"DELETE", "/api/users/medic1?agency=ems", ""},
		{"POST", "/api/users/medic1/enroll?agency=ems", ""},
		{"GET", "/api/agencies", ""},
		{"POST", "/api/agencies", `{"id":"x","name":"x"}`},
		{"GET", "/api/config", ""},
		{"POST", "/api/update/check", ""},
		{"GET", "/api/my-agency/zello-bridges", ""},
		{"GET", "/api/my-agency/lmr-bridges", ""},
		{"GET", "/api/zello-bridges", ""},
		{"GET", "/api/clients", ""},
		{"GET", "/api/activity", ""},
		{"GET", "/api/stats", ""},
		{"GET", "/api/guest-events", ""},
		{"GET", "/api/mutual-aid/requests", ""},
	}
	for _, d := range denied {
		if rec := doReq(h, d.method, d.path, "firedispatch", d.body); rec.Code != http.StatusForbidden {
			t.Errorf("dispatch %s %s = %d, want 403 (%s)", d.method, d.path, rec.Code, rec.Body.String())
		}
	}
	if _, ok := s.users[userKey("ems", "evil")]; ok {
		t.Fatal("dispatch created a user")
	}
	if !checkPassword("x", s.users[userKey("ems", "medic1")].PasswordHash) {
		t.Fatal("dispatch reset a password")
	}

	// What the console needs still works.
	for _, p := range []string{"/api/recordings", "/api/dispatch/clients"} {
		if rec := doReq(h, "GET", p, "firedispatch", ""); rec.Code != http.StatusOK {
			t.Errorf("dispatch GET %s = %d, want 200", p, rec.Code)
		}
	}
	if rec := doReq(h, "GET", "/api/session", "firedispatch", ""); rec.Code != http.StatusOK {
		t.Errorf("dispatch GET /api/session = %d", rec.Code)
	}
	// And the dashboard feed is not for dispatch.
	if rec := doReq(h, "GET", "/ws", "firedispatch", ""); rec.Code != http.StatusForbidden {
		t.Errorf("dispatch /ws = %d, want 403", rec.Code)
	}
}

// W2: the matrix is deny-by-default. Walk every registered /api route and
// pin exactly which ones dispatch can reach, so a new route cannot quietly
// open up to it.
func TestRolePolicyDispatchAllowListIsExact(t *testing.T) {
	s := newWebTestServer(t)
	var got []string
	walkAPIRoutes(t, s, func(method, tmpl string) {
		if routeAllowed("dispatch", method, tmpl) {
			got = append(got, method+" "+tmpl)
		}
		if !routeAllowed("super_admin", method, tmpl) {
			t.Errorf("super_admin denied %s %s", method, tmpl)
		}
	})
	sort.Strings(got)
	want := []string{
		"GET /api/dispatch/clients",
		"GET /api/dispatch/maps", // read-only: the console's map layers
		"GET /api/recordings",
		"GET /api/recordings/{id}/audio",
		"POST /api/admin/change-password",
		"POST /api/dispatch/channel",
		"POST /api/logout",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dispatch can reach:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Unknown roles and unlisted routes are refused.
	if routeAllowed("user", "GET", "/api/recordings") || routeAllowed("agency_admin", "GET", "/api/config") ||
		routeAllowed("agency_admin", "GET", "/api/brand-new-route") {
		t.Fatal("deny-by-default broken")
	}
}

// walkAPIRoutes visits every method+template registered under /api.
func walkAPIRoutes(t *testing.T, s *Server, fn func(method, tmpl string)) {
	t.Helper()
	rt := s.webRouter()
	n := 0
	err := rt.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil || !strings.HasPrefix(tmpl, "/api/") {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil {
			return nil
		}
		for _, m := range methods {
			fn(m, tmpl)
			n++
		}
		return nil
	})
	if err != nil || n < 50 {
		t.Fatalf("walk: %v (%d routes)", err, n)
	}
}

// W3: no username-only fallback — an agency admin cannot mint an enrollment
// token for another agency's user of the same name.
func TestEnrollTokenScopedToCallerAgency(t *testing.T) {
	s := newWebTestServer(t)
	s.users[userKey("ems", "medic1")] = &UserAccount{Username: "medic1", AgencyID: "ems", Role: "user", PasswordHash: hashPassword("x")}
	h := s.webHandler()

	rec := doReq(h, "POST", "/api/users/medic1/enroll?agency=ems", "fireadmin", "")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "enrollment_token") {
		t.Fatalf("fire admin got a token for an EMS user: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.enrollmentTokens) != 0 {
		t.Fatal("token was minted")
	}
	// Super admin, naming the agency, still can.
	rec = doReq(h, "POST", "/api/users/medic1/enroll?agency=ems", "super", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("super enroll = %d %s", rec.Code, rec.Body.String())
	}
	// And a super admin who names the WRONG agency gets nothing either.
	rec = doReq(h, "POST", "/api/users/medic1/enroll?agency=fire", "super", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("username-only fallback still active: %d", rec.Code)
	}
}

// W4: the dashboard feed is filtered on the server.
func TestDashboardUpdateScopedToAgency(t *testing.T) {
	s := newWebTestServer(t)
	addr := func(p int) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: p} }
	s.clients[userKey("fire", "e1")] = &Client{Username: "e1", AgencyID: "fire", Addr: addr(1)}
	s.clients[userKey("ems", "m1")] = &Client{Username: "m1", AgencyID: "ems", Addr: addr(2)}
	s.activityLog.Add("x", "e1", "fire thing", "fire")
	s.activityLog.Add("x", "m1", "ems thing", "ems")

	up := s.dashboardUpdate("fire", "fire")
	b, _ := json.Marshal(up)
	if strings.Contains(string(b), "m1") || strings.Contains(string(b), "ems thing") {
		t.Fatalf("fire's feed leaks EMS data: %s", b)
	}
	if !strings.Contains(string(b), "e1") {
		t.Fatal("fire's own client missing")
	}
	all, _ := json.Marshal(s.dashboardUpdate("", ""))
	if !strings.Contains(string(all), "m1") || !strings.Contains(string(all), "e1") {
		t.Fatal("super admin feed should include every agency")
	}
}

// W6: a guest event may only bridge in agencies that agreed.
func TestGuestEventNeedsAgreementToIncludeAgency(t *testing.T) {
	s := newWebTestServer(t)
	s.saveAgencies() // CreateGuestEvent saves
	h := s.webHandler()

	rec := doReq(h, "POST", "/api/guest-events", "fireadmin", `{"name":"Parade","channel":3,"agencies":["ems"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("fire included EMS without consent: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.mutualAidLinks) != 0 || len(s.guestEvents) != 0 {
		t.Fatal("event or link created despite refusal")
	}

	// EMS trusts fire (standing agreement) -> allowed.
	s.agencies["ems"].AutoAcceptFrom = []string{"fire"}
	rec = doReq(h, "POST", "/api/guest-events", "fireadmin", `{"name":"Parade","channel":3,"agencies":["ems","fire"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("with agreement: %d %s", rec.Code, rec.Body.String())
	}

	// The guest-event link itself does not count as an agreement for later.
	s.agencies["ems"].AutoAcceptFrom = nil
	s.mu.RLock()
	ok := s.guestIncludeAllowedLocked("fire", "ems")
	s.mu.RUnlock()
	if ok {
		t.Fatal("a guest-event bridge was treated as a mutual-aid agreement")
	}

	// An approved (non-event) mutual-aid patch does count.
	s.mutualAidLinks["ma1"] = &MutualAidLink{ID: "ma1", Agencies: []string{"ems", "fire"}, Active: true}
	s.mu.RLock()
	ok = s.guestIncludeAllowedLocked("fire", "ems")
	s.mu.RUnlock()
	if !ok {
		t.Fatal("approved patch should allow inclusion")
	}

	// Super admin is not limited.
	delete(s.mutualAidLinks, "ma1")
	rec = doReq(h, "POST", "/api/guest-events", "super", `{"name":"Fair","channel":4,"agencies":["ems"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("super: %d %s", rec.Code, rec.Body.String())
	}
}

// W7: refusing a bridge share must not leave s.mu locked (it used to freeze
// the whole server).
func TestBridgeShareRefusalReleasesLock(t *testing.T) {
	s := newWebTestServer(t)
	s.agencies["ems"].Discoverable = true // target visible; fire is NOT discoverable
	s.zelloBridges["z1"] = &ZelloBridge{ID: "z1", OwnerAgencyID: "fire", AgencyID: "fire",
		Mappings: []ZelloMapping{{ZelloChannel: "Ops", MeshChannel: 150, AgencyID: "fire"}}}
	s.lmrBridges["l1"] = &LMRBridge{ID: "l1", OwnerAgencyID: "fire", Agencies: []string{"fire"}, Channel: 100}
	h := s.webHandler()

	for _, path := range []string{"/api/my-agency/zello-bridges/z1/share", "/api/my-agency/lmr-bridges/l1/share"} {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- doReq(h, "POST", path, "fireadmin", `{"agency_id":"ems"}`) }()
		select {
		case rec := <-done:
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s = %d %s, want 403", path, rec.Code, rec.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s hung", path)
		}
		if !s.mu.TryLock() {
			t.Fatalf("%s returned with s.mu still held", path)
		}
		s.mu.Unlock()
	}
}

// W8: password reset by an admin ends all of the account's sessions; a
// self-service change keeps the current one and ends the rest.
func TestPasswordChangeRevokesSessions(t *testing.T) {
	s := newWebTestServer(t)
	key := userKey("fire", "lt")
	s.users[key] = &UserAccount{Username: "lt", AgencyID: "fire", Role: "admin", PasswordHash: hashPassword("old")}
	addTestSession(s, "lt-laptop", "lt", "agency_admin", "fire")
	addTestSession(s, "lt-phone", "lt", "agency_admin", "fire")
	h := s.webHandler()

	if s.sessionFor("lt-laptop") == nil {
		t.Fatal("setup: session should be valid")
	}
	rec := doReq(h, "PUT", "/api/users/lt/password", "fireadmin", `{"password":"reset-by-chief"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	for _, tok := range []string{"lt-laptop", "lt-phone"} {
		if s.sessionFor(tok) != nil {
			t.Errorf("%s survived a password reset", tok)
		}
		if rec := doReq(h, "GET", "/api/stats", tok, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s still authorised: %d", tok, rec.Code)
		}
	}

	// Self-service change: this session continues, the other one ends.
	addTestSession(s, "chief-2", "chief", "agency_admin", "fire")
	rec = doReq(h, "POST", "/api/admin/change-password", "fireadmin", `{"current_password":"initial-password-chief","new_password":"a-much-longer-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if s.sessionFor("fireadmin") == nil {
		t.Fatal("the session that changed the password was logged out")
	}
	if s.sessionFor("chief-2") != nil {
		t.Fatal("other session survived the password change")
	}
}

// W8: role change, deletion, idle timeout and absolute lifetime.
func TestSessionRevocationAndExpiry(t *testing.T) {
	s := newWebTestServer(t)
	key := userKey("fire", "sgt")
	s.users[key] = &UserAccount{Username: "sgt", AgencyID: "fire", Role: "admin", PasswordHash: hashPassword("pw")}
	addTestSession(s, "sgt", "sgt", "agency_admin", "fire")
	h := s.webHandler()

	// Demotion to user removes dashboard access immediately.
	if rec := doReq(h, "PUT", "/api/users/sgt/role", "fireadmin", `{"role":"user"}`); rec.Code != http.StatusOK {
		t.Fatalf("role: %d %s", rec.Code, rec.Body.String())
	}
	if s.sessionFor("sgt") != nil {
		t.Fatal("demoted admin kept a session")
	}

	// Role is looked up per request: a super admin demoted in the store
	// (by any path) is no longer super on the next request.
	addTestSession(s, "root2", "root2", "super_admin", "")
	s.mu.Lock()
	s.webAdmins[userKey("", "root2")].Role = "agency_admin"
	s.mu.Unlock()
	if rec := doReq(h, "GET", "/api/config", "root2", ""); rec.Code == http.StatusOK {
		t.Fatal("cached super_admin role still honoured")
	}

	// Deletion.
	addTestSession(s, "desk2", "desk2", "dispatch", "fire")
	if rec := doReq(h, "DELETE", "/api/users/desk2", "fireadmin", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if s.sessionFor("desk2") != nil {
		t.Fatal("deleted account kept a session")
	}

	// Idle timeout.
	addTestSession(s, "idle", "idle", "agency_admin", "fire")
	s.webSessions["idle"].lastSeen.Store(time.Now().Add(-webSessionIdleTimeout - time.Minute).UnixNano())
	if s.sessionFor("idle") != nil {
		t.Fatal("idle session still valid")
	}
	// Activity within the window keeps it alive.
	addTestSession(s, "busy", "busy", "agency_admin", "fire")
	s.webSessions["busy"].lastSeen.Store(time.Now().Add(-webSessionIdleTimeout + time.Hour).UnixNano())
	if s.sessionFor("busy") == nil {
		t.Fatal("active session expired early")
	}
	// Absolute lifetime, however active.
	s.webSessions["busy"].createdAt = time.Now().Add(-webSessionMaxLifetime - time.Minute)
	if s.sessionFor("busy") != nil {
		t.Fatal("session outlived the absolute lifetime")
	}
}

// W9: logging in as "ADMIN" must still hit the forced password change of
// the "admin" account.
func TestMustChangePasswordNotBypassedByCase(t *testing.T) {
	s := newWebTestServer(t)
	h, _ := hashPasswordSecure("initial-pw-1234")
	s.webAdmins[userKey("", "admin")] = &WebAdmin{Username: "admin", Role: "super_admin", PasswordHash: h, MustChangePassword: true}
	handler := s.webHandler()

	rec := doReq(handler, "POST", "/api/login", "", `{"username":"ADMIN","password":"initial-pw-1234"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("no session cookie")
	}
	if !strings.Contains(rec.Body.String(), `"username":"admin"`) {
		t.Errorf("login should report the canonical username: %s", rec.Body.String())
	}
	rec = doReq(handler, "GET", "/api/users", tok, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "must_change_password") {
		t.Fatalf("forced change bypassed: %d %s", rec.Code, rec.Body.String())
	}
}

// W5: account-name checks refuse markup but keep ordinary callsigns.
func TestAccountNameValidation(t *testing.T) {
	for _, ok := range []string{"Engine 7-1", "O'Brien", "K9 #2", "unit3", "Ünit_5", "medic.1", "A&B"} {
		if err := validateAccountName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " lead", "trail ", `<img src=x onerror=alert(1)>`, `a"b`, "a`b", `a\b`, "a/b", "a:b", "a\nb", "a‮b",
		strings.Repeat("x", 65)} {
		if err := validateAccountName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if validateAgencyID("fire-dept_2") != nil || validateAgencyID("fire dept") == nil || validateAgencyID("<x>") == nil {
		t.Error("agency ID rule")
	}
}

// W5: the server refuses a script-bearing username and event name, and
// every response carries the security headers.
func TestCreateRejectsMarkupAndSetsHeaders(t *testing.T) {
	s := newWebTestServer(t)
	h := s.webHandler()
	rec := doReq(h, "POST", "/api/users", "fireadmin", `{"username":"x\"><img src=x onerror=alert(1)>","password":"pw123456789abc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("markup username: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(h, "POST", "/api/guest-events", "fireadmin", `{"name":"<script>alert(1)</script>","channel":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("markup event name: %d %s", rec.Code, rec.Body.String())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"object-src 'none'", "base-uri 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Frame-Options / X-Content-Type-Options")
	}
	// Case-variant duplicates are refused.
	s.users[userKey("fire", "Engine1")] = &UserAccount{Username: "Engine1", AgencyID: "fire", Role: "user"}
	rec = doReq(h, "POST", "/api/users", "fireadmin", `{"username":"ENGINE1","password":"pw123456789abc"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("case duplicate: %d %s", rec.Code, rec.Body.String())
	}
}
