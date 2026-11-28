// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The binary's version has one source: the git tag, passed in with
// -X main.Version (install.sh, build.sh, the Dockerfile). A VERSION file
// next to it went stale for years (it said v3.3.0 at v4.10.1), so there is
// none, and nothing may read one.
func TestNoVersionFile(t *testing.T) {
	if _, err := os.Stat("VERSION"); err == nil {
		t.Fatal("a VERSION file is back; the version comes from the git tag only")
	}
	for _, f := range []string{"install.sh", "build.sh", "Dockerfile"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`["/]VERSION["\s]|cat [^|]*VERSION`).Match(b) {
			t.Errorf("%s reads a VERSION file", f)
		}
	}
}

// install.sh reads the Go version from go.mod and states none of its own,
// builds without VCS stamping (git's ownership check must not stop an
// update), and does not open parent folders to everyone.
func TestInstallScriptRules(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if m := regexp.MustCompile(`go1\.\d+|golang-go`).FindString(src); m != "" {
		t.Errorf("install.sh names a Go version or package (%q); it must come from go.mod", m)
	}
	for _, want := range []string{`"toolchain"`, "go.mod", "-buildvcs=false", "sha256sum -c", "-X main.Version="} {
		if !strings.Contains(src, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	for _, bad := range []string{"chmod o+x", "chown -R \"$SERVICE_USER", "chown -R $SERVICE_USER"} {
		if strings.Contains(src, bad) {
			t.Errorf("install.sh contains %q", bad)
		}
	}
	gomod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^toolchain go\d+\.\d+\.\d+$`).Match(gomod) {
		t.Error("go.mod has no `toolchain goX.Y.Z` line for install.sh to read")
	}
}
