// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Self-signed HTTPS for the dashboard when nothing fronts it: an infra-TAK
// box in IP mode (no domain, so no Caddy and no Let's Encrypt). With
// MESHRELAY_TLS_SELF_SIGNED=1 and no tls_cert_file configured, the server
// keeps a certificate in tls/ under its data directory, for the names and
// addresses in MESHRELAY_TLS_HOSTS, and serves the dashboard over HTTPS.
// Browsers warn once; passwords still never cross the network in clear text.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const selfSignedDir = "tls"

// ensureSelfSignedTLS points the config at the self-signed certificate,
// creating it (or re-creating it when the host list changed or it is close to
// expiry).
func ensureSelfSignedTLS(c *Config) error {
	if c == nil || !c.TLSSelfSigned || c.TLSCertFile != "" {
		return nil
	}
	certPath := filepath.Join(selfSignedDir, "selfsigned.crt")
	keyPath := filepath.Join(selfSignedDir, "selfsigned.key")
	hosts := normalizedHosts(c.TLSHosts)
	if !selfSignedCurrent(certPath, keyPath, hosts) {
		if err := writeSelfSigned(certPath, keyPath, hosts); err != nil {
			return err
		}
		log.Printf("TLS: new self-signed dashboard certificate for %s", strings.Join(hosts, ", "))
	}
	c.TLSCertFile, c.TLSKeyFile = certPath, keyPath
	return nil
}

func normalizedHosts(in []string) []string {
	set := map[string]bool{"localhost": true, "127.0.0.1": true}
	for _, h := range in {
		if h = strings.TrimSpace(h); h != "" {
			set[strings.ToLower(h)] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func certHosts(c *x509.Certificate) []string {
	var hs []string
	hs = append(hs, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		hs = append(hs, ip.String())
	}
	return normalizedHosts(hs)
}

func selfSignedCurrent(certPath, keyPath string, hosts []string) bool {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return false
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil || time.Until(c.NotAfter) < 30*24*time.Hour {
		return false
	}
	return strings.Join(certHosts(c), ",") == strings.Join(hosts, ",")
}

func writeSelfSigned(certPath, keyPath string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "MeshRelay dashboard", Organization: []string{"MeshRelay"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(5 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(selfSignedDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return fmt.Errorf("writing TLS key: %w", err)
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
