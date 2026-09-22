package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// upgrader is initialized in StartWeb() with proper origin checking
var upgrader websocket.Upgrader

type WebSocketMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

func (s *Server) StartWeb() error {
	// Initialize WebSocket upgrader with proper origin checking
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // Non-browser clients (no Origin header)
			}
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			return u.Host == r.Host
		},
	}

	r := mux.NewRouter()

	// Unauthenticated routes
	r.HandleFunc("/api/login", s.handleLogin).Methods("POST")
	r.HandleFunc("/api/session", s.handleSession).Methods("GET")

	// Authenticated API routes
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/logout", s.handleLogout).Methods("POST")
	api.HandleFunc("/stats", s.handleStats).Methods("GET")
	api.HandleFunc("/clients", s.handleClients).Methods("GET")
	api.HandleFunc("/channels", s.handleChannels).Methods("GET")
	api.HandleFunc("/groups", s.handleGroups).Methods("GET")
	// MCX Step 4: get/set a channel's MCX group type.
	api.HandleFunc("/channel-group-types", s.handleGetChannelGroupTypes).Methods("GET")
	api.HandleFunc("/channel-group-types", s.handleSetChannelGroupType).Methods("POST")
	api.HandleFunc("/config", s.handleGetConfig).Methods("GET")
	api.HandleFunc("/config", s.handleUpdateConfig).Methods("POST")
	api.HandleFunc("/users", s.handleGetUsers).Methods("GET")
	api.HandleFunc("/users", s.handleCreateUser).Methods("POST")
	api.HandleFunc("/users/template", s.handleUserCsvTemplate).Methods("GET")
	api.HandleFunc("/users/bulk", s.handleBulkCreateUsers).Methods("POST")
	api.HandleFunc("/users/{username}", s.handleUpdateUser).Methods("PUT")
	api.HandleFunc("/users/{username}", s.handleDeleteUser).Methods("DELETE")
	api.HandleFunc("/users/{username}/password", s.handleUpdatePassword).Methods("PUT")
	api.HandleFunc("/users/{username}/role", s.handleUpdateRole).Methods("PUT")
	api.HandleFunc("/users/{username}/enroll", s.handleGenerateEnrollToken).Methods("POST")
	api.HandleFunc("/auth/status", s.handleAuthStatus).Methods("GET")
	api.HandleFunc("/auth/toggle", s.requireSuperAdmin(s.handleAuthToggle)).Methods("POST")
	api.HandleFunc("/admin/change-password", s.handleAdminChangePassword).Methods("POST")
	api.HandleFunc("/version", s.handleVersion).Methods("GET")
	api.HandleFunc("/update/check", s.requireSuperAdmin(s.handleUpdateCheck)).Methods("POST")
	api.HandleFunc("/update/apply", s.requireSuperAdmin(s.handleUpdateApply)).Methods("POST")
	api.HandleFunc("/update/status", s.handleUpdateStatus).Methods("GET")
	api.HandleFunc("/github-token", s.requireSuperAdmin(s.handleGetGitHubToken)).Methods("GET")
	api.HandleFunc("/github-token", s.requireSuperAdmin(s.handleSetGitHubToken)).Methods("POST")

	// Agency management (super_admin only)
	api.HandleFunc("/agencies", s.requireSuperAdmin(s.handleGetAgencies)).Methods("GET")
	api.HandleFunc("/agencies", s.requireSuperAdmin(s.handleCreateAgency)).Methods("POST")
	api.HandleFunc("/agencies/{id}", s.requireSuperAdmin(s.handleUpdateAgency)).Methods("PUT")
	api.HandleFunc("/agencies/{id}", s.requireSuperAdmin(s.handleDeleteAgency)).Methods("DELETE")
	api.HandleFunc("/agencies/{id}/admin", s.requireSuperAdmin(s.handleCreateAgencyAdmin)).Methods("POST")

	// Mutual Aid management (super_admin only)
	api.HandleFunc("/mutual-aid", s.requireSuperAdmin(s.handleGetMutualAid)).Methods("GET")
	api.HandleFunc("/mutual-aid", s.requireSuperAdmin(s.handleCreateMutualAid)).Methods("POST")
	api.HandleFunc("/mutual-aid/{id}", s.requireSuperAdmin(s.handleDeleteMutualAid)).Methods("DELETE")

	// Agency-driven mutual aid (agency_admin): discovery settings, the
	// directory, and the request/approve flow. Each handler scopes itself to
	// the caller's own agency; super_admin sees everything.
	api.HandleFunc("/my-agency/discovery", s.requireAdmin(s.handleGetMyDiscovery)).Methods("GET")
	api.HandleFunc("/my-agency/discovery", s.requireAdmin(s.handleUpdateMyDiscovery)).Methods("PUT")
	api.HandleFunc("/agencies/discoverable", s.requireAdmin(s.handleGetDiscoverableAgencies)).Methods("GET")
	api.HandleFunc("/my-agency/mutual-aid", s.requireAdmin(s.handleGetMyMutualAid)).Methods("GET")
	api.HandleFunc("/mutual-aid/requests", s.requireAdmin(s.handleGetMutualAidRequests)).Methods("GET")
	api.HandleFunc("/mutual-aid/requests", s.requireAdmin(s.handleCreateMutualAidRequest)).Methods("POST")
	api.HandleFunc("/mutual-aid/requests/{id}/approve", s.requireAdmin(s.handleApproveMutualAidRequest)).Methods("POST")
	api.HandleFunc("/mutual-aid/requests/{id}/decline", s.requireAdmin(s.handleDeclineMutualAidRequest)).Methods("POST")
	api.HandleFunc("/mutual-aid/{id}/leave", s.requireAdmin(s.handleLeaveMutualAid)).Methods("POST")

	// Agency-owned Zello bridges (agency_admin). A bridge is private to the
	// agency that created it; the owner may grant another agency USE of it.
	api.HandleFunc("/my-agency/zello-bridges", s.requireAdmin(s.handleGetMyZelloBridges)).Methods("GET")
	api.HandleFunc("/my-agency/zello-bridges", s.requireAdmin(s.handleCreateMyZelloBridge)).Methods("POST")
	api.HandleFunc("/my-agency/zello-bridges/{id}", s.requireAdmin(s.handleDeleteMyZelloBridge)).Methods("DELETE")
	api.HandleFunc("/my-agency/zello-bridges/{id}/share", s.requireAdmin(s.handleShareZelloBridge)).Methods("POST")
	api.HandleFunc("/my-agency/zello-bridges/{id}/unshare", s.requireAdmin(s.handleUnshareZelloBridge)).Methods("POST")

	// Agency-owned LMR bridges (agency_admin). Private to the owning agency;
	// the owner may grant another agency USE of the bridged channel.
	api.HandleFunc("/my-agency/lmr-bridges", s.requireAdmin(s.handleGetMyLMRBridges)).Methods("GET")
	api.HandleFunc("/my-agency/lmr-bridges", s.requireAdmin(s.handleCreateMyLMRBridge)).Methods("POST")
	api.HandleFunc("/my-agency/lmr-bridges/{id}", s.requireAdmin(s.handleDeleteMyLMRBridge)).Methods("DELETE")
	api.HandleFunc("/my-agency/lmr-bridges/{id}/share", s.requireAdmin(s.handleShareMyLMRBridge)).Methods("POST")
	api.HandleFunc("/my-agency/lmr-bridges/{id}/unshare", s.requireAdmin(s.handleUnshareMyLMRBridge)).Methods("POST")

	// LMR (VHF/UHF/P25) radio bridges (super_admin)
	api.HandleFunc("/lmr-bridges", s.requireSuperAdmin(s.handleGetLMRBridges)).Methods("GET")
	api.HandleFunc("/lmr-bridges", s.requireSuperAdmin(s.handleCreateLMRBridge)).Methods("POST")
	api.HandleFunc("/lmr-bridges/{id}", s.requireSuperAdmin(s.handleUpdateLMRBridge)).Methods("PUT")
	api.HandleFunc("/lmr-bridges/{id}", s.requireSuperAdmin(s.handleDeleteLMRBridge)).Methods("DELETE")
	api.HandleFunc("/lmr-bridges/{id}/toggle", s.requireSuperAdmin(s.handleToggleLMRBridge)).Methods("POST")

	// Zello Work channel bridges (super_admin)
	api.HandleFunc("/zello-bridges", s.requireSuperAdmin(s.handleGetZelloBridges)).Methods("GET")
	api.HandleFunc("/zello-bridges", s.requireSuperAdmin(s.handleCreateZelloBridge)).Methods("POST")
	api.HandleFunc("/zello-bridges/{id}", s.requireSuperAdmin(s.handleUpdateZelloBridge)).Methods("PUT")
	api.HandleFunc("/zello-bridges/{id}", s.requireSuperAdmin(s.handleDeleteZelloBridge)).Methods("DELETE")
	api.HandleFunc("/zello-bridges/{id}/toggle", s.requireSuperAdmin(s.handleToggleZelloBridge)).Methods("POST")
	api.HandleFunc("/zello-bridges/{id}/map", s.requireSuperAdmin(s.handleMapZelloBridge)).Methods("POST")
	api.HandleFunc("/zello-bridges/{id}/channels", s.requireSuperAdmin(s.handleZelloBridgeChannels)).Methods("GET")
	api.HandleFunc("/zello-channels", s.requireSuperAdmin(s.handleZelloChannels)).Methods("POST")

	// Guest Event management (super_admin sees all; agency_admin scoped to own agency)
	api.HandleFunc("/guest-events", s.requireAdmin(s.handleGetGuestEvents)).Methods("GET")
	api.HandleFunc("/guest-events", s.requireAdmin(s.handleCreateGuestEvent)).Methods("POST")
	api.HandleFunc("/guest-events/{id}", s.requireAdmin(s.handleDeleteGuestEvent)).Methods("DELETE")

	// Dispatch console API
	api.HandleFunc("/dispatch/clients", s.requireDispatchOrAdmin(s.handleDispatchClients)).Methods("GET")
	api.HandleFunc("/dispatch/channel", s.requireDispatchOrAdmin(s.handleDispatchSetChannel)).Methods("POST")

	// Activity log
	api.HandleFunc("/activity", s.handleGetActivity).Methods("GET")

	// Debug endpoint (super_admin only)
	api.HandleFunc("/debug/state", s.requireSuperAdmin(s.handleDebugState)).Methods("GET")

	// WebSocket for real-time updates
	r.HandleFunc("/ws", s.handleWebSocket)

	// WebSocket for dispatch console (bidirectional audio + client list)
	r.HandleFunc("/ws/dispatch", s.handleDispatchWebSocket)

	// Serve static files (web GUI)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./web")))

	addr := fmt.Sprintf(":%d", s.config.WebPort)

	// pprof: only start if explicitly enabled, and bind to loopback only
	if s.config.DebugPprof {
		go func() {
			log.Printf("pprof debug server on 127.0.0.1:6060 (loopback only)")
			if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
				log.Printf("pprof server error: %v", err)
			}
		}()
	}

	// Use http.Server with timeouts to prevent stuck connections from blocking the server
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.authMiddleware(r),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// TLS: if cert and key are configured, serve HTTPS
	if s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		log.Printf("Web GUI server starting on https://localhost%s (TLS enabled)", addr)
		return srv.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
	}

	// No TLS — warn if binding to non-loopback
	log.Printf("WARNING: Web dashboard running without TLS on %s — admin credentials transmitted in cleartext", addr)
	log.Printf("WARNING: Set tls_cert_file and tls_key_file in config.json to enable TLS")
	log.Printf("Web GUI server starting on http://localhost%s", addr)
	return srv.ListenAndServe()
}

// authMiddleware checks for valid session cookie on all requests
// except login, session check, and login page assets.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Allow unauthenticated access to login-related paths
		if path == "/api/login" || path == "/api/session" ||
			path == "/login.html" || path == "/dispatch.html" ||
			path == "/dispatch.js" || path == "/dispatch.css" ||
			path == "/style.css" ||
			path == "/logo.png" || path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		// Check session cookie
		cookie, err := r.Cookie("session")
		if err != nil || !s.validateWebSession(cookie.Value) {
			// API requests get 401 JSON
			if strings.HasPrefix(path, "/api/") || path == "/ws" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			// Page requests get redirected to login
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}

		// Enforce MustChangePassword server-side: block all privileged
		// API calls until the admin has changed their password
		s.mu.RLock()
		session := s.webSessions[cookie.Value]
		var mustChange bool
		if session != nil {
			if admin, exists := s.webAdmins[userKey(session.AgencyID, session.Username)]; exists {
				mustChange = admin.MustChangePassword
			}
		}
		s.mu.RUnlock()

		if mustChange {
			allowedWhenForced := path == "/api/admin/change-password" ||
				path == "/api/logout" ||
				path == "/api/session" ||
				// Allow static assets so the dashboard page (with password change modal) can load
				path == "/" || path == "/index.html" ||
				path == "/app.js" || path == "/style.css" ||
				path == "/logo.png" || path == "/favicon.ico" ||
				path == "/dispatch.html" || path == "/dispatch.js" || path == "/dispatch.css"
			if !allowedWhenForced {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"must_change_password","message":"You must change your password before accessing other features"}`))
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		AgencyID string `json:"agency_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.AgencyID = strings.TrimSpace(req.AgencyID)

	s.mu.RLock()
	admin, exists := s.webAdmins[userKey(req.AgencyID, req.Username)]
	if !exists {
		// The login page's Agency ID and username are free text. Store keys
		// are case-sensitive ("dev" vs "DEV"), which read as "wrong password"
		// with no way to tell. Fall back to a case-insensitive match.
		for k, a := range s.webAdmins {
			if strings.EqualFold(k, userKey(req.AgencyID, req.Username)) {
				admin, exists = a, true
				break
			}
		}
	}
	s.mu.RUnlock()

	if !exists || !checkPassword(req.Password, admin.PasswordHash) {
		// Say WHICH half failed in the journal (never to the client): a
		// missing account and a wrong password are different problems.
		if !exists {
			log.Printf("Failed web login: no dashboard account for %s [%s] from %s", req.Username, req.AgencyID, r.RemoteAddr)
		} else {
			log.Printf("Failed web login: wrong password for %s [%s] from %s", req.Username, req.AgencyID, r.RemoteAddr)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid username or password"}`))
		return
	}

	// Transparently migrate legacy SHA-256 hashes to bcrypt on successful login
	if !strings.HasPrefix(admin.PasswordHash, "$2a$") && !strings.HasPrefix(admin.PasswordHash, "$2b$") {
		if newHash, err := hashPasswordSecure(req.Password); err == nil {
			s.mu.Lock()
			admin.PasswordHash = newHash
			s.mu.Unlock()
			s.saveWebAdmins()
			log.Printf("Upgraded password hash to bcrypt for user: %s", req.Username)
		}
	}

	// Create session with role info
	token := s.generateSessionToken()
	s.mu.Lock()
	s.webSessions[token] = &WebSession{
		Username: req.Username,
		Role:     admin.Role,
		AgencyID: admin.AgencyID,
	}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400, // 24 hours
		Secure:   s.config.TLSCertFile != "",
		SameSite: http.SameSiteStrictMode,
	})

	log.Printf("Web login successful: %s (role: %s) from %s", req.Username, admin.Role, r.RemoteAddr)
	s.activityLog.Add("admin_login", req.Username, fmt.Sprintf("Web dashboard login (role: %s)", admin.Role), admin.AgencyID)
	w.Header().Set("Content-Type", "application/json")
	mustChange := admin.MustChangePassword
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":               "ok",
		"username":             req.Username,
		"role":                 admin.Role,
		"agency_id":            admin.AgencyID,
		"must_change_password": mustChange,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session")
	if err == nil {
		s.mu.Lock()
		delete(s.webSessions, cookie.Value)
		s.mu.Unlock()
	}

	// Clear the cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Secure:   s.config.TLSCertFile != "",
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session")
	if err != nil || !s.validateWebSession(cookie.Value) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}

	s.mu.RLock()
	session := s.webSessions[cookie.Value]
	admin, exists := s.webAdmins[userKey(session.AgencyID, session.Username)]
	mustChange := exists && admin.MustChangePassword
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":               "ok",
		"username":             session.Username,
		"role":                 session.Role,
		"agency_id":            session.AgencyID,
		"must_change_password": mustChange,
		"ptt_port":             s.config.PTTPort,
	})
}

// getSession extracts the WebSession from the request cookie.
func (s *Server) getSession(r *http.Request) *WebSession {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.webSessions[cookie.Value]
}

// requireSuperAdmin wraps a handler to require super_admin role.
func (s *Server) requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := s.getSession(r)
		if session == nil || session.Role != "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"super admin access required"}`))
			return
		}
		next(w, r)
	}
}

// requireAdmin wraps a handler to require super_admin or agency_admin role.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := s.getSession(r)
		if session == nil || (session.Role != "super_admin" && session.Role != "agency_admin") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"admin access required"}`))
			return
		}
		next(w, r)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)
	agencyFilter := ""
	if session != nil && session.Role == "agency_admin" {
		agencyFilter = session.AgencyID
	}
	stats := s.GetStats(agencyFilter)
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)
	agencyFilter := r.URL.Query().Get("agency")
	if session != nil && session.Role == "agency_admin" {
		agencyFilter = session.AgencyID
	}
	clients := s.GetClients(agencyFilter)
	json.NewEncoder(w).Encode(clients)
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)
	agencyFilter := r.URL.Query().Get("agency")
	if session != nil && session.Role == "agency_admin" {
		agencyFilter = session.AgencyID
	}
	channels := s.GetChannels(agencyFilter)
	json.NewEncoder(w).Encode(channels)
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups := s.GetGroups()
	json.NewEncoder(w).Encode(groups)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(s.config)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var newConfig Config
	if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Update config (would require restart for some changes)
	s.config = &newConfig

	// Save to file
	if err := s.config.Save("config.json"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleGetChannelGroupTypes returns the configured MCX group types and broadcast
// users, so the dashboard can display/edit them.
func (s *Server) handleGetChannelGroupTypes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	types := s.config.ChannelGroupTypes
	bcast := s.config.BroadcastUsers
	s.mu.RUnlock()
	if types == nil {
		types = map[string]string{}
	}
	if bcast == nil {
		bcast = map[string]string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"channel_group_types": types,
		"broadcast_users":     bcast,
	})
}

// handleSetChannelGroupType sets one channel's MCX group type (and broadcast user
// for broadcast type). Body: {agency_id, channel, group_type, broadcast_user}.
func (s *Server) handleSetChannelGroupType(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgencyID      string `json:"agency_id"`
		Channel       int    `json:"channel"`
		GroupType     string `json:"group_type"`
		BroadcastUser string `json:"broadcast_user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}
	// Agency admins can only edit their own agency.
	if session := s.getSession(r); session != nil && session.Role == "agency_admin" {
		req.AgencyID = session.AgencyID
	}
	if req.AgencyID == "" {
		req.AgencyID = "default"
	}
	switch req.GroupType {
	case GroupTypePreconfigured, GroupTypeBroadcast, GroupTypeEmergency, "":
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid group_type"}`))
		return
	}
	key := channelKey(req.AgencyID, req.Channel)
	s.mu.Lock()
	if s.config.ChannelGroupTypes == nil {
		s.config.ChannelGroupTypes = map[string]string{}
	}
	if s.config.BroadcastUsers == nil {
		s.config.BroadcastUsers = map[string]string{}
	}
	if req.GroupType == "" || req.GroupType == GroupTypePreconfigured {
		delete(s.config.ChannelGroupTypes, key)
		delete(s.config.BroadcastUsers, key)
	} else {
		s.config.ChannelGroupTypes[key] = req.GroupType
		if req.GroupType == GroupTypeBroadcast {
			s.config.BroadcastUsers[key] = req.BroadcastUser
		} else {
			delete(s.config.BroadcastUsers, key)
		}
	}
	s.mu.Unlock()
	_ = s.config.Save("config.json")

	// Push updated group info to everyone currently on that channel.
	s.broadcastGroupInfo(req.AgencyID, req.Channel)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleDebugState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	defer s.mu.RUnlock()

	clients := make([]map[string]interface{}, 0)
	for key, client := range s.clients {
		clients = append(clients, map[string]interface{}{
			"key":      key,
			"username": client.Username,
			"agency":   client.AgencyID,
			"channel":  client.Channel,
			"addr":     client.Addr.String(),
		})
	}

	links := make([]map[string]interface{}, 0)
	for id, link := range s.mutualAidLinks {
		links = append(links, map[string]interface{}{
			"id":       id,
			"agencies": link.Agencies,
			"channel":  link.Channel,
			"name":     link.ChannelName,
			"active":   link.Active,
		})
	}

	agencies := make([]string, 0)
	for id := range s.agencies {
		agencies = append(agencies, id)
	}

	// Show which clients are in each relevant channel
	channelClients := make(map[string][]string)
	for key, ch := range s.channels {
		ch.mu.RLock()
		if len(ch.Clients) > 0 {
			names := make([]string, 0)
			for _, c := range ch.Clients {
				names = append(names, c.Username+"["+c.AgencyID+"]")
			}
			channelClients[key] = names
		}
		ch.mu.RUnlock()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"clients":         clients,
		"mutual_aid":      links,
		"agencies":        agencies,
		"channel_clients": channelClients,
	})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Capture the session BEFORE upgrading: the dashboard socket carries a
	// per-agency mutual-aid signal, and after the upgrade there is no request
	// to read cookies from.
	wsAgency := ""
	if session := s.getSession(r); session != nil {
		wsAgency = session.AgencyID
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()
	atomic.AddInt64(&s.wsConnCount, 1)
	defer atomic.AddInt64(&s.wsConnCount, -1)

	log.Printf("New WebSocket connection from %s", r.RemoteAddr)

	// Set up ping/pong to detect dead connections
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Read pump — needed so pong frames are processed
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()

	// Send initial data
	s.sendWebSocketUpdate(conn, wsAgency)

	// Send updates every 2 seconds, ping every 30 seconds
	ticker := time.NewTicker(2 * time.Second)
	pingTicker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer pingTicker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("WebSocket ping error: %v", err)
				return
			}
		case <-ticker.C:
			if err := s.sendWebSocketUpdate(conn, wsAgency); err != nil {
				log.Printf("WebSocket send error: %v", err)
				return
			}
		}
	}
}

func (s *Server) sendWebSocketUpdate(conn *websocket.Conn, agencyID string) error {
	// WebSocket sends unfiltered data; the frontend filters by role
	msg := WebSocketMessage{
		Type: "update",
		Data: map[string]interface{}{
			"stats":              s.GetStats(""),
			"clients":            s.GetClients(""),
			"channels":           s.GetChannels(""),
			"network_timeseries": s.networkTimeSeries.GetPoints(),
			"health":             s.CheckHealth(),
			"activity":           s.activityLog.GetRecent(20),
			// A cheap fingerprint of everything this agency's pages show:
			// mutual aid plus the Zello and LMR bridges it owns or has been
			// granted. The dashboard reloads the open page only when this
			// CHANGES, so a request arriving, a bridge being shared, or one
			// being deleted appears without the operator hitting refresh —
			// and without polling those endpoints every two seconds.
			"mutual_aid_rev": s.agencyRevision(agencyID),
		},
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(msg)
}

func (s *Server) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	agencyFilter := ""
	if session != nil && session.Role == "agency_admin" {
		agencyFilter = session.AgencyID
	}
	events := s.activityLog.GetRecentForAgency(50, agencyFilter)
	json.NewEncoder(w).Encode(events)
}

func (s *Server) handleGetUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	agencyFilter := r.URL.Query().Get("agency")
	callerRole := "super_admin"
	callerAgency := ""
	callerUsername := ""
	if session != nil {
		callerRole = session.Role
		callerAgency = session.AgencyID
		callerUsername = session.Username
		if session.Role == "agency_admin" {
			agencyFilter = session.AgencyID
		}
	}
	users := s.GetUsers(agencyFilter, callerRole, callerAgency, callerUsername)
	json.NewEncoder(w).Encode(users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Password string `json:"password"`
		AgencyID string `json:"agency_id"`
		Role     string `json:"role"`
		Priority int    `json:"priority"` // MCX Step 3 floor priority (1-4)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"username and password required"}`))
		return
	}

	// Determine agency based on session role
	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		req.AgencyID = session.AgencyID
		// Agency admins can create "user" or "admin" but not "super_admin"
		if req.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"only super admins can create super admin users"}`))
			return
		}
	}
	if req.AgencyID == "" {
		req.AgencyID = "default"
	}

	// Only super admins can create super_admin users
	if req.Role == "super_admin" && (session == nil || session.Role != "super_admin") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"only super admins can create super admin users"}`))
		return
	}

	if req.Role == "" {
		req.Role = "user"
	}

	// Dispatch users are web-only (no PTT account needed) — create a WebAdmin entry directly
	if req.Role == "dispatch" {
		if req.Username == "" || req.Password == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"username and password required"}`))
			return
		}
		if err := s.AddDispatchUser(req.Username, req.Name, req.Password, req.AgencyID); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"username":  req.Username,
			"agency_id": req.AgencyID,
			"role":      "dispatch",
		})
		return
	}

	enrollToken, err := s.AddUser(req.Username, req.Name, req.Password, req.AgencyID, req.Role)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}
	// MCX Step 3: apply provisioned floor priority if given.
	if req.Priority != 0 {
		s.SetUserPriority(req.AgencyID, req.Username, req.Priority)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":           "ok",
		"username":         req.Username,
		"agency_id":        req.AgencyID,
		"role":             req.Role,
		"enrollment_token": enrollToken,
		"mcx_id":           userMCXID(req.Username, req.AgencyID, s.config.McpttDomain),
	})
}

// handleUserCsvTemplate returns a CSV the admin can fill in and upload via the
// bulk-upload endpoint. Columns match the fields handleCreateUser accepts.
func (s *Server) handleUserCsvTemplate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="meshrelay-users-template.csv"`)
	// Header row + two example rows (examples are comments the parser ignores? No —
	// keep them as real sample rows the admin can edit or delete).
	// priority: 1=EMERGENCY 2=COMMAND 3=RESPONDER 4=MONITOR (blank = RESPONDER).
	tmpl := "username,name,password,agency_id,role,priority\n" +
		"jdoe,John Doe,ChangeMe123,default,user,3\n" +
		"asmith,Amy Smith,ChangeMe123,default,admin,2\n"
	w.Write([]byte(tmpl))
}

// handleBulkCreateUsers accepts a CSV (raw body or multipart "file") with the
// columns username,name,password,agency_id,role and creates each user via the
// same AddUser path as single creation. Returns a per-row result so the admin
// sees exactly which rows succeeded, were skipped (already exist), or errored.
func (s *Server) handleBulkCreateUsers(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)

	// Accept either a multipart file upload ("file") or a raw CSV body.
	var reader io.Reader
	if file, _, err := r.FormFile("file"); err == nil {
		defer file.Close()
		reader = file
	} else {
		reader = r.Body
	}

	cr := csv.NewReader(reader)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we validate per row
	cr.TrimLeadingSpace = true

	rows, err := cr.ReadAll()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf(`{"error":"could not parse CSV: %s"}`, err.Error())))
		return
	}
	if len(rows) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"CSV is empty"}`))
		return
	}

	// Map header columns → indices (order-independent, case-insensitive). If the
	// first row doesn't look like a header, assume the fixed column order.
	col := map[string]int{}
	dataStart := 0
	head := rows[0]
	looksLikeHeader := false
	for _, h := range head {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "username", "name", "password", "agency_id", "role", "priority":
			looksLikeHeader = true
		}
	}
	if looksLikeHeader {
		for i, h := range head {
			col[strings.ToLower(strings.TrimSpace(h))] = i
		}
		dataStart = 1
	} else {
		col = map[string]int{"username": 0, "name": 1, "password": 2, "agency_id": 3, "role": 4, "priority": 5}
	}

	get := func(row []string, key string) string {
		if idx, ok := col[key]; ok && idx < len(row) {
			return strings.TrimSpace(row[idx])
		}
		return ""
	}

	type rowResult struct {
		Line     int    `json:"line"`
		Username string `json:"username"`
		Status   string `json:"status"` // "created" | "skipped" | "error"
		Detail   string `json:"detail,omitempty"`
		Token    string `json:"enrollment_token,omitempty"`
	}
	results := []rowResult{}
	created, skipped, failed := 0, 0, 0

	for i := dataStart; i < len(rows); i++ {
		row := rows[i]
		line := i + 1 // 1-indexed for the admin
		username := get(row, "username")
		name := get(row, "name")
		password := get(row, "password")
		agencyID := get(row, "agency_id")
		role := get(row, "role")

		// Skip fully blank lines silently.
		if username == "" && name == "" && password == "" {
			continue
		}
		if username == "" || password == "" {
			failed++
			results = append(results, rowResult{line, username, "error", "username and password required", ""})
			continue
		}
		if role == "" {
			role = "user"
		}

		// Enforce the same agency/role scoping as single create.
		if session != nil && session.Role == "agency_admin" {
			agencyID = session.AgencyID
			if role == "super_admin" {
				failed++
				results = append(results, rowResult{line, username, "error", "agency admins cannot create super_admin", ""})
				continue
			}
		}
		if role == "super_admin" && (session == nil || session.Role != "super_admin") {
			failed++
			results = append(results, rowResult{line, username, "error", "only super admins can create super_admin", ""})
			continue
		}
		if agencyID == "" {
			agencyID = "default"
		}

		if role == "dispatch" {
			if err := s.AddDispatchUser(username, name, password, agencyID); err != nil {
				if strings.Contains(err.Error(), "exists") {
					skipped++
					results = append(results, rowResult{line, username, "skipped", err.Error(), ""})
				} else {
					failed++
					results = append(results, rowResult{line, username, "error", err.Error(), ""})
				}
				continue
			}
			created++
			results = append(results, rowResult{line, username, "created", "", ""})
			continue
		}

		token, err := s.AddUser(username, name, password, agencyID, role)
		if err != nil {
			if strings.Contains(err.Error(), "exists") {
				skipped++
				results = append(results, rowResult{line, username, "skipped", err.Error(), ""})
			} else {
				failed++
				results = append(results, rowResult{line, username, "error", err.Error(), ""})
			}
			continue
		}
		// MCX Step 3: apply provisioned floor priority from the CSV if present.
		if p := get(row, "priority"); p != "" {
			if pv, e := strconv.Atoi(p); e == nil {
				s.SetUserPriority(agencyID, username, pv)
			}
		}
		created++
		results = append(results, rowResult{line, username, "created", "", token})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"created": created,
		"skipped": skipped,
		"failed":  failed,
		"total":   created + skipped + failed,
		"results": results,
	})
}

// handleGenerateEnrollToken creates a new enrollment token for an existing user.
// Used when the admin clicks QR Code for an existing user.
func (s *Server) handleGenerateEnrollToken(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]
	agencyID := r.URL.Query().Get("agency")

	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		agencyID = session.AgencyID
	}
	if agencyID == "" {
		agencyID = "default"
	}

	s.mu.Lock()
	key := userKey(agencyID, username)
	user, exists := s.users[key]
	if !exists {
		// Try finding by username alone
		for k, u := range s.users {
			if u.Username == username {
				user = u
				key = k
				exists = true
				break
			}
		}
	}
	if !exists {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	token := s.GenerateEnrollmentToken(user.Username, user.AgencyID, user.PasswordHash)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"enrollment_token": token,
		"username":         user.Username,
		"agency_id":        user.AgencyID,
	})
}

// handleUpdateUser updates a user's name, username, agency, and/or password.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]
	agencyID := r.URL.Query().Get("agency")

	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		agencyID = session.AgencyID
	}
	if agencyID == "" {
		agencyID = "default"
	}

	var req struct {
		Name        *string `json:"name"`
		NewUsername *string `json:"new_username"`
		NewAgencyID *string `json:"new_agency_id"`
		Password    *string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	oldKey := userKey(agencyID, username)
	user, exists := s.users[oldKey]
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	// Agency admins can only edit users in their agency (not super_admin)
	if session != nil && session.Role == "agency_admin" {
		if user.AgencyID != session.AgencyID || user.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot edit this user"}`))
			return
		}
		// Agency admins cannot move users to a different agency
		if req.NewAgencyID != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"only super admins can change user agency"}`))
			return
		}
	}

	// Update name
	if req.Name != nil {
		user.Name = *req.Name
	}

	// Update password
	if req.Password != nil && *req.Password != "" {
		user.PasswordHash = hashPassword(*req.Password)
		// Also update web admin password if applicable
		if admin, ok := s.webAdmins[oldKey]; ok {
			if bh, err := hashPasswordSecure(*req.Password); err == nil {
				admin.PasswordHash = bh
			} else {
				admin.PasswordHash = hashPassword(*req.Password)
			}
		}
	}

	// Determine new key (username or agency may have changed)
	newUsername := username
	newAgency := agencyID
	if req.NewUsername != nil && *req.NewUsername != "" {
		newUsername = *req.NewUsername
	}
	if req.NewAgencyID != nil && *req.NewAgencyID != "" {
		newAgency = *req.NewAgencyID
		// Verify target agency exists
		if _, ok := s.agencies[newAgency]; !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"target agency not found"}`))
			return
		}
	}

	newKey := userKey(newAgency, newUsername)

	// If key changed, check for conflicts and re-key
	if newKey != oldKey {
		if _, conflict := s.users[newKey]; conflict {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"a user with that username already exists in the target agency"}`))
			return
		}
		user.Username = newUsername
		user.AgencyID = newAgency
		delete(s.users, oldKey)
		s.users[newKey] = user

		// Re-key web admin if exists
		if admin, ok := s.webAdmins[oldKey]; ok {
			admin.Username = newUsername
			admin.AgencyID = newAgency
			delete(s.webAdmins, oldKey)
			s.webAdmins[newKey] = admin
			s.saveWebAdmins()
		}
	}

	if err := s.saveUsers(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to save"}`))
		return
	}

	log.Printf("User updated: %s -> %s (agency: %s -> %s)", username, newUsername, agencyID, newAgency)
	s.activityLog.Add("user_updated", newUsername, fmt.Sprintf("Updated user (was: %s in %s)", username, agencyID), newAgency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"username":  newUsername,
		"agency_id": newAgency,
	})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]
	agencyID := r.URL.Query().Get("agency")

	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		agencyID = session.AgencyID
		// Agency admins can delete "user" and "admin" role accounts (not super_admin)
		s.mu.RLock()
		user, exists := s.users[userKey(agencyID, username)]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID || user.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot delete this user"}`))
			return
		}
	}

	if err := s.RemoveUser(agencyID, username); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]
	agencyID := r.URL.Query().Get("agency")

	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		agencyID = session.AgencyID
		// Agency admins can edit "user" and "admin" passwords (not super_admin)
		s.mu.RLock()
		user, exists := s.users[userKey(agencyID, username)]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID || user.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot edit this user"}`))
			return
		}
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"password required"}`))
		return
	}

	if err := s.UpdatePassword(agencyID, username, req.Password); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	log.Printf("Password updated for user: %s [%s]", username, agencyID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]
	agencyID := r.URL.Query().Get("agency")

	session := s.getSession(r)

	// Agency admins can change roles of "user" and "admin" accounts in their agency (not super_admin)
	if session != nil && session.Role == "agency_admin" {
		agencyID = session.AgencyID
		s.mu.RLock()
		user, exists := s.users[userKey(agencyID, username)]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID || user.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot edit this user"}`))
			return
		}
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"role is required"}`))
		return
	}

	// Only super admins can set super_admin role
	if req.Role == "super_admin" && (session == nil || session.Role != "super_admin") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"only super admins can assign super admin role"}`))
		return
	}

	if req.Role != "user" && req.Role != "admin" && req.Role != "super_admin" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"role must be user, admin, or super_admin"}`))
		return
	}

	if err := s.UpdateUserRole(agencyID, username, req.Role); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "role": req.Role})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"auth_enabled": s.config.AuthEnabled,
	})
}

func (s *Server) handleAuthToggle(w http.ResponseWriter, r *http.Request) {
	s.config.AuthEnabled = !s.config.AuthEnabled

	if err := s.config.Save("config.json"); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to save config"}`))
		return
	}

	status := map[bool]string{true: "enabled", false: "disabled"}[s.config.AuthEnabled]
	log.Printf("Authentication %s", status)
	s.activityLog.Add("auth_toggle", "", fmt.Sprintf("Authentication %s", status), "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if s.config.AuthEnabled {
		w.Write([]byte(`{"auth_enabled":true}`))
	} else {
		w.Write([]byte(`{"auth_enabled":false}`))
	}
}

func (s *Server) handleAdminChangePassword(w http.ResponseWriter, r *http.Request) {
	// Get current user from session
	session := s.getSession(r)
	if session == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	username := session.Username

	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if len(req.NewPassword) < 12 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"password must be at least 12 characters"}`))
		return
	}
	lowPw := strings.ToLower(req.NewPassword)
	if lowPw == "admin" || lowPw == "password" || lowPw == "123456789012" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"password is too common, choose a stronger one"}`))
		return
	}

	s.mu.Lock()
	admin, exists := s.webAdmins[userKey(session.AgencyID, username)]
	if !exists {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"admin not found"}`))
		return
	}
	newHash, err := hashPasswordSecure(req.NewPassword)
	if err != nil {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to hash password"}`))
		return
	}
	admin.PasswordHash = newHash
	admin.MustChangePassword = false

	// Keep the PTT account in step. An agency_admin usually has a users.json
	// entry too, and changing only the dashboard hash leaves the app unable to
	// log in with the new password — the two stores must not drift apart.
	pttSynced := false
	if user, ok := s.users[userKey(session.AgencyID, username)]; ok {
		user.PasswordHash = hashPassword(req.NewPassword)
		pttSynced = true
	}
	s.mu.Unlock()

	if err := s.saveWebAdmins(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to save"}`))
		return
	}
	if pttSynced {
		if err := s.saveUsers(); err != nil {
			log.Printf("Password change: failed to save users for %s: %v", username, err)
		}
	}

	log.Printf("Web admin password changed for: %s [%s]", username, session.AgencyID)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// --- Agency Management ---

func (s *Server) handleGetAgencies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	defer s.mu.RUnlock()

	agencies := make([]map[string]interface{}, 0, len(s.agencies))
	for _, agency := range s.agencies {
		// Count users in this agency
		userCount := 0
		adminUsername := ""
		for _, user := range s.users {
			if user.AgencyID == agency.ID {
				userCount++
				// Find agency admin from users with admin role
				if user.Role == "admin" && adminUsername == "" {
					adminUsername = user.Username
				}
			}
		}
		// Also check webAdmins for legacy agency admins
		if adminUsername == "" {
			for _, admin := range s.webAdmins {
				if admin.AgencyID == agency.ID && admin.Role == "agency_admin" {
					adminUsername = admin.Username
					break
				}
			}
		}
		agencies = append(agencies, map[string]interface{}{
			"id":           agency.ID,
			"name":         agency.Name,
			"created_at":   agency.CreatedAt,
			"max_users":    agency.MaxUsers,
			"max_channels": agency.MaxChannels,
			"user_count":   userCount,
			"admin":        adminUsername,
		})
	}
	json.NewEncoder(w).Encode(agencies)
}

func (s *Server) handleCreateAgency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		MaxUsers    int    `json:"max_users"`
		MaxChannels int    `json:"max_channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.ID == "" || req.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"id and name required"}`))
		return
	}

	if req.MaxUsers <= 0 {
		req.MaxUsers = 100
	}
	// Talkgroups only. 10-19 is the pool the server allocates from for
	// mutual-aid patches, Zello mappings and ad-hoc groups, and LMR bridges sit
	// at 100+, so an agency does not own those.
	if req.MaxChannels <= 0 {
		req.MaxChannels = MaxAgencyTalkgroups
	}
	if req.MaxChannels > MaxAgencyTalkgroups {
		req.MaxChannels = MaxAgencyTalkgroups
	}

	s.mu.Lock()
	if _, exists := s.agencies[req.ID]; exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"agency already exists"}`))
		return
	}

	s.agencies[req.ID] = &Agency{
		ID:          req.ID,
		Name:        req.Name,
		CreatedAt:   time.Now().Format(time.RFC3339),
		MaxUsers:    req.MaxUsers,
		MaxChannels: req.MaxChannels,
	}
	s.ensureAgencyChannels(req.ID)
	s.mu.Unlock()

	if err := s.saveAgencies(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to save: %s"}`, err.Error())))
		return
	}

	log.Printf("Created agency: %s (%s)", req.Name, req.ID)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": req.ID})
}

func (s *Server) handleUpdateAgency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	vars := mux.Vars(r)
	id := vars["id"]

	var req struct {
		Name        string `json:"name"`
		MaxUsers    int    `json:"max_users"`
		MaxChannels int    `json:"max_channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	s.mu.Lock()
	agency, exists := s.agencies[id]
	if !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agency not found"}`))
		return
	}

	if req.Name != "" {
		agency.Name = req.Name
	}
	if req.MaxUsers > 0 {
		agency.MaxUsers = req.MaxUsers
	}
	if req.MaxChannels > 0 {
		agency.MaxChannels = req.MaxChannels
	}
	s.mu.Unlock()

	if err := s.saveAgencies(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to save: %s"}`, err.Error())))
		return
	}

	log.Printf("Updated agency: %s", id)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteAgency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	vars := mux.Vars(r)
	id := vars["id"]

	if id == "default" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"cannot delete default agency"}`))
		return
	}

	s.mu.Lock()
	if _, exists := s.agencies[id]; !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agency not found"}`))
		return
	}

	// Check if agency has users
	for _, user := range s.users {
		if user.AgencyID == id {
			s.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"agency still has users, remove them first"}`))
			return
		}
	}

	// Remove agency channels
	for key := range s.channels {
		if strings.HasPrefix(key, id+":") {
			delete(s.channels, key)
		}
	}

	// Remove agency admin(s)
	for username, admin := range s.webAdmins {
		if admin.AgencyID == id {
			delete(s.webAdmins, username)
		}
	}

	delete(s.agencies, id)
	s.mu.Unlock()

	s.saveAgencies()
	s.saveWebAdmins()

	log.Printf("Deleted agency: %s", id)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleCreateAgencyAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	vars := mux.Vars(r)
	agencyID := vars["id"]

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.Username == "" || req.Password == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"username and password required"}`))
		return
	}

	s.mu.Lock()
	if _, exists := s.agencies[agencyID]; !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agency not found"}`))
		return
	}

	key := userKey(agencyID, req.Username)
	if _, exists := s.webAdmins[key]; exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"admin username already exists in this agency"}`))
		return
	}

	bcryptHash, err := hashPasswordSecure(req.Password)
	if err != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to hash password"}`))
		return
	}
	s.webAdmins[key] = &WebAdmin{
		Username:           req.Username,
		PasswordHash:       bcryptHash,
		Role:               "agency_admin",
		AgencyID:           agencyID,
		MustChangePassword: true,
	}
	s.mu.Unlock()

	if err := s.saveWebAdmins(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to save: %s"}`, err.Error())))
		return
	}

	log.Printf("Created agency admin: %s for agency %s", req.Username, agencyID)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "username": req.Username})
}

// --- Mutual Aid Management ---

func (s *Server) handleGetMutualAid(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	defer s.mu.RUnlock()

	links := make([]map[string]interface{}, 0, len(s.mutualAidLinks))
	for _, link := range s.mutualAidLinks {
		// Build agency names list
		agencyNames := make([]string, 0, len(link.Agencies))
		for _, aid := range link.Agencies {
			name := aid
			if a, ok := s.agencies[aid]; ok {
				name = a.Name
			}
			agencyNames = append(agencyNames, name)
		}
		links = append(links, map[string]interface{}{
			"id":           link.ID,
			"agencies":     link.Agencies,
			"agency_names": agencyNames,
			"channel":      link.Channel,
			"channel_name": link.ChannelName,
			"created_at":   link.CreatedAt,
			"created_by":   link.CreatedBy,
			"active":       link.Active,
		})
	}
	json.NewEncoder(w).Encode(links)
}

func (s *Server) handleCreateMutualAid(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Agencies    []string `json:"agencies"`
		ChannelName string   `json:"channel_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if len(req.Agencies) < 2 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"at least two agencies are required"}`))
		return
	}

	// Check for duplicates
	seen := make(map[string]bool)
	for _, a := range req.Agencies {
		if seen[a] {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"duplicate agency in list"}`))
			return
		}
		seen[a] = true
	}

	if req.ChannelName == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"channel name is required"}`))
		return
	}

	session := s.getSession(r)

	s.mu.Lock()
	// Verify all agencies exist
	for _, aid := range req.Agencies {
		if _, ok := s.agencies[aid]; !ok {
			s.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(fmt.Sprintf(`{"error":"agency %s not found"}`, aid)))
			return
		}
	}

	// Auto-assign channel number from the shared 10-19 block.
	channel := s.pickMutualAidChannel()
	if channel == 0 {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"no free channel — the 10-19 block is full (remove a patch, Zello mapping, or ad-hoc group)"}`))
		return
	}

	// Generate link ID
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	linkID := hex.EncodeToString(idBytes)

	createdBy := ""
	if session != nil {
		createdBy = session.Username
	}

	link := &MutualAidLink{
		ID:          linkID,
		Agencies:    req.Agencies,
		Channel:     channel,
		ChannelName: req.ChannelName,
		CreatedAt:   time.Now().Format(time.RFC3339),
		CreatedBy:   createdBy,
		Active:      true,
	}
	s.mutualAidLinks[linkID] = link
	s.mu.Unlock()

	// Notify in background so the HTTP response returns immediately
	go s.notifyMutualAidChannel(req.Agencies, channel, req.ChannelName, true)
	// Linked agencies can now see each other, so push a fresh roster to each —
	// otherwise their merged user list stays stale until the next join/leave.
	go func(agencies []string) {
		for _, a := range agencies {
			s.broadcastRoster(a)
		}
	}(append([]string(nil), req.Agencies...))

	if err := s.saveMutualAid(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to save: %s"}`, err.Error())))
		return
	}

	log.Printf("Created mutual aid link: %v on channel %d (%s) (by %s)", req.Agencies, channel, req.ChannelName, createdBy)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": linkID})
}

func (s *Server) handleDeleteMutualAid(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	vars := mux.Vars(r)
	id := vars["id"]

	s.mu.Lock()
	link, exists := s.mutualAidLinks[id]
	if !exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"mutual aid link not found"}`))
		return
	}

	agencies := link.Agencies
	oldChannel := link.Channel
	channelName := link.ChannelName

	delete(s.mutualAidLinks, id)
	s.mu.Unlock()

	// Notify clients in background so HTTP response returns immediately
	go s.notifyMutualAidChannel(agencies, oldChannel, channelName, false)
	// The link is gone: re-push each agency's roster so partner users drop off.
	go func(ags []string) {
		for _, a := range ags {
			s.broadcastRoster(a)
		}
	}(append([]string(nil), agencies...))

	s.saveMutualAid()

	log.Printf("Deleted mutual aid link: %v on channel %d", agencies, oldChannel)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Guest Events ---

func (s *Server) handleGetGuestEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Agency admins only see events their agency created; super_admins see all.
	session := s.getSession(r)
	ownerFilter := ""
	if session != nil && session.Role == "agency_admin" {
		ownerFilter = session.AgencyID
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]map[string]interface{}, 0, len(s.guestEvents))
	for _, ev := range s.guestEvents {
		if ownerFilter != "" && ev.OwnerAgencyID != ownerFilter {
			continue
		}
		agencyName := ev.AgencyID
		if a, ok := s.agencies[ev.AgencyID]; ok {
			agencyName = a.Name
		}
		// Count currently connected clients for this guest user
		connectedCount := 0
		guestKey := userKey(ev.AgencyID, ev.Username)
		for k := range s.clients {
			if k == guestKey {
				connectedCount++
			}
		}
		e := map[string]interface{}{
			"id":          ev.ID,
			"name":        ev.Name,
			"agency_id":   ev.AgencyID,
			"agency_name": agencyName,
			"channel":     ev.Channel,
			"username":    ev.Username,
			"token":       ev.Token,
			"created_at":  ev.CreatedAt,
			"created_by":  ev.CreatedBy,
			"connected":   connectedCount,
		}
		if ev.ExpiresAt != nil {
			e["expires_at"] = ev.ExpiresAt.Format(time.RFC3339)
		}
		events = append(events, e)
	}
	json.NewEncoder(w).Encode(events)
}

func (s *Server) handleCreateGuestEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		Name      string   `json:"name"`
		Channel   int      `json:"channel"`
		ExpiresIn string   `json:"expires_in"` // e.g. "2h", "24h", "72h", "" = never
		Agencies  []string `json:"agencies"`   // existing agencies to bridge into the event channel
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"event name is required"}`))
		return
	}

	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		dur, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid expires_in duration (use e.g. 2h, 24h, 168h)"}`))
			return
		}
		t := time.Now().Add(dur)
		expiresAt = &t
	}

	session := s.getSession(r)
	createdBy := ""
	ownerAgencyID := ""
	if session != nil {
		createdBy = session.Username
		// Agency admins own the events they create; super_admin events are unscoped.
		if session.Role == "agency_admin" {
			ownerAgencyID = session.AgencyID
		}
	}

	ev, err := s.CreateGuestEvent(req.Name, ownerAgencyID, req.Channel, expiresAt, createdBy, req.Agencies)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	resp := map[string]interface{}{
		"status":    "ok",
		"id":        ev.ID,
		"name":      ev.Name,
		"username":  ev.Username,
		"token":     ev.Token,
		"channel":   ev.Channel,
		"agency_id": ev.AgencyID,
	}
	if ev.ExpiresAt != nil {
		resp["expires_at"] = ev.ExpiresAt.Format(time.RFC3339)
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDeleteGuestEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	vars := mux.Vars(r)
	id := vars["id"]

	// Agency admins may only delete events their own agency created.
	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		s.mu.RLock()
		ev, ok := s.guestEvents[id]
		owner := ""
		if ok {
			owner = ev.OwnerAgencyID
		}
		s.mu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"guest event not found"}`))
			return
		}
		if owner != session.AgencyID {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"not authorized to delete this event"}`))
			return
		}
	}

	if err := s.DeleteGuestEvent(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Dispatch Console ---

// requireDispatchOrAdmin allows dispatch, agency_admin, and super_admin roles.
func (s *Server) requireDispatchOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := s.getSession(r)
		if session == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if session.Role != "dispatch" && session.Role != "agency_admin" && session.Role != "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"dispatch access required"}`))
			return
		}
		next(w, r)
	}
}

// handleDispatchClients returns the list of active PTT clients for the dispatch user's agency.
func (s *Server) handleDispatchClients(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	if session == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	clients := s.GetClients(session.AgencyID)
	json.NewEncoder(w).Encode(clients)
}

// handleDispatchSetChannel updates the dispatch user's active listening/transmit channel.
func (s *Server) handleDispatchSetChannel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	if session == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req struct {
		Channel int `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	key := userKey(session.AgencyID, session.Username)
	s.mu.Lock()
	if client, ok := s.clients[key]; ok {
		client.Channel = req.Channel
	}
	s.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "channel": req.Channel})
}

// handleDispatchWebSocket handles the bidirectional dispatch WebSocket connection.
// Binary frames from browser → relay as audio to PTT clients on the dispatch user's channel.
// Binary audio frames from PTT clients → pushed to dispatch browser.
// JSON frames from browser → handle channel switch.
func (s *Server) handleDispatchWebSocket(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if session.Role != "dispatch" && session.Role != "agency_admin" && session.Role != "super_admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Dispatch WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	key := userKey(session.AgencyID, session.Username)
	dc := &DispatchConn{Send: make(chan []byte, 64)}

	// Register virtual PTT client for this dispatch user (channel 0 by default)
	fakeAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	virtualClient := &Client{
		Username:      session.Username,
		AgencyID:      session.AgencyID,
		Addr:          fakeAddr,
		Channel:       0,
		LastHeartbeat: time.Now(),
		ConnectedAt:   time.Now(),
	}
	s.mu.Lock()
	s.clients[key] = virtualClient
	s.dispatchConns[key] = dc
	s.mu.Unlock()

	log.Printf("Dispatch connected: %s [%s]", session.Username, session.AgencyID)
	s.activityLog.Add("dispatch_connect", session.Username, "Dispatch console connected", session.AgencyID)

	defer func() {
		// Only tear down the registration if it still points to THIS connection.
		// On a quick refresh the new connection registers under the same key
		// before this old connection's cleanup runs; deleting blindly would
		// deregister the live connection and silently break audio fan-out.
		s.mu.Lock()
		if s.dispatchConns[key] == dc {
			delete(s.dispatchConns, key)
			delete(s.clients, key)
			s.mu.Unlock()
			log.Printf("Dispatch disconnected: %s [%s]", session.Username, session.AgencyID)
			s.activityLog.Add("dispatch_disconnect", session.Username, "Dispatch console disconnected", session.AgencyID)
		} else {
			s.mu.Unlock()
			log.Printf("Dispatch stale connection closed for %s [%s] (newer connection active)", session.Username, session.AgencyID)
		}
	}()

	// Set up ping/pong
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Send initial client list
	s.sendDispatchUpdate(conn, session.AgencyID)

	// Read pump — handles inbound binary audio + JSON channel switch
	go func() {
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Dispatch read pump closing for %s [%s]: %v", session.Username, session.AgencyID, err)
				conn.Close()
				return
			}
			if msgType == websocket.BinaryMessage {
				// PCM audio from dispatch browser → relay to PTT clients on current channel
				s.mu.RLock()
				dispClient := s.clients[key]
				s.mu.RUnlock()
				if dispClient != nil {
					pkt := &AudioPacket{
						Username:  session.Username,
						AudioData: data,
					}
					s.handleDispatchAudio(pkt, dispClient)
				}
			} else if msgType == websocket.TextMessage {
				// JSON control message (e.g., channel switch)
				var msg struct {
					Type    string `json:"type"`
					Channel int    `json:"channel"`
				}
				if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "channel" {
					s.mu.Lock()
					if c, ok := s.clients[key]; ok {
						c.Channel = msg.Channel
					}
					s.mu.Unlock()
					log.Printf("Dispatch %s switched to channel %d", session.Username, msg.Channel)
				}
			}
		}
	}()

	// Send loop — pump outbound audio frames and periodic JSON updates to browser
	ticker := time.NewTicker(2 * time.Second)
	pingTicker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer pingTicker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case frame := <-dc.Send:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-ticker.C:
			if err := s.sendDispatchUpdate(conn, session.AgencyID); err != nil {
				return
			}
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// sendDispatchUpdate sends a JSON client-list update to a dispatch browser connection.
func (s *Server) sendDispatchUpdate(conn *websocket.Conn, agencyID string) error {
	msg := WebSocketMessage{
		Type: "update",
		Data: map[string]interface{}{
			"clients": s.GetClients(agencyID),
		},
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(msg)
}

// handleDispatchAudio relays audio received from the dispatch browser to PTT clients
// on the same agency+channel, skipping the normal UDP auth checks since dispatch
// is already authenticated via WebSocket session.
func (s *Server) handleDispatchAudio(packet *AudioPacket, dispClient *Client) {
	audioData := append([]byte{PacketTypeAudio}, SerializeAudioPacket(packet)...)

	// Collect the target channels. A super_admin dispatch user (empty
	// AgencyID) transmits to every agency's matching channel number; an
	// agency-scoped dispatch user transmits only to its own agency channel.
	s.mu.RLock()
	var targets []*Channel
	if dispClient.AgencyID == "" {
		for key, ch := range s.channels {
			parts := strings.SplitN(key, ":", 2)
			if len(parts) == 2 && parts[1] == strconv.Itoa(dispClient.Channel) {
				targets = append(targets, ch)
			}
		}
	} else if ch := s.channels[channelKey(dispClient.AgencyID, dispClient.Channel)]; ch != nil {
		targets = append(targets, ch)
	}
	s.mu.RUnlock()

	sent := 0
	for _, channel := range targets {
		channel.mu.RLock()
		for _, otherClient := range channel.Clients {
			if otherClient.Username != dispClient.Username {
				s.sendPacket(audioData, otherClient.Addr)
				sent++
			}
		}
		channel.mu.RUnlock()
	}

	log.Printf("Dispatch audio from %s [%s] on channel %d to %d clients (%d bytes)",
		packet.Username, dispClient.AgencyID, dispClient.Channel, sent, len(packet.AudioData))
}

// --- Update Management ---

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		URL                string `json:"url"` // API URL for private repo downloads
	} `json:"assets"`
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": Version})
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	ghReq, _ := http.NewRequest("GET", "https://api.github.com/repos/RyanR3/MeshRelayServer/releases/latest", nil)
	if s.config.GitHubToken != "" {
		ghReq.Header.Set("Authorization", "token "+s.config.GitHubToken)
	}
	resp, err := client.Do(ghReq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to reach GitHub: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"current_version":  Version,
			"latest_version":   Version,
			"update_available": false,
			"message":          "No releases found on GitHub",
		})
		return
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("GitHub API returned %d: %s", resp.StatusCode, string(bodyBytes))})
		return
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to parse GitHub response"})
		return
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(Version, "v")
	updateAvailable := latestVersion != currentVersion || Version == "dev"

	var binaryURL, webURL, checksumURL, binaryName string
	for _, asset := range release.Assets {
		// Use API URL for private repos (requires token + Accept: application/octet-stream)
		// Use browser URL for public repos
		dlURL := asset.BrowserDownloadURL
		if s.config.GitHubToken != "" && asset.URL != "" {
			dlURL = asset.URL
		}
		// Pick the asset matching THIS machine's architecture. Raspberry Pi
		// deployments are aarch64 and releases ship both; without this the
		// updater always took the amd64 "ptt-server-linux" and installed a
		// binary that could not execute. A generic name is the last resort,
		// and an exact arch match always wins over one already chosen.
		if asset.Name == archBinaryAsset() {
			binaryURL = dlURL
			binaryName = asset.Name
		} else if binaryName != archBinaryAsset() &&
			(asset.Name == "ptt-server-linux" || asset.Name == "ptt-server") {
			binaryURL = dlURL
			binaryName = asset.Name
		}
		if asset.Name == "web.tar.gz" {
			webURL = dlURL
		}
		if asset.Name == "SHA256SUMS" {
			checksumURL = dlURL
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"current_version":  Version,
		"latest_version":   release.TagName,
		"update_available": updateAvailable,
		"release_notes":    release.Body,
		"release_url":      release.HTMLURL,
		"binary_url":       binaryURL,
		"binary_name":      binaryName,
		"web_url":          webURL,
		"checksum_url":     checksumURL,
		"assets_found":     binaryURL != "",
	})
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.updateStatus.mu.RLock()
	if s.updateStatus.InProgress {
		s.updateStatus.mu.RUnlock()
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Update already in progress"})
		return
	}
	s.updateStatus.mu.RUnlock()

	var req struct {
		BinaryURL   string `json:"binary_url"`
		BinaryName  string `json:"binary_name"`
		WebURL      string `json:"web_url"`
		ChecksumURL string `json:"checksum_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BinaryURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "binary_url is required"})
		return
	}

	allowedPrefixes := []string{
		"https://github.com/RyanR3/MeshRelayServer/",
		"https://api.github.com/repos/RyanR3/MeshRelayServer/",
	}

	validateURL := func(u string) bool {
		for _, prefix := range allowedPrefixes {
			if strings.HasPrefix(u, prefix) {
				return true
			}
		}
		return false
	}

	if !validateURL(req.BinaryURL) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Binary URL not in allowlist"})
		return
	}

	if req.WebURL != "" && !validateURL(req.WebURL) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Web URL not in allowlist"})
		return
	}

	if req.ChecksumURL != "" && !validateURL(req.ChecksumURL) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Checksum URL not in allowlist"})
		return
	}

	go s.performUpdate(req.BinaryURL, req.BinaryName, req.WebURL, req.ChecksumURL, s.config.GitHubToken)

	json.NewEncoder(w).Encode(map[string]string{"status": "update_started"})
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	s.updateStatus.mu.RLock()
	defer s.updateStatus.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.updateStatus)
}

func (s *Server) setUpdateStatus(stage string, progress int, message string, errMsg string) {
	s.updateStatus.mu.Lock()
	defer s.updateStatus.mu.Unlock()
	s.updateStatus.Stage = stage
	s.updateStatus.Progress = progress
	s.updateStatus.Message = message
	s.updateStatus.Error = errMsg
	if stage == "error" || stage == "done" {
		s.updateStatus.InProgress = false
	}
}

func (s *Server) performUpdate(binaryURL, binaryName, webURL, checksumURL, ghToken string) {
	s.updateStatus.mu.Lock()
	s.updateStatus.InProgress = true
	s.updateStatus.Error = ""
	s.updateStatus.mu.Unlock()

	log.Printf("Starting server update...")

	execPath, err := os.Executable()
	if err != nil {
		s.setUpdateStatus("error", 0, "", "Failed to determine executable path: "+err.Error())
		return
	}
	installDir := filepath.Dir(execPath)

	// Stage 1: Download binary
	s.setUpdateStatus("downloading", 10, "Downloading new binary...", "")
	log.Printf("Downloading binary from: %s", binaryURL)

	client := &http.Client{Timeout: 5 * time.Minute}
	dlReq, _ := http.NewRequest("GET", binaryURL, nil)
	if ghToken != "" {
		dlReq.Header.Set("Authorization", "token "+ghToken)
		dlReq.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := client.Do(dlReq)
	if err != nil {
		s.setUpdateStatus("error", 10, "", "Failed to download binary: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		s.setUpdateStatus("error", 10, "", fmt.Sprintf("Download failed with status %d", resp.StatusCode))
		return
	}

	tmpBinary := filepath.Join(installDir, "ptt-server.new")
	outFile, err := os.Create(tmpBinary)
	if err != nil {
		s.setUpdateStatus("error", 10, "", "Failed to create temp file: "+err.Error())
		return
	}
	const maxBinarySize = 100 * 1024 * 1024 // 100 MB
	limitedReader := io.LimitReader(resp.Body, maxBinarySize+1)
	written, err := io.Copy(outFile, limitedReader)
	if err != nil {
		outFile.Close()
		os.Remove(tmpBinary)
		s.setUpdateStatus("error", 20, "", "Failed to write binary: "+err.Error())
		return
	}
	outFile.Close()
	if written > maxBinarySize {
		os.Remove(tmpBinary)
		s.setUpdateStatus("error", 20, "", "Binary exceeds maximum allowed size (100 MB)")
		return
	}
	os.Chmod(tmpBinary, 0755)

	// Verify SHA-256 checksum if a checksums file is available in the release
	s.setUpdateStatus("verifying", 25, "Verifying binary checksum...", "")
	verified, checksumErr := s.verifyBinaryChecksum(tmpBinary, binaryName, checksumURL, ghToken)
	if checksumErr != nil {
		// FAIL CLOSED: cannot verify => do not install.
		os.Remove(tmpBinary)
		log.Printf("Checksum verification could not complete: %v", checksumErr)
		s.setUpdateStatus("error", 25, "",
			"Binary checksum could not be verified (checksum file missing or "+
				"unreachable) — update aborted")
		return
	}
	if !verified {
		// hash mismatch — possible tampering
		os.Remove(tmpBinary)
		s.setUpdateStatus("error", 25, "",
			"Binary checksum verification FAILED — possible tampering")
		return
	}
	log.Printf("Binary checksum verified successfully")

	// Stage 2: Download web files
	var tmpWebDir string
	if webURL != "" {
		s.setUpdateStatus("downloading", 30, "Downloading web files...", "")
		log.Printf("Downloading web files from: %s", webURL)

		tmpWebDir = filepath.Join(installDir, "web.new")
		if err := downloadAndExtractTarGz(webURL, tmpWebDir, ghToken); err != nil {
			os.Remove(tmpBinary)
			s.setUpdateStatus("error", 30, "", "Failed to download web files: "+err.Error())
			return
		}
	}

	// Stage 3: Backup
	s.setUpdateStatus("backing_up", 50, "Creating backup...", "")
	log.Printf("Creating backup...")

	backupDir := filepath.Join(installDir, "backups", time.Now().Format("20060102-150405"))
	os.MkdirAll(backupDir, 0755)

	currentBinary := filepath.Join(installDir, "ptt-server")
	copyFile(currentBinary, filepath.Join(backupDir, "ptt-server"))

	webDir := filepath.Join(installDir, "web")
	if _, err := os.Stat(webDir); err == nil {
		copyDir(webDir, filepath.Join(backupDir, "web"))
	}

	// Stage 4: Install
	s.setUpdateStatus("installing", 70, "Installing update...", "")
	log.Printf("Installing update...")

	if err := os.Rename(tmpBinary, currentBinary); err != nil {
		if err := copyFile(tmpBinary, currentBinary); err != nil {
			s.setUpdateStatus("error", 70, "", "Failed to replace binary: "+err.Error())
			return
		}
		os.Remove(tmpBinary)
	}
	os.Chmod(currentBinary, 0755)

	// Re-apply setcap so the new binary can bind to privileged ports (e.g. 443)
	exec.Command("setcap", "cap_net_bind_service=+ep", currentBinary).Run()

	if tmpWebDir != "" {
		entries, _ := os.ReadDir(tmpWebDir)
		for _, entry := range entries {
			src := filepath.Join(tmpWebDir, entry.Name())
			dst := filepath.Join(webDir, entry.Name())
			if entry.IsDir() {
				copyDir(src, dst)
			} else {
				copyFile(src, dst)
			}
		}
		os.RemoveAll(tmpWebDir)
	}

	// Stage 5: Restart
	s.setUpdateStatus("restarting", 90, "Restarting server...", "")
	log.Printf("Restarting server via systemd...")

	cmd := exec.Command("sudo", "systemctl", "restart", "ptt-server")
	if err := cmd.Start(); err != nil {
		log.Printf("systemctl restart failed: %v", err)
		s.setUpdateStatus("done", 100, "Update installed. Please restart the server manually.", "")
		return
	}

	s.setUpdateStatus("done", 100, "Update installed. Server is restarting...", "")
}

// archBinaryAsset is the release asset name built for the architecture this
// server is running on. Releases ship ptt-server-linux (amd64) alongside
// ptt-server-linux-arm64; the updater must not mix them up.
func archBinaryAsset() string {
	if runtime.GOARCH == "arm64" {
		return "ptt-server-linux-arm64"
	}
	return "ptt-server-linux"
}

// verifyBinaryChecksum downloads the SHA256SUMS file and verifies the binary's
// hash against it. binaryName is the real asset filename (e.g. "ptt-server-linux")
// used to look up the entry in SHA256SUMS. Returns (true, nil) if verified,
// (false, nil) if hash mismatch, or (false, err) if checksum file unavailable.
// NOTE: caller treats any error as fatal (fail closed) — only (true, nil) proceeds.
func (s *Server) verifyBinaryChecksum(binaryPath, binaryName, checksumURL, ghToken string) (bool, error) {
	if checksumURL == "" {
		return false, fmt.Errorf("no checksum URL provided")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", checksumURL, nil)
	if ghToken != "" {
		req.Header.Set("Authorization", "token "+ghToken)
		req.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to download checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false, fmt.Errorf("checksum file not found (status %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024)) // 10 KB max for checksum file
	if err != nil {
		return false, fmt.Errorf("failed to read checksums: %w", err)
	}

	log.Printf("Checksum verification: looking for binary_name=%q in SHA256SUMS", binaryName)

	// Parse SHA256SUMS format: "hash  filename\n"
	// Try exact match first, then try common binary names as fallback
	var expectedHash string
	namesToTry := []string{binaryName}
	// Only fall back to the generic amd64 name when that is actually this
	// machine's architecture. Falling back on arm64 would happily verify the
	// wrong binary and install something that cannot run.
	if binaryName != "ptt-server-linux" && archBinaryAsset() == "ptt-server-linux" {
		namesToTry = append(namesToTry, "ptt-server-linux")
	}
	if binaryName != "ptt-server" {
		namesToTry = append(namesToTry, "ptt-server")
	}

	lines := strings.Split(string(body), "\n")
	for _, name := range namesToTry {
		if name == "" {
			continue
		}
		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) == 2 && parts[1] == name {
				expectedHash = parts[0]
				break
			}
		}
		if expectedHash != "" {
			break
		}
	}
	if expectedHash == "" {
		return false, fmt.Errorf("no checksum entry found for %s in SHA256SUMS content: %s", binaryName, string(body))
	}

	// Compute actual hash
	f, err := os.Open(binaryPath)
	if err != nil {
		return false, fmt.Errorf("failed to open binary for hashing: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return false, fmt.Errorf("failed to hash binary: %w", err)
	}
	actualHash := hex.EncodeToString(hasher.Sum(nil))

	log.Printf("Checksum verification: expected=%s actual=%s match=%v", expectedHash, actualHash, actualHash == expectedHash)
	return actualHash == expectedHash, nil
}

func (s *Server) handleGetGitHubToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Only return whether a token is configured, not the token itself
	json.NewEncoder(w).Encode(map[string]interface{}{
		"configured": s.config.GitHubToken != "",
	})
}

func (s *Server) handleSetGitHubToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request"})
		return
	}

	s.config.GitHubToken = req.Token
	if err := s.config.Save(s.configFile); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"configured": req.Token != "",
		"message":    "GitHub token updated",
	})
}

func downloadAndExtractTarGz(url string, destDir string, ghToken string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	dlReq, _ := http.NewRequest("GET", url, nil)
	if ghToken != "" {
		dlReq.Header.Set("Authorization", "token "+ghToken)
		dlReq.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := client.Do(dlReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	os.MkdirAll(destDir, 0755)

	const maxTarGzSize = 200 * 1024 * 1024 // 200 MB
	limitedBody := io.LimitReader(resp.Body, maxTarGzSize+1)

	gzReader, err := gzip.NewReader(limitedBody)
	if err != nil {
		return fmt.Errorf("gzip error: %w", err)
	}
	defer gzReader.Close()

	// Resolve destDir to absolute for path traversal checks
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to resolve destination: %w", err)
	}

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		name := strings.TrimPrefix(header.Name, "web/")
		if name == "" || name == "." {
			continue
		}

		// Path traversal protection: ensure target stays inside destDir
		target := filepath.Join(absDestDir, name)
		cleanTarget := filepath.Clean(target)
		if !strings.HasPrefix(cleanTarget, absDestDir+string(filepath.Separator)) && cleanTarget != absDestDir {
			return fmt.Errorf("tar entry %q attempts path traversal outside destination", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(cleanTarget, 0755)
		case tar.TypeReg:
			// Reject individual files over 50 MB
			if header.Size > 50*1024*1024 {
				return fmt.Errorf("tar entry %q exceeds 50 MB limit (%d bytes)", header.Name, header.Size)
			}
			os.MkdirAll(filepath.Dir(cleanTarget), 0755)
			f, err := os.Create(cleanTarget)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tarReader, header.Size+1)); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		return copyFile(path, target)
	})
}
