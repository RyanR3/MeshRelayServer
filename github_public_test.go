// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub stands in for the GitHub API and release downloads. Every
// request is counted and fails the test if it carries an Authorization
// header.
type fakeGitHub struct {
	srv      *httptest.Server
	apiCalls atomic.Int32
	latest   func(w http.ResponseWriter, r *http.Request)
	files    map[string][]byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			t.Errorf("%s sent Authorization %q", r.URL.Path, a)
		}
		switch {
		case r.URL.Path == "/repos/owner/repo/releases/latest":
			f.apiCalls.Add(1)
			f.latest(w, r)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			data, ok := f.files[strings.TrimPrefix(r.URL.Path, "/download/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	prevAPI, prevCache := githubReleasesAPI, latestReleaseCache
	githubReleasesAPI = f.srv.URL + "/repos/owner/repo/releases"
	latestReleaseCache = &updateCheckCache{}
	t.Cleanup(func() { githubReleasesAPI, latestReleaseCache = prevAPI, prevCache })
	return f
}

func (f *fakeGitHub) releaseJSON(names ...string) string {
	type asset struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Browser string `json:"browser_download_url"`
	}
	var assets []asset
	for i, n := range names {
		assets = append(assets, asset{
			Name:    n,
			URL:     fmt.Sprintf("%s/repos/owner/repo/releases/assets/%d", f.srv.URL, i+1),
			Browser: f.srv.URL + "/download/" + n,
		})
	}
	b, _ := json.Marshal(map[string]interface{}{"tag_name": "v9.9.9", "assets": assets})
	return string(b)
}

// The update check takes the public download URL (never the API asset URL),
// sends no Authorization header, and asks GitHub once per cache period.
func TestUpdateCheckUsesPublicURLsWithoutToken(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.latest = func(w http.ResponseWriter, r *http.Request) {
		rel := gh.releaseJSON(archBinaryAsset(), "web.tar.gz", "SHA256SUMS")
		w.Write([]byte(rel))
	}
	s := newWebTestServer(t)
	h := s.webHandler()

	var got map[string]interface{}
	for i := 0; i < 3; i++ {
		rec := doReq(h, "POST", "/api/update/check", "super", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("update check = %d %s", rec.Code, rec.Body.String())
		}
		json.Unmarshal(rec.Body.Bytes(), &got)
	}
	want := map[string]string{
		"binary_url":   gh.srv.URL + "/download/" + archBinaryAsset(),
		"web_url":      gh.srv.URL + "/download/web.tar.gz",
		"checksum_url": gh.srv.URL + "/download/SHA256SUMS",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %s", k, got[k], v)
		}
	}
	if n := gh.apiCalls.Load(); n != 1 {
		t.Fatalf("GitHub API called %d times for 3 checks, want 1 (cached)", n)
	}
}

// The download helpers used by the updater and the helper installer send no
// Authorization header.
func TestUpdateDownloadsSendNoAuthorization(t *testing.T) {
	gh := newFakeGitHub(t)
	binary := []byte("new server binary")
	sum := sha256.Sum256(binary)
	gh.files["ptt-server-linux"] = binary
	gh.files["SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  ptt-server-linux\n")

	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	body := []byte("<html></html>")
	tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	gh.files["web.tar.gz"] = tgz.Bytes()

	s := newWebTestServer(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := downloadFile(gh.srv.URL+"/download/ptt-server-linux", bin, 1<<20); err != nil {
		t.Fatal(err)
	}
	ok, err := s.verifyBinaryChecksum(bin, "ptt-server-linux", gh.srv.URL+"/download/SHA256SUMS")
	if err != nil || !ok {
		t.Fatalf("checksum = %v, %v", ok, err)
	}
	if err := downloadAndExtractTarGz(gh.srv.URL+"/download/web.tar.gz", filepath.Join(dir, "web")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "web", "index.html")); !bytes.Equal(b, body) {
		t.Fatalf("web file = %q", b)
	}
}

// The update apply allowlist takes only the public download URLs.
func TestUpdateApplyRejectsAPIAssetURL(t *testing.T) {
	s := newWebTestServer(t)
	h := s.webHandler()
	rec := doReq(h, "POST", "/api/update/apply", "super",
		`{"binary_url":"https://api.github.com/repos/RyanR3/MeshRelayServer/releases/assets/1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("API asset URL = %d, want 400", rec.Code)
	}
}

// A rate-limited answer from GitHub gives the admin a clear message with the
// wait, and no further calls are made until the limit resets.
func TestUpdateCheckRateLimited(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		want   string
	}{
		{"403 with reset", http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     fmt.Sprint(time.Now().Add(25 * time.Minute).Unix()),
		}, "GitHub is limiting requests from this address; try again in 25 minutes"},
		{"429 retry-after", http.StatusTooManyRequests, map[string]string{
			"Retry-After": "120",
		}, "GitHub is limiting requests from this address; try again in 2 minutes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.latest = func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			}
			s := newWebTestServer(t)
			h := s.webHandler()
			for i := 0; i < 2; i++ {
				rec := doReq(h, "POST", "/api/update/check", "super", "")
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("check %d = %d, want 503", i, rec.Code)
				}
				var got map[string]interface{}
				json.Unmarshal(rec.Body.Bytes(), &got)
				if got["rate_limited"] != true {
					t.Errorf("rate_limited = %v", got["rate_limited"])
				}
				if i == 0 && got["error"] != c.want {
					t.Errorf("error = %q, want %q", got["error"], c.want)
				}
				if msg, _ := got["error"].(string); !strings.HasPrefix(msg, "GitHub is limiting requests from this address; try again in ") {
					t.Errorf("check %d error = %q", i, msg)
				}
			}
			if n := gh.apiCalls.Load(); n != 1 {
				t.Fatalf("GitHub API called %d times while limited, want 1", n)
			}
		})
	}
}

// A 403 that is not a rate limit stays a plain error.
func TestUpdateCheckOtherForbiddenIsNotRateLimit(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.latest = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	}
	s := newWebTestServer(t)
	rec := doReq(s.webHandler(), "POST", "/api/update/check", "super", "")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "limiting") {
		t.Fatalf("plain 403 = %d %s", rec.Code, rec.Body.String())
	}
}

// A config.json from an older version with github_token loads, the token is
// not kept anywhere, and it is removed from the file (at start-up and on any
// later save), with one log line that does not print it.
func TestRetiredGitHubTokenDropped(t *testing.T) {
	const secret = "ghp_oldtoken0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	orig := `{
  "ptt_port": 5001,
  "web_port": 8081,
  "log_level": "DEBUG",
  "github_token": "` + secret + `",
  "some_future_key": {"a": 1}
}
`
	os.WriteFile(path, []byte(orig), 0o600)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PTTPort != 5001 || cfg.WebPort != 8081 || cfg.LogLevel != "DEBUG" {
		t.Fatalf("config not loaded: %+v", cfg)
	}
	if b, _ := json.Marshal(cfg); bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte("github_token")) {
		t.Fatalf("loaded config still carries the token: %s", b)
	}

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	if err := dropRetiredConfigKeys(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte("github_token")) {
		t.Fatalf("rewritten config still has the token: %s", data)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("rewritten config is not JSON: %v", err)
	}
	for _, k := range []string{"ptt_port", "web_port", "log_level", "some_future_key"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("key %s lost in the rewrite", k)
		}
	}
	logs := logBuf.String()
	if strings.Count(logs, "github_token is no longer used and was removed from config.json") != 1 {
		t.Errorf("log = %q", logs)
	}
	if strings.Contains(logs, secret) {
		t.Fatal("token printed in the log")
	}

	// Second start: nothing to drop, file left alone, nothing logged.
	logBuf.Reset()
	before, _ := os.ReadFile(path)
	if err := dropRetiredConfigKeys(path); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) || logBuf.Len() != 0 {
		t.Fatalf("second run changed the file or logged %q", logBuf.String())
	}

	// A save of a config loaded from a file that still had the token
	// leaves it out too.
	os.WriteFile(path, []byte(orig), 0o600)
	cfg, _ = LoadConfig(path)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); bytes.Contains(data, []byte("github_token")) {
		t.Fatalf("saved config has the token: %s", data)
	}
}

// The token routes are gone.
func TestGitHubTokenRoutesRemoved(t *testing.T) {
	s := newWebTestServer(t)
	h := s.webHandler()
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if rec := doReq(h, m, "/api/github-token", "super", `{"token":"x"}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s /api/github-token = %d, want 404", m, rec.Code)
		}
	}
}
