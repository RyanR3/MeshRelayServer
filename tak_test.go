package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTAK: a TAK Server stand-in — certificate enrollment over HTTPS and a TLS
// CoT stream that requires a client certificate signed by its CA.
type fakeTAK struct {
	caCert   *x509.Certificate
	caKey    *rsa.PrivateKey
	enroll   *httptest.Server
	stream   net.Listener
	api      net.Listener
	putUID   string
	putBody  []map[string]interface{}
	mu       sync.Mutex
	conn     net.Conn
	received strings.Builder
	connCh   chan struct{}
}

func newCert(t *testing.T, tmpl *x509.Certificate, parent *x509.Certificate, pub *rsa.PublicKey, signer *rsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func startFakeTAK(t *testing.T) *fakeTAK {
	t.Helper()
	f := &fakeTAK{connCh: make(chan struct{}, 4)}
	f.caKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test TAK CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	f.caCert = newCert(t, caTmpl, caTmpl, &f.caKey.PublicKey, f.caKey)

	// Enrollment API.
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { u, p, ok := r.BasicAuth(); return ok && u == "dispatch" && p == "secret" }
	mux.HandleFunc("/Marti/api/tls/config", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `<certificateConfig><nameEntries><nameEntry name="O" value="TAK Test"/><nameEntry name="OU" value="Sullivan"/></nameEntries></certificateConfig>`)
	})
	mux.HandleFunc("/Marti/api/tls/signClient/v2", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), ""))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil || csr.Subject.CommonName != "dispatch" || csr.Subject.Organization[0] != "TAK Test" {
			w.WriteHeader(400)
			return
		}
		cert := newCert(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: csr.Subject,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, f.caCert, csr.PublicKey.(*rsa.PublicKey), f.caKey)
		json.NewEncoder(w).Encode(map[string]string{
			"signedCert": base64.StdEncoding.EncodeToString(cert.Raw),
			"ca0":        base64.StdEncoding.EncodeToString(f.caCert.Raw),
		})
	})
	f.enroll = httptest.NewTLSServer(mux)
	t.Cleanup(f.enroll.Close)

	// CoT stream: server cert from the same CA (for a hostname, as real TAK
	// servers often are), client certificate required.
	srvKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srvCert := newCert(t, &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "takserver.example"},
		DNSNames: []string{"takserver.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, f.caCert, &srvKey.PublicKey, f.caKey)
	pool := x509.NewCertPool()
	pool.AddCert(f.caCert)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvCert.Raw}, PrivateKey: srvKey}},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.stream = ln
	t.Cleanup(func() { ln.Close() })

	// HTTPS API (client certificate required): groups.
	groups := []map[string]interface{}{
		{"name": "Fire", "direction": "IN", "type": "LDAP", "bitpos": 3.0, "active": true},
		{"name": "Fire", "direction": "OUT", "type": "LDAP", "bitpos": 3.0, "active": true},
		{"name": "EMS", "direction": "IN", "type": "LDAP", "bitpos": 4.0, "active": true, "description": "Ambulance"},
		{"name": "EMS", "direction": "OUT", "type": "LDAP", "bitpos": 4.0, "active": true},
	}
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/Marti/api/groups/all", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"version": "3", "data": groups})
	})
	apiMux.HandleFunc("/Marti/api/groups/active", func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.putUID, f.putBody = r.URL.Query().Get("clientUid"), body
		for _, g := range body { // reflect the choice in later listings
			for _, cur := range groups {
				if cur["name"] == g["name"] && cur["direction"] == g["direction"] {
					cur["active"] = g["active"]
				}
			}
		}
		f.mu.Unlock()
	})
	apiLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvCert.Raw}, PrivateKey: srvKey}},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.api = apiLn
	go http.Serve(apiLn, apiMux)
	t.Cleanup(func() { apiLn.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conn = c
			f.mu.Unlock()
			f.connCh <- struct{}{}
			go func() {
				r := bufio.NewReader(c)
				buf := make([]byte, 4096)
				for {
					n, err := r.Read(buf)
					if n > 0 {
						f.mu.Lock()
						f.received.Write(buf[:n])
						f.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return f
}

func (f *fakeTAK) send(cot string) {
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	c.Write([]byte(cot))
}

func (f *fakeTAK) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTakConnectorEndToEnd(t *testing.T) {
	s, dc, _, _ := dispatchTestServer(t)
	dc.Text = make(chan []byte, 64)
	s.agencies = map[string]*Agency{"ag": {ID: "ag", Name: "Agency"}}
	s.clients[userKey("ag", "alice")].Loc = &UnitLocation{Lat: 41.65, Lon: -74.69}
	f := startFakeTAK(t)

	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	streamPort := f.stream.Addr().(*net.TCPAddr).Port
	cfg := TakConfig{AgencyID: "ag", Host: "127.0.0.1", StreamPort: streamPort, EnrollPort: enrollPort,
		Username: "dispatch", Callsign: "MeshRelay Dispatch", UID: "MeshRelay-Dispatch-ag-test", Enabled: true}

	if err := takEnroll(&cfg, "wrong"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("bad password should be reported, got %v", err)
	}
	if err := takEnroll(&cfg, "secret"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if cfg.CertPEM == "" || len(cfg.CAPEMs) != 1 {
		t.Fatalf("enrollment did not store cert + CA")
	}

	s.startTak(cfg)
	defer s.stopTak("ag")
	select {
	case <-f.connCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("connector never connected (status %v)", s.takStatusJSON("ag"))
	}
	waitFor(t, "presence CoT", func() bool { return strings.Contains(f.got(), `callsign="MeshRelay Dispatch"`) })

	// TAK users: a real ATAK user, and alice's own MeshRelay phone reporting into TAK.
	f.send(`<event version="2.0" type="a-f-G-U-C" uid="ANDROID-abc" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.70" lon="-74.70"/><detail><contact callsign="Bravo-1"/><__group name="Cyan" role="Team Member"/><status battery="55"/></detail></event>`)
	f.send(`<event version="2.0" type="a-f-G-U-C" uid="MeshRelay-1234" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.65" lon="-74.69"/><detail><contact callsign="alice"/><takv device="MeshRelay"/></detail></event>`)
	waitFor(t, "TAK unit", func() bool { return len(s.takUnitsFor("ag")) == 1 })
	u := s.takUnitsFor("ag")[0]
	if u["callsign"] != "Bravo-1" || u["battery"] != 55 || u["group"] != "Cyan" {
		t.Fatalf("TAK unit: %v", u)
	}

	// GeoChat to dispatch -> console Messages.
	f.send(`<event version="2.0" type="b-t-f" uid="GeoChat.ANDROID-abc.x.1" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0"/><detail><__chat chatroom="MeshRelay Dispatch" id="MeshRelay-Dispatch-ag-test" senderCallsign="Bravo-1"><chatgrp uid0="ANDROID-abc" uid1="MeshRelay-Dispatch-ag-test"/></__chat><remarks>Need water at the CP</remarks></detail></event>`)
	waitFor(t, "chat in", func() bool { return len(s.messageHistory("ag")) == 1 })
	in := s.messageHistory("ag")[0]
	if in.From != "Bravo-1" || in.Text != "Need water at the CP" || in.TakUID != "ANDROID-abc" {
		t.Fatalf("incoming TAK chat: %+v", in)
	}

	// Console -> TAK user.
	s.consoleSendTakMessage(s.clients[userKey("ag", "dispatcher")], "ag", "ANDROID-abc", "Water en route")
	waitFor(t, "chat out", func() bool {
		g := f.got()
		return strings.Contains(g, `type="b-t-f"`) && strings.Contains(g, "Water en route") && strings.Contains(g, `uid1="ANDROID-abc"`) &&
			!strings.Contains(g, "<marti>") && regexp.MustCompile(`messageId="[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"`).MatchString(g)
	})

	// TAK 911 -> console emergency; MeshRelay emergency -> TAK.
	f.send(`<event version="2.0" type="b-a-o-tbl" uid="ANDROID-abc-9-1-1" time="2026-01-01T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="41.70" lon="-74.70"/><detail><emergency type="911 Alert">Bravo-1</emergency></detail></event>`)
	waitFor(t, "TAK emergency on console", func() bool {
		for len(dc.Text) > 0 {
			var m map[string]interface{}
			json.Unmarshal(<-dc.Text, &m)
			if m["type"] == "emergency" && m["user"] == "Bravo-1 (TAK)" && m["active"] == true && m["lat"] == 41.70 {
				return true
			}
		}
		return false
	})
	s.notifyDispatchEmergency("ag", 0, "alice", true)
	waitFor(t, "MeshRelay emergency in TAK", func() bool {
		g := f.got()
		return strings.Contains(g, `type="b-a-o-tbl"`) && strings.Contains(g, `uid="MeshRelay-911-ag-alice"`) && strings.Contains(g, `lat="41.650000"`)
	})

	if st := s.takStatusJSON("ag"); st["status"] != "connected" || st["enrolled"] != true {
		t.Fatalf("status: %v", st)
	}
}

func TestTakRejectsServerFromAnotherCA(t *testing.T) {
	f := startFakeTAK(t)
	other := startFakeTAK(t) // enrolled with f, but connect to `other`
	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	cfg := TakConfig{AgencyID: "ag", Host: "127.0.0.1", EnrollPort: enrollPort, Username: "dispatch", UID: "u"}
	if err := takEnroll(&cfg, "secret"); err != nil {
		t.Fatal(err)
	}
	tlsCfg, _ := cfg.tlsConfig()
	_, err := tls.Dial("tcp", other.stream.Addr().String(), tlsCfg)
	if err == nil {
		t.Fatalf("a TAK server signed by a different CA must be refused")
	}
	_ = pem.Block{}
}

func TestTakGroupsListAndApply(t *testing.T) {
	s, _, _, _ := dispatchTestServer(t)
	s.agencies = map[string]*Agency{"ag": {ID: "ag"}}
	f := startFakeTAK(t)
	enrollPort, _ := strconv.Atoi(strings.Split(f.enroll.Listener.Addr().String(), ":")[1])
	cfg := TakConfig{AgencyID: "ag", Host: "127.0.0.1", StreamPort: f.stream.Addr().(*net.TCPAddr).Port,
		EnrollPort: enrollPort, APIPort: f.api.Addr().(*net.TCPAddr).Port, Username: "dispatch",
		Callsign: "MeshRelay Dispatch", UID: "MeshRelay-Dispatch-ag-test", Enabled: true}
	if err := takEnroll(&cfg, "secret"); err != nil {
		t.Fatal(err)
	}

	groups, err := takListGroups(&cfg)
	if err != nil || len(groups) != 2 || groups[0].Name != "EMS" || groups[0].Description != "Ambulance" || !groups[1].Active {
		t.Fatalf("groups: %+v %v", groups, err)
	}

	cfg.Groups = []string{"Fire"}
	if err := takApplyGroups(&cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	uid, body := f.putUID, f.putBody
	f.mu.Unlock()
	if uid != "MeshRelay-Dispatch-ag-test" || len(body) != 4 {
		t.Fatalf("PUT active groups: uid %q, %d entries", uid, len(body))
	}
	for _, g := range body {
		if (g["name"] == "Fire") != (g["active"] == true) || g["bitpos"] == nil {
			t.Fatalf("group %v sent wrong (other fields must be kept)", g)
		}
	}
	groups, _ = takListGroups(&cfg)
	if groups[0].Active || !groups[1].Active {
		t.Fatalf("after apply, only Fire should be active: %+v", groups)
	}

	// The choice is re-applied whenever the connector (re)connects.
	f.mu.Lock()
	f.putUID = ""
	f.mu.Unlock()
	s.startTak(cfg)
	defer s.stopTak("ag")
	for i := 0; i < 200; i++ {
		f.mu.Lock()
		got := f.putUID
		f.mu.Unlock()
		if got != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("groups were not applied after connecting")
}
