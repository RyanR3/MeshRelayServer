// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Single sign-on for the dashboard and the Command console through an OpenID
// Connect provider (infra-TAK's Authentik, or any other).
//
// The server is a confidential OIDC client using the authorization code flow
// with PKCE. It validates the ID token itself: RS256 signature against the
// provider's published keys, issuer, audience, expiry and nonce. Nothing the
// browser sends about who the user is gets trusted.
//
// Who may sign in, and as what, comes from the provider's groups claim
// (prefix "meshrelay" by default):
//
//	meshrelay-super-admins        super admin (every agency)
//	meshrelay-<agency>-admins     agency admin of <agency>
//	meshrelay-<agency>-dispatch   dispatch (Command console) for <agency>
//
// The highest role wins; an agency must already exist in MeshRelay. A user in
// none of these groups is refused. On each sign-in the account's role and
// agency follow the groups, so removing someone from a group in the provider
// takes effect at their next sign-in, and their open sessions end at once
// (a session is tied to the role it was created with).
//
// SSO accounts are separate from password accounts: they have no usable
// password, and a sign-in never takes over a password account of the same
// name. Password sign-in keeps working alongside (break-glass for when the
// provider is down).
//
// Environment:
//
//	MESHRELAY_OIDC_ISSUER         e.g. https://tak.example.org/application/o/meshrelay/
//	MESHRELAY_OIDC_CLIENT_ID
//	MESHRELAY_OIDC_CLIENT_SECRET
//	MESHRELAY_OIDC_REDIRECT_URL   https://meshrelay.example.org/api/oidc/callback
//	MESHRELAY_OIDC_GROUP_PREFIX   default "meshrelay"
//	MESHRELAY_OIDC_LABEL          login button text, default "Sign in with SSO"

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	GroupPrefix  string
	Label        string
}

func (c *OIDCConfig) enabled() bool {
	return c != nil && c.Issuer != "" && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

func oidcConfigFromEnv() *OIDCConfig {
	c := &OIDCConfig{
		Issuer:       strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_ISSUER")),
		ClientID:     strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_CLIENT_SECRET")),
		RedirectURL:  strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_REDIRECT_URL")),
		GroupPrefix:  strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_GROUP_PREFIX")),
		Label:        strings.TrimSpace(os.Getenv("MESHRELAY_OIDC_LABEL")),
	}
	os.Unsetenv("MESHRELAY_OIDC_CLIENT_SECRET")
	if c.GroupPrefix == "" {
		c.GroupPrefix = "meshrelay"
	}
	if c.Label == "" {
		c.Label = "Sign in with SSO"
	}
	if !c.enabled() {
		if c.Issuer != "" || c.ClientID != "" {
			log.Printf("SSO: incomplete OIDC settings (issuer, client id, client secret and redirect URL are all needed) — SSO off")
		}
		return nil
	}
	if !strings.HasPrefix(c.Issuer, "https://") || !strings.HasPrefix(c.RedirectURL, "https://") {
		log.Printf("SSO: issuer and redirect URL must be https:// — SSO off")
		return nil
	}
	return c
}

// --- provider metadata and keys -------------------------------------------------

type oidcProvider struct {
	cfg    *OIDCConfig
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	meta      *oidcMeta
	metaAt    time.Time
	keys      map[string]*rsa.PublicKey
	keysAt    time.Time
	refetchAt time.Time

	pending map[string]oidcPending // state -> login in progress
}

type oidcMeta struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
	EndSession    string `json:"end_session_endpoint"`
}

type oidcPending struct {
	nonce, verifier, next string
	at                    time.Time
}

const (
	oidcPendingTTL = 10 * time.Minute
	oidcPendingMax = 5000
	oidcCacheTTL   = time.Hour
	oidcClockSkew  = 2 * time.Minute
)

func newOIDCProvider(cfg *OIDCConfig) *oidcProvider {
	return &oidcProvider{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now,
		pending: map[string]oidcPending{}}
}

func (p *oidcProvider) getJSON(u string, out interface{}) error {
	resp, err := p.client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (p *oidcProvider) metadata() (*oidcMeta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil && p.now().Sub(p.metaAt) < oidcCacheTTL {
		return p.meta, nil
	}
	var m oidcMeta
	u := strings.TrimRight(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := p.getJSON(u, &m); err != nil {
		if p.meta != nil {
			return p.meta, nil // keep working on the cached copy
		}
		return nil, err
	}
	if strings.TrimRight(m.Issuer, "/") != strings.TrimRight(p.cfg.Issuer, "/") {
		return nil, fmt.Errorf("provider says its issuer is %q, configured %q", m.Issuer, p.cfg.Issuer)
	}
	for _, e := range []string{m.AuthEndpoint, m.TokenEndpoint, m.JWKSURI} {
		if !strings.HasPrefix(e, "https://") {
			return nil, fmt.Errorf("provider endpoint %q is not https", e)
		}
	}
	p.meta, p.metaAt = &m, p.now()
	return p.meta, nil
}

// key returns the RSA key with this id, refetching the key set once when an
// unknown id shows up (the provider rotated its keys), at most every 30 s.
func (p *oidcProvider) key(kid string) (*rsa.PublicKey, error) {
	m, err := p.metadata()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	k, fresh := p.keys[kid], p.now().Sub(p.keysAt) < oidcCacheTTL
	canRefetch := p.now().After(p.refetchAt)
	p.mu.Unlock()
	if k != nil && fresh {
		return k, nil
	}
	if k == nil && !canRefetch && fresh {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, Use, Alg, N, E string
		} `json:"keys"`
	}
	if err := p.getJSON(m.JWKSURI, &set); err != nil {
		if k != nil {
			return k, nil
		}
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" || (jk.Use != "" && jk.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(jk.N)
		e, err2 := base64.RawURLEncoding.DecodeString(jk.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		ev := 0
		for _, b := range e {
			ev = ev<<8 | int(b)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ev}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[jk.Kid] = pub
	}
	p.mu.Lock()
	p.keys, p.keysAt, p.refetchAt = keys, p.now(), p.now().Add(30*time.Second)
	k = keys[kid]
	p.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

// --- ID token ------------------------------------------------------------------

type idClaims struct {
	Iss               string      `json:"iss"`
	Sub               string      `json:"sub"`
	Aud               interface{} `json:"aud"`
	Azp               string      `json:"azp"`
	Exp               float64     `json:"exp"`
	Iat               float64     `json:"iat"`
	Nbf               float64     `json:"nbf"`
	Nonce             string      `json:"nonce"`
	PreferredUsername string      `json:"preferred_username"`
	Email             string      `json:"email"`
	Name              string      `json:"name"`
	Groups            []string    `json:"groups"`
}

func (c *idClaims) audiences() []string {
	switch v := c.Aud.(type) {
	case string:
		return []string{v}
	case []interface{}:
		var out []string
		for _, a := range v {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// verifyIDToken checks signature, issuer, audience, time and nonce.
func (p *oidcProvider) verifyIDToken(raw, nonce string) (*idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed ID token")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed ID token header")
	}
	var hdr struct{ Alg, Kid, Typ string }
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, errors.New("malformed ID token header")
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("ID token algorithm %q not accepted (RS256 only)", hdr.Alg)
	}
	key, err := p.key(hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("malformed ID token signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("ID token signature is not valid")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed ID token payload")
	}
	var c idClaims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, errors.New("malformed ID token payload")
	}
	m, err := p.metadata()
	if err != nil {
		return nil, err
	}
	if c.Iss != m.Issuer {
		return nil, fmt.Errorf("ID token issuer %q is not %q", c.Iss, m.Issuer)
	}
	aud := c.audiences()
	okAud := false
	for _, a := range aud {
		if a == p.cfg.ClientID {
			okAud = true
		}
	}
	if !okAud || (len(aud) > 1 && c.Azp != p.cfg.ClientID) {
		return nil, errors.New("ID token is not for this client")
	}
	now := p.now()
	if c.Exp == 0 || now.After(time.Unix(int64(c.Exp), 0).Add(oidcClockSkew)) {
		return nil, errors.New("ID token has expired")
	}
	if c.Nbf != 0 && now.Add(oidcClockSkew).Before(time.Unix(int64(c.Nbf), 0)) {
		return nil, errors.New("ID token is not valid yet")
	}
	if c.Iat != 0 && now.Add(oidcClockSkew).Before(time.Unix(int64(c.Iat), 0)) {
		return nil, errors.New("ID token was issued in the future")
	}
	if nonce == "" || c.Nonce != nonce {
		return nil, errors.New("ID token nonce does not match this sign-in")
	}
	if c.Sub == "" {
		return nil, errors.New("ID token has no subject")
	}
	return &c, nil
}

// --- groups → role -------------------------------------------------------------

// oidcRole maps group names to the strongest MeshRelay role. agencyExists
// decides which agency groups count.
func oidcRole(prefix string, groups []string, agencyExists func(string) bool) (role, agency string) {
	p := strings.ToLower(prefix) + "-"
	rank := map[string]int{"dispatch": 1, "agency_admin": 2, "super_admin": 3}
	sort.Strings(groups) // deterministic when two agencies give the same rank
	for _, g := range groups {
		lg := strings.ToLower(strings.TrimSpace(g))
		if !strings.HasPrefix(lg, p) {
			continue
		}
		rest := strings.TrimSpace(g)[len(p):]
		var r, a string
		switch {
		case strings.EqualFold(rest, "super-admins"):
			r = "super_admin"
		case len(rest) > len("-admins") && strings.HasSuffix(strings.ToLower(rest), "-admins"):
			r, a = "agency_admin", rest[:len(rest)-len("-admins")]
		case len(rest) > len("-dispatch") && strings.HasSuffix(strings.ToLower(rest), "-dispatch"):
			r, a = "dispatch", rest[:len(rest)-len("-dispatch")]
		default:
			continue
		}
		if r != "super_admin" && !agencyExists(a) {
			continue
		}
		if rank[r] > rank[role] {
			role, agency = r, a
		}
	}
	return role, agency
}

// --- HTTP ----------------------------------------------------------------------

func randomURLToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeNext keeps the post-login redirect on this site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") || strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

func (s *Server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.oidc == nil {
		w.Write([]byte(`{"enabled":false}`))
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"enabled": true, "label": s.oidc.cfg.Label})
}

func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.oidc.metadata()
	if err != nil {
		log.Printf("SSO: cannot reach the provider: %v", err)
		http.Redirect(w, r, "/login.html?sso_error="+url.QueryEscape("The sign-in service cannot be reached. Use your MeshRelay password."), http.StatusFound)
		return
	}
	state, nonce, verifier := randomURLToken(24), randomURLToken(24), randomURLToken(48)
	now := s.oidc.now()
	s.oidc.mu.Lock()
	for k, v := range s.oidc.pending {
		if now.Sub(v.at) > oidcPendingTTL {
			delete(s.oidc.pending, k)
		}
	}
	if len(s.oidc.pending) >= oidcPendingMax {
		s.oidc.mu.Unlock()
		http.Error(w, "too many sign-ins in progress, try again shortly", http.StatusServiceUnavailable)
		return
	}
	s.oidc.pending[state] = oidcPending{nonce: nonce, verifier: verifier, next: safeNext(r.URL.Query().Get("next")), at: now}
	s.oidc.mu.Unlock()

	// Ties the callback to THIS browser (login CSRF). Lax: the provider's
	// redirect back is a top-level GET.
	http.SetCookie(w, &http.Cookie{Name: "oidc_state", Value: state, Path: "/api/oidc/",
		MaxAge: int(oidcPendingTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})

	ch := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.oidc.cfg.ClientID},
		"redirect_uri":          {s.oidc.cfg.RedirectURL},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(ch[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, m.AuthEndpoint+sep+q.Encode(), http.StatusFound)
}

func (s *Server) oidcFail(w http.ResponseWriter, r *http.Request, userMsg, logMsg string) {
	log.Printf("SSO sign-in refused from %s: %q", s.clientIP(r), logMsg)
	http.SetCookie(w, &http.Cookie{Name: "oidc_state", Value: "", Path: "/api/oidc/", MaxAge: -1, HttpOnly: true, Secure: true})
	http.Redirect(w, r, "/login.html?sso_error="+url.QueryEscape(userMsg), http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie("oidc_state")
	if state == "" || err != nil || c.Value != state {
		s.oidcFail(w, r, "Sign-in expired or was started in another browser. Try again.", "state missing or not this browser's")
		return
	}
	s.oidc.mu.Lock()
	pend, ok := s.oidc.pending[state]
	delete(s.oidc.pending, state)
	s.oidc.mu.Unlock()
	if !ok || s.oidc.now().Sub(pend.at) > oidcPendingTTL {
		s.oidcFail(w, r, "Sign-in expired. Try again.", "unknown or expired state")
		return
	}
	if e := q.Get("error"); e != "" {
		s.oidcFail(w, r, "The sign-in service did not sign you in.", "provider error "+e)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.oidcFail(w, r, "Sign-in failed. Try again.", "no code")
		return
	}
	m, err := s.oidc.metadata()
	if err != nil {
		s.oidcFail(w, r, "The sign-in service cannot be reached.", err.Error())
		return
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.oidc.cfg.RedirectURL},
		"code_verifier": {pend.verifier},
	}
	req, _ := http.NewRequest("POST", m.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(s.oidc.cfg.ClientID), url.QueryEscape(s.oidc.cfg.ClientSecret))
	resp, err := s.oidc.client.Do(req)
	if err != nil {
		s.oidcFail(w, r, "The sign-in service cannot be reached.", "token request: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if resp.StatusCode != 200 || tok.IDToken == "" {
		s.oidcFail(w, r, "Sign-in failed. Try again.", fmt.Sprintf("token endpoint HTTP %d %s", resp.StatusCode, tok.Error))
		return
	}
	claims, err := s.oidc.verifyIDToken(tok.IDToken, pend.nonce)
	if err != nil {
		s.oidcFail(w, r, "Sign-in failed. Try again.", err.Error())
		return
	}
	username := strings.TrimSpace(claims.PreferredUsername)
	if username == "" {
		username = strings.TrimSpace(claims.Email)
	}
	if username == "" || len(username) > 64 || strings.ContainsAny(username, "\r\n\t:/\\<>\"'") {
		s.oidcFail(w, r, "Your account has no usable username.", fmt.Sprintf("unusable username %q (sub %s)", username, claims.Sub))
		return
	}
	s.mu.RLock()
	role, agency := oidcRole(s.oidc.cfg.GroupPrefix, claims.Groups, func(a string) bool {
		for id := range s.agencies {
			if strings.EqualFold(id, a) {
				return true
			}
		}
		return false
	})
	// Agency ids are case-sensitive in the stores; use the stored spelling.
	for id := range s.agencies {
		if agency != "" && strings.EqualFold(id, agency) {
			agency = id
		}
	}
	s.mu.RUnlock()
	if role == "" {
		s.oidcFail(w, r, "Your account is not allowed into MeshRelay. Ask an administrator to add you to a MeshRelay group.",
			fmt.Sprintf("%s is in no %s-* group", username, s.oidc.cfg.GroupPrefix))
		return
	}

	key := userKey(agency, username)
	s.mu.Lock()
	admin := s.webAdmins[key]
	if admin != nil && !admin.SSO {
		s.mu.Unlock()
		s.oidcFail(w, r, "A MeshRelay password account with your name already exists. Sign in with its password, or ask an administrator.",
			fmt.Sprintf("%s [%s] is a password account; SSO will not take it over", username, agency))
		return
	}
	// One SSO identity, one account: drop this person's SSO account in another
	// agency/role key (their groups changed).
	for k, a := range s.webAdmins {
		if a.SSO && a.Subject == claims.Sub && k != key {
			delete(s.webAdmins, k)
		}
	}
	if admin == nil {
		admin = &WebAdmin{Username: username, SSO: true, Subject: claims.Sub,
			// Unusable for password sign-in; also the session binding (sessionFor).
			PasswordHash: "sso:" + randomURLToken(24)}
		s.webAdmins[key] = admin
	}
	admin.Role, admin.AgencyID, admin.Subject, admin.MustChangePassword = role, agency, claims.Sub, false
	snap := *admin
	s.mu.Unlock()
	s.saveWebAdmins()

	token := s.createWebSession(key, &snap)
	http.SetCookie(w, &http.Cookie{Name: "oidc_state", Value: "", Path: "/api/oidc/", MaxAge: -1, HttpOnly: true, Secure: true})
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true,
		MaxAge: int(webSessionMaxLifetime / time.Second), Secure: true, SameSite: http.SameSiteStrictMode})
	log.Printf("Web login successful (SSO): %q (role: %s, agency: %s) from %s", username, role, agency, s.clientIP(r))
	s.activityLog.Add("admin_login", username, fmt.Sprintf("Web dashboard login via SSO (role: %s)", role), agency)

	// SameSite=Strict cookies are not sent on the redirect chain that started
	// at the provider, so land on a tiny page that moves on from our own origin.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Same rule as login.html: only known pages; dispatch always lands in Command.
	next := "/"
	if pend.next == "/command" || role == "dispatch" {
		next = "/command"
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><title>Signing in…</title><a href="%s">Continue</a>`,
		htmlAttrEscape(next), htmlAttrEscape(next))
}

func htmlAttrEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
