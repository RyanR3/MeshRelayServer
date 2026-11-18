// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that returns whatever ID token the test prepared for the code.
type fakeIdP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	clientID string
	secret   string
	claims   map[string]interface{} // ID token claims for the next code
	sawPKCE  string
	tokenErr bool
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: k, kid: "k1", clientID: "meshrelay", secret: "s3cret"}
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL + "/application/o/meshrelay/"
	mux.HandleFunc("/application/o/meshrelay/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.issuer,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"jwks_uri":               f.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, sec, _ := r.BasicAuth()
		r.ParseForm()
		f.sawPKCE = r.PostForm.Get("code_verifier")
		if f.tokenErr || id != f.clientID || sec != f.secret || r.PostForm.Get("code") != "good-code" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": f.sign(f.claims, "RS256", f.key)})
	})
	return f
}

func (f *fakeIdP) sign(claims map[string]interface{}, alg string, key *rsa.PrivateKey) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": f.kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	d := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	return in + "." + b64(sig)
}

func (f *fakeIdP) baseClaims(nonce string, groups ...string) map[string]interface{} {
	now := time.Now().Unix()
	return map[string]interface{}{"iss": f.issuer, "aud": f.clientID, "sub": "uuid-1", "exp": now + 300,
		"iat": now, "nonce": nonce, "preferred_username": "jdoe", "groups": groups}
}

func (f *fakeIdP) provider() *oidcProvider {
	p := newOIDCProvider(&OIDCConfig{Issuer: f.issuer, ClientID: f.clientID, ClientSecret: f.secret,
		RedirectURL: "https://meshrelay.example.org/api/oidc/callback", GroupPrefix: "meshrelay", Label: "Sign in with infra-TAK"})
	p.client = f.srv.Client()
	return p
}

func TestOIDCRoleFromGroups(t *testing.T) {
	exists := func(a string) bool { return a == "fire" || a == "ems" || a == "sar-team" }
	cases := []struct {
		groups       []string
		role, agency string
	}{
		{[]string{"authentik Admins"}, "", ""},
		{[]string{"meshrelay-fire-dispatch"}, "dispatch", "fire"},
		{[]string{"meshrelay-fire-dispatch", "meshrelay-ems-admins"}, "agency_admin", "ems"},
		{[]string{"MeshRelay-Super-Admins", "meshrelay-fire-admins"}, "super_admin", ""},
		{[]string{"meshrelay-sar-team-dispatch"}, "dispatch", "sar-team"},
		{[]string{"meshrelay-police-admins"}, "", ""}, // agency does not exist
		{[]string{"meshrelay--admins", "meshrelay-admins"}, "", ""},
		{[]string{"other-fire-admins"}, "", ""},
	}
	for _, c := range cases {
		r, a := oidcRole("meshrelay", c.groups, exists)
		if r != c.role || a != c.agency {
			t.Errorf("%v → %q/%q, want %q/%q", c.groups, r, a, c.role, c.agency)
		}
	}
}

func TestOIDCVerifyIDToken(t *testing.T) {
	f := newFakeIdP(t)
	p := f.provider()
	good := f.baseClaims("n1", "x")
	if c, err := p.verifyIDToken(f.sign(good, "RS256", f.key), "n1"); err != nil || c.PreferredUsername != "jdoe" {
		t.Fatalf("good token: %v", err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	mut := func(k string, v interface{}) map[string]interface{} {
		c := f.baseClaims("n1")
		c[k] = v
		return c
	}
	bad := map[string]string{
		"wrong key":    f.sign(good, "RS256", other),
		"alg none":     strings.Join(strings.Split(f.sign(good, "none", f.key), ".")[:2], ".") + ".",
		"alg HS256":    f.sign(good, "HS256", f.key),
		"wrong issuer": f.sign(mut("iss", "https://evil/"), "RS256", f.key),
		"wrong aud":    f.sign(mut("aud", "someone-else"), "RS256", f.key),
		"multi aud":    f.sign(mut("aud", []string{"meshrelay", "other"}), "RS256", f.key),
		"expired":      f.sign(mut("exp", time.Now().Add(-10*time.Minute).Unix()), "RS256", f.key),
		"future iat":   f.sign(mut("iat", time.Now().Add(10*time.Minute).Unix()), "RS256", f.key),
		"no subject":   f.sign(mut("sub", ""), "RS256", f.key),
		"wrong nonce":  f.sign(mut("nonce", "n2"), "RS256", f.key),
		"garbage":      "a.b.c",
		"tampered body": func() string {
			s := strings.Split(f.sign(good, "RS256", f.key), ".")
			s[1] = b64([]byte(`{"iss":"x"}`))
			return strings.Join(s, ".")
		}(),
	}
	for name, tok := range bad {
		if _, err := p.verifyIDToken(tok, "n1"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// azp makes a multi-audience token acceptable.
	ok := mut("aud", []string{"meshrelay", "other"})
	ok["azp"] = "meshrelay"
	if _, err := p.verifyIDToken(f.sign(ok, "RS256", f.key), "n1"); err != nil {
		t.Fatalf("multi aud with azp: %v", err)
	}
}

// ssoLogin runs /api/oidc/login then /api/oidc/callback with the claims the
// IdP will put in the ID token; returns the callback response.
func ssoLogin(t *testing.T, s *Server, f *fakeIdP, groups ...string) *httptest.ResponseRecorder {
	t.Helper()
	h := s.webHandler()
	rec := doReq(h, "GET", "/api/oidc/login?next=/command", "", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "meshrelay" || q.Get("nonce") == "" {
		t.Fatalf("authorize URL %s", loc)
	}
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "oidc_state" {
			stateCookie = c
		}
	}
	if stateCookie == nil || !stateCookie.HttpOnly || !stateCookie.Secure {
		t.Fatal("no secure state cookie")
	}
	f.claims = f.baseClaims(q.Get("nonce"), groups...)
	req := httptest.NewRequest("GET", "/api/oidc/callback?code=good-code&state="+url.QueryEscape(q.Get("state")), nil)
	req.AddCookie(stateCookie)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	// PKCE: the verifier sent to the token endpoint matches the challenge.
	if out.Code == 200 {
		sum := sha256.Sum256([]byte(f.sawPKCE))
		if b64(sum[:]) != q.Get("code_challenge") {
			t.Fatal("PKCE verifier does not match the challenge")
		}
	}
	return out
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" && c.MaxAge > 0 {
			return c.Value
		}
	}
	return ""
}

func TestOIDCSignInFlow(t *testing.T) {
	f := newFakeIdP(t)
	s := newWebTestServer(t)
	s.oidc = f.provider()
	h := s.webHandler()

	// Config is public and says SSO is on.
	if rec := doReq(h, "GET", "/api/oidc/config", "", ""); !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("config = %s", rec.Body)
	}

	// Dispatch for fire: signed in, lands in Command, account created as SSO.
	rec := ssoLogin(t, s, f, "meshrelay-fire-dispatch")
	tok := sessionCookie(rec)
	if rec.Code != 200 || tok == "" || !strings.Contains(rec.Body.String(), "/command") {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	sess := s.sessionFor(tok)
	if sess == nil || sess.Role != "dispatch" || sess.AgencyID != "fire" {
		t.Fatalf("session = %+v", sess)
	}
	acct := s.webAdmins[userKey("fire", "jdoe")]
	if acct == nil || !acct.SSO || acct.Subject != "uuid-1" || checkPassword("", acct.PasswordHash) {
		t.Fatalf("account = %+v", acct)
	}
	// The SSO account cannot be used with a password, even one an admin set.
	acct.PasswordHash, _ = hashPasswordSecure("set-by-admin")
	if r := doReq(h, "POST", "/api/login", "", `{"username":"jdoe","password":"set-by-admin","agency_id":"fire"}`); r.Code == 200 {
		t.Fatal("password login into an SSO account")
	}

	// Promoted to agency admin of ems: old session ends, account moves agency.
	rec = ssoLogin(t, s, f, "meshrelay-ems-admins")
	if s.sessionFor(tok) != nil {
		t.Fatal("session survived a role change")
	}
	if s.webAdmins[userKey("fire", "jdoe")] != nil || s.webAdmins[userKey("ems", "jdoe")] == nil {
		t.Fatal("SSO account not moved to the new agency")
	}
	if sess := s.sessionFor(sessionCookie(rec)); sess == nil || sess.Role != "agency_admin" || sess.AgencyID != "ems" {
		t.Fatalf("new session = %+v", sess)
	}

	// No MeshRelay group: refused, no session.
	rec = ssoLogin(t, s, f, "authentik Admins")
	if sessionCookie(rec) != "" || !strings.Contains(rec.Header().Get("Location"), "sso_error") {
		t.Fatalf("no-group user signed in: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOIDCDoesNotTakeOverPasswordAccounts(t *testing.T) {
	f := newFakeIdP(t)
	s := newWebTestServer(t)
	s.oidc = f.provider()
	hash, _ := hashPasswordSecure("pw")
	s.webAdmins[userKey("fire", "jdoe")] = &WebAdmin{Username: "jdoe", AgencyID: "fire", Role: "dispatch", PasswordHash: hash}
	rec := ssoLogin(t, s, f, "meshrelay-fire-admins")
	if sessionCookie(rec) != "" {
		t.Fatal("SSO took over a password account")
	}
	if a := s.webAdmins[userKey("fire", "jdoe")]; a.SSO || a.Role != "dispatch" || a.PasswordHash != hash {
		t.Fatalf("password account changed: %+v", a)
	}
}

func TestOIDCCallbackRejectsForgedState(t *testing.T) {
	f := newFakeIdP(t)
	s := newWebTestServer(t)
	s.oidc = f.provider()
	h := s.webHandler()
	rec := doReq(h, "GET", "/api/oidc/login", "", "")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	f.claims = f.baseClaims(loc.Query().Get("nonce"), "meshrelay-super-admins")

	// Attacker's browser: right state in the URL, but no matching cookie.
	req := httptest.NewRequest("GET", "/api/oidc/callback?code=good-code&state="+state, nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if sessionCookie(out) != "" {
		t.Fatal("callback without the state cookie signed in")
	}
	// Replaying a used state fails too.
	req = httptest.NewRequest("GET", "/api/oidc/callback?code=good-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: "oidc_state", Value: state})
	out = httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if sessionCookie(out) == "" {
		t.Fatal("first use of the state should succeed")
	}
	out = httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if sessionCookie(out) != "" {
		t.Fatal("state replay signed in")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/command": "/command", "": "/", "//evil.com": "/",
		"https://evil.com": "/", "/\\evil": "/", "/ok\r\nX: y": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func TestOIDCDisabledByDefault(t *testing.T) {
	s := newWebTestServer(t)
	h := s.webHandler()
	if rec := doReq(h, "GET", "/api/oidc/config", "", ""); strings.TrimSpace(rec.Body.String()) != `{"enabled":false}` {
		t.Fatalf("config = %s", rec.Body)
	}
	if rec := doReq(h, "GET", "/api/oidc/login", "", ""); rec.Code != 404 {
		t.Fatalf("login without SSO = %d", rec.Code)
	}
}
