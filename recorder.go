// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Radio traffic recording.
//
// Every Opus transmission relayed on a channel is written to disk as it
// happens (the server never decodes audio, so this is cheap: ~3 KB/s while
// someone talks). One file per transmission:
//
//	<dir>/<YYYY-MM-DD>/<agency>/<id>.mrr   frames: [u32 ms offset][u16 len][Opus]
//	<dir>/<YYYY-MM-DD>/index.jsonl         one JSON line per finished recording
//
// A transmission ends after 1.5 s without audio (or at 10 minutes). Agencies
// can switch recording off (Agency.NoRecording); recordings older than the
// agency's retention period (Agency.RecordingRetentionDays, else the server's
// recording_retention_days) are deleted. Consoles list and play them through
// /api/recordings — scoped to the operator's agency.

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

const (
	recGap              = 1500 * time.Millisecond
	recMaxLen           = 10 * time.Minute
	recDayFmt           = "2006-01-02"
	recDefaultRetention = 30
)

type RecordingInfo struct {
	ID         string `json:"id"`
	Agency     string `json:"agency"`
	Channel    int    `json:"channel"`
	User       string `json:"user"`
	Start      int64  `json:"start"` // unix ms
	DurationMs int64  `json:"duration_ms"`
	Frames     int    `json:"frames"`
	File       string `json:"file"` // relative to the recording dir
	// Transcript (transcriber.go), filled in by Query; not stored in the index.
	Text        string `json:"text,omitempty"`
	Transcribed bool   `json:"transcribed,omitempty"`
}

type activeRec struct {
	info  RecordingInfo
	start time.Time
	last  time.Time
	f     *os.File
	w     *bufio.Writer
}

type Recorder struct {
	s         *Server
	dir       string
	retention int
	mu        sync.Mutex
	active    map[string]*activeRec
	indexMu   sync.Mutex
}

func NewRecorder(s *Server, dir string, retentionDays int) *Recorder {
	if dir == "" {
		dir = "recordings"
	}
	if retentionDays <= 0 {
		retentionDays = recDefaultRetention
	}
	r := &Recorder{s: s, dir: dir, retention: retentionDays, active: map[string]*activeRec{}}
	go r.loop()
	return r
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Record appends one Opus frame heard on agency/channel from user.
func (r *Recorder) Record(agency string, channel int, user string, opus []byte) {
	if r == nil || len(opus) == 0 || len(opus) > 4000 {
		return
	}
	now := time.Now()
	key := agency + "\x00" + strconv.Itoa(channel) + "\x00" + user
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.active[key]
	if a != nil && (now.Sub(a.last) > recGap || now.Sub(a.start) > recMaxLen) {
		r.finish(key, a)
		a = nil
	}
	if a == nil {
		a = r.open(agency, channel, user, now)
		if a == nil {
			return
		}
		r.active[key] = a
	}
	var hdr [6]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(now.Sub(a.start).Milliseconds()))
	binary.BigEndian.PutUint16(hdr[4:], uint16(len(opus)))
	a.w.Write(hdr[:])
	a.w.Write(opus)
	a.info.Frames++
	a.last = now
}

func (r *Recorder) open(agency string, channel int, user string, now time.Time) *activeRec {
	day := now.Format(recDayFmt)
	rnd := make([]byte, 3)
	rand.Read(rnd)
	id := fmt.Sprintf("%s-%d-%s", strings.ReplaceAll(day, "-", ""), now.UnixMilli(), hex.EncodeToString(rnd))
	rel := filepath.Join(day, safeName.ReplaceAllString(nonEmpty(agency, "_"), "_"), id+".mrr")
	path := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		log.Printf("Recorder: %v", err)
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		log.Printf("Recorder: %v", err)
		return nil
	}
	return &activeRec{
		info:  RecordingInfo{ID: id, Agency: agency, Channel: channel, User: user, Start: now.UnixMilli(), File: rel},
		start: now, last: now, f: f, w: bufio.NewWriterSize(f, 16*1024),
	}
}

// finish closes a recording and indexes it. Caller holds r.mu.
func (r *Recorder) finish(key string, a *activeRec) {
	delete(r.active, key)
	a.w.Flush()
	a.f.Close()
	a.info.DurationMs = a.last.Sub(a.start).Milliseconds() + 20
	info := a.info
	go func() {
		r.appendIndex(info)
		if r.s != nil {
			queued := r.s.queueTranscription(info)
			// Phones with the channel's History open see it at once.
			if queued {
				r.s.pushTranscriptToPhones(info, trStatePending, "")
			} else {
				r.s.pushTranscriptToPhones(info, trStatePlain, "")
			}
			r.s.sendToConsoles(info.Agency, map[string]interface{}{
				"type": "recording", "id": info.ID, "agency": info.Agency, "channel": info.Channel,
				"user": info.User, "start": info.Start, "duration_ms": info.DurationMs, "transcribing": queued,
			})
		}
	}()
}

func (r *Recorder) appendIndex(info RecordingInfo) {
	day := time.UnixMilli(info.Start).Format(recDayFmt)
	line, _ := json.Marshal(info)
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.dir, day, "index.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("Recorder index: %v", err)
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}

func (r *Recorder) loop() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	lastPrune := time.Time{}
	for now := range tick.C {
		r.mu.Lock()
		for k, a := range r.active {
			if now.Sub(a.last) > recGap {
				r.finish(k, a)
			}
		}
		r.mu.Unlock()
		if now.Sub(lastPrune) > time.Hour {
			lastPrune = now
			r.prune(now)
		}
	}
}

// Retention limits for an agency's own setting (Agency.RecordingRetentionDays).
const (
	recMinRetention = 1
	recMaxRetention = 3650
)

// retentionFor is how many days an agency keeps its recordings: its own
// setting, else the server's. Caller holds s.mu (read) or passes nil.
func (r *Recorder) retentionFor(a *Agency) int {
	if a != nil && a.RecordingRetentionDays > 0 {
		return a.RecordingRetentionDays
	}
	return r.retention
}

// prune deletes recordings older than their agency's retention period. Each
// day folder holds one sub-folder per agency; an agency's folder goes when it
// is too old for that agency, and the day's index and transcripts are
// rewritten without its recordings. An empty day folder is removed.
func (r *Recorder) prune(now time.Time) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	// Folder name → retention days (folders are named after the agency id).
	days := map[string]int{}
	if r.s != nil {
		r.s.mu.RLock()
		for id, a := range r.s.agencies {
			days[safeName.ReplaceAllString(nonEmpty(id, "_"), "_")] = r.retentionFor(a)
		}
		r.s.mu.RUnlock()
	}
	for _, e := range entries {
		d, err := time.ParseInLocation(recDayFmt, e.Name(), time.Local)
		if err != nil || !e.IsDir() {
			continue
		}
		dayDir := filepath.Join(r.dir, e.Name())
		subs, err := os.ReadDir(dayDir)
		if err != nil {
			continue
		}
		removed := map[string]bool{}
		left := 0
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			keep := r.retention
			if n, ok := days[sub.Name()]; ok {
				keep = n
			}
			if d.Before(now.AddDate(0, 0, -keep)) {
				if os.RemoveAll(filepath.Join(dayDir, sub.Name())) == nil {
					removed[sub.Name()] = true
					log.Printf("Recorder: deleted %q recordings from %q (kept %d days)", sub.Name(), e.Name(), keep)
					continue
				}
			}
			left++
		}
		if left == 0 && len(subs) > 0 {
			os.RemoveAll(dayDir)
			continue
		}
		if len(removed) > 0 {
			r.dropFromIndex(e.Name(), removed)
		}
	}
}

// dropFromIndex rewrites a day's index and transcripts without the
// recordings of the given agency folders.
func (r *Recorder) dropFromIndex(day string, folders map[string]bool) {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	idxPath := filepath.Join(r.dir, day, "index.jsonl")
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return
	}
	var keepLines []string
	gone := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var info RecordingInfo
		if json.Unmarshal([]byte(line), &info) == nil &&
			folders[safeName.ReplaceAllString(nonEmpty(info.Agency, "_"), "_")] {
			gone[info.ID] = true
			continue
		}
		keepLines = append(keepLines, line)
	}
	writeLines(idxPath, keepLines)
	trPath := filepath.Join(r.dir, day, "transcripts.jsonl")
	if tdata, err := os.ReadFile(trPath); err == nil {
		var keepT []string
		for _, line := range strings.Split(string(tdata), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var t transcriptLine
			if json.Unmarshal([]byte(line), &t) == nil && gone[t.ID] {
				continue
			}
			keepT = append(keepT, line)
		}
		writeLines(trPath, keepT)
	}
}

func writeLines(path string, lines []string) {
	tmp := path + ".tmp"
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		log.Printf("Recorder: %v", err)
		return
	}
	os.Rename(tmp, path)
}

// Query lists finished recordings on one day, newest first, with their
// transcripts. text ("" = any) keeps recordings whose transcript contains it.
func (r *Recorder) Query(day string, scopeAgency string, agency string, channel int, user string, limit int, text ...string) []RecordingInfo {
	var out []RecordingInfo
	r.indexMu.Lock()
	data, err := os.ReadFile(filepath.Join(r.dir, day, "index.jsonl"))
	trs := r.transcripts(day)
	r.indexMu.Unlock()
	if err != nil {
		return out
	}
	user = strings.ToLower(user)
	words := ""
	if len(text) > 0 {
		words = strings.ToLower(strings.TrimSpace(text[0]))
	}
	for _, line := range strings.Split(string(data), "\n") {
		var info RecordingInfo
		if json.Unmarshal([]byte(line), &info) != nil {
			continue
		}
		if (scopeAgency != "" && info.Agency != scopeAgency) || (agency != "" && info.Agency != agency) ||
			(channel >= 0 && info.Channel != channel) || (user != "" && !strings.Contains(strings.ToLower(info.User), user)) {
			continue
		}
		if t, ok := trs[info.ID]; ok {
			info.Text, info.Transcribed = t, true
		}
		if words != "" && !strings.Contains(strings.ToLower(info.Text), words) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Find looks a recording up by id (the id starts with its day).
func (r *Recorder) Find(id string) *RecordingInfo {
	if len(id) < 8 {
		return nil
	}
	day := id[0:4] + "-" + id[4:6] + "-" + id[6:8]
	if _, err := time.Parse(recDayFmt, day); err != nil {
		return nil
	}
	for _, info := range r.Query(day, "", "", -1, "", 0) {
		if info.ID == id {
			return &info
		}
	}
	return nil
}

// --- server hooks + HTTP ------------------------------------------------------

// recordAudio is called for every Opus frame relayed on a channel.
func (s *Server) recordAudio(agency string, channel int, user string, opus []byte) {
	if s.recorder == nil {
		return
	}
	s.mu.RLock()
	a := s.agencies[agency]
	off := a != nil && a.NoRecording
	s.mu.RUnlock()
	if !off {
		s.recorder.Record(agency, channel, user, opus)
	}
}

func (s *Server) handleListRecordings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	session := s.getSession(r)
	if session == nil || s.recorder == nil {
		json.NewEncoder(w).Encode([]RecordingInfo{})
		return
	}
	q := r.URL.Query()
	day := q.Get("date")
	if _, err := time.Parse(recDayFmt, day); err != nil {
		day = time.Now().Format(recDayFmt)
	}
	channel := -1
	if c := q.Get("channel"); c != "" {
		if n, err := strconv.Atoi(c); err == nil {
			channel = n
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	list := s.recorder.Query(day, session.AgencyID, q.Get("agency"), channel, q.Get("user"), limit, q.Get("text"))
	if list == nil {
		list = []RecordingInfo{}
	}
	json.NewEncoder(w).Encode(list)
}

func (s *Server) handleRecordingAudio(w http.ResponseWriter, r *http.Request) {
	session := s.getSession(r)
	if session == nil || s.recorder == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info := s.recorder.Find(mux.Vars(r)["id"])
	if info == nil || (session.AgencyID != "" && info.Agency != session.AgencyID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(s.recorder.dir, filepath.Clean(info.File))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, path)
}

// Agency switch (Settings): same scoping as floor control / guest invites.
func (s *Server) handleGetRecordingSetting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	s.mu.RLock()
	a := s.agencies[agencyID]
	s.mu.RUnlock()
	if a == nil {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	retention := recDefaultRetention
	if s.recorder != nil {
		retention = s.recorder.retention
	}
	avail, _ := s.transcriber.ready()
	s.mu.RLock()
	if s.recorder != nil {
		retention = s.recorder.retentionFor(a)
	}
	resp := map[string]interface{}{"agency_id": agencyID, "enabled": !a.NoRecording, "retention_days": retention,
		"agency_retention_days": a.RecordingRetentionDays, "server_retention_days": s.recorderRetention(),
		"retention_min": recMinRetention, "retention_max": recMaxRetention,
		"transcribe": a.Transcribe, "transcribe_words": a.TranscribeWords, "transcription_available": avail}
	s.mu.RUnlock()
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleUpdateRecordingSetting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID        *string `json:"agency_id"`
		Enabled         *bool   `json:"enabled"`
		Transcribe      *bool   `json:"transcribe"`
		TranscribeWords *string `json:"transcribe_words"`
		RetentionDays   *int    `json:"retention_days"` // 0 = the server's default
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		(req.Enabled == nil && req.Transcribe == nil && req.TranscribeWords == nil && req.RetentionDays == nil) {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.RetentionDays != nil && *req.RetentionDays != 0 &&
		(*req.RetentionDays < recMinRetention || *req.RetentionDays > recMaxRetention) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("keep recordings for %d to %d days (or leave it blank for the server default)", recMinRetention, recMaxRetention))
		return
	}
	if req.TranscribeWords != nil {
		words := strings.Join(strings.Fields(*req.TranscribeWords), " ")
		if len(words) > trMaxWords {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the word list is too long (%d characters max)", trMaxWords))
			return
		}
		req.TranscribeWords = &words
	}
	if isSuper && req.AgencyID != nil && *req.AgencyID != "" {
		agencyID = *req.AgencyID
	}
	s.mu.Lock()
	a := s.agencies[agencyID]
	if a != nil {
		if req.Enabled != nil {
			a.NoRecording = !*req.Enabled
		}
		if req.Transcribe != nil {
			a.Transcribe = *req.Transcribe
		}
		if req.TranscribeWords != nil {
			a.TranscribeWords = *req.TranscribeWords
		}
		if req.RetentionDays != nil {
			a.RecordingRetentionDays = *req.RetentionDays
		}
	}
	s.mu.Unlock()
	if a == nil {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if err := s.saveAgencies(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.RLock()
	enabled, transcribe := !a.NoRecording, a.Transcribe
	s.mu.RUnlock()
	if req.RetentionDays != nil && s.recorder != nil {
		go s.recorder.prune(time.Now()) // a shorter period takes effect now, not in an hour
	}
	log.Printf("Recording for agency %s: %v, transcription %v", agencyID, enabled, transcribe)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "enabled": enabled, "transcribe": transcribe})
}

// recorderRetention is the server-wide default retention in days.
func (s *Server) recorderRetention() int {
	if s.recorder != nil {
		return s.recorder.retention
	}
	return recDefaultRetention
}
