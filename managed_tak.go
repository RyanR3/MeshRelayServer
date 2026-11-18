// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Platform-provisioned TAK Server link (infra-TAK).
//
// On an infra-TAK box the TAK Server sits next to MeshRelay, so the platform
// sets up the link instead of an operator typing a TAK username and password
// into the dashboard:
//
//  1. infra-TAK creates a TAK service account in Authentik and gives it a
//     one-time random password.
//  2. It POSTs that password here, over loopback, with MESHRELAY_MANAGE_TOKEN.
//  3. MeshRelay enrolls with the TAK Server itself (the private key never
//     leaves this process), verifying the enrollment port against the TAK CA
//     bundle infra-TAK sends, then connects.
//  4. infra-TAK replaces the account password with one it throws away; the
//     client certificate is the only credential left.
//
// The endpoint answers 404 unless the server runs managed with a token, and
// refuses anything that came through the reverse proxy.

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var managedTakUserRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var managedTakHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

// managedCaller: managed mode, a configured token that matches, and a direct
// (not proxied) request. Writes the refusal itself and returns false.
func (s *Server) managedCaller(w http.ResponseWriter, r *http.Request) bool {
	if s.config == nil || !s.config.isManaged() || s.config.ManageToken == "" {
		http.NotFound(w, r)
		return false
	}
	// Caddy always adds X-Forwarded-For; the platform calls 127.0.0.1 directly.
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Forwarded-Host") != "" {
		http.NotFound(w, r)
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.config.ManageToken)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		writeJSONError(w, http.StatusUnauthorized, "bad manage token")
		return false
	}
	return true
}

// handleManagedTak: GET ?agency_id= reports the link; POST enrolls it.
func (s *Server) handleManagedTak(w http.ResponseWriter, r *http.Request) {
	if !s.managedCaller(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		agency := nonEmpty(r.URL.Query().Get("agency_id"), "default")
		json.NewEncoder(w).Encode(s.takStatusJSON(agency))
		return
	}
	var req struct {
		AgencyID   string   `json:"agency_id"`
		Host       string   `json:"host"`
		StreamPort int      `json:"stream_port"`
		EnrollPort int      `json:"enroll_port"`
		APIPort    int      `json:"api_port"`
		Username   string   `json:"username"`
		Password   string   `json:"password"`
		Callsign   string   `json:"callsign"`
		TrustPEMs  []string `json:"trust_pems"`
		Force      bool     `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	agencyID := nonEmpty(strings.TrimSpace(req.AgencyID), "default")
	host := strings.TrimSpace(req.Host)
	switch {
	case !managedTakHostRe.MatchString(host):
		writeJSONError(w, http.StatusBadRequest, "bad host")
		return
	case !managedTakUserRe.MatchString(req.Username) || req.Password == "":
		writeJSONError(w, http.StatusBadRequest, "username and password are required")
		return
	case len(req.TrustPEMs) == 0:
		// The managed path never enrolls blind.
		writeJSONError(w, http.StatusBadRequest, "trust_pems (the TAK CA bundle) is required")
		return
	}
	for _, port := range []int{req.StreamPort, req.EnrollPort, req.APIPort} {
		if port < 0 || port > 65535 {
			writeJSONError(w, http.StatusBadRequest, "bad port")
			return
		}
	}
	s.mu.RLock()
	_, known := s.agencies[agencyID]
	s.mu.RUnlock()
	if !known {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}

	m := s.tak()
	m.mu.Lock()
	cfg, existed := m.cfgs[agencyID]
	m.mu.Unlock()
	// An operator's own TAK connection is never replaced unasked.
	if existed && !cfg.Managed && cfg.CertPEM != "" && !req.Force {
		writeJSONError(w, http.StatusConflict, "this agency already has a TAK connection set up in the dashboard")
		return
	}

	next := cfg
	next.AgencyID, next.Host, next.Username = agencyID, host, req.Username
	next.StreamPort = positiveOr(req.StreamPort, 8089)
	next.EnrollPort = positiveOr(req.EnrollPort, 8446)
	next.APIPort = positiveOr(req.APIPort, 8443)
	next.Callsign = nonEmpty(strings.TrimSpace(req.Callsign), nonEmpty(cfg.Callsign, "MeshRelay Dispatch"))
	if next.UID == "" {
		next.UID = "MeshRelay-Dispatch-" + safeName.ReplaceAllString(agencyID, "_") + "-" + randomHex(3)
	}
	if err := takEnrollVerified(&next, req.Password, req.TrustPEMs); err != nil {
		log.Printf("TAK [%s]: managed enrollment failed: %v", agencyID, err)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	next.Enabled, next.Managed = true, true
	m.mu.Lock()
	m.cfgs[agencyID] = next
	m.mu.Unlock()
	if err := s.saveTakConnections(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("TAK [%s]: enrolled by the managing platform with %s as %s", agencyID, host, req.Username)
	s.startTak(next)
	time.Sleep(1500 * time.Millisecond) // let the first connect attempt report
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

func positiveOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// enrollTLSConfig verifies the enrollment port's chain against the given CA
// PEMs or the system roots. The host name is not checked: TAK is reached via
// the Docker host gateway, and its certificate names the TAK server instead.
func enrollTLSConfig(trustPEMs []string) *tls.Config {
	pool := x509.NewCertPool()
	for _, p := range trustPEMs {
		pool.AppendCertsFromPEM([]byte(p))
	}
	sys, _ := x509.SystemCertPool()
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("TAK enrollment port sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, b := range raw[1:] {
				if c, err := x509.ParseCertificate(b); err == nil {
					inter.AddCert(c)
				}
			}
			opts := x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
			if _, err = leaf.Verify(opts); err == nil {
				return nil
			}
			if sys != nil {
				opts.Roots = sys
				if _, err2 := leaf.Verify(opts); err2 == nil {
					return nil
				}
			}
			return errors.New("TAK enrollment port certificate is not signed by the TAK CA: " + err.Error())
		},
	}
}
