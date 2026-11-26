// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Moving users to another MeshRelay server (Settings -> Move users).
//
// Export (super admin): the accounts of every agency or of one, in one file,
// encrypted with a passphrase the admin types:
//   - the agencies (their settings),
//   - the phone accounts of users.json, with their password hash and phone
//     (device) keys,
//   - the dashboard accounts of web_admins.json (a separate password store,
//     kept separate on import),
//   - optionally the server's identity key.
//
// What still works after the move. Nothing stored is tied to this server:
//   - The password hash and device keys are plain SHA-256 values. They sign in
//     with the older challenge sign-in on any server.
//   - The secure sign-in's verifier is derived on demand from the same hash and
//     the server's public key (secure.go), so it works on the new server as
//     well, whatever its identity key.
//   - Dashboard hashes are bcrypt and work as they are.
// What can stop a phone is the server key it pinned (from the QR or its first
// secure sign-in), keyed by the server address it uses:
//   - A phone that keeps using the same address refuses a server with a
//     different key ("server key changed").
//   - Exporting with the identity key and importing it gives the new server
//     the old identity, so those phones connect without noticing.
//   - Without the key, they need a new QR code (or their saved server key
//     cleared).
//   - A phone pointed at a new address has no pin for it and pins the new
//     server on first contact.
//
// File format (JSON):
//
//	{"format":"meshrelay-user-export","version":1,"server_version":"v4.8.0",
//	 "created_at":"...","kdf":{"name":"scrypt","n":131072,"r":8,"p":1,"salt":"<b64>"},
//	 "cipher":"AES-256-GCM","nonce":"<b64>","data":"<b64 ciphertext>"}
//
//   - key = scrypt(passphrase, salt, n, r, p, 32).
//   - The additional data is the same JSON with "data" empty, so every header
//     field is authenticated.
//   - The plaintext is exportPayload as JSON.
//   - The passphrase and the plaintext are never written to disk or to the
//     log. The upload is read into memory, never into a temporary file.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	exportFormat  = "meshrelay-user-export"
	exportVersion = 1
	exportCipher  = "AES-256-GCM"

	migrationMaxFile   = 32 << 20 // bytes of upload accepted
	migrationMaxAgency = 10000
	migrationMaxUsers  = 200000
	migrationMaxAdmins = 10000
	migrationMaxDevice = 100

	exportScryptR = 8
	exportScryptP = 1
)

// exportScryptN is the scrypt cost of new exports (128 MiB, about half a
// second on a Raspberry Pi 5). Tests lower it. Imports accept 2^14 to 2^20.
var exportScryptN = 1 << 17

// One export or import at a time: each holds the scrypt work area.
var migrationSlot = make(chan struct{}, 1)

type exportKDF struct {
	Name string `json:"name"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
}

type exportEnvelope struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	ServerVersion string    `json:"server_version"`
	CreatedAt     string    `json:"created_at"`
	KDF           exportKDF `json:"kdf"`
	Cipher        string    `json:"cipher"`
	Nonce         string    `json:"nonce"`
	Data          string    `json:"data"`
}

// aad: the envelope without its ciphertext.
func (e exportEnvelope) aad() []byte {
	e.Data = ""
	b, _ := json.Marshal(e)
	return b
}

type exportPayload struct {
	FormatVersion       int            `json:"format_version"`
	ServerVersion       string         `json:"server_version"`
	ExportedAt          string         `json:"exported_at"`
	ExportedBy          string         `json:"exported_by"`
	Scope               string         `json:"scope"` // "" = every agency
	IdentityFingerprint string         `json:"identity_fingerprint,omitempty"`
	IdentityKey         string         `json:"identity_key,omitempty"` // hex X25519 private key, only when chosen
	Agencies            []*Agency      `json:"agencies"`
	Users               []*UserAccount `json:"users"`
	WebAdmins           []*WebAdmin    `json:"web_admins"`
}

// ---- export ------------------------------------------------------------------

// buildExport collects the accounts of scope ("" = all agencies).
func (s *Server) buildExport(scope string, withIdentity bool, by string) (*exportPayload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if scope != "" && s.agencies[scope] == nil {
		return nil, fmt.Errorf("agency %s not found", scope)
	}
	p := &exportPayload{
		FormatVersion:       exportVersion,
		ServerVersion:       Version,
		ExportedAt:          time.Now().UTC().Format(time.RFC3339),
		ExportedBy:          by,
		Scope:               scope,
		IdentityFingerprint: s.identityFingerprint(),
	}
	for _, a := range s.agencies {
		if scope == "" || a.ID == scope {
			cp := *a
			p.Agencies = append(p.Agencies, &cp)
		}
	}
	for _, u := range s.users {
		if scope == "" || u.AgencyID == scope {
			cp := *u
			cp.Devices = nil
			for _, d := range u.Devices {
				dc := *d
				cp.Devices = append(cp.Devices, &dc)
			}
			p.Users = append(p.Users, &cp)
		}
	}
	for _, w := range s.webAdmins {
		if scope == "" || w.AgencyID == scope {
			cp := *w
			p.WebAdmins = append(p.WebAdmins, &cp)
		}
	}
	sort.Slice(p.Agencies, func(i, j int) bool { return p.Agencies[i].ID < p.Agencies[j].ID })
	sort.Slice(p.Users, func(i, j int) bool {
		return userKey(p.Users[i].AgencyID, p.Users[i].Username) < userKey(p.Users[j].AgencyID, p.Users[j].Username)
	})
	sort.Slice(p.WebAdmins, func(i, j int) bool {
		return userKey(p.WebAdmins[i].AgencyID, p.WebAdmins[i].Username) < userKey(p.WebAdmins[j].AgencyID, p.WebAdmins[j].Username)
	})
	if withIdentity {
		k := s.identityKey()
		if k == nil {
			return nil, errors.New("this server has no identity key")
		}
		p.IdentityKey = hex.EncodeToString(k.Bytes())
	}
	return p, nil
}

func exportAEAD(passphrase string, salt []byte, n, r, p int) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, n, r, p, 32)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// sealExport encrypts the payload into the file's bytes.
func sealExport(p *exportPayload, passphrase string) ([]byte, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	defer zero(plain)
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	env := exportEnvelope{
		Format:        exportFormat,
		Version:       exportVersion,
		ServerVersion: p.ServerVersion,
		CreatedAt:     p.ExportedAt,
		KDF:           exportKDF{Name: "scrypt", N: exportScryptN, R: exportScryptR, P: exportScryptP, Salt: base64.StdEncoding.EncodeToString(salt)},
		Cipher:        exportCipher,
		Nonce:         base64.StdEncoding.EncodeToString(nonce),
	}
	aead, err := exportAEAD(passphrase, salt, env.KDF.N, env.KDF.R, env.KDF.P)
	if err != nil {
		return nil, err
	}
	env.Data = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, env.aad()))
	return json.MarshalIndent(env, "", "  ")
}

var errExportPassphrase = errors.New("wrong passphrase, or the file was changed or damaged")

// openExport checks and decrypts an export file. Everything in it is
// untrusted: the header is checked before any work, and the cost of the key
// derivation is bounded.
func openExport(file []byte, passphrase string) (*exportPayload, *exportEnvelope, error) {
	var env exportEnvelope
	if err := json.Unmarshal(file, &env); err != nil || env.Format != exportFormat {
		return nil, nil, errors.New("not a MeshRelay user export file")
	}
	if env.Version > exportVersion {
		return nil, nil, fmt.Errorf("this file was made by a newer MeshRelay server (%s, file format %d); update this server first",
			printable(env.ServerVersion, 40), env.Version)
	}
	if env.Version < 1 {
		return nil, nil, errors.New("unknown file format version")
	}
	k := env.KDF
	salt, err1 := base64.StdEncoding.DecodeString(k.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(env.Nonce)
	ct, err3 := base64.StdEncoding.DecodeString(env.Data)
	if k.Name != "scrypt" || k.N < 1<<14 || k.N > 1<<20 || k.N&(k.N-1) != 0 || k.R != 8 || k.P < 1 || k.P > 4 ||
		err1 != nil || len(salt) < 16 || len(salt) > 64 || env.Cipher != exportCipher ||
		err2 != nil || len(nonce) != 12 || err3 != nil || len(ct) < 16 {
		return nil, nil, errors.New("the file's encryption header is not valid")
	}
	aead, err := exportAEAD(passphrase, salt, k.N, k.R, k.P)
	if err != nil {
		return nil, nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, env.aad())
	if err != nil {
		return nil, nil, errExportPassphrase
	}
	defer zero(plain)
	var p exportPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, nil, errors.New("the file's contents are not valid")
	}
	if p.FormatVersion != env.Version {
		return nil, nil, errors.New("the file's contents do not match its header")
	}
	if len(p.Agencies) > migrationMaxAgency || len(p.Users) > migrationMaxUsers || len(p.WebAdmins) > migrationMaxAdmins {
		return nil, nil, errors.New("the file holds more accounts than one import takes")
	}
	env.Data = ""
	return &p, &env, nil
}

// printable trims untrusted text for display: valid UTF-8, no control
// characters, at most max runes.
func printable(v string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(v, "?") {
		if n >= max {
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// ---- validation of what the file holds --------------------------------------

var (
	hex64Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	deviceIDRe    = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	bcryptPattern = regexp.MustCompile(`^\$2[ab]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)
)

func validTime(v string) bool {
	if v == "" {
		return true
	}
	_, err := time.Parse(time.RFC3339, v)
	return err == nil && len(v) <= 40
}

func validFreeText(v string, maxBytes int) bool {
	if len(v) > maxBytes || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune("<>\"`\\", r) {
			return false
		}
	}
	return true
}

// checkImportAgency validates an agency from a file and returns the copy to
// store (its LMR bridge link is cleared: bridges are not moved).
func checkImportAgency(a *Agency) (*Agency, error) {
	if a == nil {
		return nil, errors.New("empty record")
	}
	if err := validateAgencyID(a.ID); err != nil {
		return nil, err
	}
	if a.Name == "" {
		return nil, errors.New("agency name is required")
	}
	if err := validateDisplayName("agency name", a.Name); err != nil {
		return nil, err
	}
	if a.MaxUsers < 0 || a.MaxUsers > 1000000 || a.MaxChannels < 0 || a.MaxChannels > MaxAgencyTalkgroups ||
		a.ChannelCount < 0 || a.ChannelCount > MaxAgencyTalkgroups ||
		a.RecordingRetentionDays < 0 || a.RecordingRetentionDays > 3650 {
		return nil, errors.New("a number is out of range")
	}
	if len(a.ChannelNames) > MaxAgencyTalkgroups {
		return nil, errors.New("too many channel names")
	}
	for ch, name := range a.ChannelNames {
		if ch < 0 || ch >= MaxAgencyTalkgroups {
			return nil, fmt.Errorf("channel %d is not a talkgroup", ch)
		}
		if err := validateChannelName(ch, name); err != nil {
			return nil, err
		}
	}
	if err := validateChannelName(EmergencyChannel, a.EmergencyChannelName); err != nil {
		return nil, err
	}
	if !validFreeText(a.TranscribeWords, 400) {
		return nil, errors.New("transcription words are not valid")
	}
	if len(a.AutoAcceptFrom) > migrationMaxAgency {
		return nil, errors.New("too many trusted agencies")
	}
	cp := *a
	cp.AutoAcceptFrom = nil
	for _, id := range a.AutoAcceptFrom {
		if validateAgencyID(id) == nil {
			cp.AutoAcceptFrom = append(cp.AutoAcceptFrom, id)
		}
	}
	if len(a.ChannelNames) > 0 {
		cp.ChannelNames = make(map[int]string, len(a.ChannelNames))
		for k, v := range a.ChannelNames {
			cp.ChannelNames[k] = v
		}
	}
	if !validTime(cp.CreatedAt) {
		cp.CreatedAt = ""
	}
	if cp.CreatedAt == "" {
		cp.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if cp.MaxUsers == 0 {
		cp.MaxUsers = 100
	}
	if cp.MaxChannels == 0 {
		cp.MaxChannels = MaxAgencyTalkgroups
	}
	cp.EmergencyLMRBridge = "" // LMR bridges are not part of the move
	return &cp, nil
}

func checkImportUser(u *UserAccount) (*UserAccount, error) {
	if u == nil {
		return nil, errors.New("empty record")
	}
	if err := validateAccountName(u.Username); err != nil {
		return nil, err
	}
	if err := validateAgencyID(u.AgencyID); err != nil {
		return nil, err
	}
	if err := validateDisplayName("name", u.Name); err != nil {
		return nil, err
	}
	switch u.Role {
	case "", "user", "admin", "super_admin":
	default:
		return nil, errors.New("unknown role")
	}
	if u.PasswordHash != "" && !hex64Pattern.MatchString(u.PasswordHash) {
		return nil, errors.New("password hash is not valid")
	}
	if u.Priority < 0 || u.Priority > PriorityMonitor {
		return nil, errors.New("priority is out of range")
	}
	if u.InvitedBy != "" && !validFreeText(u.InvitedBy, 200) {
		return nil, errors.New("invited-by is not valid")
	}
	if len(u.Devices) > migrationMaxDevice {
		return nil, errors.New("too many phone keys")
	}
	cp := *u
	if cp.Role == "" {
		cp.Role = "user"
	}
	if !validTime(cp.CreatedAt) {
		cp.CreatedAt = ""
	}
	if !validTime(cp.LastLogin) {
		cp.LastLogin = ""
	}
	cp.Devices = nil
	seen := map[string]bool{}
	for _, d := range u.Devices {
		if d == nil || !deviceIDRe.MatchString(d.ID) || !hex64Pattern.MatchString(d.Hash) || seen[d.ID] {
			return nil, errors.New("a phone key is not valid")
		}
		seen[d.ID] = true
		dc := *d
		if !validTime(dc.CreatedAt) {
			dc.CreatedAt = ""
		}
		if !validTime(dc.LastUsed) {
			dc.LastUsed = ""
		}
		if dc.EnrolledFrom != "" && net.ParseIP(dc.EnrolledFrom) == nil {
			dc.EnrolledFrom = ""
		}
		cp.Devices = append(cp.Devices, &dc)
	}
	if u.PasswordHash == "" && len(cp.Devices) == 0 {
		return nil, errors.New("the account has no password and no phone key")
	}
	return &cp, nil
}

func checkImportWebAdmin(w *WebAdmin) (*WebAdmin, error) {
	if w == nil {
		return nil, errors.New("empty record")
	}
	if err := validateAccountName(w.Username); err != nil {
		return nil, err
	}
	switch w.Role {
	case "super_admin":
		if w.AgencyID != "" {
			if err := validateAgencyID(w.AgencyID); err != nil {
				return nil, err
			}
		}
	case "agency_admin", "dispatch":
		if err := validateAgencyID(w.AgencyID); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown role")
	}
	switch {
	case w.PasswordHash == "" && w.SSO:
	case bcryptPattern.MatchString(w.PasswordHash), hex64Pattern.MatchString(w.PasswordHash):
	default:
		return nil, errors.New("password hash is not valid")
	}
	if !validFreeText(w.Subject, 256) {
		return nil, errors.New("single sign-on subject is not valid")
	}
	cp := *w
	return &cp, nil
}

// ---- the plan (preview) ----------------------------------------------------

type migrationOptions struct {
	Replace         bool // replace accounts that already exist (default: skip them)
	CreateAgencies  bool // create agencies that do not exist here
	IncludeGuests   bool // import guest accounts that have not expired
	ImportWebAdmins bool // import dashboard accounts too
	UseIdentityKey  bool // take the file's server identity key
}

type planItem struct {
	Kind    string `json:"kind"` // agency, user, dashboard
	Agency  string `json:"agency"`
	Name    string `json:"name"`
	Display string `json:"display,omitempty"`
	Role    string `json:"role,omitempty"`
	Phones  int    `json:"phones,omitempty"`
	Guest   bool   `json:"guest,omitempty"`
	Action  string `json:"action"` // create, exists, new, replace, skip
	Reason  string `json:"reason,omitempty"`

	key     string // userKey to store under
	oldKey  string // existing key being replaced (case may differ)
	agency  *Agency
	user    *UserAccount
	webUser *WebAdmin
}

type migrationPlan struct {
	Items    []*planItem    `json:"items"`
	Counts   map[string]int `json:"counts"`
	Identity struct {
		InFile      bool   `json:"in_file"`
		Fingerprint string `json:"fingerprint,omitempty"`
		Current     string `json:"current"`
		Replace     bool   `json:"replace"`
		Same        bool   `json:"same"`
	} `json:"identity"`
	identity *ecdh.PrivateKey
}

// findKeyFold returns the key in m equal to key ignoring case, or "".
func findKeyFold[V any](m map[string]V, key string) string {
	if _, ok := m[key]; ok {
		return key
	}
	for k := range m {
		if strings.EqualFold(k, key) {
			return k
		}
	}
	return ""
}

// planImportLocked works out what an import would do. It changes nothing.
// importer is the webAdmins key of the admin doing it. Caller holds s.mu.
func (s *Server) planImportLocked(p *exportPayload, opt migrationOptions, importer string, now time.Time) (*migrationPlan, error) {
	plan := &migrationPlan{Counts: map[string]int{}}
	add := func(it *planItem) {
		plan.Items = append(plan.Items, it)
		plan.Counts[it.Kind+"_"+it.Action]++
	}
	skip := func(it *planItem, why string) {
		it.Action, it.Reason = "skip", why
		add(it)
	}

	// Agencies.
	willExist := map[string]bool{}
	for id := range s.agencies {
		willExist[strings.ToLower(id)] = true
	}
	seenAg := map[string]bool{}
	for _, raw := range p.Agencies {
		it := &planItem{Kind: "agency"}
		if raw != nil {
			it.Agency, it.Name = printable(raw.ID, 64), printable(raw.Name, 100)
		}
		a, err := checkImportAgency(raw)
		switch {
		case err != nil:
			skip(it, "not valid: "+err.Error())
		case seenAg[strings.ToLower(a.ID)]:
			skip(it, "listed twice in the file")
		case findKeyFold(s.agencies, a.ID) != "":
			seenAg[strings.ToLower(a.ID)] = true
			it.Action, it.Reason = "exists", "already on this server; its settings are kept"
			add(it)
		case !opt.CreateAgencies:
			seenAg[strings.ToLower(a.ID)] = true
			skip(it, "not on this server (creating agencies is off)")
		default:
			seenAg[strings.ToLower(a.ID)] = true
			it.Action, it.agency, it.key = "create", a, a.ID
			willExist[strings.ToLower(a.ID)] = true
			add(it)
		}
	}
	agencyHere := func(id string) string {
		if k := findKeyFold(s.agencies, id); k != "" {
			return k
		}
		for _, it := range plan.Items {
			if it.Kind == "agency" && it.Action == "create" && strings.EqualFold(it.key, id) {
				return it.key
			}
		}
		return ""
	}

	// Phone accounts (users.json).
	seenUser := map[string]bool{}
	for _, raw := range p.Users {
		it := &planItem{Kind: "user"}
		if raw != nil {
			it.Agency, it.Name, it.Display, it.Role = printable(raw.AgencyID, 64), printable(raw.Username, 64), printable(raw.Name, 100), printable(raw.Role, 20)
			it.Phones, it.Guest = len(raw.Devices), raw.GuestExpiresAt != nil
		}
		u, err := checkImportUser(raw)
		if err != nil {
			skip(it, "not valid: "+err.Error())
			continue
		}
		ag := agencyHere(u.AgencyID)
		if ag == "" {
			skip(it, "its agency is not on this server")
			continue
		}
		u.AgencyID = ag
		key := userKey(ag, u.Username)
		lk := strings.ToLower(key)
		switch {
		case seenUser[lk]:
			skip(it, "listed twice in the file")
			continue
		case u.GuestExpiresAt != nil && !u.GuestExpiresAt.After(now):
			skip(it, "guest access has expired")
			continue
		case u.GuestExpiresAt != nil && !opt.IncludeGuests:
			skip(it, "guest account (import guests is off)")
			continue
		case strings.EqualFold(key, importer):
			skip(it, "your own account is never changed by an import")
			continue
		}
		seenUser[lk] = true
		it.key, it.user = key, u
		if old := findKeyFold(s.users, key); old != "" {
			if !opt.Replace {
				skip(it, "already exists here (kept as it is)")
				continue
			}
			it.Action, it.oldKey = "replace", old
			it.Reason = "replaces the account on this server"
		} else {
			it.Action = "new"
		}
		add(it)
	}

	// Dashboard accounts (web_admins.json): only when chosen.
	seenAdmin := map[string]bool{}
	for _, raw := range p.WebAdmins {
		it := &planItem{Kind: "dashboard"}
		if raw != nil {
			it.Agency, it.Name, it.Role = printable(raw.AgencyID, 64), printable(raw.Username, 64), printable(raw.Role, 20)
		}
		if !opt.ImportWebAdmins {
			skip(it, "dashboard accounts are not imported (option off)")
			continue
		}
		w, err := checkImportWebAdmin(raw)
		if err != nil {
			skip(it, "not valid: "+err.Error())
			continue
		}
		if w.AgencyID != "" {
			ag := agencyHere(w.AgencyID)
			if ag == "" {
				skip(it, "its agency is not on this server")
				continue
			}
			w.AgencyID = ag
		}
		key := userKey(w.AgencyID, w.Username)
		switch {
		case seenAdmin[strings.ToLower(key)]:
			skip(it, "listed twice in the file")
			continue
		case strings.EqualFold(key, importer):
			skip(it, "your own account is never changed by an import")
			continue
		}
		seenAdmin[strings.ToLower(key)] = true
		it.key, it.webUser = key, w
		if old := findKeyFold(s.webAdmins, key); old != "" {
			if !opt.Replace {
				skip(it, "already exists here (kept as it is)")
				continue
			}
			it.Action, it.oldKey = "replace", old
			it.Reason = "replaces the dashboard account on this server"
		} else {
			it.Action = "new"
		}
		add(it)
	}

	// The identity key.
	plan.Identity.Current = s.identityFingerprint()
	plan.Identity.Fingerprint = printable(p.IdentityFingerprint, 64)
	if p.IdentityKey != "" {
		raw, err := hex.DecodeString(p.IdentityKey)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("the identity key in the file is not valid")
		}
		k, err := ecdh.X25519().NewPrivateKey(raw)
		zero(raw)
		if err != nil {
			return nil, errors.New("the identity key in the file is not valid")
		}
		plan.Identity.InFile = true
		pub := k.PublicKey().Bytes()
		fp := fingerprintOf(pub)
		plan.Identity.Fingerprint = fp
		plan.Identity.Same = fp == plan.Identity.Current
		if opt.UseIdentityKey && !plan.Identity.Same {
			plan.Identity.Replace = true
			plan.identity = k
		}
	} else if opt.UseIdentityKey {
		return nil, errors.New("this file does not include a server identity key")
	}
	return plan, nil
}

func fingerprintOf(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// ---- apply ---------------------------------------------------------------

type migrationResult struct {
	Agencies, Users, WebAdmins, Replaced, Skipped int
	SessionsEnded                                 int
	IdentityReplaced                              bool
	Backups                                       []string
}

// backupForImport copies a data file next to itself before the import.
func backupForImport(path, stamp string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	bak := path + ".before-import-" + stamp
	return bak, writeFileAtomic(bak, data)
}

// applyImport carries out the plan for p, all or nothing: the current
// agencies.json, users.json and web_admins.json (and the identity key when
// it is replaced) are copied next to themselves first, and if writing any
// new file fails every file is put back and nothing changes in memory.
func (s *Server) applyImport(p *exportPayload, opt migrationOptions, importer, by string) (*migrationResult, *migrationPlan, error) {
	now := time.Now()
	s.mu.Lock()
	plan, err := s.planImportLocked(p, opt, importer, now)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	res := &migrationResult{}
	for _, it := range plan.Items {
		switch {
		case it.Action == "skip":
			res.Skipped++
		case it.Kind == "agency" && it.Action == "create":
			res.Agencies++
		case it.Kind == "user" && (it.Action == "new" || it.Action == "replace"):
			res.Users++
		case it.Kind == "dashboard" && (it.Action == "new" || it.Action == "replace"):
			res.WebAdmins++
		}
		if it.Action == "replace" {
			res.Replaced++
		}
	}
	if res.Agencies+res.Users+res.WebAdmins == 0 && !plan.Identity.Replace {
		s.mu.Unlock()
		return res, plan, nil
	}

	// New contents, built beside the live maps.
	newAgencies := make(map[string]*Agency, len(s.agencies))
	for k, v := range s.agencies {
		newAgencies[k] = v
	}
	newUsers := make(map[string]*UserAccount, len(s.users))
	for k, v := range s.users {
		newUsers[k] = v
	}
	newAdmins := make(map[string]*WebAdmin, len(s.webAdmins))
	for k, v := range s.webAdmins {
		newAdmins[k] = v
	}
	type replaced struct {
		oldKey, newKey string
		old, new       *UserAccount
	}
	var replacedUsers []replaced
	for _, it := range plan.Items {
		switch {
		case it.Kind == "agency" && it.Action == "create":
			newAgencies[it.key] = it.agency
		case it.Kind == "user" && (it.Action == "new" || it.Action == "replace"):
			if it.oldKey != "" {
				replacedUsers = append(replacedUsers, replaced{it.oldKey, it.key, newUsers[it.oldKey], it.user})
				delete(newUsers, it.oldKey)
			}
			newUsers[it.key] = it.user
		case it.Kind == "dashboard" && (it.Action == "new" || it.Action == "replace"):
			if it.oldKey != "" {
				delete(newAdmins, it.oldKey)
			}
			newAdmins[it.key] = it.webUser
		}
	}

	// Backups, then the writes.
	stamp := now.Format("20060102-150405")
	files := []string{s.config.AgenciesFile, s.config.UsersFile, s.config.WebAdminsFile}
	if plan.Identity.Replace {
		files = append(files, s.identityFile())
	}
	old := map[string][]byte{}
	for _, f := range files {
		bak, err := backupForImport(f, stamp)
		if err != nil {
			s.mu.Unlock()
			return nil, plan, fmt.Errorf("could not back up %s: %v", f, err)
		}
		if bak != "" {
			res.Backups = append(res.Backups, bak)
			data, _ := os.ReadFile(f)
			old[f] = data
		}
	}
	restore := func() {
		for _, f := range files {
			if data, ok := old[f]; ok {
				writeFileAtomic(f, data)
			} else {
				os.Remove(f)
			}
		}
	}
	agList := make([]*Agency, 0, len(newAgencies))
	for _, a := range newAgencies {
		agList = append(agList, a)
	}
	userList := make([]*UserAccount, 0, len(newUsers))
	for _, u := range newUsers {
		userList = append(userList, u)
	}
	adminList := make([]WebAdmin, 0, len(newAdmins))
	for _, w := range newAdmins {
		adminList = append(adminList, *w)
	}
	writeErr := writeJSONFileAtomic(s.config.AgenciesFile, agList)
	if writeErr == nil {
		writeErr = writeJSONFileAtomic(s.config.UsersFile, userList)
	}
	if writeErr == nil {
		writeErr = writeJSONFileAtomic(s.config.WebAdminsFile, adminList)
	}
	if writeErr == nil && plan.Identity.Replace {
		writeErr = writeFileAtomic(s.identityFile(), []byte(hex.EncodeToString(plan.identity.Bytes())+"\n"))
	}
	if writeErr != nil {
		restore()
		s.mu.Unlock()
		return nil, plan, fmt.Errorf("could not save the import (nothing was changed): %v", writeErr)
	}

	// Saved: switch the live state over.
	s.agencies, s.users, s.webAdmins = newAgencies, newUsers, newAdmins
	for _, it := range plan.Items {
		if it.Kind == "agency" && it.Action == "create" {
			s.ensureAgencyChannels(it.key)
			if m := it.agency.floorMode(); m != floorModeOff {
				s.floorOn.Store(it.key, m)
			}
		}
	}
	// Phones of a replaced account: a session whose credential (the password,
	// or its phone key) is no longer valid in the imported account ends, as
	// for a password reset; the others keep going.
	var kicks []expiredGuestKick
	for _, r := range replacedUsers {
		if r.oldKey != r.newKey || r.old == nil {
			kicks = append(kicks, s.takeAccountSessionsLocked(r.oldKey)...)
			continue
		}
		old, nu := r.old, r.new
		kicks = append(kicks, s.takeSessionsLocked(r.oldKey, func(cred string) bool {
			if cred == credPassword {
				return old.PasswordHash != nu.PasswordHash
			}
			if id, ok := strings.CutPrefix(cred, "dev:"); ok {
				for _, d := range nu.Devices {
					if d.ID == id {
						for _, od := range old.Devices {
							if od.ID == id {
								return od.Hash != d.Hash
							}
						}
						return true
					}
				}
			}
			return true
		})...)
	}
	s.mu.Unlock()

	res.SessionsEnded = len(kicks)
	s.finishSessionKicks(kicks, "Your account was changed by an administrator: sign in again")
	if plan.Identity.Replace {
		s.setIdentity(plan.identity)
		res.IdentityReplaced = true
	}
	log.Printf("User import by %q: %d agencies created, %d phone accounts, %d dashboard accounts (%d replaced), %d skipped, %d sessions ended, identity key replaced: %v",
		by, res.Agencies, res.Users, res.WebAdmins, res.Replaced, res.Skipped, res.SessionsEnded, res.IdentityReplaced)
	s.activityLog.Add("users_imported", by, fmt.Sprintf("Imported from a file exported by %s on %s: %d agencies created, %d phone accounts, %d dashboard accounts (%d replaced), %d skipped%s",
		printable(p.ExportedBy, 64), printable(p.ExportedAt, 40), res.Agencies, res.Users, res.WebAdmins, res.Replaced, res.Skipped,
		map[bool]string{true: ", server identity key replaced", false: ""}[res.IdentityReplaced]), "")
	return res, plan, nil
}

// ---- HTTP -------------------------------------------------------------------

func takeMigrationSlot(w http.ResponseWriter) bool {
	select {
	case migrationSlot <- struct{}{}:
		return true
	default:
		writeJSONError(w, http.StatusTooManyRequests, "another export or import is running; try again in a moment")
		return false
	}
}

func releaseMigrationSlot() { <-migrationSlot }

func checkPassphrase(p string) error {
	if utf8.RuneCountInString(p) < MinPasswordLength {
		return fmt.Errorf("the passphrase must be at least %d characters", MinPasswordLength)
	}
	if len(p) > 1024 {
		return errors.New("the passphrase is too long")
	}
	return nil
}

// handleUserExport: POST {agency, include_identity_key, passphrase} -> the
// encrypted file as a download. Super admin only.
func (s *Server) handleUserExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	session := s.getSession(r)
	if !isSuperAdmin(session) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return
	}
	var req struct {
		Agency             string `json:"agency"`
		IncludeIdentityKey bool   `json:"include_identity_key"`
		Passphrase         string `json:"passphrase"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := checkPassphrase(req.Passphrase); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Agency != "" {
		if err := validateAgencyID(req.Agency); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !takeMigrationSlot(w) {
		return
	}
	defer releaseMigrationSlot()
	p, err := s.buildExport(req.Agency, req.IncludeIdentityKey, session.Username)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, err := sealExport(p, req.Passphrase)
	if p.IdentityKey != "" {
		p.IdentityKey = ""
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not encrypt the export")
		return
	}
	scope := req.Agency
	if scope == "" {
		scope = "all"
	}
	name := fmt.Sprintf("meshrelay-users-%s-%s.mrusers", scope, time.Now().Format("20060102-1504"))
	log.Printf("User export by %q: %d agencies, %d phone accounts, %d dashboard accounts, scope %s, identity key included: %v",
		session.Username, len(p.Agencies), len(p.Users), len(p.WebAdmins), scope, req.IncludeIdentityKey)
	s.activityLog.Add("users_exported", session.Username, fmt.Sprintf("Exported %d phone accounts, %d dashboard accounts and %d agencies (%s)%s",
		len(p.Users), len(p.WebAdmins), len(p.Agencies), map[bool]string{true: "all agencies", false: "agency " + req.Agency}[req.Agency == ""],
		map[bool]string{true: ", with the server identity key", false: ""}[req.IncludeIdentityKey]), req.Agency)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(file)
}

// readImportForm reads the upload (multipart: file, passphrase, options)
// into memory only, with a size limit.
func readImportForm(w http.ResponseWriter, r *http.Request) (file []byte, passphrase string, opt migrationOptions, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, migrationMaxFile+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", opt, errors.New("expected a file upload")
	}
	fields := map[string]string{}
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, "", opt, errors.New("the upload is too large or damaged")
		}
		name := part.FormName()
		limit := int64(4 << 10)
		if name == "file" {
			limit = migrationMaxFile
		}
		data, rerr := io.ReadAll(io.LimitReader(part, limit+1))
		part.Close()
		if rerr != nil || int64(len(data)) > limit {
			return nil, "", opt, errors.New("the upload is too large")
		}
		if name == "file" {
			file = data
		} else {
			fields[name] = string(data)
		}
	}
	if len(file) == 0 {
		return nil, "", opt, errors.New("choose an export file")
	}
	on := func(k string) bool { v := fields[k]; return v == "1" || v == "true" || v == "on" }
	opt = migrationOptions{
		Replace:         fields["mode"] == "replace",
		CreateAgencies:  on("create_agencies"),
		IncludeGuests:   on("include_guests"),
		ImportWebAdmins: on("import_web_admins"),
		UseIdentityKey:  on("use_identity_key"),
	}
	if m := fields["mode"]; m != "" && m != "skip" && m != "replace" {
		return nil, "", opt, errors.New("mode must be skip or replace")
	}
	return file, fields["passphrase"], opt, nil
}

func (s *Server) migrationRequest(w http.ResponseWriter, r *http.Request) (*WebSession, *exportPayload, *exportEnvelope, migrationOptions, bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	session := s.getSession(r)
	if !isSuperAdmin(session) {
		writeJSONError(w, http.StatusForbidden, "super admin access required")
		return nil, nil, nil, migrationOptions{}, false
	}
	file, pass, opt, err := readImportForm(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, nil, nil, opt, false
	}
	if pass == "" {
		writeJSONError(w, http.StatusBadRequest, "type the file's passphrase")
		return nil, nil, nil, opt, false
	}
	if !takeMigrationSlot(w) {
		return nil, nil, nil, opt, false
	}
	p, env, err := openExport(file, pass)
	releaseMigrationSlot()
	if err != nil {
		if errors.Is(err, errExportPassphrase) {
			log.Printf("User import by %q refused: wrong passphrase or changed file", session.Username)
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, nil, nil, opt, false
	}
	return session, p, env, opt, true
}

func importSource(p *exportPayload, env *exportEnvelope) map[string]interface{} {
	return map[string]interface{}{
		"server_version":   printable(env.ServerVersion, 40),
		"format_version":   env.Version,
		"exported_at":      printable(p.ExportedAt, 40),
		"exported_by":      printable(p.ExportedBy, 64),
		"scope":            printable(p.Scope, 64),
		"agencies":         len(p.Agencies),
		"users":            len(p.Users),
		"dashboard":        len(p.WebAdmins),
		"has_identity_key": p.IdentityKey != "",
	}
}

// handleUserImportPreview: the dry run. Nothing is changed.
func (s *Server) handleUserImportPreview(w http.ResponseWriter, r *http.Request) {
	session, p, env, opt, ok := s.migrationRequest(w, r)
	if !ok {
		return
	}
	defer func() { p.IdentityKey = "" }()
	s.mu.RLock()
	plan, err := s.planImportLocked(p, opt, session.adminKey, time.Now())
	s.mu.RUnlock()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"source": importSource(p, env), "plan": plan})
}

// handleUserImport applies the import.
func (s *Server) handleUserImport(w http.ResponseWriter, r *http.Request) {
	session, p, env, opt, ok := s.migrationRequest(w, r)
	if !ok {
		return
	}
	defer func() { p.IdentityKey = "" }()
	res, plan, err := s.applyImport(p, opt, session.adminKey, session.Username)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "source": importSource(p, env), "plan": plan,
		"agencies_created": res.Agencies, "users": res.Users, "dashboard": res.WebAdmins,
		"replaced": res.Replaced, "skipped": res.Skipped, "sessions_ended": res.SessionsEnded,
		"identity_replaced": res.IdentityReplaced, "backups": res.Backups,
	})
}
