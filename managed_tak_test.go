// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func managedTakFixture(t *testing.T) (*Server, *fakeTAK, map[string]interface{}) {
	t.Helper()
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir) // tak_connections.json is written to the working directory
	t.Cleanup(func() { os.Chdir(old) })
	s, _, _, _ := dispatchTestServer(t)
	s.config.Managed = "infratak"
	s.config.ManageToken = "tok-123"
	s.agencies = map[string]*Agency{"default": {ID: "default", Name: "Default"}}
	f := startFakeTAK(t)
	t.Cleanup(func() { s.stopTak("default") })
	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	enrollCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.enroll.Certificate().Raw}))
	body := map[string]interface{}{
		"host": "127.0.0.1", "stream_port": f.stream.Addr().(*net.TCPAddr).Port, "enroll_port": enrollPort,
		"username": "dispatch", "password": "secret", "trust_pems": []string{enrollCA},
	}
	return s, f, body
}

func managedCall(s *Server, method, token string, body interface{}, hdr map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, "/api/managed/tak", &buf)
	r.RemoteAddr = "127.0.0.1:41000" // the platform calls from the box itself
	if v := hdr["RemoteAddr"]; v != "" {
		r.RemoteAddr = v
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.handleManagedTak(w, r)
	return w
}

func TestManagedTakEnrollsAndConnects(t *testing.T) {
	s, f, body := managedTakFixture(t)
	w := managedCall(s, "POST", "tok-123", body, nil)
	if w.Code != 200 {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	select {
	case <-f.connCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("never connected: %v", s.takStatusJSON("default"))
	}
	st := s.takStatusJSON("default")
	if st["managed"] != true || st["enrolled"] != true || st["username"] != "dispatch" {
		t.Fatalf("status = %v", st)
	}
	// Saved for restarts, owner-only, never with the password.
	raw, err := os.ReadFile(takConnectionsFile)
	if err != nil || bytes.Contains(raw, []byte("secret")) || !bytes.Contains(raw, []byte(`"managed": true`)) {
		t.Fatalf("saved file wrong (err %v): %s", err, raw)
	}
	if fi, _ := os.Stat(takConnectionsFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// GET reports it.
	g := managedCall(s, "GET", "tok-123", nil, nil)
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"managed":true`) {
		t.Fatalf("GET = %d %s", g.Code, g.Body)
	}
	// Renewal: a managed link can be re-enrolled without force.
	if w := managedCall(s, "POST", "tok-123", body, nil); w.Code != 200 {
		t.Fatalf("re-enroll = %d %s", w.Code, w.Body)
	}
}

func TestManagedTakRefusals(t *testing.T) {
	s, _, body := managedTakFixture(t)
	cases := []struct {
		name   string
		token  string
		hdr    map[string]string
		mutate func(m map[string]interface{})
		want   int
	}{
		{"no token", "", nil, nil, 401},
		{"wrong token", "tok-124", nil, nil, 401},
		{"through the proxy", "tok-123", map[string]string{"X-Forwarded-For": "203.0.113.9"}, nil, 404},
		{"from the LAN", "tok-123", map[string]string{"RemoteAddr": "10.0.0.50:5555"}, nil, 404},
		{"no trust bundle", "tok-123", nil, func(m map[string]interface{}) { delete(m, "trust_pems") }, 400},
		{"bad host", "tok-123", nil, func(m map[string]interface{}) { m["host"] = "a b" }, 400},
		{"unknown agency", "tok-123", nil, func(m map[string]interface{}) { m["agency_id"] = "nope" }, 404},
		{"wrong password", "tok-123", nil, func(m map[string]interface{}) { m["password"] = "nope" }, 502},
		{"untrusted enrollment cert", "tok-123", nil, func(m map[string]interface{}) {
			m["trust_pems"] = []string{"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}
		}, 502},
	}
	for _, c := range cases {
		b := map[string]interface{}{}
		for k, v := range body {
			b[k] = v
		}
		if c.mutate != nil {
			c.mutate(b)
		}
		if w := managedCall(s, "POST", c.token, b, c.hdr); w.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.want, w.Body)
		}
	}
	if st := s.takStatusJSON("default"); st["configured"] == true {
		t.Fatalf("a refused call configured TAK: %v", st)
	}

	// Not managed, or no token configured: the endpoint does not exist.
	s.config.ManageToken = ""
	if w := managedCall(s, "POST", "", body, nil); w.Code != http.StatusNotFound {
		t.Fatalf("no token configured = %d", w.Code)
	}
	s.config.ManageToken, s.config.Managed = "tok-123", ""
	if w := managedCall(s, "POST", "tok-123", body, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unmanaged = %d", w.Code)
	}
}

func TestManagedTakKeepsOperatorConnection(t *testing.T) {
	s, _, body := managedTakFixture(t)
	m := s.tak()
	m.cfgs["default"] = TakConfig{AgencyID: "default", Host: "tak.example", Username: "op", CertPEM: "x"}
	if w := managedCall(s, "POST", "tok-123", body, nil); w.Code != http.StatusConflict {
		t.Fatalf("operator link overwritten: %d %s", w.Code, w.Body)
	}
	if m.cfgs["default"].Host != "tak.example" {
		t.Fatal("operator config changed")
	}
	body["force"] = true
	if w := managedCall(s, "POST", "tok-123", body, nil); w.Code != 200 {
		t.Fatalf("force = %d %s", w.Code, w.Body)
	}
}

func TestManageTokenFromEnv(t *testing.T) {
	t.Setenv("MESHRELAY_MANAGE_TOKEN", "abc")
	c := &Config{}
	applyEnvOverrides(c)
	if c.ManageToken != "abc" || os.Getenv("MESHRELAY_MANAGE_TOKEN") != "" {
		t.Fatalf("token %q env %q", c.ManageToken, os.Getenv("MESHRELAY_MANAGE_TOKEN"))
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), "abc") {
		t.Fatal("token would be saved to config.json")
	}
}
