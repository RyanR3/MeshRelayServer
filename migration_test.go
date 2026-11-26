// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exports in tests use the lowest cost an import accepts.
func init() { exportScryptN = 1 << 14 }

const migPass = "correct horse battery staple"

type migFixture struct {
	s         *Server
	sink      *secSink
	devSecret string // engine7's phone key
}

// migSource is a server with two agencies, phone accounts (password, phone
// key, guests) and dashboard accounts.
func migSource(t *testing.T) *migFixture {
	t.Helper()
	s, sink := newSecServer(t)
	s.identityPath = filepath.Join(t.TempDir(), "server_identity.key")
	s.agencies["a1"].ChannelCount = 8
	s.agencies["a1"].ChannelNames = map[int]string{0: "Ops", 3: "Fire Ground"}
	s.agencies["a1"].FloorControl = true
	s.agencies["a1"].EmergencyLMRBridge = "lmr-1"
	addUser(s, "a1", "medic", "medic-password-1")
	s.users[userKey("a1", "medic")].Name = "Medic One"
	s.users[userKey("a1", "medic")].Priority = PriorityCommand
	secret := strings.Repeat("ab", 32)
	s.users[userKey("a1", "engine7")] = &UserAccount{Username: "engine7", AgencyID: "a1", Role: "user",
		PasswordHash: hashPassword("engine-password-7"),
		Devices:      []*DeviceKey{{ID: "0011223344556677", Hash: hashPassword(secret), CreatedAt: time.Now().Format(time.RFC3339), EnrolledFrom: "198.51.100.4"}}}
	addUser(s, "a2", "chief", "chief-ptt-password")
	s.users[userKey("a2", "chief")].Role = "admin"
	future, past := time.Now().Add(24*time.Hour), time.Now().Add(-time.Hour)
	addUser(s, "a1", "guest-visitor", "guest-password-1")
	s.users[userKey("a1", "guest-visitor")].GuestExpiresAt = &future
	s.users[userKey("a1", "guest-visitor")].InvitedBy = "medic"
	addUser(s, "a1", "guest-old", "guest-password-2")
	s.users[userKey("a1", "guest-old")].GuestExpiresAt = &past
	bc, _ := hashPasswordSecure("chief-dashboard-pw")
	s.webAdmins[userKey("a2", "chief")] = &WebAdmin{Username: "chief", Role: "agency_admin", AgencyID: "a2", PasswordHash: bc}
	s.webAdmins[userKey("", "root")] = &WebAdmin{Username: "root", Role: "super_admin", PasswordHash: hashPassword("root-dashboard-pw")}
	return &migFixture{s: s, sink: sink, devSecret: secret}
}

// migDest is an empty new server (no agencies), with files on disk.
func migDest(t *testing.T) (*Server, *secSink) {
	t.Helper()
	s, sink := newSecServer(t)
	s.identityPath = filepath.Join(t.TempDir(), "server_identity.key")
	os.WriteFile(s.identityPath, []byte(hex.EncodeToString(s.identityKey().Bytes())+"\n"), 0o600)
	for id := range s.agencies {
		delete(s.agencies, id)
	}
	s.webAdmins[userKey("", "admin")] = &WebAdmin{Username: "admin", Role: "super_admin", PasswordHash: hashPassword("new-server-admin")}
	s.saveAgencies()
	s.saveUsers()
	s.saveWebAdmins()
	return s, sink
}

func exportFile(t *testing.T, s *Server, scope string, withKey bool) []byte {
	t.Helper()
	p, err := s.buildExport(scope, withKey, "root")
	if err != nil {
		t.Fatal(err)
	}
	f, err := sealExport(p, migPass)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func importFile(t *testing.T, s *Server, file []byte, opt migrationOptions, importer string) *migrationResult {
	t.Helper()
	p, _, err := openExport(file, migPass)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := s.applyImport(p, opt, importer, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var allOn = migrationOptions{CreateAgencies: true, ImportWebAdmins: true}

// Export -> import into a fresh server, with the identity key: every account
// comes over, signs in with the old sign-in and the secure one, and a phone
// that pinned the old server's key accepts the new server.
func TestMigrationRoundTripWithIdentityKey(t *testing.T) {
	src := migSource(t)
	srcPub := src.s.identityPublic()
	dst, sink := migDest(t)
	file := exportFile(t, src.s, "", true)
	opt := allOn
	opt.UseIdentityKey = true
	res := importFile(t, dst, file, opt, userKey("", "admin"))

	if res.Agencies != 2 || res.Users != 3 || res.WebAdmins != 2 || !res.IdentityReplaced {
		t.Fatalf("result %+v", res)
	}
	a1 := dst.agencies["a1"]
	if a1 == nil || a1.ChannelCount != 8 || a1.ChannelNames[3] != "Fire Ground" || !a1.FloorControl {
		t.Fatalf("agency settings not carried over: %+v", a1)
	}
	if a1.EmergencyLMRBridge != "" {
		t.Fatal("an LMR bridge link was imported without its bridge")
	}
	if dst.channels[channelKey("a1", 0)] == nil || !dst.floorEnabledForAgency("a1") {
		t.Fatal("created agency is not live (channels / floor control)")
	}
	for _, k := range []string{"a1:medic", "a1:engine7", "a2:chief"} {
		got, want := dst.users[k], src.s.users[k]
		if got == nil || got.PasswordHash != want.PasswordHash || got.Name != want.Name || got.Role != want.Role ||
			got.Priority != want.Priority || len(got.Devices) != len(want.Devices) {
			t.Fatalf("%s: got %+v want %+v", k, got, want)
		}
	}
	if dst.users["a1:guest-visitor"] != nil || dst.users["a1:guest-old"] != nil {
		t.Fatal("guests imported by default")
	}
	// The two password stores stay separate.
	if dst.webAdmins["a2:chief"].PasswordHash != src.s.webAdmins["a2:chief"].PasswordHash ||
		dst.users["a2:chief"].PasswordHash == dst.webAdmins["a2:chief"].PasswordHash {
		t.Fatal("dashboard and phone passwords mixed up")
	}
	if dst.webAdmins[userKey("", "admin")] == nil {
		t.Fatal("the importing admin's account is gone")
	}
	// Sign-ins on the new server.
	if !login(t, dst, sink, udp(9701), "medic", "a1", "medic-password-1") {
		t.Fatal("password sign-in (challenge) failed after import")
	}
	if !login(t, dst, sink, udp(9702), "engine7", "a1", src.devSecret) {
		t.Fatal("phone-key sign-in (challenge) failed after import")
	}
	if !bytes.Equal(dst.identityPublic(), srcPub) {
		t.Fatal("identity key not taken over")
	}
	ph := &secPhone{t: t, s: dst, sink: sink, addr: udp(9703), pinned: srcPub}
	if st := ph.signIn("medic", "a1", "medic-password-1"); st != 1 {
		t.Fatalf("secure sign-in with the old pinned key failed: %d %s", st, ph.msg)
	}
	ph2 := &secPhone{t: t, s: dst, sink: sink, addr: udp(9704), pinned: srcPub}
	if st := ph2.signIn("engine7", "a1", src.devSecret); st != 1 {
		t.Fatalf("secure sign-in with a phone key failed: %d", st)
	}
	// The key file on disk is the old server's, and the old files were kept.
	if b, _ := os.ReadFile(dst.identityPath); !strings.Contains(string(b), hex.EncodeToString(src.s.identityKey().Bytes())) {
		t.Fatal("identity key file not written")
	}
	if len(res.Backups) < 4 {
		t.Fatalf("backups: %v", res.Backups)
	}
	for _, b := range res.Backups {
		if _, err := os.Stat(b); err != nil {
			t.Fatalf("backup missing: %v", err)
		}
	}
	// What was saved reloads the same.
	re := &Server{config: dst.config, users: map[string]*UserAccount{}, agencies: map[string]*Agency{}, webAdmins: map[string]*WebAdmin{}}
	re.loadUsers()
	re.loadAgencies()
	re.loadWebAdmins()
	if len(re.users) != len(dst.users) || len(re.agencies) != 2 || len(re.webAdmins) != 3 {
		t.Fatalf("saved files: %d users %d agencies %d admins", len(re.users), len(re.agencies), len(re.webAdmins))
	}
}

// Without the identity key the accounts still work (both sign-ins); the
// new server has its own key, which a phone that pinned the old one refuses
// at the same address.
func TestMigrationWithoutIdentityKey(t *testing.T) {
	src := migSource(t)
	dst, sink := migDest(t)
	before := dst.identityPublic()
	res := importFile(t, dst, exportFile(t, src.s, "", false), allOn, userKey("", "admin"))
	if res.IdentityReplaced || !bytes.Equal(dst.identityPublic(), before) {
		t.Fatal("identity changed without being asked")
	}
	if bytes.Equal(dst.identityPublic(), src.s.identityPublic()) {
		t.Fatal("test servers share a key")
	}
	if !login(t, dst, sink, udp(9711), "medic", "a1", "medic-password-1") {
		t.Fatal("password sign-in failed")
	}
	ph := &secPhone{t: t, s: dst, sink: sink, addr: udp(9712)} // first contact with the new server
	if st := ph.signIn("medic", "a1", "medic-password-1"); st != 1 {
		t.Fatal("secure sign-in on the new key failed")
	}
	// Asking for the key when the file has none is refused.
	p, _, _ := openExport(exportFile(t, src.s, "", false), migPass)
	o := allOn
	o.UseIdentityKey = true
	if _, _, err := dst.applyImport(p, o, "", "admin"); err == nil {
		t.Fatal("use identity key accepted for a file without one")
	}
}

func TestMigrationFileIsAuthenticated(t *testing.T) {
	src := migSource(t)
	file := exportFile(t, src.s, "", true)
	if _, _, err := openExport(file, "wrong passphrase here"); err == nil || err != errExportPassphrase {
		t.Fatalf("wrong passphrase: %v", err)
	}
	var env exportEnvelope
	json.Unmarshal(file, &env)

	mut := func(f func(e *exportEnvelope)) []byte {
		e := env
		f(&e)
		b, _ := json.Marshal(e)
		return b
	}
	ct, _ := base64.StdEncoding.DecodeString(env.Data)
	ct[len(ct)/2] ^= 1
	cases := map[string][]byte{
		"flipped ciphertext bit": mut(func(e *exportEnvelope) { e.Data = base64.StdEncoding.EncodeToString(ct) }),
		"changed header":         mut(func(e *exportEnvelope) { e.ServerVersion = "v9.9.9" }),
		"changed date":           mut(func(e *exportEnvelope) { e.CreatedAt = "2020-01-01T00:00:00Z" }),
	}
	for name, f := range cases {
		if _, _, err := openExport(f, migPass); err != errExportPassphrase {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string][]byte{
		"newer format":  mut(func(e *exportEnvelope) { e.Version = exportVersion + 1 }),
		"huge scrypt":   mut(func(e *exportEnvelope) { e.KDF.N = 1 << 24 }),
		"weak scrypt":   mut(func(e *exportEnvelope) { e.KDF.N = 1 << 10 }),
		"other cipher":  mut(func(e *exportEnvelope) { e.Cipher = "none" }),
		"not an export": []byte(`{"hello":1}`),
		"not json":      []byte("PK\x03\x04 zip"),
	}
	for name, f := range bad {
		_, _, err := openExport(f, migPass)
		if err == nil || err == errExportPassphrase {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := openExport(bad["newer format"], migPass); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer format message: %v", err)
	}
}

// Existing accounts: skipped by default, replaced only when chosen; a
// replaced account's phones that signed in with a credential that no longer
// exists are signed out, the others stay; the importing admin's own account
// is never touched.
func TestMigrationSkipReplaceAndSessions(t *testing.T) {
	src := migSource(t)
	file := exportFile(t, src.s, "", false)
	dst, sink := migDest(t)
	dst.agencies["a1"] = &Agency{ID: "a1", Name: "Here", MaxUsers: 5}
	dst.ensureAgencyChannels("a1")
	addUser(dst, "a1", "medic", "old-medic-password")
	addUser(dst, "a1", "Engine7", "old-engine-password") // same account, other case
	dst.users["a1:Engine7"].Devices = []*DeviceKey{{ID: "0011223344556677", Hash: hashPassword(src.devSecret)}}
	dst.webAdmins[userKey("", "root")] = &WebAdmin{Username: "root", Role: "super_admin", PasswordHash: hashPassword("importer-pw")}

	// Skip (default).
	res := importFile(t, dst, file, allOn, userKey("", "root"))
	if dst.users["a1:medic"].PasswordHash != hashPassword("old-medic-password") || dst.agencies["a1"].Name != "Here" {
		t.Fatal("skip mode changed an existing account or agency")
	}
	if res.Agencies != 1 || dst.agencies["a2"] == nil {
		t.Fatalf("missing agency not created: %+v", res)
	}
	if dst.webAdmins[userKey("", "root")].PasswordHash != hashPassword("importer-pw") {
		t.Fatal("the importing admin's account was replaced")
	}

	// Sessions before the replace.
	if !login(t, dst, sink, udp(9721), "medic", "a1", "old-medic-password") {
		t.Fatal("setup")
	}
	join(dst, udp(9721), "medic", 1)
	if !login(t, dst, sink, udp(9722), "Engine7", "a1", src.devSecret) {
		t.Fatal("setup")
	}
	join(dst, udp(9722), "Engine7", 1)

	o := allOn
	o.Replace = true
	res = importFile(t, dst, file, o, userKey("", "root"))
	if dst.users["a1:medic"].PasswordHash != hashPassword("medic-password-1") {
		t.Fatal("replace did not replace")
	}
	if dst.users["a1:Engine7"] != nil || dst.users["a1:engine7"] == nil {
		t.Fatal("replace kept the old-case record beside the new one")
	}
	if dst.webAdmins[userKey("", "root")].PasswordHash != hashPassword("importer-pw") {
		t.Fatal("replace mode replaced the importing admin's own account")
	}
	if dst.isAuthenticated(udp(9721)) {
		t.Fatal("a phone signed in with the replaced password is still on")
	}
	if res.SessionsEnded == 0 {
		t.Fatal("no sessions reported ended")
	}
	if !login(t, dst, sink, udp(9723), "medic", "a1", "medic-password-1") {
		t.Fatal("the imported password does not work")
	}
}

func TestMigrationOptionsAgenciesGuestsAdmins(t *testing.T) {
	src := migSource(t)
	file := exportFile(t, src.s, "", false)

	dst, _ := migDest(t)
	dst.agencies["a1"] = &Agency{ID: "a1", Name: "A1"}
	res := importFile(t, dst, file, migrationOptions{}, userKey("", "admin"))
	if dst.agencies["a2"] != nil || dst.users["a2:chief"] != nil {
		t.Fatal("agency created / user imported with creating agencies off")
	}
	if len(dst.webAdmins) != 1 || res.WebAdmins != 0 {
		t.Fatal("dashboard accounts imported without being chosen")
	}
	if dst.users["a1:medic"] == nil {
		t.Fatal("user of an existing agency not imported")
	}

	dst2, _ := migDest(t)
	importFile(t, dst2, file, migrationOptions{CreateAgencies: true, IncludeGuests: true}, "")
	if dst2.users["a1:guest-visitor"] == nil {
		t.Fatal("guest not imported when chosen")
	}
	if dst2.users["a1:guest-old"] != nil {
		t.Fatal("expired guest imported")
	}

	// One agency only.
	dst3, _ := migDest(t)
	importFile(t, dst3, exportFile(t, src.s, "a2", false), allOn, "")
	if dst3.agencies["a1"] != nil || dst3.users["a2:chief"] == nil || dst3.webAdmins["a2:chief"] == nil || dst3.webAdmins[":root"] != nil {
		t.Fatalf("single-agency export: agencies %v", dst3.agencies)
	}
}

// Everything in the file is untrusted.
func TestMigrationValidatesRecords(t *testing.T) {
	dst, _ := migDest(t)
	p := &exportPayload{FormatVersion: 1,
		Agencies: []*Agency{
			{ID: "ok", Name: "Fine"},
			{ID: "bad id", Name: "x"},
			{ID: "nl", Name: "line\nbreak"},
			{ID: "ch", Name: "Ch", ChannelNames: map[int]string{120: "LMR"}},
			{ID: "ok", Name: "Twice"},
		},
		Users: []*UserAccount{
			{Username: "good", AgencyID: "ok", PasswordHash: hashPassword("p")},
			{Username: "<script>", AgencyID: "ok", PasswordHash: hashPassword("p")},
			{Username: "nohash", AgencyID: "ok", PasswordHash: "secret"},
			{Username: "dev", AgencyID: "ok", PasswordHash: hashPassword("p"), Devices: []*DeviceKey{{ID: "zz", Hash: "x"}}},
			{Username: "role", AgencyID: "ok", PasswordHash: hashPassword("p"), Role: "root"},
			{Username: "orphan", AgencyID: "nowhere", PasswordHash: hashPassword("p")},
			{Username: "GOOD", AgencyID: "ok", PasswordHash: hashPassword("p")},
			nil,
		},
		WebAdmins: []*WebAdmin{
			{Username: "w1", Role: "agency_admin", AgencyID: "ok", PasswordHash: "plain-text"},
			{Username: "w2", Role: "god", AgencyID: "ok", PasswordHash: hashPassword("p")},
		},
	}
	o := allOn
	dst.mu.RLock()
	plan, err := dst.planImportLocked(p, o, "", time.Now())
	dst.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"agency:ok": "create", "agency:bad id": "skip", "agency:nl": "skip", "agency:ch": "skip",
		"user:good": "new", "user:<script>": "skip", "user:nohash": "skip", "user:dev": "skip",
		"user:role": "skip", "user:orphan": "skip", "user:GOOD": "skip",
		"dashboard:w1": "skip", "dashboard:w2": "skip",
	}
	got := map[string]string{}
	for _, it := range plan.Items {
		k := it.Kind + ":" + it.Name
		if it.Kind == "agency" {
			k = it.Kind + ":" + it.Agency
		}
		if _, dup := got[k]; !dup {
			got[k] = it.Action
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	res, _, err := dst.applyImport(p, o, "", "admin")
	if err != nil || res.Users != 1 || res.Agencies != 1 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	// A bad identity key is refused outright.
	p.IdentityKey = "00"
	dst.mu.RLock()
	_, err = dst.planImportLocked(p, o, "", time.Now())
	dst.mu.RUnlock()
	if err == nil {
		t.Fatal("bad identity key accepted")
	}
}

// If saving fails, nothing changes: not the files, not memory.
func TestMigrationIsAllOrNothing(t *testing.T) {
	src := migSource(t)
	file := exportFile(t, src.s, "", false)
	dst, _ := migDest(t)
	agBefore, _ := os.ReadFile(dst.config.AgenciesFile)
	// web_admins.json is written last: make it fail.
	dst.config.WebAdminsFile = filepath.Join(t.TempDir(), "missing-dir", "web_admins.json")
	p, _, _ := openExport(file, migPass)
	if _, _, err := dst.applyImport(p, allOn, "", "admin"); err == nil {
		t.Fatal("import reported success with an unwritable file")
	}
	if len(dst.agencies) != 0 || len(dst.users) != 0 {
		t.Fatal("memory changed by a failed import")
	}
	if agAfter, _ := os.ReadFile(dst.config.AgenciesFile); !bytes.Equal(agBefore, agAfter) {
		t.Fatal("agencies.json not restored after a failed import")
	}
}

func multipartImport(t *testing.T, file []byte, fields map[string]string) (*bytes.Buffer, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "users.mrusers")
	fw.Write(file)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	return &b, mw.FormDataContentType()
}

// Through the dashboard: only the super admin gets in; nothing secret is
// logged; preview changes nothing; import applies.
func TestMigrationHTTPPermissionsAndLogs(t *testing.T) {
	s := newWebTestServer(t)
	s.identityPath = filepath.Join(t.TempDir(), "server_identity.key")
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	s.identity = k
	addUser(s, "fire", "medic", "medic-password-1")
	h := s.webHandler()
	body := `{"agency":"","include_identity_key":true,"passphrase":"` + migPass + `"}`

	for _, tok := range []string{"fireadmin", "firedispatch", ""} {
		if rec := doReq(h, "POST", "/api/user-migration/export", tok, body); rec.Code != 403 && rec.Code != 401 {
			t.Fatalf("export as %q: %d", tok, rec.Code)
		}
		for _, path := range []string{"/api/user-migration/preview", "/api/user-migration/import"} {
			if rec := doReq(h, "POST", path, tok, ""); rec.Code != 403 && rec.Code != 401 {
				t.Fatalf("%s as %q: %d", path, tok, rec.Code)
			}
		}
	}
	if rec := doReq(h, "POST", "/api/user-migration/export", "super", `{"passphrase":"short"}`); rec.Code != 400 {
		t.Fatalf("short passphrase accepted: %d", rec.Code)
	}

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	rec := doReq(h, "POST", "/api/user-migration/export", "super", body)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), ".mrusers") {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	file := rec.Body.Bytes()
	if bytes.Contains(file, []byte("medic")) || bytes.Contains(file, []byte(hashPassword("medic-password-1"))) {
		t.Fatal("export file is not encrypted")
	}

	// Import into another server, preview first.
	d := newWebTestServer(t)
	d.identityPath = filepath.Join(t.TempDir(), "server_identity.key")
	dk, _ := ecdh.X25519().GenerateKey(rand.Reader)
	d.identity = dk
	delete(d.agencies, "fire")
	dh := d.webHandler()
	fields := map[string]string{"passphrase": migPass, "mode": "skip", "create_agencies": "1", "use_identity_key": "1"}
	mb, ct := multipartImport(t, file, fields)
	req := httptest.NewRequest("POST", "/api/user-migration/preview", mb)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie("super"))
	prec := httptest.NewRecorder()
	dh.ServeHTTP(prec, req)
	if prec.Code != 200 || !strings.Contains(prec.Body.String(), `"action":"create"`) {
		t.Fatalf("preview: %d %s", prec.Code, prec.Body.String())
	}
	if d.agencies["fire"] != nil || d.users["fire:medic"] != nil || bytes.Equal(d.identityPublic(), s.identityPublic()) {
		t.Fatal("preview changed something")
	}
	// Wrong passphrase.
	bad := map[string]string{"passphrase": "not the passphrase", "create_agencies": "1"}
	mb, ct = multipartImport(t, file, bad)
	req = httptest.NewRequest("POST", "/api/user-migration/import", mb)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie("super"))
	brec := httptest.NewRecorder()
	dh.ServeHTTP(brec, req)
	if brec.Code != 400 || d.users["fire:medic"] != nil {
		t.Fatalf("wrong passphrase import: %d", brec.Code)
	}
	// The real import.
	mb, ct = multipartImport(t, file, fields)
	req = httptest.NewRequest("POST", "/api/user-migration/import", mb)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie("super"))
	irec := httptest.NewRecorder()
	dh.ServeHTTP(irec, req)
	if irec.Code != 200 || d.users["fire:medic"] == nil || !bytes.Equal(d.identityPublic(), s.identityPublic()) {
		t.Fatalf("import: %d %s", irec.Code, irec.Body.String())
	}

	logs := logBuf.String()
	for name, secret := range map[string]string{
		"passphrase":   migPass,
		"password":     hashPassword("medic-password-1"),
		"identity key": hex.EncodeToString(k.Bytes()),
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the %s appears in the log", name)
		}
	}
	if !strings.Contains(logs, "User export by") || !strings.Contains(logs, "User import by") {
		t.Fatalf("export/import not logged: %s", logs)
	}
	for _, act := range []*ActivityLog{s.activityLog, d.activityLog} {
		for _, ev := range act.GetRecent(50) {
			if strings.Contains(ev.Details, migPass) || strings.Contains(ev.Details, hex.EncodeToString(k.Bytes())) {
				t.Fatal("secret in the activity log")
			}
		}
	}
	found := 0
	for _, ev := range s.activityLog.GetRecent(50) {
		if ev.Type == "users_exported" && ev.Username == "root" {
			found++
		}
	}
	for _, ev := range d.activityLog.GetRecent(50) {
		if ev.Type == "users_imported" {
			found++
		}
	}
	if found != 2 {
		t.Fatal("export/import missing from the activity log")
	}
}

func cookie(token string) *http.Cookie { return &http.Cookie{Name: "session", Value: token} }
