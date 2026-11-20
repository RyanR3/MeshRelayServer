// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEnvOverrides(t *testing.T) {
	t.Setenv("MESHRELAY_WEB_PORT", "8770")
	t.Setenv("MESHRELAY_PTT_PORT", "nope")
	t.Setenv("MESHRELAY_WEB_BIND", " 127.0.0.1 ")
	t.Setenv("MESHRELAY_TRUST_PROXY", "true")
	t.Setenv("MESHRELAY_TRUSTED_PROXIES", "172.31.250.1, 10.0.0.0/8")
	t.Setenv("MESHRELAY_MANAGED", "infratak")
	t.Setenv("MESHRELAY_ADMIN_PASSWORD", "s3cret-initial")
	c := DefaultConfig()
	applyEnvOverrides(c)
	if c.WebPort != 8770 || c.PTTPort != 5000 || c.WebBind != "127.0.0.1" || !c.TrustProxy ||
		!c.isManaged() || c.AdminPassword != "s3cret-initial" || len(c.TrustedProxies) != 2 {
		t.Fatalf("config after env = %+v", c)
	}
	if _, still := os.LookupEnv("MESHRELAY_ADMIN_PASSWORD"); still {
		t.Fatal("admin password left in the environment")
	}
}

func proxyReq(remote, xff, proto string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if proto != "" {
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	return r
}

func TestForwardedHeadersOnlyFromTrustedProxy(t *testing.T) {
	s := &Server{config: &Config{TrustProxy: true, TrustedProxies: []string{"172.31.250.1"}}}

	// Through the proxy: the last X-Forwarded-For entry (what Caddy saw).
	r := proxyReq("172.31.250.1:40000", "6.6.6.6, 203.0.113.9", "https")
	if ip := s.clientIP(r); ip != "203.0.113.9" {
		t.Fatalf("clientIP via proxy = %s", ip)
	}
	if !s.requestIsHTTPS(r) {
		t.Fatal("X-Forwarded-Proto https from the proxy not honoured")
	}

	// Straight to the port from anywhere else: headers ignored.
	r = proxyReq("198.51.100.7:5555", "1.2.3.4", "https")
	if ip := s.clientIP(r); ip != "198.51.100.7" || s.requestIsHTTPS(r) {
		t.Fatalf("spoofed headers honoured: ip=%s https=%v", ip, s.requestIsHTTPS(r))
	}

	// Proxy mode off: never honoured, even from loopback.
	s2 := &Server{config: &Config{}}
	r = proxyReq("127.0.0.1:1", "1.2.3.4", "https")
	if s2.clientIP(r) != "127.0.0.1" || s2.requestIsHTTPS(r) {
		t.Fatal("headers honoured with trust_proxy off")
	}

	// Default trusted list is loopback.
	s3 := &Server{config: &Config{TrustProxy: true}}
	if s3.clientIP(proxyReq("127.0.0.1:1", "9.9.9.9", "")) != "9.9.9.9" ||
		s3.clientIP(proxyReq("10.1.1.1:1", "9.9.9.9", "")) != "10.1.1.1" {
		t.Fatal("default trusted proxy list is not loopback-only")
	}
}

func TestHealthIsPublicAndSaysNothing(t *testing.T) {
	s := newWebTestServer(t)
	rec := doReq(s.webHandler(), "GET", "/health", "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("/health = %d %s", rec.Code, rec.Body.String())
	}
}

func TestManagedServerRefusesSelfUpdate(t *testing.T) {
	s := newWebTestServer(t)
	s.config.Managed = "infratak"
	h := s.webHandler()
	rec := doReq(h, "POST", "/api/update/apply", "super",
		`{"binary_url":"https://github.com/RyanR3/MeshRelayServer/releases/download/v1/ptt-server-linux"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "infra-TAK") {
		t.Fatalf("apply on a managed server = %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(h, "POST", "/api/update/check", "super", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"managed":"infratak"`) ||
		!strings.Contains(rec.Body.String(), `"update_available":false`) {
		t.Fatalf("check on a managed server = %d %s", rec.Code, rec.Body.String())
	}
	if s.updateStatus.InProgress {
		t.Fatal("update started")
	}
}

func TestDashboardLoginLockout(t *testing.T) {
	s := newWebTestServer(t)
	s.loginLimit = newSignInGuard(3, 6, time.Minute, time.Minute)
	hash, _ := hashPasswordSecure("right-password")
	s.webAdmins[userKey("fire", "chief2")] = &WebAdmin{Username: "chief2", AgencyID: "fire", Role: "agency_admin", PasswordHash: hash}
	h := s.webHandler()
	from := func(ip, user, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+user+`","password":"`+pw+`","agency_id":"fire"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// The chief signs in from the station: that address is now known for the account.
	if rec := from("198.51.100.7", "chief2", "right-password"); rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	// Unknown user and wrong password look the same.
	a, b := from("203.0.113.1", "nobody", "x"), from("203.0.113.1", "chief2", "x")
	if a.Code != 401 || b.Code != 401 || a.Body.String() != b.Body.String() {
		t.Fatalf("unknown %d %q vs wrong %d %q", a.Code, a.Body, b.Code, b.Body)
	}
	// A third wrong password locks THAT ADDRESS...
	from("203.0.113.1", "chief2", "y")
	if rec := from("203.0.113.1", "chief2", "right-password"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("locked address accepted: %d %s", rec.Code, rec.Body)
	}
	// ...and nobody else: the account is not locked out for its real user.
	if rec := from("198.51.100.7", "chief2", "right-password"); rec.Code != 200 {
		t.Fatalf("a guesser locked the real user out: %d %s", rec.Code, rec.Body)
	}
	// A guess from many addresses puts the account under guard: strangers
	// are refused even with the right password, the known address is not.
	for i := 2; i < 8; i++ {
		from("203.0.113."+strconv.Itoa(i), "chief2", "bad")
	}
	if rec := from("203.0.113.99", "chief2", "right-password"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("guarded account accepted a new address: %d", rec.Code)
	}
	if rec := from("198.51.100.7", "chief2", "right-password"); rec.Code != 200 {
		t.Fatalf("guarded account refused its known address: %d %s", rec.Code, rec.Body)
	}
	// An oversized body is refused before any password check.
	big := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"a","password":"`+strings.Repeat("x", 40000)+`","agency_id":"fire"}`))
	big.RemoteAddr = "192.0.2.50:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, big)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized login body = %d", rec.Code)
	}
}

func TestPhoneAuthLockoutAndUniformErrors(t *testing.T) {
	s, sink := newSecTestServer(t)
	s.pttAuthLimit = newSignInGuard(3, 6, time.Minute, time.Minute)
	addUser(s, "a1", "medic", "pw")
	station := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 41000}
	bad := func(i int) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(203, 0, 113, byte(i)), Port: 41000} }

	reply := func(a *net.UDPAddr) string {
		for _, p := range sink.take(a) {
			if p[0] == PacketTypeAuthResponse {
				return string(p)
			}
		}
		return ""
	}
	if login(t, s, sink, bad(1), "ghost", "a1", "pw") {
		t.Fatal("unknown user signed in")
	}
	if !login(t, s, sink, station, "medic", "a1", "pw") {
		t.Fatal("right password refused")
	}

	// Same reply text for an unknown user and a wrong password.
	s.handleAuthRequest(&AuthRequestPacket{Username: "ghost", PasswordHash: "00", AgencyID: "a1"}, bad(2))
	unknown := reply(bad(2))
	s.handleAuthRequest(&AuthRequestPacket{Username: "medic", PasswordHash: "00", AgencyID: "a1"}, bad(3))
	wrong := reply(bad(3))
	if unknown == "" || unknown != wrong {
		t.Fatalf("unknown user %q vs wrong password %q", unknown, wrong)
	}

	// Three wrong passwords lock the guesser's ADDRESS (it gets no more
	// challenges), not the account: the station still signs in.
	g := bad(10)
	for i := 0; i < 3; i++ {
		s.handleAuthRequest(&AuthRequestPacket{Username: "medic", PasswordHash: "00", AgencyID: "a1"}, g)
	}
	sink.take(g)
	s.handleAuthChallengeRequest(&AuthChallengeRequestPacket{Username: "medic", AgencyID: "a1"}, g)
	if len(sink.take(g)) != 0 {
		t.Fatal("a locked address was still given a challenge")
	}
	if !login(t, s, sink, station, "medic", "a1", "pw") {
		t.Fatal("a guesser locked the real unit out")
	}

	// A junk agency name does not give a fresh counter: the failures count
	// against the account the request resolves to.
	for i := 20; i < 26; i++ {
		s.handleAuthRequest(&AuthRequestPacket{Username: "medic", PasswordHash: "00", AgencyID: "junk" + strconv.Itoa(i)}, bad(i))
	}
	// Now guarded: a new address is refused even with the right password,
	// the station (known for this account) is not.
	if login(t, s, sink, bad(40), "medic", "a1", "pw") {
		t.Fatal("guarded account accepted a new address")
	}
	if !login(t, s, sink, station, "medic", "a1", "pw") {
		t.Fatal("guarded account refused its known address")
	}
	now := time.Now().Add(2 * time.Minute)
	s.pttAuthLimit.acct.now = func() time.Time { return now }
	s.pttAuthLimit.ips.now = func() time.Time { return now }
	if !login(t, s, sink, bad(40), "medic", "a1", "pw") {
		t.Fatal("guard did not expire")
	}
}

// Challenges are stateless: a flood stores nothing, and a nonce only works
// for the address it was made for, for a short time.
func TestChallengesAreStatelessAndBound(t *testing.T) {
	now := time.Now()
	a, b := "198.51.100.7:5000", "198.51.100.7:5001"
	n := challengeNonce(a, now)
	if n == challengeNonce(b, now) {
		t.Fatal("two addresses got the same nonce")
	}
	ok := func(addr string, at time.Time) bool {
		for _, c := range challengeNonces(addr, at) {
			if c == n {
				return true
			}
		}
		return false
	}
	if !ok(a, now) || !ok(a, now.Add(challengeSlot)) {
		t.Fatal("a fresh nonce was not accepted")
	}
	if ok(a, now.Add(2*challengeSlot+time.Second)) {
		t.Fatal("a stale nonce was accepted")
	}
	if ok(b, now) {
		t.Fatal("a nonce was accepted from another address")
	}
	if guardAddr("2001:db8:1:2:aaaa::1") != guardAddr("2001:db8:1:2:bbbb::9") || guardAddr("2001:db8:1:3::1") == guardAddr("2001:db8:1:2::1") {
		t.Fatal("IPv6 addresses are not grouped by /64")
	}
}

func TestFailLimiterBoundedMemory(t *testing.T) {
	l := newFailLimiter(5, 3, time.Minute, time.Minute)
	l.maxKeys = 100
	for i := 0; i < 1000; i++ {
		l.fail("ip:" + string(rune('a'+i%26)) + strings.Repeat("x", i/26))
	}
	if l.size() > 100 {
		t.Fatalf("limiter holds %d keys", l.size())
	}
}

func TestBundledTranscribeHelpers(t *testing.T) {
	want := helperAssets()
	if want == nil {
		t.Skip("no helper build for this platform")
	}
	bundle, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(bundle, want[0]), []byte("#!/bin/sh\n"), 0o755)
	tr := &Transcriber{s: &Server{config: &Config{TranscribeHelperDir: bundle}}, dir: dir}
	got, err := tr.bundledHelpers(bundle)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("bundled = %v, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, want[0]+".new")); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("copied helper missing or not executable: %v", err)
	}
	if _, err := tr.bundledHelpers(t.TempDir()); err == nil {
		t.Fatal("empty bundle accepted")
	}
}

func TestInitialAdminPasswordFromInstaller(t *testing.T) {
	s := newWebTestServer(t)
	s.webAdmins = map[string]*WebAdmin{}
	s.config.AdminPassword = "from-infratak"
	s.createDefaultAdmin()
	a := s.webAdmins[userKey("", "admin")]
	if a == nil || a.Role != "super_admin" || !a.MustChangePassword || !checkPassword("from-infratak", a.PasswordHash) {
		t.Fatalf("admin = %+v", a)
	}
}

func TestDefaultGatewayFromProcRoute(t *testing.T) {
	f := filepath.Join(t.TempDir(), "route")
	os.WriteFile(f, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t0012A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+
		"eth0\t00000000\t0112A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"), 0o644)
	if gw := defaultGateway(f); gw == nil || gw.String() != "192.168.18.1" {
		t.Fatalf("gateway = %v", gw)
	}
	if defaultGateway(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("gateway from a missing file")
	}
}
