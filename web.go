package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

type WebSocketMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

func (s *Server) StartWeb() error {
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
	api.HandleFunc("/config", s.handleGetConfig).Methods("GET")
	api.HandleFunc("/config", s.handleUpdateConfig).Methods("POST")
	api.HandleFunc("/users", s.handleGetUsers).Methods("GET")
	api.HandleFunc("/users", s.handleCreateUser).Methods("POST")
	api.HandleFunc("/users/{username}", s.handleDeleteUser).Methods("DELETE")
	api.HandleFunc("/users/{username}/password", s.handleUpdatePassword).Methods("PUT")
	api.HandleFunc("/users/{username}/role", s.handleUpdateRole).Methods("PUT")
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

	// WebSocket for real-time updates
	r.HandleFunc("/ws", s.handleWebSocket)

	// Serve static files (web GUI)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./web")))

	addr := fmt.Sprintf(":%d", s.config.WebPort)
	log.Printf("Web GUI server starting on http://localhost%s", addr)

	// Wrap with auth middleware
	return http.ListenAndServe(addr, s.authMiddleware(r))
}

// authMiddleware checks for valid session cookie on all requests
// except login, session check, and login page assets.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Allow unauthenticated access to login-related paths
		if path == "/api/login" || path == "/api/session" ||
			path == "/login.html" || path == "/style.css" ||
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

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	s.mu.RLock()
	admin, exists := s.webAdmins[req.Username]
	s.mu.RUnlock()

	if !exists || admin.PasswordHash != hashPassword(req.Password) {
		log.Printf("Failed web login attempt for user: %s from %s", req.Username, r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid username or password"}`))
		return
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
		MaxAge:   86400 * 7, // 7 days
	})

	log.Printf("Web login successful: %s (role: %s) from %s", req.Username, admin.Role, r.RemoteAddr)
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
	admin, exists := s.webAdmins[session.Username]
	mustChange := exists && admin.MustChangePassword
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":               "ok",
		"username":             session.Username,
		"role":                 session.Role,
		"agency_id":            session.AgencyID,
		"must_change_password": mustChange,
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

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("New WebSocket connection from %s", r.RemoteAddr)

	// Send initial data
	s.sendWebSocketUpdate(conn)

	// Send updates every 2 seconds
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			if err := s.sendWebSocketUpdate(conn); err != nil {
				log.Printf("WebSocket send error: %v", err)
				return
			}
		}
	}
}

func (s *Server) sendWebSocketUpdate(conn *websocket.Conn) error {
	// WebSocket sends unfiltered data; the frontend filters by role
	msg := WebSocketMessage{
		Type: "update",
		Data: map[string]interface{}{
			"stats":    s.GetStats(""),
			"clients":  s.GetClients(""),
			"channels": s.GetChannels(""),
		},
	}

	return conn.WriteJSON(msg)
}

func (s *Server) handleGetUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	agencyFilter := r.URL.Query().Get("agency")
	if session != nil && session.Role == "agency_admin" {
		agencyFilter = session.AgencyID
	}
	users := s.GetUsers(agencyFilter)
	json.NewEncoder(w).Encode(users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		AgencyID string `json:"agency_id"`
		Role     string `json:"role"`
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
		// Agency admins can only create users, not other admins or super_admins
		if req.Role == "super_admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot create super admin users"}`))
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

	if err := s.AddUser(req.Username, req.Password, req.AgencyID, req.Role); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "username": req.Username, "agency_id": req.AgencyID, "role": req.Role})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]

	// Agency admins can only delete users in their agency
	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		s.mu.RLock()
		user, exists := s.users[username]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot delete users outside your agency"}`))
			return
		}
	}

	if err := s.RemoveUser(username); err != nil {
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

	// Agency admins can only edit users in their agency
	session := s.getSession(r)
	if session != nil && session.Role == "agency_admin" {
		s.mu.RLock()
		user, exists := s.users[username]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot edit users outside your agency"}`))
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

	if err := s.UpdatePassword(username, req.Password); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	log.Printf("Password updated for user: %s", username)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]

	session := s.getSession(r)

	// Agency admins can only change roles within their agency, and cannot set super_admin
	if session != nil && session.Role == "agency_admin" {
		s.mu.RLock()
		user, exists := s.users[username]
		s.mu.RUnlock()
		if !exists || user.AgencyID != session.AgencyID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"cannot edit users outside your agency"}`))
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

	if err := s.UpdateUserRole(username, req.Role); err != nil {
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

	log.Printf("Authentication %s", map[bool]string{true: "enabled", false: "disabled"}[s.config.AuthEnabled])
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

	if req.NewPassword == "" || len(req.NewPassword) < 4 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"password must be at least 4 characters"}`))
		return
	}

	s.mu.Lock()
	admin, exists := s.webAdmins[username]
	if !exists {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"admin not found"}`))
		return
	}
	admin.PasswordHash = hashPassword(req.NewPassword)
	admin.MustChangePassword = false
	s.mu.Unlock()

	if err := s.saveWebAdmins(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to save"}`))
		return
	}

	log.Printf("Web admin password changed for: %s", username)
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
	if req.MaxChannels <= 0 {
		req.MaxChannels = 20
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

	if _, exists := s.webAdmins[req.Username]; exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"admin username already exists"}`))
		return
	}

	s.webAdmins[req.Username] = &WebAdmin{
		Username:           req.Username,
		PasswordHash:       hashPassword(req.Password),
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
		agencyAName := link.AgencyA
		agencyBName := link.AgencyB
		if a, ok := s.agencies[link.AgencyA]; ok {
			agencyAName = a.Name
		}
		if b, ok := s.agencies[link.AgencyB]; ok {
			agencyBName = b.Name
		}
		links = append(links, map[string]interface{}{
			"id":            link.ID,
			"agency_a":      link.AgencyA,
			"agency_b":      link.AgencyB,
			"agency_a_name": agencyAName,
			"agency_b_name": agencyBName,
			"channel":       link.Channel,
			"created_at":    link.CreatedAt,
			"created_by":    link.CreatedBy,
			"active":        link.Active,
		})
	}
	json.NewEncoder(w).Encode(links)
}

func (s *Server) handleCreateMutualAid(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		AgencyA string `json:"agency_a"`
		AgencyB string `json:"agency_b"`
		Channel int    `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if req.AgencyA == "" || req.AgencyB == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"both agencies are required"}`))
		return
	}

	if req.AgencyA == req.AgencyB {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"cannot link an agency to itself"}`))
		return
	}

	if req.Channel < 0 || req.Channel > 19 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"channel must be between 0 and 19"}`))
		return
	}

	session := s.getSession(r)

	s.mu.Lock()
	// Verify agencies exist
	if _, ok := s.agencies[req.AgencyA]; !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agency A not found"}`))
		return
	}
	if _, ok := s.agencies[req.AgencyB]; !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agency B not found"}`))
		return
	}

	// Check for existing active link between these agencies on this channel
	for _, link := range s.mutualAidLinks {
		if link.Active && link.Channel == req.Channel &&
			((link.AgencyA == req.AgencyA && link.AgencyB == req.AgencyB) ||
				(link.AgencyA == req.AgencyB && link.AgencyB == req.AgencyA)) {
			s.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"active link already exists between these agencies on this channel"}`))
			return
		}
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
		ID:        linkID,
		AgencyA:   req.AgencyA,
		AgencyB:   req.AgencyB,
		Channel:   req.Channel,
		CreatedAt: time.Now().Format(time.RFC3339),
		CreatedBy: createdBy,
		Active:    true,
	}
	s.mutualAidLinks[linkID] = link
	s.mu.Unlock()

	if err := s.saveMutualAid(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to save: %s"}`, err.Error())))
		return
	}

	log.Printf("Created mutual aid link: %s <-> %s on channel %d (by %s)", req.AgencyA, req.AgencyB, req.Channel, createdBy)
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

	delete(s.mutualAidLinks, id)
	s.mu.Unlock()

	s.saveMutualAid()

	log.Printf("Deleted mutual aid link: %s <-> %s on channel %d", link.AgencyA, link.AgencyB, link.Channel)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
	updateAvailable := latestVersion != currentVersion && Version != "dev"

	var binaryURL, webURL string
	for _, asset := range release.Assets {
		// Use API URL for private repos (requires token + Accept: application/octet-stream)
		// Use browser URL for public repos
		dlURL := asset.BrowserDownloadURL
		if s.config.GitHubToken != "" && asset.URL != "" {
			dlURL = asset.URL
		}
		if asset.Name == "ptt-server-linux" || asset.Name == "ptt-server" {
			binaryURL = dlURL
		}
		if asset.Name == "web.tar.gz" {
			webURL = dlURL
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"current_version":  Version,
		"latest_version":   release.TagName,
		"update_available": updateAvailable,
		"release_notes":    release.Body,
		"release_url":      release.HTMLURL,
		"binary_url":       binaryURL,
		"web_url":          webURL,
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
		BinaryURL string `json:"binary_url"`
		WebURL    string `json:"web_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BinaryURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "binary_url is required"})
		return
	}

	if !strings.HasPrefix(req.BinaryURL, "https://github.com/RyanR3/MeshRelayServer/") &&
		!strings.HasPrefix(req.BinaryURL, "https://api.github.com/repos/RyanR3/MeshRelayServer/") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid download URL"})
		return
	}

	go s.performUpdate(req.BinaryURL, req.WebURL, s.config.GitHubToken)

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

func (s *Server) performUpdate(binaryURL, webURL, ghToken string) {
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
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		outFile.Close()
		os.Remove(tmpBinary)
		s.setUpdateStatus("error", 20, "", "Failed to write binary: "+err.Error())
		return
	}
	outFile.Close()
	os.Chmod(tmpBinary, 0755)

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

	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip error: %w", err)
	}
	defer gzReader.Close()

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

		target := filepath.Join(destDir, name)

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			io.Copy(f, tarReader)
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
