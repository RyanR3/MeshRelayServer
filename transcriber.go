// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Speech-to-text for recorded radio traffic.
//
// When an agency switches transcription on (Settings → Radio Recording), every
// finished recording of that agency is queued here and turned into text by
// mrtranscribe — a small static helper built from whisper.cpp (OpenAI's
// Whisper model) and libopus (transcribe/ in the source). Everything runs on
// this server: no audio or text is sent anywhere.
//
// The helper and the model are not part of the server binary. A super admin
// installs them from the dashboard: the helper comes from this release's
// GitHub assets (checked against SHA256SUMS like a server update), the model
// from Hugging Face (checked against the SHA-256 below). Files live in
// <install dir>/transcribe/.
//
// One helper process loads the model once and handles one recording at a time
// at low CPU priority, so voice relay is never starved; it exits after a few
// idle minutes to give its memory back. Results go to
// <recordings>/<day>/transcripts.jsonl next to the recording index, so they
// are deleted with the recordings, and are pushed to the agency's consoles.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type transcribeModel struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	File   string `json:"-"`
	URL    string `json:"-"`
	Size   int64  `json:"size"`
	SHA256 string `json:"-"`
	RAMMB  int    `json:"ram_mb"`
}

const hfWhisper = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"

var transcribeModels = []transcribeModel{
	{"tiny.en", "Tiny — fastest, least accurate", "ggml-tiny.en.bin", hfWhisper + "ggml-tiny.en.bin",
		77704715, "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f", 200},
	{"base.en", "Base — recommended", "ggml-base.en.bin", hfWhisper + "ggml-base.en.bin",
		147964211, "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002", 300},
	{"small.en", "Small — most accurate, needs a fast server", "ggml-small.en.bin", hfWhisper + "ggml-small.en.bin",
		487614201, "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d", 750},
}

// The speech detector: transmissions without speech (squelch noise, key-ups)
// are not given to Whisper, which would otherwise invent words for them.
var vadModel = transcribeModel{"silero", "Speech detector", "ggml-silero-v5.1.2.bin",
	"https://huggingface.co/ggml-org/whisper-vad/resolve/main/ggml-silero-v5.1.2.bin",
	885098, "29940d98d42b91fbd05ce489f3ecf7c72f0a42f027e4875919a28fb4c04ea2cf", 0}

const (
	trHelperFile   = "mrtranscribe"
	trQueueSize    = 2000
	trIdleExit     = 5 * time.Minute
	trMinAudio     = 400 * time.Millisecond
	trMaxWords     = 400 // characters of agency vocabulary given to Whisper
	trNice         = 10
	trStartTimeout = 90 * time.Second
)

func findTranscribeModel(name string) *transcribeModel {
	for i := range transcribeModels {
		if transcribeModels[i].Name == name {
			return &transcribeModels[i]
		}
	}
	return nil
}

type trSettings struct {
	Model  string `json:"model"`
	Helper string `json:"helper"` // release asset the helper came from
}

type trInstall struct {
	InProgress bool   `json:"in_progress"`
	Stage      string `json:"stage"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error"`
}

type trJob struct {
	info RecordingInfo
	path string
}

type Transcriber struct {
	s   *Server
	dir string

	mu       sync.Mutex
	settings trSettings
	install  trInstall

	queue chan trJob
	gen   atomic.Int64 // bumped on install/remove: the worker restarts its helper

	done, failed, empty atomic.Int64
	lastMs, lastAudio   atomic.Int64
}

func NewTranscriber(s *Server, dir string) *Transcriber {
	if dir == "" {
		dir = "transcribe"
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Join(filepath.Dir(exe), "transcribe")
		}
	}
	t := &Transcriber{s: s, dir: dir, queue: make(chan trJob, trQueueSize)}
	if data, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		json.Unmarshal(data, &t.settings)
	}
	go t.worker()
	return t
}

func (t *Transcriber) helperPath() string { return filepath.Join(t.dir, trHelperFile) }

// ready reports whether the helper and the chosen model are installed.
func (t *Transcriber) ready() (bool, *transcribeModel) {
	if t == nil {
		return false, nil
	}
	t.mu.Lock()
	m := findTranscribeModel(t.settings.Model)
	busy := t.install.InProgress
	t.mu.Unlock()
	if m == nil || busy {
		return false, m
	}
	for _, f := range []string{t.helperPath(), filepath.Join(t.dir, m.File), filepath.Join(t.dir, vadModel.File)} {
		if _, err := os.Stat(f); err != nil {
			return false, m
		}
	}
	return true, m
}

func trThreads() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return n
}

// --- queue -----------------------------------------------------------------------

// queueTranscription is called when a recording finishes. Returns true when
// it will be transcribed (the console then shows "transcribing…").
func (s *Server) queueTranscription(info RecordingInfo) bool {
	t := s.transcriber
	if t == nil || s.recorder == nil || time.Duration(info.DurationMs)*time.Millisecond < trMinAudio {
		return false
	}
	s.mu.RLock()
	a := s.agencies[info.Agency]
	on := a != nil && a.Transcribe && !a.NoRecording
	s.mu.RUnlock()
	if !on {
		return false
	}
	if ok, _ := t.ready(); !ok {
		return false
	}
	select {
	case t.queue <- trJob{info: info, path: filepath.Join(s.recorder.dir, filepath.Clean(info.File))}:
		return true
	default:
		log.Printf("Transcription: queue full, skipping recording %s", info.ID)
		return false
	}
}

type trProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	gen   int64
}

func (p *trProc) stop() {
	if p == nil {
		return
	}
	p.stdin.Close()
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		p.cmd.Process.Kill()
		<-done
	}
}

func (t *Transcriber) startHelper() (*trProc, error) {
	ok, m := t.ready()
	if !ok {
		return nil, fmt.Errorf("transcription is not installed")
	}
	cmd := exec.Command(t.helperPath(), "--model", filepath.Join(t.dir, m.File),
		"--vad", filepath.Join(t.dir, vadModel.File), "--threads", fmt.Sprint(trThreads()), "--lang", "en")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &trLogWriter{}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Low priority: radio relay always comes first.
	syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, trNice)
	p := &trProc{cmd: cmd, stdin: stdin, lines: make(chan string, 4), gen: t.gen.Load()}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	select {
	case line, ok := <-p.lines:
		if !ok || !strings.Contains(line, `"ready"`) {
			p.stop()
			return nil, fmt.Errorf("helper did not start: %s", line)
		}
	case <-time.After(trStartTimeout):
		p.stop()
		return nil, fmt.Errorf("helper did not start within %v", trStartTimeout)
	}
	log.Printf("Transcription: helper started (model %q, %d threads)", m.Name, trThreads())
	return p, nil
}

// trLogWriter passes the helper's error output to the server log.
type trLogWriter struct{}

func (trLogWriter) Write(b []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			log.Printf("Transcription helper: %s", l)
		}
	}
	return len(b), nil
}

type trResult struct {
	OK       bool    `json:"ok"`
	Text     string  `json:"text"`
	Error    string  `json:"error"`
	NoSpeech float64 `json:"no_speech"`
	SpeechMs int     `json:"speech_ms"`
	AudioMs  int64   `json:"audio_ms"`
	Ms       int64   `json:"ms"`
}

func (t *Transcriber) worker() {
	var p *trProc
	lastUse := time.Now()
	idle := time.NewTicker(30 * time.Second)
	defer idle.Stop()
	for {
		select {
		case job := <-t.queue:
			if p != nil && p.gen != t.gen.Load() {
				p.stop()
				p = nil
			}
			if p == nil {
				var err error
				if p, err = t.startHelper(); err != nil {
					log.Printf("Transcription: %v", err)
					t.failed.Add(1)
					continue
				}
			}
			lastUse = time.Now()
			res, err := t.ask(p, job)
			if err != nil {
				log.Printf("Transcription of %s failed: %v", job.info.ID, err)
				t.failed.Add(1)
				p.cmd.Process.Kill()
				p.stop()
				p = nil
				continue
			}
			t.finish(job, res)
		case <-idle.C:
			if p != nil && (time.Since(lastUse) > trIdleExit || p.gen != t.gen.Load()) {
				p.stop()
				p = nil
				log.Printf("Transcription: helper stopped (idle)")
			}
		}
	}
}

func (t *Transcriber) ask(p *trProc, job trJob) (*trResult, error) {
	words := t.agencyWords(job.info.Agency)
	if _, err := fmt.Fprintf(p.stdin, "%s\t%s\n", job.path, words); err != nil {
		return nil, err
	}
	// Generous: a slow CPU may run at a few times real time.
	timeout := 60*time.Second + 8*time.Duration(job.info.DurationMs)*time.Millisecond
	select {
	case line, ok := <-p.lines:
		if !ok {
			return nil, fmt.Errorf("helper exited")
		}
		var res trResult
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			return nil, fmt.Errorf("bad reply %q", line)
		}
		if !res.OK {
			return nil, fmt.Errorf("%s", res.Error)
		}
		return &res, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out after %v", timeout)
	}
}

// agencyWords is the agency's vocabulary (unit names, places…) given to
// Whisper as a hint, on one line.
func (t *Transcriber) agencyWords(agency string) string {
	t.s.mu.RLock()
	a := t.s.agencies[agency]
	w := ""
	if a != nil {
		w = a.TranscribeWords
	}
	t.s.mu.RUnlock()
	w = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, w)), " ")
	if len(w) > trMaxWords {
		w = w[:trMaxWords]
	}
	return w
}

// Phrases Whisper is known to produce from noise or silence. Only dropped when
// they are the whole transcript of a short transmission.
var trPhantoms = map[string]bool{
	"you": true, "thank you": true, "thanks for watching": true, "thank you for watching": true,
	"bye": true, "okay": false, "the end": true, "so": true, "uh": true, "um": true, "hmm": true,
}

func cleanTranscript(text string, audioMs int64) string {
	text = strings.TrimSpace(text)
	// Remove bracketed non-speech tags such as [BLANK_AUDIO] or (static).
	for _, pair := range [][2]string{{"[", "]"}, {"(", ")"}, {"*", "*"}} {
		for {
			i := strings.Index(text, pair[0])
			if i < 0 {
				break
			}
			j := strings.Index(text[i+1:], pair[1])
			if j < 0 {
				break
			}
			text = text[:i] + text[i+1+j+1:]
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	key := strings.ToLower(strings.Trim(text, " .!?,"))
	if key == "" || (trPhantoms[key] && audioMs < 3000) {
		return ""
	}
	return text
}

type transcriptLine struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
	At    int64  `json:"at"`
}

func (t *Transcriber) finish(job trJob, res *trResult) {
	text := cleanTranscript(res.Text, res.AudioMs)
	t.lastMs.Store(res.Ms)
	t.lastAudio.Store(res.AudioMs)
	if text == "" {
		t.empty.Add(1)
	} else {
		t.done.Add(1)
	}
	_, m := t.ready()
	model := ""
	if m != nil {
		model = m.Name
	}
	t.s.recorder.appendTranscript(job.info, transcriptLine{ID: job.info.ID, Text: text, Model: model, At: time.Now().UnixMilli()})
	state := byte(trStateDone)
	if text == "" {
		state = trStateEmpty
	}
	t.s.pushTranscriptToPhones(job.info, state, text)
	t.s.sendToConsoles(job.info.Agency, map[string]interface{}{
		"type": "transcript", "id": job.info.ID, "agency": job.info.Agency, "channel": job.info.Channel,
		"user": job.info.User, "start": job.info.Start, "text": text,
	})
}

// --- recorder side -----------------------------------------------------------------

func (r *Recorder) appendTranscript(info RecordingInfo, line transcriptLine) {
	day := time.UnixMilli(info.Start).Format(recDayFmt)
	b, _ := json.Marshal(line)
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.dir, day, "transcripts.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("Transcripts: %v", err)
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// transcripts loads one day's transcripts (id → text). Caller holds indexMu.
func (r *Recorder) transcripts(day string) map[string]string {
	data, err := os.ReadFile(filepath.Join(r.dir, day, "transcripts.jsonl"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		var t transcriptLine
		if json.Unmarshal([]byte(l), &t) == nil && t.ID != "" {
			out[t.ID] = t.Text
		}
	}
	return out
}

// --- install (super admin, dashboard) -------------------------------------------------

func (t *Transcriber) setInstall(stage string, progress int, msg, errMsg string) {
	t.mu.Lock()
	t.install.Stage, t.install.Progress, t.install.Message, t.install.Error = stage, progress, msg, errMsg
	if stage == "done" || stage == "error" {
		t.install.InProgress = false
	}
	t.mu.Unlock()
	if errMsg != "" {
		log.Printf("Transcription install: %s", errMsg)
	} else if msg != "" {
		log.Printf("Transcription install: %q", msg)
	}
}

func (t *Transcriber) saveSettings() error {
	if err := os.MkdirAll(t.dir, 0o750); err != nil {
		return err
	}
	return writeJSONFileAtomic(filepath.Join(t.dir, "settings.json"), t.settings)
}

// helperAssets are the release assets to try for this machine, best first.
func helperAssets() []string {
	switch runtime.GOARCH {
	case "arm64":
		return []string{"mrtranscribe-linux-arm64", "mrtranscribe-linux-arm64-basic"}
	case "amd64":
		return []string{"mrtranscribe-linux-amd64", "mrtranscribe-linux-amd64-basic"}
	}
	return nil
}

func (t *Transcriber) runInstall(modelName string) {
	m := findTranscribeModel(modelName)
	if err := os.MkdirAll(t.dir, 0o750); err != nil {
		t.setInstall("error", 0, "", "cannot create "+t.dir+": "+err.Error())
		return
	}

	// 1. Models (the big download first, so a bad network fails early).
	for i, mm := range []*transcribeModel{&vadModel, m} {
		base := 5 + i*5
		span := 5
		if mm == m {
			span = 60
		}
		if err := t.fetchModel(mm, base, span); err != nil {
			t.setInstall("error", base, "", err.Error())
			return
		}
	}

	// 2. Helper from this release, verified against its SHA256SUMS. A server
	//    without a matching release (a dev build) keeps a helper that is
	//    already there.
	var candidates []string
	var err error
	if dir := t.s.config.TranscribeHelperDir; dir != "" {
		t.setInstall("helper", 75, "Preparing the transcription program…", "")
		candidates, err = t.bundledHelpers(dir)
	} else {
		t.setInstall("helper", 75, "Downloading the transcription program…", "")
		candidates, err = t.fetchHelpers()
	}
	if err != nil {
		if _, statErr := os.Stat(t.helperPath()); statErr != nil {
			t.setInstall("error", 75, "", err.Error())
			return
		}
		log.Printf("Transcription install: %v — keeping the installed helper", err)
		candidates = []string{""}
	}

	// 3. Self-test each candidate with the model; keep the first that works
	//    (an x86 server without AVX2 falls back to the basic build).
	t.setInstall("testing", 85, "Testing on this server…", "")
	var lastErr error
	chosen, passed := "", false
	for _, c := range candidates {
		if c != "" {
			if err := os.Rename(filepath.Join(t.dir, c+".new"), t.helperPath()); err != nil {
				lastErr = err
				continue
			}
		}
		ms, err := t.selfTest(m)
		if err == nil {
			chosen, passed = c, true
			log.Printf("Transcription install: self-test passed in %d ms", ms)
			break
		}
		lastErr = err
		log.Printf("Transcription install: self-test of %s failed: %v", nonEmpty(c, "installed helper"), err)
	}
	for _, c := range candidates {
		if c != "" {
			os.Remove(filepath.Join(t.dir, c+".new"))
		}
	}
	if !passed {
		t.setInstall("error", 85, "", "the transcription program does not run on this server: "+lastErr.Error())
		return
	}

	// Remove other models to free disk space.
	for _, other := range transcribeModels {
		if other.Name != m.Name {
			os.Remove(filepath.Join(t.dir, other.File))
		}
	}
	t.mu.Lock()
	t.settings.Model = m.Name
	if chosen != "" {
		t.settings.Helper = chosen
	}
	err = t.saveSettings()
	t.mu.Unlock()
	if err != nil {
		t.setInstall("error", 95, "", "failed to save settings: "+err.Error())
		return
	}
	t.gen.Add(1)
	t.setInstall("done", 100, "Transcription is installed ("+m.Name+")", "")
}

// fetchModel downloads a model unless an intact copy is already there.
func (t *Transcriber) fetchModel(m *transcribeModel, base, span int) error {
	path := filepath.Join(t.dir, m.File)
	if st, err := os.Stat(path); err == nil && st.Size() == m.Size {
		t.setInstall("verifying", base, "Checking "+m.File+"…", "")
		if sum, err := fileSHA256(path); err == nil && sum == m.SHA256 {
			return nil
		}
	}
	t.setInstall("downloading", base, fmt.Sprintf("Downloading %s (%d MB)…", m.File, m.Size>>20), "")
	client := &http.Client{Timeout: 60 * time.Minute}
	resp, err := client.Get(m.URL)
	if err != nil {
		return fmt.Errorf("download of %s failed: %v", m.File, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download of %s failed: HTTP %d", m.File, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	var got int64
	buf := make([]byte, 256*1024)
	lastPct := -1
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			f.Write(buf[:n])
			h.Write(buf[:n])
			got += int64(n)
			if got > m.Size {
				break
			}
			if pct := int(got * 100 / m.Size); pct != lastPct {
				lastPct = pct
				t.mu.Lock()
				t.install.Progress = base + span*pct/100
				t.install.Message = fmt.Sprintf("Downloading %s — %d of %d MB", m.File, got>>20, m.Size>>20)
				t.mu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("download of %s failed: %v", m.File, rerr)
		}
	}
	f.Close()
	if got != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("%s did not match its published checksum — not installed", m.File)
	}
	return os.Rename(tmp, path)
}

// bundledHelpers copies this machine's helper builds from dir (shipped with
// the server, e.g. inside the Docker image) as <asset>.new, best first.
func (t *Transcriber) bundledHelpers(dir string) ([]string, error) {
	want := helperAssets()
	if want == nil {
		return nil, fmt.Errorf("no transcription program is built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var got []string
	for _, name := range want {
		src, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		tmp := filepath.Join(t.dir, name+".new")
		dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err == nil {
			_, err = io.Copy(dst, src)
			if cerr := dst.Close(); err == nil {
				err = cerr
			}
		}
		src.Close()
		if err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("cannot copy %s: %v", name, err)
		}
		got = append(got, name)
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("no transcription program for this server in %s", dir)
	}
	return got, nil
}

// fetchHelpers downloads this machine's helper builds from the release that
// matches the running server and verifies each against SHA256SUMS. They are
// left as <asset>.new; returns the asset names, best first.
func (t *Transcriber) fetchHelpers() ([]string, error) {
	want := helperAssets()
	if want == nil {
		return nil, fmt.Errorf("no transcription program is built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	// Public release, unauthenticated API call and public downloads.
	api := githubReleasesAPI + "/latest"
	if Version != "dev" && Version != "" {
		api = githubReleasesAPI + "/tags/" + Version
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(api)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %v", err)
	}
	defer resp.Body.Close()
	if limited, wait := githubRateLimitWait(resp, time.Now()); limited {
		return nil, fmt.Errorf("%s", githubRateLimitMessage(wait))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("release %s not found on GitHub (HTTP %d)", nonEmpty(Version, "latest"), resp.StatusCode)
	}
	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("bad GitHub response")
	}
	urls := map[string]string{}
	sums := ""
	for _, a := range rel.Assets {
		u := a.BrowserDownloadURL
		urls[a.Name] = u
		if a.Name == "SHA256SUMS" {
			sums = u
		}
	}
	var got []string
	var lastErr error
	for _, name := range want {
		u := urls[name]
		if u == "" {
			continue
		}
		tmp := filepath.Join(t.dir, name+".new")
		if err := downloadFile(u, tmp, 64<<20); err != nil {
			lastErr = err
			continue
		}
		ok, err := t.s.verifyBinaryChecksum(tmp, name, sums)
		if err != nil || !ok {
			os.Remove(tmp)
			lastErr = fmt.Errorf("%s failed checksum verification", name)
			continue
		}
		os.Chmod(tmp, 0o755)
		got = append(got, name)
	}
	if len(got) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("release %s has no transcription program for this server", rel.TagName)
		}
		return nil, lastErr
	}
	return got, nil
}

func downloadFile(url, dest string, max int64) error {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	f.Close()
	if err == nil && n > max {
		err = fmt.Errorf("file too large")
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// selfTest runs the helper once on silence; returns how long that took.
func (t *Transcriber) selfTest(m *transcribeModel) (int64, error) {
	cmd := exec.Command(t.helperPath(), "--model", filepath.Join(t.dir, m.File),
		"--vad", filepath.Join(t.dir, vadModel.File), "--threads", fmt.Sprint(trThreads()), "--selftest")
	cmd.Stderr = &trLogWriter{}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return 0, fmt.Errorf("timed out")
	}
	if err != nil {
		return 0, err
	}
	var res trResult
	if json.Unmarshal(out, &res) != nil || !res.OK {
		return 0, fmt.Errorf("unexpected output %q", strings.TrimSpace(string(out)))
	}
	return res.Ms, nil
}

// --- HTTP ---------------------------------------------------------------------------

func (t *Transcriber) status() map[string]interface{} {
	ok, _ := t.ready()
	t.mu.Lock()
	inst := t.install
	model := t.settings.Model
	helper := t.settings.Helper
	t.mu.Unlock()
	models := []map[string]interface{}{}
	for _, mm := range transcribeModels {
		_, err := os.Stat(filepath.Join(t.dir, mm.File))
		models = append(models, map[string]interface{}{
			"name": mm.Name, "label": mm.Label, "size_mb": mm.Size >> 20, "ram_mb": mm.RAMMB, "downloaded": err == nil,
		})
	}
	return map[string]interface{}{
		"installed": ok, "model": model, "helper": helper, "models": models, "install": inst,
		"queue": len(t.queue), "transcribed": t.done.Load(), "no_speech": t.empty.Load(), "failed": t.failed.Load(),
		"last_ms": t.lastMs.Load(), "last_audio_ms": t.lastAudio.Load(),
		"arch": runtime.GOARCH, "cpus": runtime.NumCPU(), "threads": trThreads(),
	}
}

// GET /api/transcription (super admin)
func (s *Server) handleTranscriptionStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.transcriber.status())
}

// POST /api/transcription/install {model} (super admin)
func (s *Server) handleTranscriptionInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if findTranscribeModel(req.Model) == nil {
		writeJSONError(w, http.StatusBadRequest, "unknown model")
		return
	}
	t := s.transcriber
	t.mu.Lock()
	if t.install.InProgress {
		t.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "an install is already running")
		return
	}
	t.install = trInstall{InProgress: true, Stage: "starting", Message: "Starting…"}
	t.mu.Unlock()
	log.Printf("Transcription install of %q started by %q", req.Model, s.sessionUser(r))
	go t.runInstall(req.Model)
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// POST /api/transcription/remove (super admin): delete the model and helper.
func (s *Server) handleTranscriptionRemove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	t := s.transcriber
	t.mu.Lock()
	if t.install.InProgress {
		t.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "an install is running")
		return
	}
	t.settings = trSettings{}
	err := t.saveSettings()
	t.install = trInstall{}
	t.mu.Unlock()
	t.gen.Add(1)
	for _, m := range append(transcribeModels, vadModel) {
		os.Remove(filepath.Join(t.dir, m.File))
	}
	os.Remove(t.helperPath())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("Transcription removed by %q", s.sessionUser(r))
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) sessionUser(r *http.Request) string {
	if sess := s.getSession(r); sess != nil {
		return sess.Username
	}
	return "?"
}
