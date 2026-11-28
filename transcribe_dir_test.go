// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeProbe answers from fixed sets so the rule is tested without touching
// permissions (which root ignores).
type fakeProbe struct {
	dirs, files, w map[string]bool
}

func (f fakeProbe) isDir(p string) bool    { return f.dirs[p] }
func (f fakeProbe) exists(p string) bool   { return f.dirs[p] || f.files[p] }
func (f fakeProbe) writable(p string) bool { return f.w[p] }

func TestTranscribeDirFor(t *testing.T) {
	exe := "/opt/meshrelay"
	next := filepath.Join(exe, "transcribe")
	cases := []struct {
		name, configured string
		p                fakeProbe
		want             string
	}{
		{"explicit transcribe_dir always wins", "/data/tr",
			fakeProbe{dirs: map[string]bool{next: true}, w: map[string]bool{next: true, exe: true}}, "/data/tr"},
		{"existing writable folder next to the binary", "",
			fakeProbe{dirs: map[string]bool{next: true}, w: map[string]bool{next: true}}, next},
		{"existing read-only folder with an installed model", "",
			fakeProbe{dirs: map[string]bool{next: true}, files: map[string]bool{filepath.Join(next, "settings.json"): true}}, next},
		{"existing read-only empty folder falls back", "",
			fakeProbe{dirs: map[string]bool{next: true}}, "transcribe"},
		{"no folder, writable binary folder (install.sh layout)", "",
			fakeProbe{w: map[string]bool{exe: true}}, next},
		{"no folder, read-only binary folder falls back to the working directory", "",
			fakeProbe{}, "transcribe"},
	}
	for _, c := range cases {
		if got := transcribeDirFor(c.configured, exe, c.p); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := transcribeDirFor("", "", fakeProbe{}); got != "transcribe" {
		t.Errorf("unknown binary folder: got %q", got)
	}
}

// The real probe: a folder we can write is writable, a missing one is not.
func TestOSDirProbe(t *testing.T) {
	dir := t.TempDir()
	p := osDirProbe{}
	if !p.isDir(dir) || !p.writable(dir) {
		t.Fatal("temp dir not seen as a writable folder")
	}
	missing := filepath.Join(dir, "nope")
	if p.isDir(missing) || p.writable(missing) || p.exists(missing) {
		t.Fatal("missing folder reported as usable")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("writable() left %d file(s) behind", len(entries))
	}
}

// installWebTree renames new files over old ones, so it works when the
// service may write the folder but not the existing files (install.sh: the
// files belong to the user who cloned the repository).
func TestInstallWebTreeReplacesReadOnlyFiles(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "web.new"), filepath.Join(root, "web")
	for _, d := range []string{filepath.Join(src, "vendor"), filepath.Join(dst, "vendor")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dst, "app.js"), []byte("old"), 0o444)
	os.WriteFile(filepath.Join(dst, "vendor", "x.js"), []byte("old"), 0o444)
	os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("mine"), 0o644)
	os.WriteFile(filepath.Join(src, "app.js"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(src, "vendor", "x.js"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(src, "added.css"), []byte("new"), 0o644)

	if err := installWebTree(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"app.js", "vendor/x.js", "added.css"} {
		if b, _ := os.ReadFile(filepath.Join(dst, f)); string(b) != "new" {
			t.Errorf("%s = %q, want new", f, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "mine" {
		t.Error("a file not in the update was touched")
	}
}
