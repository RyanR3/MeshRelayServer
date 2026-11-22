// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Dashboard / console access control: session lifetime and revocation, the
// per-route role matrix, response security headers and the server-side
// checks on names that end up in the dashboard's HTML.

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/mux"
)

// Session lifetime. An open dashboard or /command WebSocket refreshes
// lastSeen, so a dispatcher on shift is never idled out; the absolute limit
// still forces a fresh login once a week.
const (
	webSessionIdleTimeout = 12 * time.Hour
	webSessionMaxLifetime = 7 * 24 * time.Hour
)

// createWebSession records a new login for the dashboard account stored at
// adminKey and returns its token. Expired sessions are pruned on the way.
func (s *Server) createWebSession(adminKey string, admin *WebAdmin) string {
	token := s.generateSessionToken()
	now := time.Now()
	sess := &WebSession{
		Username:  admin.Username,
		Role:      admin.Role,
		AgencyID:  admin.AgencyID,
		adminKey:  adminKey,
		pwHash:    admin.PasswordHash,
		createdAt: now,
	}
	sess.lastSeen.Store(now.UnixNano())

	s.mu.Lock()
	for t, old := range s.webSessions {
		if old.expired(now) {
			delete(s.webSessions, t)
		}
	}
	s.webSessions[token] = sess
	s.mu.Unlock()
	return token
}

func (ws *WebSession) expired(now time.Time) bool {
	if ws.createdAt.IsZero() || now.Sub(ws.createdAt) > webSessionMaxLifetime {
		return true
	}
	return now.Sub(time.Unix(0, ws.lastSeen.Load())) > webSessionIdleTimeout
}

// sessionFor resolves a session token to a snapshot of the session with the
// account's CURRENT role and agency, and records the activity. It returns nil
// (and forgets the token) when the session has expired, the account was
// deleted or renamed, its password changed, or its role or agency changed.
func (s *Server) sessionFor(token string) *WebSession {
	if token == "" {
		return nil
	}
	now := time.Now()
	s.mu.RLock()
	sess := s.webSessions[token]
	var out *WebSession
	if sess != nil && !sess.expired(now) {
		admin := s.webAdmins[sess.adminKey]
		if sess.adminKey != "" && admin != nil &&
			admin.PasswordHash == sess.pwHash &&
			admin.Role == sess.Role && admin.AgencyID == sess.AgencyID {
			sess.lastSeen.Store(now.UnixNano())
			out = &WebSession{
				Username:  admin.Username,
				Role:      admin.Role,
				AgencyID:  admin.AgencyID,
				adminKey:  sess.adminKey,
				createdAt: sess.createdAt,
			}
		}
	}
	s.mu.RUnlock()

	if sess != nil && out == nil {
		s.mu.Lock()
		if s.webSessions[token] == sess {
			delete(s.webSessions, token)
		}
		s.mu.Unlock()
	}
	return out
}

// revokeWebSessionsLocked ends every session of the dashboard account at
// adminKey except the one whose token is keep (pass "" to end them all).
// Caller holds s.mu for writing.
func (s *Server) revokeWebSessionsLocked(adminKey, keep string) int {
	n := 0
	for t, sess := range s.webSessions {
		if sess.adminKey == adminKey && t != keep {
			delete(s.webSessions, t)
			n++
		}
	}
	return n
}

// revokeWebSessions is revokeWebSessionsLocked for callers not holding s.mu.
func (s *Server) revokeWebSessions(adminKey, keep string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeWebSessionsLocked(adminKey, keep)
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie("session"); err == nil {
		return c.Value
	}
	return ""
}

func isSuperAdmin(sess *WebSession) bool {
	return sess != nil && sess.Role == "super_admin"
}

// ---- Role matrix ----------------------------------------------------------

const (
	allowSuper = 1 << iota
	allowAgencyAdmin
	allowDispatch

	allowAdmins = allowSuper | allowAgencyAdmin
	allowAll    = allowAdmins | allowDispatch
)

// apiRolePolicy lists which roles may call each /api route ("METHOD
// template"). Anything not listed is super_admin only — a new route is closed
// until someone decides who needs it. Handlers still scope agency_admin and
// dispatch callers to their own agency; this table decides who gets in at all.
//
// dispatch accounts get exactly what the /command console uses (web/
// dispatch.js): session, logout, own password, recordings, and the dispatch
// client list / channel API. Users, agencies, config, bridges, mutual aid and
// the updater are closed to them.
var apiRolePolicy = map[string]int{
	// Every signed-in account.
	"POST /api/logout":                allowAll,
	"POST /api/admin/change-password": allowAll,
	"GET /api/recordings":             allowAll,
	"GET /api/recordings/{id}/audio":  allowAll,
	"GET /api/dispatch/clients":       allowAll,
	"GET /api/dispatch/maps":          allowAll,
	"POST /api/dispatch/channel":      allowAll,

	// Dashboard: super_admin and agency_admin (handlers scope by agency).
	"GET /api/stats":                             allowAdmins,
	"GET /api/clients":                           allowAdmins,
	"GET /api/channels":                          allowAdmins,
	"GET /api/channel-group-types":               allowAdmins,
	"POST /api/channel-group-types":              allowAdmins,
	"GET /api/activity":                          allowAdmins,
	"GET /api/version":                           allowAdmins,
	"GET /api/users":                             allowAdmins,
	"POST /api/users":                            allowAdmins,
	"GET /api/users/template":                    allowAdmins,
	"POST /api/users/bulk":                       allowAdmins,
	"PUT /api/users/{username}":                  allowAdmins,
	"DELETE /api/users/{username}":               allowAdmins,
	"PUT /api/users/{username}/password":         allowAdmins,
	"PUT /api/users/{username}/role":             allowAdmins,
	"POST /api/users/{username}/sign-out-phones": allowAdmins,
	"GET /api/server-identity":                   allowAdmins,
	"POST /api/users/{username}/enroll":          allowAdmins,

	"GET /api/my-agency/discovery":                   allowAdmins,
	"GET /api/my-agency/emergency":                   allowAdmins,
	"PUT /api/my-agency/emergency":                   allowAdmins,
	"POST /api/my-agency/emergency/clear":            allowAdmins,
	"PUT /api/my-agency/discovery":                   allowAdmins,
	"GET /api/my-agency/maps":                        allowAdmins,
	"POST /api/my-agency/maps":                       allowAdmins,
	"DELETE /api/my-agency/maps/{id}":                allowAdmins,
	"GET /api/my-agency/channels":                    allowAdmins,
	"PUT /api/my-agency/channels":                    allowAdmins,
	"GET /api/my-agency/floor-control":               allowAdmins,
	"PUT /api/my-agency/floor-control":               allowAdmins,
	"GET /api/my-agency/tak/groups":                  allowAdmins,
	"PUT /api/my-agency/tak/groups":                  allowAdmins,
	"GET /api/my-agency/tak":                         allowAdmins,
	"PUT /api/my-agency/tak":                         allowAdmins,
	"GET /api/my-agency/recording":                   allowAdmins,
	"PUT /api/my-agency/recording":                   allowAdmins,
	"GET /api/my-agency/guest-invites":               allowAdmins,
	"PUT /api/my-agency/guest-invites":               allowAdmins,
	"GET /api/agencies/discoverable":                 allowAdmins,
	"GET /api/my-agency/mutual-aid":                  allowAdmins,
	"GET /api/mutual-aid/requests":                   allowAdmins,
	"POST /api/mutual-aid/requests":                  allowAdmins,
	"POST /api/mutual-aid/requests/{id}/approve":     allowAdmins,
	"POST /api/mutual-aid/requests/{id}/decline":     allowAdmins,
	"POST /api/mutual-aid/{id}/leave":                allowAdmins,
	"GET /api/my-agency/zello-bridges":               allowAdmins,
	"POST /api/my-agency/zello-bridges":              allowAdmins,
	"DELETE /api/my-agency/zello-bridges/{id}":       allowAdmins,
	"POST /api/my-agency/zello-bridges/{id}/share":   allowAdmins,
	"POST /api/my-agency/zello-bridges/{id}/unshare": allowAdmins,
	"GET /api/my-agency/lmr-bridges":                 allowAdmins,
	"POST /api/my-agency/lmr-bridges":                allowAdmins,
	"DELETE /api/my-agency/lmr-bridges/{id}":         allowAdmins,
	"POST /api/my-agency/lmr-bridges/{id}/toggle":    allowAdmins,
	"POST /api/my-agency/lmr-bridges/{id}/guests":    allowAdmins,
	"PUT /api/my-agency/lmr-bridges/{id}/atak":       allowAdmins,
	"POST /api/my-agency/lmr-bridges/{id}/share":     allowAdmins,
	"POST /api/my-agency/lmr-bridges/{id}/unshare":   allowAdmins,
	"GET /api/guest-events":                          allowAdmins,
	"GET /api/guest-events/agencies":                 allowAdmins,
	"POST /api/guest-events":                         allowAdmins,
	"DELETE /api/guest-events/{id}":                  allowAdmins,

	// Everything else — /config, /update/*, /github-token,
	// /agencies (CRUD), /mutual-aid (operator links), /lmr-bridges,
	// /zello-bridges, /zello-channels, /groups, /debug/state — is
	// super_admin only by omission.
}

func roleBit(role string) int {
	switch role {
	case "super_admin":
		return allowSuper
	case "agency_admin":
		return allowAgencyAdmin
	case "dispatch":
		return allowDispatch
	}
	return 0
}

// routeAllowed reports whether role may call the route "METHOD template".
func routeAllowed(role, method, tmpl string) bool {
	bit := roleBit(role)
	if bit == 0 {
		return false
	}
	if bit == allowSuper {
		return true
	}
	return apiRolePolicy[method+" "+tmpl]&bit != 0
}

// rolePolicyMiddleware enforces apiRolePolicy on the /api subrouter.
func (s *Server) rolePolicyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.getSession(r)
		if sess == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		tmpl := ""
		if route := mux.CurrentRoute(r); route != nil {
			tmpl, _ = route.GetPathTemplate()
		}
		if tmpl == "" || !routeAllowed(sess.Role, r.Method, tmpl) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"not permitted for your role"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Response headers -----------------------------------------------------

// contentSecurityPolicy: scripts only from this origin, the two CDNs
// index.html loads (QR code, Chart.js) and blob: (the /command console builds
// its AudioWorklet from a Blob); images from https: for the console's map
// tiles. 'unsafe-inline' stays in script-src for now because
// index.html and the rendered tables still use inline on*= handlers (every
// value placed in one is JSON-encoded and attribute-escaped — see jsArg in
// app.js). object-src/base-uri/frame-ancestors are locked down regardless.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' blob: https://cdnjs.cloudflare.com https://cdn.jsdelivr.net; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' data: https://fonts.gstatic.com; " +
	"img-src 'self' data: blob: https:; " +
	"media-src 'self' blob: data:; " +
	"connect-src 'self' ws: wss:; " +
	"worker-src 'self' blob:; " +
	"object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ---- Name validation ------------------------------------------------------

// The Android app accepts any trimmed text as a callsign, so account names
// are checked with a deny-list rather than an allow-list: letters in any
// script, digits, spaces and ordinary punctuation (O'Brien, Engine 7-1,
// K9 #2) stay valid. Refused: control/format characters, the HTML/JS
// metacharacters < > " ` \, '/' (breaks the /api/users/{name} path) and ':'
// (the agency:username key separator). Only new names are checked — existing
// accounts keep working whatever they contain.
const maxAccountNameLen = 64
const maxDisplayNameLen = 100

func checkNameText(kind, v string, max int, extraBanned string) error {
	if v == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if v != strings.TrimSpace(v) {
		return fmt.Errorf("%s must not start or end with a space", kind)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s is not valid text", kind)
	}
	if utf8.RuneCountInString(v) > max {
		return fmt.Errorf("%s is too long (max %d characters)", kind, max)
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return fmt.Errorf("%s contains a control character", kind)
		}
		if strings.ContainsRune(`<>"`+"`"+`\`+extraBanned, r) {
			return fmt.Errorf("%s must not contain %q", kind, r)
		}
	}
	return nil
}

// validateAccountName checks a new username / callsign.
func validateAccountName(v string) error {
	return checkNameText("username", v, maxAccountNameLen, "/:")
}

// validateDisplayName checks a free-text label (person, agency, event name).
// Empty is allowed where the field is optional; callers check required-ness.
func validateDisplayName(kind, v string) error {
	if v == "" {
		return nil
	}
	return checkNameText(kind, v, maxDisplayNameLen, "")
}

var agencyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// validateAgencyID checks a new agency ID (letters, digits, - _ .).
func validateAgencyID(v string) error {
	if !agencyIDPattern.MatchString(v) {
		return fmt.Errorf("agency ID must be 1-64 letters, digits, '-', '_' or '.', starting with a letter or digit")
	}
	return nil
}

// accountNameTakenLocked reports whether username already exists in agency
// under any letter case, in either store. The login page matches names
// case-insensitively, so "ADMIN" next to "admin" would be ambiguous.
// Caller holds s.mu. skipKey is ignored (the account being renamed).
func (s *Server) accountNameTakenLocked(agencyID, username, skipKey string) bool {
	want := userKey(agencyID, username)
	for k := range s.users {
		if k != skipKey && strings.EqualFold(k, want) {
			return true
		}
	}
	for k := range s.webAdmins {
		if k != skipKey && strings.EqualFold(k, want) {
			return true
		}
	}
	return false
}
