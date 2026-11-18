// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

// mrtranscribe — speech-to-text for MeshRelay radio recordings.
//
// The server (transcriber.go) starts one long-running process:
//
//   mrtranscribe --model ggml-base.en.bin [--vad ggml-silero.bin] [--threads N] [--lang en]
//
// and writes one request per line on stdin:
//
//   <path to .mrr recording>\t<initial prompt (vocabulary), may be empty>\n
//
// For each request it prints exactly one JSON line on stdout:
//
//   {"ok":true,"text":"...","no_speech":0.12,"audio_ms":2300,"ms":410}
//   {"ok":false,"error":"..."}
//
// Recordings (.mrr, recorder.go) are a series of frames
// [u32 ms offset][u16 len][Opus packet], big endian. They are decoded with
// libopus at 16 kHz mono (Whisper's rate, whatever rate the talker encoded at)
// with pauses kept (capped), then transcribed with whisper.cpp. The model is
// loaded once. Everything runs locally — no audio leaves the server.
//
// Other modes:
//   --selftest            load the model, transcribe 1 s of silence, print ok
//   --file X.mrr [-p ..]  transcribe one recording and exit (testing)
//   --version

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include <opus.h>
#include "whisper.h"

#define MRT_VERSION "1.0"
#define RATE 16000
#define MAX_AUDIO_S 600          // recordings stop at 10 min
#define MAX_GAP_MS 1000          // a longer pause in a transmission is shortened to this

static void quiet_log(enum ggml_log_level level, const char *text, void *ud) {
    (void)ud;
    if (level == GGML_LOG_LEVEL_ERROR) fputs(text, stderr);
}

static long now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000L + ts.tv_nsec / 1000000L;
}

// --- JSON output ---------------------------------------------------------------

static void json_str(FILE *o, const char *s) {
    fputc('"', o);
    for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
        switch (*p) {
        case '"': fputs("\\\"", o); break;
        case '\\': fputs("\\\\", o); break;
        case '\n': fputs("\\n", o); break;
        case '\r': fputs("\\r", o); break;
        case '\t': fputs("\\t", o); break;
        default:
            if (*p < 0x20) fprintf(o, "\\u%04x", *p);
            else fputc(*p, o);
        }
    }
    fputc('"', o);
}

static void reply_error(const char *msg) {
    fputs("{\"ok\":false,\"error\":", stdout);
    json_str(stdout, msg);
    fputs("}\n", stdout);
    fflush(stdout);
}

// --- .mrr → 16 kHz float PCM -----------------------------------------------------

typedef struct { float *s; int n, cap; } pcm_t;

static int pcm_grow(pcm_t *p, int add) {
    if (p->n + add <= p->cap) return 0;
    int cap = p->cap ? p->cap : RATE * 4;
    while (cap < p->n + add) cap *= 2;
    if (cap > RATE * (MAX_AUDIO_S + 5)) return -1;
    float *ns = realloc(p->s, (size_t)cap * sizeof(float));
    if (!ns) return -1;
    p->s = ns; p->cap = cap;
    return 0;
}

static int pcm_silence(pcm_t *p, int n) {
    if (n <= 0) return 0;
    if (pcm_grow(p, n)) return -1;
    memset(p->s + p->n, 0, (size_t)n * sizeof(float));
    p->n += n;
    return 0;
}

// decode_mrr fills p with the recording's audio. Returns NULL or an error.
static const char *decode_mrr(const char *path, pcm_t *p) {
    FILE *f = fopen(path, "rb");
    if (!f) return strerror(errno);
    int err = 0;
    OpusDecoder *dec = opus_decoder_create(RATE, 1, &err);
    if (!dec || err != OPUS_OK) { fclose(f); return "opus decoder init failed"; }

    static unsigned char pkt[4000];
    static opus_int16 frame[RATE / 50 * 6];   // up to 120 ms
    long played_ms = -1;                       // where the previous frame ended
    const char *res = NULL;
    for (;;) {
        unsigned char hdr[6];
        size_t got = fread(hdr, 1, 6, f);
        if (got == 0) break;
        if (got < 6) break; // truncated tail (recording still being written) — keep what we have
        uint32_t off = (uint32_t)hdr[0] << 24 | (uint32_t)hdr[1] << 16 | (uint32_t)hdr[2] << 8 | hdr[3];
        uint16_t len = (uint16_t)(hdr[4] << 8 | hdr[5]);
        if (len == 0 || len > sizeof(pkt)) { res = "corrupt recording"; break; }
        if (fread(pkt, 1, len, f) != len) break;

        // Keep pauses inside the transmission, shortened, so words stay apart.
        if (played_ms >= 0 && (long)off > played_ms + 40) {
            long gap = (long)off - played_ms;
            if (gap > MAX_GAP_MS) gap = MAX_GAP_MS;
            if (pcm_silence(p, (int)(gap * RATE / 1000))) { res = "recording too long"; break; }
        }
        int n = opus_decode(dec, pkt, len, frame, (int)(sizeof(frame) / sizeof(frame[0])), 0);
        if (n < 0) continue; // a bad packet — skip it
        if (pcm_grow(p, n)) { res = "recording too long"; break; }
        for (int i = 0; i < n; i++) p->s[p->n + i] = frame[i] / 32768.0f;
        p->n += n;
        played_ms = (long)off + (long)n * 1000 / RATE;
    }
    opus_decoder_destroy(dec);
    fclose(f);
    return res;
}

// --- transcription -----------------------------------------------------------------

static struct whisper_context *ctx;
static struct whisper_vad_context *vctx;   // optional speech detector (--vad)
static int g_threads = 2;
static int g_ctx_fit = 1;      // size Whisper's window to the audio
static int g_ctx_min = 768;    // smallest window (positions; 50 per second) — smaller loses accuracy
static const char *g_lang = "en";

static void transcribe(const char *path, const char *prompt) {
    long t0 = now_ms();
    pcm_t p = {0};
    const char *err = decode_mrr(path, &p);
    if (err) { free(p.s); reply_error(err); return; }
    int audio_ms = (int)((long)p.n * 1000 / RATE);

    // With a speech detector loaded, a transmission without speech (squelch
    // noise, a key-up) is answered empty — Whisper would invent words for it.
    // Only the speech goes to Whisper: the parts the detector found, joined
    // with short pauses. Long stretches of squelch noise make Whisper repeat
    // itself or invent words, and every second it does not see is faster.
    int speech_ms = -1;
    if (vctx && p.n > 0) {
        struct whisper_vad_params vp = whisper_vad_default_params();
        vp.min_speech_duration_ms = 200;
        vp.speech_pad_ms = 200;
        struct whisper_vad_segments *segs = whisper_vad_segments_from_samples(vctx, vp, p.s, p.n);
        speech_ms = 0;
        pcm_t sp = {0};
        if (segs) {
            int ns = whisper_vad_segments_n_segments(segs);
            for (int i = 0; i < ns; i++) {
                // t0/t1 are in centiseconds
                long a = (long)(whisper_vad_segments_get_segment_t0(segs, i) * RATE / 100);
                long b = (long)(whisper_vad_segments_get_segment_t1(segs, i) * RATE / 100);
                if (a < 0) a = 0;
                if (b > p.n) b = p.n;
                if (b <= a) continue;
                if (sp.n && pcm_silence(&sp, RATE / 4)) break;
                if (pcm_grow(&sp, (int)(b - a))) break;
                memcpy(sp.s + sp.n, p.s + a, (size_t)(b - a) * sizeof(float));
                sp.n += (int)(b - a);
                speech_ms += (int)((b - a) * 1000 / RATE);
            }
            whisper_vad_free_segments(segs);
        }
        if (speech_ms == 0) {
            free(p.s);
            free(sp.s);
            fprintf(stdout, "{\"ok\":true,\"text\":\"\",\"speech_ms\":0,\"audio_ms\":%d,\"ms\":%ld}\n", audio_ms, now_ms() - t0);
            fflush(stdout);
            return;
        }
        free(p.s);
        p = sp;
    }

    // Whisper wants at least ~1 s; pad short transmissions (and add a little
    // tail so the last word is not clipped).
    int pad = RATE / 4;
    if (p.n + pad < RATE + RATE / 10) pad = RATE + RATE / 10 - p.n;
    if (pcm_silence(&p, pad)) { free(p.s); reply_error("out of memory"); return; }

    struct whisper_full_params wp = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
    wp.n_threads = g_threads;
    wp.language = g_lang;
    wp.translate = false;
    wp.no_context = true;          // every transmission stands alone
    wp.no_timestamps = true;
    wp.single_segment = false;
    wp.print_progress = false;
    wp.print_realtime = false;
    wp.print_special = false;
    wp.print_timestamps = false;
    wp.suppress_blank = true;
    wp.suppress_nst = true;        // no "(static)", "[music]" tokens
    wp.initial_prompt = (prompt && *prompt) ? prompt : NULL;
    // At most two retries at a higher temperature (default five): a retry
    // costs a full decode, and a stubborn clip is not worth 20 s of CPU.
    wp.temperature_inc = 0.4f;
    // Nobody says more than ~5 words a second on the radio; a transcript far
    // longer than that is Whisper repeating itself.
    wp.max_tokens = p.n / RATE * 8 + 24;
    // Whisper normally encodes a full 30 s window; a radio transmission is a
    // few seconds. Size the window to the audio (1500 positions = 30 s) with
    // headroom — several times faster on short clips. --full-window disables.
    if (g_ctx_fit) {
        int ctxn = p.n / 320 + 128;
        if (ctxn < g_ctx_min) ctxn = g_ctx_min;
        if (ctxn < 1500) wp.audio_ctx = ctxn;
    }

    if (whisper_full(ctx, wp, p.s, p.n) != 0) {
        free(p.s);
        reply_error("transcription failed");
        return;
    }
    free(p.s);

    // Join the segments; report the highest no-speech probability so the
    // server can drop transmissions that were only squelch noise.
    size_t cap = 256, len = 0;
    char *text = malloc(cap);
    text[0] = 0;
    float no_speech = 0;
    int nseg = whisper_full_n_segments(ctx);
    for (int i = 0; i < nseg; i++) {
        const char *seg = whisper_full_get_segment_text(ctx, i);
        float ns = whisper_full_get_segment_no_speech_prob(ctx, i);
        if (ns > no_speech) no_speech = ns;
        size_t sl = strlen(seg);
        if (len + sl + 2 > cap) {
            while (len + sl + 2 > cap) cap *= 2;
            text = realloc(text, cap);
        }
        while (sl && (*seg == ' ')) { seg++; sl--; }
        if (!sl) continue;
        if (len) text[len++] = ' ';
        memcpy(text + len, seg, sl);
        len += sl;
        text[len] = 0;
    }

    fputs("{\"ok\":true,\"text\":", stdout);
    json_str(stdout, text);
    fprintf(stdout, ",\"no_speech\":%.3f,\"speech_ms\":%d,\"audio_ms\":%d,\"ms\":%ld}\n", no_speech, speech_ms, audio_ms, now_ms() - t0);
    fflush(stdout);
    free(text);
}

static void usage(void) {
    fputs("usage: mrtranscribe --model FILE [--vad SILERO.bin] [--threads N] [--lang en] [--selftest | --file X.mrr [--prompt TEXT]]\n"
          "       requests on stdin: <file.mrr>\\t<prompt>\\n, one JSON reply line each\n", stderr);
}

int main(int argc, char **argv) {
    const char *model = NULL, *file = NULL, *prompt = "", *vad = NULL;
    int selftest = 0;
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        const char *v = i + 1 < argc ? argv[i + 1] : NULL;
        if (!strcmp(a, "--version")) { printf("mrtranscribe %s (whisper.cpp, opus %s)\n", MRT_VERSION, opus_get_version_string()); return 0; }
        else if (!strcmp(a, "--selftest")) selftest = 1;
        else if (!strcmp(a, "--model") && v) { model = v; i++; }
        else if (!strcmp(a, "--threads") && v) { g_threads = atoi(v); i++; }
        else if (!strcmp(a, "--lang") && v) { g_lang = v; i++; }
        else if (!strcmp(a, "--vad") && v) { vad = v; i++; }
        else if (!strcmp(a, "--full-window")) g_ctx_fit = 0;
        else if (!strcmp(a, "--min-window") && v) { g_ctx_min = atoi(v); i++; }
        else if (!strcmp(a, "--file") && v) { file = v; i++; }
        else if (!strcmp(a, "--prompt") && v) { prompt = v; i++; }
        else { usage(); return 2; }
    }
    if (!model) { usage(); return 2; }
    if (g_threads < 1) g_threads = 1;
    if (g_threads > 16) g_threads = 16;

    whisper_log_set(quiet_log, NULL);
    struct whisper_context_params cp = whisper_context_default_params();
    cp.use_gpu = false;
    ctx = whisper_init_from_file_with_params(model, cp);
    if (!ctx) { fprintf(stderr, "cannot load model %s\n", model); reply_error("cannot load model"); return 1; }

    if (vad) {
        struct whisper_vad_context_params vcp = whisper_vad_default_context_params();
        vcp.n_threads = 1;
        vcp.use_gpu = false;
        vctx = whisper_vad_init_from_file_with_params(vad, vcp);
        if (!vctx) { fprintf(stderr, "cannot load VAD model %s\n", vad); reply_error("cannot load speech detector"); return 1; }
    }

    if (selftest) {
        float *z = calloc(RATE * 2, sizeof(float));
        struct whisper_full_params wp = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
        wp.n_threads = g_threads; wp.language = g_lang;
        wp.print_progress = false; wp.print_realtime = false; wp.print_timestamps = false;
        long t0 = now_ms();
        // Exercise both models, so a build using instructions this CPU lacks
        // fails here (the server then tries the next build) and not later.
        if (vctx) {
            struct whisper_vad_segments *segs = whisper_vad_segments_from_samples(vctx, whisper_vad_default_params(), z, RATE * 2);
            if (segs) whisper_vad_free_segments(segs);
        }
        int r = whisper_full(ctx, wp, z, RATE * 2);
        free(z);
        if (r != 0) { reply_error("selftest failed"); return 1; }
        printf("{\"ok\":true,\"text\":\"\",\"ms\":%ld}\n", now_ms() - t0);
        return 0;
    }
    if (file) { transcribe(file, prompt); whisper_free(ctx); return 0; }

    printf("{\"ready\":true}\n");
    fflush(stdout);
    char *line = NULL;
    size_t lcap = 0;
    ssize_t n;
    while ((n = getline(&line, &lcap, stdin)) > 0) {
        while (n && (line[n - 1] == '\n' || line[n - 1] == '\r')) line[--n] = 0;
        if (!n) continue;
        char *tab = strchr(line, '\t');
        const char *pr = "";
        if (tab) { *tab = 0; pr = tab + 1; }
        transcribe(line, pr);
    }
    free(line);
    whisper_free(ctx);
    return 0;
}
