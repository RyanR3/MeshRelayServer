// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// TAK Server connector — one per agency, configured in the dashboard.
//
// The MeshRelay server enrolls with the agency's TAK Server the way ATAK does
// (username/password -> client certificate on the enrollment port, 8446) and
// keeps a TLS CoT stream open (8089). Then:
//
//   - TAK users' positions (PLI) show on the agency's dispatch consoles as TAK
//     units. MeshRelay phones also report into TAK themselves; those copies are
//     hidden when the phone is already on the MeshRelay server.
//   - The connector appears in TAK as a contact ("MeshRelay Dispatch" by
//     default). GeoChat sent to it lands in the console's Messages panel, and a
//     console can message any TAK user (GeoChat).
//   - TAK emergencies (911, ring the bell, TIC…) appear in the console's
//     Emergencies panel; MeshRelay emergencies are sent into TAK as 911 alerts.
//
// Stored in tak_connections.json (0600): the client certificate and key the
// enrollment returned — never the password. The server-wide master TAK
// connection, shared by agencies that opt in, is in tak_master.go.

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const takConnectionsFile = "tak_connections.json"

type TakConfig struct {
	AgencyID   string `json:"agency_id"`
	Host       string `json:"host"`
	StreamPort int    `json:"stream_port"` // TLS CoT, usually 8089
	EnrollPort int    `json:"enroll_port"` // certificate enrollment, usually 8446
	APIPort    int    `json:"api_port"`    // HTTPS API with client cert, usually 8443
	// Groups: TAK groups ("channels" in ATAK) dispatch is active in. Empty =
	// leave TAK's default (every group the account belongs to).
	Groups     []string `json:"groups,omitempty"`
	Username   string   `json:"username"`
	Callsign   string   `json:"callsign"` // how dispatch appears in TAK
	UID        string   `json:"uid"`
	Enabled    bool     `json:"enabled"`
	CertPEM    string   `json:"cert_pem,omitempty"`
	KeyPEM     string   `json:"key_pem,omitempty"`
	CAPEMs     []string `json:"ca_pems,omitempty"`
	EnrolledAt string   `json:"enrolled_at,omitempty"`
	// Managed: set up by the managing platform (infra-TAK), which renews it.
	// Saving the connection from the dashboard hands it back to the operator.
	Managed bool `json:"managed,omitempty"`
	// HomeAgency (master TAK connection only, tak_master.go): the agency
	// whose consoles get messages from TAK users no agency has written to.
	// Empty or unknown: the fallback in resolveTakMasterHome.
	HomeAgency string `json:"home_agency,omitempty"`
}

// TakUnit is a TAK user seen on the stream.
type TakUnit struct {
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Speed    float64   `json:"speed,omitempty"`
	Course   float64   `json:"course,omitempty"`
	Battery  int       `json:"battery,omitempty"`
	Group    string    `json:"group,omitempty"`
	Role     string    `json:"role,omitempty"`
	Device   string    `json:"device,omitempty"`
	At       time.Time `json:"at"`
	Stale    time.Time `json:"stale"`
}

type takConn struct {
	s      *Server
	cfg    TakConfig
	mu     sync.Mutex
	conn   net.Conn
	status string // connecting | connected | error | disabled
	errMsg string
	units  map[string]*TakUnit
	stop   chan struct{}
	since  time.Time
	alerts map[string]*takAlert // alertKey(agency, user) -> 911 alert state (takEmergency)
	// master: the server-wide master TAK connection (tak_master.go), shared
	// by the agencies that opted in, instead of one agency's own.
	master bool
	// peerFP: SHA-256 of the certificate the streaming port presented on the
	// last connect (hex), used to recognise the same TAK server twice.
	peerFP string
	// chatPeers (master only, tak_master.go): TAK user UID -> agency -> when
	// that agency's console last messaged the user. Routes the user's
	// replies. Memory only, guarded by mu.
	chatPeers map[string]map[string]time.Time
}

// takAlert: one MeshRelay user's 911 alert on this TAK server. The phone may
// send ATAK's own alert over its own TAK link; while it does, the server
// withholds its MeshRelay-911-... alert so TAK users see one alert per unit.
type takAlert struct {
	agency    string    // the user's agency (always the connection's own on an agency connection)
	user      string    // MeshRelay user, as takEmergency got it
	active    bool      // the MeshRelay emergency is on
	ownOut    bool      // the server's own alert is out on TAK (not cancelled)
	phoneAt   time.Time // the phone's own alert last seen on the feed (zero = never)
	hidden    bool      // the phone's alert is kept off the console (MeshRelay one shows)
	shownName string    // the phone's alert was put on the console under this name + " (TAK)"
	clearedAt time.Time // when the MeshRelay emergency ended
	emergAt   time.Time // last time the unit (of any agency) was in a MeshRelay emergency
	sentAt    time.Time // the server's alert last written (refresh timing)
}

type takManager struct {
	mu    sync.Mutex
	conns map[string]*takConn // agency -> connection
	cfgs  map[string]TakConfig
	// The master TAK connection (tak_master.go): configured by the super
	// admin, used by the agencies that switched sharing on.
	master    *takConn
	masterCfg *TakConfig
}

func (s *Server) tak() *takManager {
	s.takOnce.Do(func() {
		s.takMgr = &takManager{conns: map[string]*takConn{}, cfgs: map[string]TakConfig{}}
	})
	return s.takMgr
}

// --- persistence ------------------------------------------------------------

func (s *Server) loadTakConnections() {
	data, err := os.ReadFile(takConnectionsFile)
	if err != nil {
		return
	}
	var cfgs []TakConfig
	if json.Unmarshal(data, &cfgs) != nil {
		log.Printf("TAK: could not parse %s", takConnectionsFile)
		return
	}
	m := s.tak()
	m.mu.Lock()
	for _, c := range cfgs {
		m.cfgs[c.AgencyID] = c
	}
	m.mu.Unlock()
	for _, c := range cfgs {
		if c.Enabled && c.CertPEM != "" {
			s.startTak(c)
		}
	}
	log.Printf("TAK: loaded %d connection(s)", len(cfgs))
}

func (s *Server) saveTakConnections() error {
	m := s.tak()
	m.mu.Lock()
	var list []TakConfig
	for _, c := range m.cfgs {
		list = append(list, c)
	}
	m.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].AgencyID < list[j].AgencyID })
	// TAK credentials: owner-only, written atomically (os.WriteFile kept the
	// mode of a file created 0644 by an older version).
	return writeJSONFileAtomic(takConnectionsFile, list)
}

// --- enrollment -------------------------------------------------------------

var nameEntryRe = regexp.MustCompile(`nameEntry[^>]*name="([A-Z]+)"[^>]*value="([^"]*)"`)

// takEnroll gets a client certificate from the TAK Server's enrollment API.
// The enrollment port's own certificate is not verified (as in ATAK: the
// server's CA comes back with the signed certificate and is pinned after).
func takEnroll(cfg *TakConfig, password string) error {
	return takEnrollVerified(cfg, password, nil)
}

// takEnrollVerified enrolls like takEnroll; with trustPEMs set (the TAK CA
// bundle a co-located platform reads from the TAK box) the enrollment port's
// certificate chain must verify against those CAs or the public roots.
func takEnrollVerified(cfg *TakConfig, password string, trustPEMs []string) error {
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	if len(trustPEMs) > 0 {
		tlsCfg = enrollTLSConfig(trustPEMs)
	}
	return takEnrollWith(cfg, password, tlsCfg, nil)
}

// --- dashboard enrollment: a confirmed certificate, and no internal hosts ----
//
// The dashboard path used to send the TAK username and password to whatever
// answered on the enrollment port (no certificate check), and let an agency
// admin point the server at any internal address and read the errors back.
// Now: the host must be a plain name or address; the server will not connect
// to loopback or link-local addresses for an agency admin; and the password
// is only sent to an enrollment port whose certificate is publicly trusted,
// or whose exact certificate the admin confirmed by fingerprint.

// takHostAllowed validates what a dashboard user typed.
func takHostAllowed(host string, ports ...int) error {
	if !managedTakHostRe.MatchString(host) {
		return errors.New("TAK server address must be a host name or IP address (no scheme, path or port)")
	}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return errors.New("TAK ports must be between 1 and 65535")
		}
	}
	return nil
}

// takDialControl refuses connections to this machine and to link-local
// addresses (cloud metadata lives there) unless allowLocal.
func takDialControl(allowLocal bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowLocal {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("that address is not allowed for a TAK server")
		}
		return nil
	}
}

// takCertInfo is what the admin is asked to confirm.
type takCertInfo struct {
	Fingerprint string `json:"fingerprint"` // SHA-256 of the certificate, hex
	Subject     string `json:"subject"`
	Issuer      string `json:"issuer"`
	NotAfter    string `json:"not_after"`
}

// takProbeCert reads the enrollment port's certificate WITHOUT sending any
// credentials. trusted is true when it verifies against the public roots for
// that host name (nothing to confirm then).
func takProbeCert(host string, port int, allowLocal bool) (info takCertInfo, trusted bool, err error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: takDialControl(allowLocal)}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true, ServerName: host})
	if err != nil {
		return info, false, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return info, false, errors.New("no certificate")
	}
	leaf := certs[0]
	sum := sha256.Sum256(leaf.Raw)
	info = takCertInfo{Fingerprint: hex.EncodeToString(sum[:]), Subject: leaf.Subject.String(),
		Issuer: leaf.Issuer.String(), NotAfter: leaf.NotAfter.Format(time.RFC3339)}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter})
	return info, verr == nil, nil
}

// takEnrollPinned enrolls, sending the password only to an enrollment port
// that presents exactly the confirmed certificate (fingerprint), or — with
// an empty fingerprint — one the public roots vouch for.
func takEnrollPinned(cfg *TakConfig, password, fingerprint string, allowLocal bool) error {
	tlsCfg := &tls.Config{ServerName: cfg.Host}
	if fingerprint != "" {
		want := strings.ToLower(strings.TrimSpace(fingerprint))
		tlsCfg.InsecureSkipVerify = true // replaced by the exact-certificate check
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
				return errors.New("the TAK server's certificate is not the one that was confirmed")
			}
			return nil
		}
	}
	return takEnrollWith(cfg, password, tlsCfg, takDialControl(allowLocal))
}

func takEnrollWith(cfg *TakConfig, password string, tlsCfg *tls.Config, control func(string, string, syscall.RawConn) error) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: control}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: dialer.DialContext},
	}
	base := fmt.Sprintf("https://%s:%d", cfg.Host, cfg.EnrollPort)
	do := func(method, url string, body io.Reader, ctype string) ([]byte, error) {
		req, _ := http.NewRequest(method, url, body)
		req.SetBasicAuth(cfg.Username, password)
		req.Header.Set("Accept", "application/json")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, errors.New("TAK server rejected the username or password")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("TAK server returned %d from %s", resp.StatusCode, url)
		}
		return b, nil
	}

	cfgXML, err := do("GET", base+"/Marti/api/tls/config", nil, "")
	if err != nil {
		return fmt.Errorf("enrollment config: %w", err)
	}
	subj := pkix.Name{CommonName: cfg.Username, Organization: []string{"TAK"}, OrganizationalUnit: []string{"TAK"}}
	for _, m := range nameEntryRe.FindAllStringSubmatch(string(cfgXML), -1) {
		switch m[1] {
		case "O":
			subj.Organization = []string{m[2]}
		case "OU":
			subj.OrganizationalUnit = []string{m[2]}
		}
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subj}, key)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	// TAK expects the base64 body without the PEM armour.
	csrBody := strings.TrimSpace(string(csrPEM))
	csrBody = strings.TrimPrefix(csrBody, "-----BEGIN CERTIFICATE REQUEST-----")
	csrBody = strings.TrimSuffix(csrBody, "-----END CERTIFICATE REQUEST-----")

	signURL := fmt.Sprintf("%s/Marti/api/tls/signClient/v2?clientUid=%s&version=3", base, urlQueryEscape(cfg.UID))
	resp, err := do("POST", signURL, strings.NewReader(strings.TrimSpace(csrBody)), "text/plain")
	if err != nil {
		return fmt.Errorf("certificate signing: %w", err)
	}
	var signed map[string]string
	if err := json.Unmarshal(resp, &signed); err != nil || signed["signedCert"] == "" {
		return errors.New("TAK server did not return a signed certificate")
	}
	certDER, err := base64.StdEncoding.DecodeString(signed["signedCert"])
	if err != nil {
		return errors.New("TAK server returned an unreadable certificate")
	}
	cfg.CertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	cfg.KeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cfg.CAPEMs = nil
	for i := 0; i < 10; i++ {
		v, ok := signed["ca"+strconv.Itoa(i)]
		if !ok {
			break
		}
		if der, err := base64.StdEncoding.DecodeString(v); err == nil {
			cfg.CAPEMs = append(cfg.CAPEMs, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
		}
	}
	cfg.EnrolledAt = time.Now().Format(time.RFC3339)
	return nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// tlsConfig: client certificate + the TAK CA as the only trusted root. TAK
// servers are usually reached by IP with certificates for a hostname, so the
// chain is verified but the name is not.
func (c *TakConfig) tlsConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("stored TAK certificate is invalid: %w", err)
	}
	pool := x509.NewCertPool()
	for _, p := range c.CAPEMs {
		pool.AppendCertsFromPEM([]byte(p))
	}
	return &tls.Config{
		// Always present the enrolled certificate. With Certificates, Go
		// only sends it when its issuer matches the server's list of
		// acceptable CA names byte for byte; some TAK servers list the CA
		// with a differently encoded name, and the connection then fails
		// with "certificate required" although the certificate is valid.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(c.CAPEMs) == 0 {
				return nil // no CA from enrollment: accept (logged at connect)
			}
			if len(raw) == 0 {
				return errors.New("TAK server sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, r := range raw[1:] {
				if ic, err := x509.ParseCertificate(r); err == nil {
					inter.AddCert(ic)
				}
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
			if err != nil {
				return fmt.Errorf("TAK server certificate not signed by its enrollment CA: %w", err)
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}

// --- connection ----------------------------------------------------------------

func (s *Server) startTak(cfg TakConfig) {
	m := s.tak()
	m.mu.Lock()
	if old := m.conns[cfg.AgencyID]; old != nil {
		old.close()
	}
	tc := &takConn{s: s, cfg: cfg, status: "connecting", units: map[string]*TakUnit{}, stop: make(chan struct{})}
	m.conns[cfg.AgencyID] = tc
	m.cfgs[cfg.AgencyID] = cfg
	m.mu.Unlock()
	go tc.run()
}

func (s *Server) stopTak(agency string) {
	m := s.tak()
	m.mu.Lock()
	if tc := m.conns[agency]; tc != nil {
		tc.close()
		delete(m.conns, agency)
	}
	m.mu.Unlock()
}

func (t *takConn) close() {
	t.mu.Lock()
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	if t.conn != nil {
		t.conn.Close()
	}
	t.mu.Unlock()
}

// label names the connection in the log: its agency, or "master".
func (t *takConn) label() string {
	if t.master {
		return "master"
	}
	return t.cfg.AgencyID
}

func (t *takConn) setStatus(status, msg string) {
	t.mu.Lock()
	t.status, t.errMsg = status, msg
	t.mu.Unlock()
}

func (t *takConn) run() {
	backoff := 2 * time.Second
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		err := t.session()
		select {
		case <-t.stop:
			return
		default:
		}
		msg := "connection closed"
		if err != nil {
			msg = err.Error()
		}
		t.setStatus("error", msg)
		log.Printf("TAK [%s]: %q — retrying in %v", t.label(), msg, backoff)
		select {
		case <-t.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (t *takConn) session() error {
	tlsCfg, err := t.cfg.tlsConfig()
	if err != nil {
		return err
	}
	t.setStatus("connecting", "")
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(t.cfg.Host, strconv.Itoa(t.cfg.StreamPort)), tlsCfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	peerFP := ""
	if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
		sum := sha256.Sum256(pc[0].Raw)
		peerFP = hex.EncodeToString(sum[:])
	}
	t.mu.Lock()
	t.conn = conn
	t.since = time.Now()
	t.peerFP = peerFP
	t.mu.Unlock()
	defer conn.Close()
	// Our own contact must be the first event on the connection: a TAK server
	// delivers direct GeoChat only to the first UID a connection announces. An
	// LMR marker sent ahead of it would take that place, and TAK users' messages
	// to dispatch would never arrive. Markers go out only on a "connected"
	// link (takDeliver), so announce before the link counts as up; a marker
	// pass that runs meanwhile (another connection coming up kicks one) then
	// cannot get in first.
	t.send(t.presenceCoT())
	t.setStatus("connected", "")
	log.Printf("TAK [%s]: connected to %q:%d as %q", t.label(), t.cfg.Host, t.cfg.StreamPort, t.cfg.Callsign)
	t.mu.Lock()
	current := t.cfg
	t.mu.Unlock()
	if len(current.Groups) > 0 {
		// TAK ties group choices to the streaming client by UID, which it
		// learns from our first presence message — so apply a moment later.
		go func(cfg TakConfig, label string) {
			time.Sleep(3 * time.Second)
			if err := takApplyGroups(&cfg); err != nil {
				log.Printf("TAK [%s]: could not set groups: %v", label, err)
			} else {
				log.Printf("TAK [%s]: active groups %q", label, cfg.Groups)
			}
		}(current, t.label())
	}

	// Presence every 30 s (also the keepalive).
	done := make(chan struct{})
	defer close(done)
	// Presence went out above, before the link counted as up; now ask for
	// the LMR bridge markers (lmr_atak.go) to be sent again.
	t.s.lmrMarkersChanged()
	go func() {
		if t.master {
			t.masterConnected()
		}
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		alerts := time.NewTicker(5 * time.Second) // one 911 alert per unit
		defer alerts.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				t.send(t.presenceCoT())
				t.send(t.pingCoT())
				t.pruneStale()
			case <-alerts.C:
				t.alertTick(time.Now())
			}
		}
	}()

	dec := xml.NewDecoder(conn)
	dec.Strict = false
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Minute)) // pings keep a quiet server talking
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "event" {
			continue
		}
		var ev cotEvent
		if err := dec.DecodeElement(&ev, &se); err != nil {
			continue
		}
		t.handle(&ev)
	}
}

// send writes one CoT document; it reports whether the write succeeded.
func (t *takConn) send(data string) bool {
	t.mu.Lock()
	c := t.conn
	t.mu.Unlock()
	if c == nil {
		return false
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(data)); err != nil {
		log.Printf("TAK [%s]: write failed: %v", t.label(), err)
		c.Close()
		return false
	}
	return true
}

func (t *takConn) pruneStale() {
	now := time.Now()
	t.mu.Lock()
	for uid, u := range t.units {
		if now.After(u.Stale.Add(5 * time.Minute)) {
			delete(t.units, uid)
		}
	}
	t.pruneChatPeersLocked(now)
	t.mu.Unlock()
}

// --- CoT ----------------------------------------------------------------------

type cotEvent struct {
	Type  string `xml:"type,attr"`
	UID   string `xml:"uid,attr"`
	Time  string `xml:"time,attr"`
	Stale string `xml:"stale,attr"`
	Point struct {
		Lat float64 `xml:"lat,attr"`
		Lon float64 `xml:"lon,attr"`
	} `xml:"point"`
	Detail struct {
		Contact *struct {
			Callsign string `xml:"callsign,attr"`
		} `xml:"contact"`
		Group *struct {
			Name string `xml:"name,attr"`
			Role string `xml:"role,attr"`
		} `xml:"__group"`
		Status *struct {
			Battery string `xml:"battery,attr"`
		} `xml:"status"`
		Track *struct {
			Course string `xml:"course,attr"`
			Speed  string `xml:"speed,attr"`
		} `xml:"track"`
		Takv *struct {
			Device string `xml:"device,attr"`
		} `xml:"takv"`
		Chat *struct {
			ID             string `xml:"id,attr"`
			Chatroom       string `xml:"chatroom,attr"`
			SenderCallsign string `xml:"senderCallsign,attr"`
			Chatgrp        *struct {
				UID0 string `xml:"uid0,attr"`
				UID1 string `xml:"uid1,attr"`
			} `xml:"chatgrp"`
		} `xml:"__chat"`
		Remarks   string `xml:"remarks"`
		Emergency *struct {
			Type   string `xml:"type,attr"`
			Cancel string `xml:"cancel,attr"`
			Text   string `xml:",chardata"`
		} `xml:"emergency"`
		Link []struct {
			UID string `xml:"uid,attr"`
		} `xml:"link"`
		// MeshRelay: a MeshRelay server's own map item (an LMR channel
		// marker, lmr_atak.go), not a TAK user.
		MeshRelay *struct {
			Kind string `xml:"kind,attr"`
		} `xml:"__meshrelay"`
	} `xml:"detail"`
}

// isMeshRelayMarker: the event is a MeshRelay server's LMR channel marker
// (ours echoed back, or another MeshRelay server's on the same TAK server),
// known by its <__meshrelay> detail or, failing that, by its uid.
func isMeshRelayMarker(ev *cotEvent) bool {
	return ev.Detail.MeshRelay != nil || strings.HasPrefix(ev.UID, lmrMarkerUIDPrefix)
}

func parseCotTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Now()
	}
	return t
}

func (t *takConn) handle(ev *cotEvent) {
	agency := t.cfg.AgencyID
	switch {
	case ev.UID == t.cfg.UID:
		return // our own presence echoed back
	case ev.Type == "b-t-f": // GeoChat
		t.handleChat(ev)
	case strings.HasPrefix(ev.Type, "b-a-o-") || ev.Detail.Emergency != nil:
		t.handleEmergency(ev)
	case ev.Type == "t-x-d-d": // delete
		for _, l := range ev.Detail.Link {
			t.mu.Lock()
			delete(t.units, l.UID)
			t.mu.Unlock()
		}
	case strings.HasPrefix(ev.Type, "a-") && ev.Detail.Contact != nil && ev.Detail.Contact.Callsign != "":
		if isMeshRelayMarker(ev) {
			return // a radio channel marker, not someone to message
		}
		if !validFix(ev.Point.Lat, ev.Point.Lon) {
			return
		}
		u := &TakUnit{UID: ev.UID, Callsign: ev.Detail.Contact.Callsign, Lat: ev.Point.Lat, Lon: ev.Point.Lon,
			At: parseCotTime(ev.Time), Stale: parseCotTime(ev.Stale)}
		if g := ev.Detail.Group; g != nil {
			u.Group, u.Role = g.Name, g.Role
		}
		if st := ev.Detail.Status; st != nil {
			u.Battery, _ = strconv.Atoi(st.Battery)
		}
		if tr := ev.Detail.Track; tr != nil {
			u.Speed, _ = strconv.ParseFloat(tr.Speed, 64)
			u.Course, _ = strconv.ParseFloat(tr.Course, 64)
		}
		if tv := ev.Detail.Takv; tv != nil {
			u.Device = tv.Device
		}
		t.mu.Lock()
		t.units[ev.UID] = u
		t.mu.Unlock()
	}
	_ = agency
}

func (t *takConn) handleChat(ev *cotEvent) {
	ch := ev.Detail.Chat
	if ch == nil {
		return
	}
	to := ch.ID
	from := ""
	if ch.Chatgrp != nil {
		to = ch.Chatgrp.UID1
		from = ch.Chatgrp.UID0
	}
	if t.master {
		// Only direct messages to the master's one contact; MeshRelay picks
		// the agencies (tak_master.go). Team chats and All Chat Rooms reach
		// no console.
		if to != t.cfg.UID {
			log.Printf("TAK [master]: message addressed to %q, not for the master contact, dropped", to)
			return
		}
	} else if to != t.cfg.UID && (ch.Chatroom != t.cfg.Callsign || t.s.takMasterOwnsUID(to)) {
		// Not for dispatch (team chats, All Chat Rooms), or for the master's
		// contact that happens to share this contact's name.
		return
	}
	text := strings.TrimSpace(ev.Detail.Remarks)
	if text == "" {
		return
	}
	if len(text) > maxMessageLen {
		text = clipUTF8(text, maxMessageLen)
	}
	name := nonEmpty(ch.SenderCallsign, from)
	if t.master {
		t.deliverMasterChat(from, name, text, time.Now())
		return
	}
	t.s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: t.cfg.AgencyID, Dir: "in",
		From: name, To: "Dispatch", Text: text, TakUID: from})
}

// TAK alert types (ATAK's emergency button).
var takAlertNames = map[string]string{
	"b-a-o-tbl": "911 Alert", "b-a-o-pan": "Ring The Bell", "b-a-o-opn": "Troops In Contact",
	"b-a-o-c": "Custom Alert", "b-a-g": "Geo-fence Breached",
}

func (t *takConn) handleEmergency(ev *cotEvent) {
	active := ev.Type != "b-a-o-can"
	name := ""
	if e := ev.Detail.Emergency; e != nil {
		if strings.EqualFold(e.Cancel, "true") {
			active = false
		}
		name = strings.TrimSpace(e.Text)
	}
	if name == "" && ev.Detail.Contact != nil {
		name = ev.Detail.Contact.Callsign
	}
	if name == "" {
		name = ev.UID
	}
	// MeshRelay's own emergencies come back from TAK too: not a new alert.
	if strings.HasPrefix(ev.UID, "MeshRelay-911-") {
		return
	}
	if takOwnAlertUID(ev.UID) && t.phoneAlert(name, active, time.Now()) {
		return // the unit's MeshRelay emergency shows on the console already
	}
	log.Printf("TAK [%s]: emergency %v from %q (%q, uid %q)", t.label(), active, name, ev.Type, ev.UID)
	for _, agency := range t.consoleAgencies() {
		msg := map[string]interface{}{
			"type": "emergency", "agency": agency, "channel": -1, "user": name + " (TAK)",
			"active": active, "tak": true, "alert": takAlertNames[ev.Type],
		}
		if t.master {
			msg["tak_source"] = "master"
		}
		if validFix(ev.Point.Lat, ev.Point.Lon) {
			msg["lat"], msg["lon"] = ev.Point.Lat, ev.Point.Lon
		}
		t.s.sendToConsoles(agency, msg)
	}
}

// consoleAgencies: whose consoles show what this connection receives. An
// agency connection: its agency. The master: the sharing agencies, except one
// whose own connection is to the same TAK server (it gets it there).
func (t *takConn) consoleAgencies() []string {
	if !t.master {
		return []string{t.cfg.AgencyID}
	}
	var out []string
	for _, tg := range t.s.takMasterTargets() {
		if !tg.SameServer {
			out = append(out, tg.Agency)
		}
	}
	return out
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), `"`, "&quot;")
}

func cotTimes(staleAfter time.Duration) (string, string) {
	now := time.Now().UTC()
	return now.Format("2006-01-02T15:04:05.000Z"), now.Add(staleAfter).Format("2006-01-02T15:04:05.000Z")
}

// presenceCoT makes dispatch a contact TAK users can chat with. It sits at the
// middle of the agency's MeshRelay units when any have a position.
// The master's one contact has no position (it serves several agencies).
func (t *takConn) presenceCoT() string {
	var lat, lon float64
	if !t.master {
		lat, lon = t.s.agencyCentroid(t.cfg.AgencyID)
	}
	return presenceCoTAs(t.cfg.UID, t.cfg.Callsign, lat, lon)
}

func presenceCoTAs(uid, callsign string, lat, lon float64) string {
	now, stale := cotTimes(2 * time.Minute)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="a-f-G-U-C" uid="%s" how="h-g-i-g-o" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999" ce="9999999" le="9999999"/><detail><contact callsign="%s" endpoint="*:-1:stcp"/><__group name="Cyan" role="HQ"/><takv device="MeshRelay Server" platform="MeshRelay Dispatch" os="" version="%s"/><uid Droid="%s"/></detail></event>`,
		xmlEsc(uid), now, now, stale, lat, lon, xmlEsc(callsign), xmlEsc(Version), xmlEsc(callsign))
}

// pingCoT: TAK's keepalive (the server answers t-x-c-t-r), so a quiet server
// still sends something before the read deadline.
func (t *takConn) pingCoT() string {
	now, stale := cotTimes(20 * time.Second)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="t-x-c-t" uid="%s-ping" how="m-g" time="%s" start="%s" stale="%s"><point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail/></event>`,
		xmlEsc(t.cfg.UID), now, now, stale)
}

// chatCoT builds a direct GeoChat the way ATAK does (a UUID message ID, how
// "h-g-i-g-o", __chat/chatgrp naming the recipient's UID), addressed with
// <marti><dest uid=...>, so the TAK server delivers it to the recipient only.
//
// Without a <marti> dest a TAK server hands the event to everyone in the
// group, and every ATAK keeps it: ATAK files a b-t-f under the conversation
// that __chat id names (ChatMessageParser), never checking that it is the
// addressee. (A message as RECEIVED from a TAK server has no <marti>: the
// server takes it off on delivery.) ATAK itself addresses a direct chat by
// callsign (commo CoTMessage::setTAKServerRecipients writes
// <dest callsign=...>); TAK Server also brokers <dest uid=...> to the client
// whose UID that is (StreamingEndpointRewriteFilter). The server uses the
// UID: two TAK users may share a callsign (both would get it), and the
// console addresses users by UID anyway, also ones the server does not list.
// chatDestDetail must be on every direct message the server sends.
func (t *takConn) chatCoT(toUID, toCallsign, text string) string {
	lat, lon := t.s.agencyCentroid(t.cfg.AgencyID)
	return chatCoTAs(t.cfg.UID, t.cfg.Callsign, lat, lon, toUID, toCallsign, text)
}

// chatCoTAs builds the GeoChat from the contact fromUID / fromCallsign.
func chatCoTAs(fromUID, fromCallsign string, lat, lon float64, toUID, toCallsign, text string) string {
	now, stale := cotTimes(24 * time.Hour)
	msgID := newUUID()
	evUID := "GeoChat." + fromUID + "." + toUID + "." + msgID
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" uid="%s" type="b-t-f" how="h-g-i-g-o" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999.0" ce="9999999.0" le="9999999.0"/><detail><__chat parent="RootContactGroup" groupOwner="false" messageId="%s" chatroom="%s" id="%s" senderCallsign="%s"><chatgrp uid0="%s" uid1="%s" id="%s"/></__chat><link uid="%s" type="a-f-G-U-C" relation="p-p"/><remarks source="BAO.F.ATAK.%s" to="%s" time="%s">%s</remarks>%s</detail></event>`,
		xmlEsc(evUID), now, now, stale, lat, lon, msgID, xmlEsc(toCallsign), xmlEsc(toUID), xmlEsc(fromCallsign),
		xmlEsc(fromUID), xmlEsc(toUID), xmlEsc(toUID), xmlEsc(fromUID), xmlEsc(fromUID), xmlEsc(toUID), now, xmlEsc(text),
		chatDestDetail(toUID))
}

// chatDestDetail: the <marti> element that has a TAK server deliver an event
// to the client with UID toUID only (see chatCoT).
func chatDestDetail(toUID string) string {
	return `<marti><dest uid="` + xmlEsc(toUID) + `"/></marti>`
}

// newUUID returns a random (version 4) UUID string.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (t *takConn) emergencyCoT(user string, lat, lon float64, active bool) string {
	return t.emergencyCoTFor(t.cfg.AgencyID, user, lat, lon, active)
}

// emergencyCoTFor: the server's alert for a user of agency. On the master,
// which carries several agencies, the unit is named with its agency:
// "medic (Alpha)".
func (t *takConn) emergencyCoTFor(agency, user string, lat, lon float64, active bool) string {
	return t.emergencyCoTPlaced(agency, user, lat, lon, true, active)
}

// emergencyCoTPlaced is emergencyCoTFor; known false: lat/lon are not the
// unit's own position (none is known) and the remarks say so.
//
// The <link> (see emergencyAlertLink) names no map item: ATAK then draws no
// line to anything and names the unit in the alert from the contact callsign.
func (t *takConn) emergencyCoTPlaced(agency, user string, lat, lon float64, known, active bool) string {
	now, stale := cotTimes(10 * time.Minute)
	uid := "MeshRelay-911-" + agency + "-" + user
	label := user
	if t.master {
		label = user + " (" + nonEmpty(strings.TrimSpace(t.s.agencyDisplayName(agency)), agency) + ")"
	}
	typ, emerg := "b-a-o-tbl", `<emergency type="911 Alert">`+xmlEsc(label)+`</emergency>`
	if !active {
		typ, emerg = "b-a-o-can", `<emergency cancel="true">`+xmlEsc(label)+`</emergency>`
	}
	remarks := "MeshRelay emergency: " + label
	if !known {
		remarks += " (position unknown: shown at the middle of the agency's units)"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="%s" uid="%s" how="m-g" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999" ce="9999999" le="9999999"/><detail><contact callsign="%s-Alert"/><link uid="%s"/>%s<remarks>%s</remarks></detail></event>`,
		typ, xmlEsc(uid), now, now, stale, lat, lon, xmlEsc(label), xmlEsc(emergencyAlertLink(uid)), emerg, xmlEsc(remarks))
}

// emergencyAlertLink: the uid the server's alert links to, one no CoT event
// ever has. ATAK (5.6-5.8) needs a <link> on a 911 alert
// (EmergencyAlertReceiver.fromCot drops one without) and uses it like this:
//   - LinkDetailHandler draws a line from the linked item to the alert only
//     for a link with a uid, a type and relation "p-p". This one has no type
//     and no relation: no line (it used to link the dispatch contact, and ATAK
//     drew a line from that contact to the alert).
//   - EmergencyAlertReceiver ties the alert to the map item with the link's
//     uid (at once only for a "p-p" link of the item's type, and every 2 s by
//     uid alone) and names the unit after that item. With no such item it
//     names the unit from the alert's own title, the contact callsign less
//     its last "-..." part: "medic-Alert" -> "medic". The link's uid must
//     therefore never be a real item's (the dispatch contact's made the alert
//     read "911 Alert Dispatch"), nor left out: ATAK looks it up as given.
func emergencyAlertLink(alertUID string) string {
	return alertUID + ".unit"
}

// --- server-side hooks ------------------------------------------------------------

func (s *Server) agencyCentroid(agency string) (float64, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var lat, lon float64
	n := 0
	for _, c := range s.clients {
		if c.AgencyID == agency && c.Loc != nil {
			lat += c.Loc.Lat
			lon += c.Loc.Lon
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return lat / float64(n), lon / float64(n)
}

// takUnitsFor lists TAK users for consoles of an agency ("" = every agency).
// MeshRelay phones that also report into TAK are left out when they are on the
// MeshRelay server already (a signed-in unit of the same agency, same
// callsignKey) — the console shows them once. Only the listing: the unit's
// GeoChat, alerts and position are handled as before (takUnitByCallsign).
func (s *Server) takUnitsFor(agencyFilter string) []map[string]interface{} {
	m := s.tak()
	m.mu.Lock()
	conns := make([]*takConn, 0, len(m.conns))
	for a, c := range m.conns {
		if agencyFilter == "" || a == agencyFilter {
			conns = append(conns, c)
		}
	}
	mc := m.master
	m.mu.Unlock()
	var masterFor []string // sharing agencies in scope, sorted
	if mc != nil {
		for _, tg := range s.takMasterTargets() {
			if agencyFilter == "" || tg.Agency == agencyFilter {
				masterFor = append(masterFor, tg.Agency)
			}
		}
	}

	// Signed-in MeshRelay units by agency + callsignKey whose client is
	// known (clientKindLocked): the same person's TAK unit is not listed to
	// that agency a second time — the MeshRelay entry stands for both (it
	// carries the TAK unit, see unitTakLink). A unit whose client is not
	// known keeps its TAK entry: it may be the only way to reach or see it.
	onMesh := map[string]bool{}
	s.mu.RLock()
	for _, c := range s.clients {
		if s.clientKindLocked(c) != clientUnknown {
			onMesh[c.AgencyID+"\x00"+callsignKey(c.Username)] = true
		}
	}
	s.mu.RUnlock()

	out := []map[string]interface{}{}
	now := time.Now()
	unitJSON := func(u *TakUnit, agency string) map[string]interface{} {
		return map[string]interface{}{
			"uid": u.UID, "callsign": u.Callsign, "agency": agency,
			"lat": u.Lat, "lon": u.Lon, "at": u.At.Format(time.RFC3339), "stale": now.After(u.Stale),
			"battery": u.Battery, "speed": u.Speed, "course": u.Course, "group": u.Group, "role": u.Role,
			"device": u.Device,
		}
	}
	own := map[string]bool{} // agency + uid already listed from the agency's own connection
	for _, tc := range conns {
		tc.mu.Lock()
		for _, u := range tc.units {
			if onMesh[tc.cfg.AgencyID+"\x00"+callsignKey(u.Callsign)] {
				continue
			}
			own[tc.cfg.AgencyID+"\x00"+u.UID] = true
			out = append(out, unitJSON(u, tc.cfg.AgencyID))
		}
		tc.mu.Unlock()
	}
	// Master units for the sharing agencies, each uid once per console: not
	// again when the agency's own connection lists it (the same TAK server
	// twice), and on an all-agency console only under the first agency. A
	// unit that is a signed-in MeshRelay unit of the agency is left out for
	// that agency; on an all-agency console, which shows that MeshRelay unit
	// too, it is left out altogether (not listed under another agency).
	if len(masterFor) > 0 {
		listed := map[string]bool{}
		mc.mu.Lock()
		for _, agency := range masterFor {
			for _, u := range mc.units {
				key := callsignKey(u.Callsign)
				meshed := onMesh[agency+"\x00"+key]
				if agencyFilter == "" {
					for _, a := range masterFor {
						meshed = meshed || onMesh[a+"\x00"+key]
					}
				}
				if own[agency+"\x00"+u.UID] || meshed || (agencyFilter == "" && listed[u.UID]) {
					continue
				}
				listed[u.UID] = true
				j := unitJSON(u, agency)
				j["source"] = "master"
				out = append(out, j)
			}
		}
		mc.mu.Unlock()
	}
	return out
}

// What a signed-in MeshRelay client can do, from what it sent (clientKindLocked).
const (
	clientUnknown    = ""            // older sign-in, a gateway, a bridge, ...: not assumed
	clientText       = "text"        // receives dispatch texts (0x30): the app (Android, iOS)
	clientATAKPlugin = "atak-plugin" // the MeshRelayTAK ATAK plugin: no texts, no 0x2E;
	// ATAK reports the person to TAK, where the server texts and locates them
)

// clientKindLocked: the kind of the unit c (never a console). Caller holds
// s.mu. Known only from what clients send today: the client name in the
// secure sign-in ("android <version>", "ios", "atak-plugin"), or a dispatch
// text the unit has sent. (Exact later: a capability bit in the sign-in's
// flags byte — see the report for rc3.)
func (s *Server) clientKindLocked(c *Client) string {
	if c == nil || c.Addr == nil || isConsoleAddr(c.Addr) {
		return clientUnknown
	}
	if c.SentText {
		return clientText
	}
	name := strings.ToLower(strings.TrimSpace(s.authSessions[c.Addr.String()].client))
	switch {
	case strings.HasPrefix(name, "android "), name == "ios", strings.HasPrefix(name, "ios "):
		return clientText
	case name == "atak-plugin":
		return clientATAKPlugin
	}
	return clientUnknown
}

// callsignKey: a callsign as compared to find the same person on MeshRelay
// and in TAK (the MeshRelay username): any case, no outer spaces.
func callsignKey(callsign string) string {
	return strings.ToLower(strings.TrimSpace(callsign))
}

func (s *Server) takConnFor(agency string) *takConn {
	m := s.tak()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[agency]
}

// consoleSendTakMessage sends an operator's message to a TAK user as GeoChat.
// source "master": to a user on the master TAK server, from the master's
// contact with the agency's name in front (only while the agency shares).
// hint is the callsign the console showed for the user: only a display
// fallback for the history when the server does not list the user, never
// used for routing.
func (s *Server) consoleSendTakMessage(console *Client, agency, uid, text, source, hint string) {
	text = strings.TrimSpace(text)
	if text == "" || (console.AgencyID != "" && console.AgencyID != agency) {
		return
	}
	if len(text) > maxMessageLen {
		text = clipUTF8(text, maxMessageLen)
	}
	tc := s.takConnFor(agency)
	if source == "" && !tc.hasUnit(uid) && s.takMasterHasUnit(agency, uid) {
		source = "master" // a console that did not say: the user is only on the master
	}
	if source == "master" {
		s.consoleSendMasterMessage(console, agency, uid, text, hint)
		return
	}
	callsign, sent, why := "", 0, "no TAK connection"
	if tc != nil {
		tc.mu.Lock()
		if u := tc.units[uid]; u != nil {
			callsign = u.Callsign
		}
		connected := tc.status == "connected"
		tc.mu.Unlock()
		why = "TAK not connected"
		if connected {
			why = "TAK write failed"
			if tc.send(tc.chatCoT(uid, nonEmpty(callsign, uid), text)) {
				sent, why = 1, ""
			}
		}
	}
	s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: agency, Dir: "out", From: console.Username,
		To: takRecipientLabel(callsign, hint) + " (TAK)", Text: text, Sent: sent, NotSent: why, TakUID: uid,
		unlisted: callsign == ""})
}

// takRecipientLabel: how the history names a TAK user a console wrote to:
// the callsign the server knows, else the console's (cleaned, shortened)
// hint, else a neutral label (never the raw uid).
func takRecipientLabel(known, hint string) string {
	if known != "" {
		return known
	}
	if h := strings.TrimSpace(printable(hint, 64)); h != "" {
		return h
	}
	return "TAK user"
}

// takEmergency mirrors a MeshRelay emergency into the agency's TAK server.
// A sharing agency's emergencies also go to the master TAK server, unless
// the agency's own connection is to that same server (sent once, there).
// why says what changed, for the journal ("raise", "moved 40 m", "cleared").
func (s *Server) takEmergency(agency, user string, active bool, why string) {
	if tc := s.takConnFor(agency); tc != nil {
		tc.ownEmergency(user, active, time.Now(), why)
	}
	if mc := s.takMasterOutbound(agency); mc != nil {
		mc.ownEmergencyFor(agency, user, active, time.Now(), why)
	}
}

// hasUnit: whether the connection (nil: none) currently lists the TAK user.
func (t *takConn) hasUnit(uid string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.units[uid] != nil
}

// userFix: where the server's TAK alert (or its cancel) for a unit is placed
// (emergencyAlertFix: the unit's position, else the middle of its agency).
func (s *Server) userFix(agency, user string) (lat, lon float64) {
	lat, lon, _ = s.emergencyAlertFix(agency, user)
	return lat, lon
}

// emergencyUser finds the agency's user called name (any case) whose
// MeshRelay emergency is on.
func (s *Server) emergencyUser(agency, name string) (string, bool) {
	s.emergMu.Lock()
	defer s.emergMu.Unlock()
	for u := range s.emergCalls[agency] {
		if strings.EqualFold(u, name) {
			return u, true
		}
	}
	return name, false
}

// emergencyAnyAgency: whether a user called name (any case) of any agency on
// this server has a MeshRelay emergency on. The phone of a unit from another
// agency may report into this agency's TAK server.
func (s *Server) emergencyAnyAgency(name string) bool {
	s.emergMu.Lock()
	defer s.emergMu.Unlock()
	for _, calls := range s.emergCalls {
		for u := range calls {
			if strings.EqualFold(u, name) {
				return true
			}
		}
	}
	return false
}

// --- one 911 alert per unit ---------------------------------------------------------
//
// A MeshRelay phone with its own TAK link sends ATAK's own 911 alert (uid =
// its PLI uid + "-9-1-1", repeated every 5 s, 10 s stale). While that alert
// keeps arriving, the server withholds its MeshRelay-911-... alert for the
// unit (cancelling it if it was out), and sends it again when the phone's
// alert stops while the MeshRelay emergency is still on. A unit whose phone
// never sends its own alert is handled as before. ATAK's own emergency tool
// (uid = ATAK device uid + "-9-1-1", the unit's callsign) is handled the
// same way (takOwnAlertUID): a MeshRelayTAK plugin user, or an app user whose
// ATAK is on the same TAK server, who uses it during a MeshRelay emergency
// raises one incident, not two.

// takPhoneAlertUID: the phone's alert uid ("MeshRelay-" + id + "-9-1-1").
var takPhoneAlertUID = regexp.MustCompile(`^MeshRelay-[0-9A-Za-z]+-9-1-1$`)

// takOwnAlertUID: an alert a device sends for itself: the MeshRelay phone's
// (takPhoneAlertUID), or ATAK's own emergency tool's (its device uid +
// "-9-1-1"), which a MeshRelayTAK plugin user or an app user whose ATAK is on
// the same TAK server may use as well as raising the MeshRelay emergency.
// Both go through phoneAlert: when the alert's callsign is a MeshRelay user
// (of any agency) in an emergency, it is the same incident and is not shown
// or sent a second time; any other alert shows as before.
func takOwnAlertUID(uid string) bool {
	if takPhoneAlertUID.MatchString(uid) {
		return true
	}
	dev, ok := strings.CutSuffix(uid, "-9-1-1")
	return ok && dev != "" && !strings.HasPrefix(uid, "MeshRelay-911-")
}

// takPhoneAlertHold: three of the phone's repeats. Missing for this long, its
// alert counts as gone (one late repeat does not flip the server's alert).
var takPhoneAlertHold = 15 * time.Second

// alertKey: one alert state per unit. On an agency connection agency is
// always the connection's own; the master keeps the units of every sharing
// agency apart (agency "" = a phone alert not matched to any agency yet).
func alertKey(agency, user string) string {
	return agency + "\x00" + strings.ToLower(user)
}

func (t *takConn) alertLocked(key, agency, user string) *takAlert {
	if t.alerts == nil {
		t.alerts = map[string]*takAlert{}
	}
	a := t.alerts[key]
	if a == nil {
		a = &takAlert{agency: agency, user: user}
		t.alerts[key] = a
	}
	return a
}

// ownEmergency: a MeshRelay emergency of the connection's agency started or ended.
func (t *takConn) ownEmergency(user string, active bool, now time.Time, why string) {
	t.ownEmergencyFor(t.cfg.AgencyID, user, active, now, why)
}

// ownEmergencyFor: a MeshRelay emergency of agency's user started or ended.
func (t *takConn) ownEmergencyFor(agency, user string, active bool, now time.Time, why string) {
	key := alertKey(agency, user)
	send, unshow := false, ""
	t.mu.Lock()
	if t.alerts[key] == nil && agency != "" {
		// The phone's alert came first, before any agency had this unit in
		// an emergency (the master): that state belongs to this unit now.
		if early := t.alerts[alertKey("", user)]; early != nil {
			delete(t.alerts, alertKey("", user))
			early.agency = agency
			t.alerts[key] = early
		}
	}
	a := t.alertLocked(key, agency, user)
	a.user = user
	if active {
		a.active = true
		if !a.phoneAt.IsZero() && now.Sub(a.phoneAt) < takPhoneAlertHold {
			// The phone's own alert is on TAK already: no second one, and
			// the console shows the MeshRelay emergency instead of it.
			a.hidden = true
			unshow, a.shownName = a.shownName, ""
		} else {
			send, a.ownOut = true, true
		}
	} else {
		// One cancel for what the server has out (as before for a unit whose
		// phone never sent its own alert).
		send = a.ownOut || a.phoneAt.IsZero()
		a.active, a.ownOut, a.clearedAt, a.emergAt = false, false, now, now
		if !a.hidden {
			delete(t.alerts, key)
		}
	}
	t.mu.Unlock()
	if unshow != "" {
		t.unshowPhoneAlert(unshow)
	}
	if send {
		t.sendOwnAlert(agency, user, active, why)
	}
}

// phoneAlert notes a MeshRelay phone's own alert (or its cancel) for the unit
// called name, and reports whether the console should not show it. It hides
// it only when name is a user of any agency on this server whose MeshRelay
// emergency is on (or ended moments ago, for repeats still on the way): that
// emergency shows on the console already. Any other alert shows: a TAK
// user's whose callsign is no MeshRelay unit in an emergency, or any alert
// with another uid form. Withholding the server's own alert stays within this
// connector's agency.
func (t *takConn) phoneAlert(name string, active bool, now time.Time) (hide bool) {
	agency := t.cfg.AgencyID
	var user string
	var inEmerg bool
	if t.master {
		// The master carries the alerts of every agency sending through it:
		// the unit is the one of those agencies in a MeshRelay emergency.
		agency, user = "", name
		for _, a := range t.s.takMasterOutboundAgencies() {
			if u, ok := t.s.emergencyUser(a, name); ok {
				agency, user, inEmerg = a, u, true
				break
			}
		}
	} else {
		user, inEmerg = t.s.emergencyUser(agency, name)
	}
	anyEmerg := inEmerg || t.s.emergencyAnyAgency(name)
	key := alertKey(agency, name)
	unshow, cancelOwn := "", false
	t.mu.Lock()
	if t.master && !inEmerg && t.alerts[key] == nil {
		// The emergency just ended: the unit's state is still kept under
		// its agency (hidden repeats, the cancel), not under "".
		if k, ok := t.agencyAlertKeyLocked(name); ok {
			key = k
			agency, user = t.alerts[k].agency, t.alerts[k].user
		}
	}
	a := t.alertLocked(key, agency, user)
	if inEmerg {
		a.user = user
		if !a.active {
			// Emergency began before this connection (settings saved since):
			// the server's alert may be out from the one before.
			a.active, a.ownOut = true, true
		}
	}
	if !active {
		hide = a.hidden || (a.shownName == "" && anyEmerg)
		a.hidden, a.shownName = false, ""
		if !a.active {
			delete(t.alerts, key)
		}
		t.mu.Unlock()
		return hide
	}
	a.phoneAt = now
	if anyEmerg {
		a.emergAt = now
	}
	hide = anyEmerg || (a.hidden && now.Sub(a.emergAt) < takPhoneAlertHold)
	if hide {
		a.hidden = true
		unshow, a.shownName = a.shownName, ""
		if a.ownOut {
			cancelOwn, a.ownOut = true, false
		}
	} else {
		a.hidden, a.shownName = false, name
	}
	user, agency = a.user, a.agency
	t.mu.Unlock()
	if unshow != "" {
		t.unshowPhoneAlert(unshow)
	}
	if cancelOwn {
		log.Printf("TAK [%s]: %q's phone sends its own 911 alert — server alert withdrawn", t.label(), user)
		t.sendOwnAlert(agency, user, false, "the phone's own alert stands in")
	}
	return hide
}

// agencyAlertKeyLocked (master): the key of the state an agency keeps for
// the unit called name (any case), if one does. Caller holds t.mu.
func (t *takConn) agencyAlertKeyLocked(name string) (string, bool) {
	for k, a := range t.alerts {
		if a.agency != "" && k == alertKey(a.agency, name) {
			return k, true
		}
	}
	return "", false
}

// takAlertRefresh: an alert that is out goes out again this often while the
// emergency lasts, well inside its 10 min stale time (otherwise it would go
// stale on ATAK maps during a long emergency). Between real changes the TAK
// server's own repeater re-delivers it to clients (see takAlertRepeaterNote).
var takAlertRefresh = 4 * time.Minute

// takAlertRepeaterNote: a TAK server re-delivers a 911 alert by itself every
// 3 s until its cancel (CoreConfig <repeater periodMillis="3000"> with a
// repeatableType for emergency type '911 Alert', on by default). ATAK logs
// each repeat as "Adding an emergency marker"; the server writes the alert
// only on a raise, a move, a resume, a refresh or the cancel.

// alertTick (every 5 s): the server's alert goes out again for a unit whose
// phone alert has stopped while its MeshRelay emergency is on, and is
// refreshed every takAlertRefresh; finished units are forgotten.
func (t *takConn) alertTick(now time.Time) {
	type again struct {
		agency, user, why string
	}
	var resend []again
	t.mu.Lock()
	for key, a := range t.alerts {
		gone := a.phoneAt.IsZero() || now.Sub(a.phoneAt) >= takPhoneAlertHold
		switch {
		case a.active && !a.ownOut && !a.phoneAt.IsZero() && gone:
			a.ownOut = true
			resend = append(resend, again{a.agency, a.user, "resume: the phone's own alert stopped"})
		case a.active && a.ownOut && !a.sentAt.IsZero() && now.Sub(a.sentAt) >= takAlertRefresh:
			a.sentAt = now
			resend = append(resend, again{a.agency, a.user, "refresh"})
		case !a.active && gone && now.Sub(a.clearedAt) >= takPhoneAlertHold:
			delete(t.alerts, key)
		}
	}
	t.mu.Unlock()
	for _, r := range resend {
		t.sendOwnAlert(r.agency, r.user, true, r.why)
	}
}

// sendOwnAlert sends (or cancels) the server's alert for agency's user. A
// failed write leaves the alert as it was, so a later phone alert or tick
// tries again.
func (t *takConn) sendOwnAlert(agency, user string, active bool, why string) {
	lat, lon, known := t.s.emergencyAlertFix(agency, user)
	what := "911 alert"
	if !active {
		what = "911 alert cancel"
	}
	if t.send(t.emergencyCoTPlaced(agency, user, lat, lon, known, active)) {
		log.Printf("TAK [%s]: %s for %q [%s] sent (%s)", t.label(), what, user, agency, nonEmpty(why, "update"))
		t.mu.Lock()
		if a := t.alerts[alertKey(agency, user)]; a != nil {
			a.sentAt = time.Now()
		}
		t.mu.Unlock()
		return
	}
	log.Printf("TAK [%s]: %s for %q [%s] NOT sent (%s): write failed", t.label(), what, user, agency, nonEmpty(why, "update"))
	t.mu.Lock()
	if a := t.alerts[alertKey(agency, user)]; a != nil && a.active {
		a.ownOut = !active
	}
	t.mu.Unlock()
}

// unshowPhoneAlert takes a phone's alert, shown before its MeshRelay
// emergency arrived, off the consoles it was shown on.
func (t *takConn) unshowPhoneAlert(name string) {
	for _, agency := range t.consoleAgencies() {
		t.s.sendToConsoles(agency, map[string]interface{}{
			"type": "emergency", "agency": agency, "channel": -1, "user": name + " (TAK)",
			"active": false, "tak": true, "alert": takAlertNames["b-a-o-tbl"],
		})
	}
}

// --- dashboard API -------------------------------------------------------------------

func (s *Server) takStatusJSON(agency string) map[string]interface{} {
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agency]
	tc := m.conns[agency]
	m.mu.Unlock()
	out := map[string]interface{}{"agency_id": agency, "configured": ok}
	if !ok {
		out["stream_port"], out["enroll_port"], out["api_port"], out["callsign"] = 8089, 8446, 8443, "MeshRelay Dispatch"
	} else {
		takCfgStatus(out, cfg, tc)
	}
	s.takMasterShareStatus(agency, out)
	return out
}

// takCfgStatus fills in a configured connection's settings and state (no
// key, no password: the password is never stored).
func takCfgStatus(out map[string]interface{}, cfg TakConfig, tc *takConn) {
	out["host"], out["stream_port"], out["enroll_port"] = cfg.Host, cfg.StreamPort, cfg.EnrollPort
	out["api_port"], out["groups"] = cfg.APIPort, cfg.Groups
	if cfg.APIPort <= 0 {
		out["api_port"] = 8443
	}
	out["username"], out["callsign"], out["enabled"] = cfg.Username, cfg.Callsign, cfg.Enabled
	out["enrolled"] = cfg.CertPEM != ""
	out["enrolled_at"] = cfg.EnrolledAt
	out["managed"] = cfg.Managed
	if block, _ := pem.Decode([]byte(cfg.CertPEM)); block != nil {
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out["cert_expires"] = c.NotAfter.Format(time.RFC3339)
		}
	}
	out["status"] = "disabled"
	if tc != nil {
		tc.mu.Lock()
		out["status"], out["error"], out["tak_users"] = tc.status, tc.errMsg, len(tc.units)
		tc.mu.Unlock()
	}
}

func (s *Server) handleGetTak(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

func (s *Server) handleUpdateTak(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID   string `json:"agency_id"`
		Host       string `json:"host"`
		StreamPort int    `json:"stream_port"`
		EnrollPort int    `json:"enroll_port"`
		APIPort    int    `json:"api_port"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		Callsign   string `json:"callsign"`
		Enabled    bool   `json:"enabled"`
		Remove     bool   `json:"remove"`
		// The enrollment certificate the admin confirmed (SHA-256, hex).
		TrustFingerprint string `json:"trust_fingerprint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	s.mu.RLock()
	_, known := s.agencies[agencyID]
	s.mu.RUnlock()
	if !known {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	m := s.tak()
	if req.Remove {
		s.stopTak(agencyID)
		m.mu.Lock()
		delete(m.cfgs, agencyID)
		m.mu.Unlock()
		s.saveTakConnections()
		log.Printf("TAK [%s]: connection removed", agencyID)
		json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
		return
	}

	m.mu.Lock()
	cfg, existed := m.cfgs[agencyID]
	m.mu.Unlock()
	if existed && cfg.Managed && !isSuper {
		writeJSONError(w, http.StatusForbidden, "this TAK link is managed by the platform and cannot be changed here")
		return
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		writeJSONError(w, http.StatusBadRequest, "TAK server address is required")
		return
	}
	reEnroll := req.Password != "" || !existed || host != cfg.Host || req.Username != cfg.Username || req.EnrollPort != cfg.EnrollPort
	cfg.AgencyID, cfg.Host, cfg.Enabled = agencyID, host, req.Enabled
	cfg.Managed = false
	cfg.StreamPort, cfg.EnrollPort = req.StreamPort, req.EnrollPort
	if cfg.StreamPort <= 0 {
		cfg.StreamPort = 8089
	}
	if cfg.EnrollPort <= 0 {
		cfg.EnrollPort = 8446
	}
	cfg.APIPort = req.APIPort
	if cfg.APIPort <= 0 {
		cfg.APIPort = 8443
	}
	cfg.Username = strings.TrimSpace(req.Username)
	cfg.Callsign = nonEmpty(strings.TrimSpace(req.Callsign), "MeshRelay Dispatch")
	if cfg.UID == "" {
		cfg.UID = "MeshRelay-Dispatch-" + safeName.ReplaceAllString(agencyID, "_") + "-" + randomHex(3)
	}
	if err := takHostAllowed(cfg.Host, cfg.StreamPort, cfg.EnrollPort, cfg.APIPort); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if reEnroll {
		if req.Password == "" {
			writeJSONError(w, http.StatusBadRequest, "Enter the TAK password to enroll for a certificate")
			return
		}
		// Only a super admin may point the server at itself (a TAK server on
		// the same machine); link-local addresses are never useful here.
		if !takDashboardEnroll(w, agencyID, &cfg, req.Password, req.TrustFingerprint, isSuper) {
			return
		}
	}
	m.mu.Lock()
	m.cfgs[agencyID] = cfg
	m.mu.Unlock()
	if err := s.saveTakConnections(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.Enabled {
		s.startTak(cfg)
		time.Sleep(1500 * time.Millisecond) // let the first connect attempt report
	} else {
		s.stopTak(agencyID)
	}
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

// takDashboardEnroll enrolls cfg for a dashboard request (agency or master
// connection). It answers the request itself when it does not succeed: 409
// with the certificate to confirm, or an error. The password is only sent to
// an enrollment port that is publicly trusted or presents exactly the
// confirmed certificate, and is never stored or logged.
func takDashboardEnroll(w http.ResponseWriter, label string, cfg *TakConfig, password, fingerprint string, allowLocal bool) bool {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		// Look at the enrollment port's certificate first, sending
		// nothing. Publicly trusted: go on. Otherwise the admin must
		// confirm that exact certificate before the password is sent.
		info, trusted, err := takProbeCert(cfg.Host, cfg.EnrollPort, allowLocal)
		if err != nil {
			log.Printf("TAK [%s]: could not reach %q:%d: %v", label, cfg.Host, cfg.EnrollPort, err)
			writeJSONError(w, http.StatusBadGateway, "Could not reach the TAK server's enrollment port")
			return false
		}
		if !trusted {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{"needs_trust": true, "certificate": info})
			return false
		}
	}
	if err := takEnrollPinned(cfg, password, fp, allowLocal); err != nil {
		log.Printf("TAK [%s]: enrollment failed: %v", label, err)
		// The journal has the detail; the caller gets only what it can
		// act on (not a map of what answers on which internal port).
		msg := "Enrollment with the TAK server failed"
		if strings.Contains(err.Error(), "rejected the username or password") {
			msg = "TAK server rejected the username or password"
		} else if strings.Contains(err.Error(), "not the one that was confirmed") {
			msg = "The TAK server's certificate changed since you confirmed it. Try again"
		}
		writeJSONError(w, http.StatusBadGateway, msg)
		return false
	}
	log.Printf("TAK [%s]: enrolled with %q as %q", label, cfg.Host, cfg.Username)
	return true
}

// --- TAK groups ("channels") ------------------------------------------------------

// TakGroup is one TAK group as the dashboard shows it (TAK lists each group
// twice, once per direction).
type TakGroup struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"` // active for dispatch's connection
}

func (c *TakConfig) apiBase() string {
	port := c.APIPort
	if port <= 0 {
		port = 8443
	}
	return fmt.Sprintf("https://%s:%d", c.Host, port)
}

func (c *TakConfig) apiClient() (*http.Client, error) {
	tc, err := c.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}, nil
}

// takAllGroups returns TAK's raw group list for this certificate (both
// directions, every field — PUT back unchanged apart from "active").
func takAllGroups(c *TakConfig) ([]map[string]interface{}, error) {
	cl, err := c.apiClient()
	if err != nil {
		return nil, err
	}
	resp, err := cl.Get(c.apiBase() + "/Marti/api/groups/all?useCache=true")
	if err != nil {
		return nil, fmt.Errorf("TAK API: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TAK API returned %d listing groups", resp.StatusCode)
	}
	var wrap struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, errors.New("TAK API returned an unreadable group list")
	}
	return wrap.Data, nil
}

// takListGroups: one row per group name, active if dispatch is in it.
func takListGroups(c *TakConfig) ([]TakGroup, error) {
	raw, err := takAllGroups(c)
	if err != nil {
		return nil, err
	}
	byName := map[string]*TakGroup{}
	var order []string
	for _, g := range raw {
		name, _ := g["name"].(string)
		if name == "" {
			continue
		}
		tg := byName[name]
		if tg == nil {
			tg = &TakGroup{Name: name}
			byName[name] = tg
			order = append(order, name)
		}
		if d, _ := g["description"].(string); d != "" {
			tg.Description = d
		}
		if a, _ := g["active"].(bool); a {
			tg.Active = true
		}
	}
	sort.Strings(order)
	out := make([]TakGroup, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// takApplyGroups makes exactly cfg.Groups active (both directions) for
// dispatch's streaming connection.
func takApplyGroups(c *TakConfig) error {
	raw, err := takAllGroups(c)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, g := range c.Groups {
		want[g] = true
	}
	for _, g := range raw {
		name, _ := g["name"].(string)
		g["active"] = want[name]
	}
	body, _ := json.Marshal(raw)
	cl, err := c.apiClient()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("PUT", c.apiBase()+"/Marti/api/groups/active?clientUid="+urlQueryEscape(c.UID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("TAK API: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("TAK API returned %d setting groups", resp.StatusCode)
	}
	return nil
}

func (s *Server) handleGetTakGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agencyID]
	m.mu.Unlock()
	if !ok || cfg.CertPEM == "" {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	groups, err := takListGroups(&cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}

func (s *Server) handleUpdateTakGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID string   `json:"agency_id"`
		Groups   []string `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agencyID]
	if ok {
		cfg.Groups = req.Groups
		m.cfgs[agencyID] = cfg
		if tc := m.conns[agencyID]; tc != nil {
			tc.mu.Lock()
			tc.cfg.Groups = req.Groups
			tc.mu.Unlock()
		}
	}
	m.mu.Unlock()
	if !ok || cfg.CertPEM == "" {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	if len(req.Groups) == 0 {
		writeJSONError(w, http.StatusBadRequest, "Pick at least one group")
		return
	}
	if err := takApplyGroups(&cfg); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.saveTakConnections()
	log.Printf("TAK [%s]: active groups set to %v", agencyID, req.Groups)
	groups, err := takListGroups(&cfg)
	if err != nil {
		groups = nil
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}
