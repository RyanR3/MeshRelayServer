package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	api.HandleFunc("/auth/status", s.handleAuthStatus).Methods("GET")
	api.HandleFunc("/auth/toggle", s.handleAuthToggle).Methods("POST")
	api.HandleFunc("/admin/change-password", s.handleAdminChangePassword).Methods("POST")

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

	// Create session
	token := s.generateSessionToken()
	s.mu.Lock()
	s.webSessions[token] = req.Username
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 7, // 7 days
	})

	log.Printf("Web login successful: %s from %s", req.Username, r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	mustChange := admin.MustChangePassword
	w.Write([]byte(fmt.Sprintf(`{"status":"ok","username":"%s","must_change_password":%t}`, req.Username, mustChange)))
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
	username := s.webSessions[cookie.Value]
	admin, exists := s.webAdmins[username]
	mustChange := exists && admin.MustChangePassword
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"status":"ok","username":"%s","must_change_password":%t}`, username, mustChange)))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.GetStats()
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	clients := s.GetClients()
	json.NewEncoder(w).Encode(clients)
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	channels := s.GetChannels()
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
	msg := WebSocketMessage{
		Type: "update",
		Data: map[string]interface{}{
			"stats":    s.GetStats(),
			"clients":  s.GetClients(),
			"channels": s.GetChannels(),
		},
	}

	return conn.WriteJSON(msg)
}

func (s *Server) handleGetUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	users := s.GetUsers()
	json.NewEncoder(w).Encode(users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
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

	if req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"username and password required"}`))
		return
	}

	if err := s.AddUser(req.Username, req.Password); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`{"status":"ok","username":"%s"}`, req.Username)))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]

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
	cookie, _ := r.Cookie("session")
	s.mu.RLock()
	username := s.webSessions[cookie.Value]
	s.mu.RUnlock()

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
