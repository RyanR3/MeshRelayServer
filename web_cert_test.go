// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCert(t *testing.T, certFile, keyFile, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0600); err != nil {
		t.Fatal(err)
	}
}

func leafCN(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// A renewed certificate written over the configured files is served without a
// restart; a broken replacement leaves the previous certificate in service.
func TestCertReloaderPicksUpRenewal(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "fullchain.pem")
	keyFile := filepath.Join(dir, "privkey.pem")
	writeTestCert(t, certFile, keyFile, "old.example")

	r, err := newCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if cn := leafCN(t, r); cn != "old.example" {
		t.Fatalf("initial cert CN = %q, want old.example", cn)
	}

	// Renewal: new files with a later mtime.
	writeTestCert(t, certFile, keyFile, "new.example")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(certFile, later, later)
	os.Chtimes(keyFile, later, later)

	// Inside the 10 s check interval nothing changes yet.
	if cn := leafCN(t, r); cn != "old.example" {
		t.Fatalf("cert reloaded before check interval: %q", cn)
	}
	r.lastCheck = time.Now().Add(-time.Minute)
	if cn := leafCN(t, r); cn != "new.example" {
		t.Fatalf("renewed cert not served: CN = %q", cn)
	}

	// Broken replacement: keep serving the renewed certificate.
	os.WriteFile(certFile, []byte("garbage"), 0644)
	much := time.Now().Add(4 * time.Second)
	os.Chtimes(certFile, much, much)
	r.lastCheck = time.Now().Add(-time.Minute)
	if cn := leafCN(t, r); cn != "new.example" {
		t.Fatalf("broken cert file replaced the good certificate: CN = %q", cn)
	}
}

func TestCertReloaderMissingFiles(t *testing.T) {
	if _, err := newCertReloader("/nonexistent/fullchain.pem", "/nonexistent/privkey.pem"); err == nil {
		t.Fatal("expected an error for missing certificate files")
	}
}
