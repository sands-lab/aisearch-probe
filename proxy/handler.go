package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aisearch-probe/internal/probe"
)

// ---------------------------------------------------------------------------
// Logger — one JSON line per request, flushed immediately.
//
// Flushing every line costs throughput but means a crash, a kill -9, or an
// unplugged server never loses observations. Collected data is the experiment;
// a lost line cannot be re-collected.
// ---------------------------------------------------------------------------

type Logger struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
}

func NewLogger(path string) (*Logger, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{w: bufio.NewWriter(f), f: f}, nil
}

func (l *Logger) Write(rec probe.Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(b); err != nil {
		return err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return err
	}
	return l.w.Flush()
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		l.f.Close()
		return err
	}
	return l.f.Close()
}

// ---------------------------------------------------------------------------
// recorder — observes the response on its way out.
//
// It must stay transparent to the origin's capabilities. Unwrap lets
// http.ResponseController reach the real writer, so streaming responses and
// WebSocket upgrades on the origin keep working with the probe in front of
// them; Flush and Hijack are forwarded explicitly for older callers.
// ---------------------------------------------------------------------------

type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	hdr         http.Header
}

func newRecorder(w http.ResponseWriter) *recorder {
	return &recorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.hdr = r.ResponseWriter.Header().Clone()
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("probe: underlying ResponseWriter is not a Hijacker")
}

// header returns the response headers as they were when the status was written.
func (r *recorder) header() http.Header {
	if r.hdr != nil {
		return r.hdr
	}
	return r.ResponseWriter.Header()
}

// ---------------------------------------------------------------------------
// Request field extraction
// ---------------------------------------------------------------------------

// clientIP prefers Cloudflare's own header, then X-Forwarded-For, then the
// socket. net.SplitHostPort is used rather than splitting on ":" so IPv6
// peers are not truncated to "[".
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestScheme(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

type Handler struct {
	rp     http.Handler
	logger *Logger
	filter string
	quiet  bool
	out    io.Writer
}

// NewHandler logs requests and forwards them to an origin server.
func NewHandler(origin *url.URL, logger *Logger, filter string) *Handler {
	rp := httputil.NewSingleHostReverseProxy(origin)
	rp.ErrorLog = newStderrLogger()
	return NewHandlerWithBackend(rp, logger, filter)
}

// NewHandlerWithBackend logs requests and hands them to any backend — the
// reverse proxy in Mode B, or the built-in static server in -serveDir mode.
func NewHandlerWithBackend(backend http.Handler, logger *Logger, filter string) *Handler {
	return &Handler{rp: backend, logger: logger, filter: filter, out: os.Stdout}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only GET is evidence about caching. Everything else passes through
	// untouched and unlogged.
	if r.Method != http.MethodGet {
		h.rp.ServeHTTP(w, r)
		return
	}

	// Capture request state before the reverse proxy can mutate the request.
	arrived := time.Now().UTC()
	ua := r.Header.Get("User-Agent")
	ip := clientIP(r)
	ifNoneMatch := r.Header.Get("If-None-Match")
	ifModifiedSince := r.Header.Get("If-Modified-Since")
	cacheControlReq := r.Header.Get("Cache-Control")
	cfCacheStatus := r.Header.Get("CF-Cache-Status")
	cfRay := r.Header.Get("CF-Ray")
	referer := r.Header.Get("Referer")
	path := r.URL.Path
	reqURL := requestScheme(r) + "://" + r.Host + r.RequestURI

	// Snapshot every header before the reverse proxy adds its own.
	reqHeaders := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		reqHeaders[k] = strings.Join(v, ", ")
	}

	shouldLog := h.filter == "" || strings.HasPrefix(path, h.filter)

	rec := newRecorder(w)
	h.rp.ServeHTTP(rec, r)
	elapsed := time.Since(arrived).Seconds()

	if !shouldLog {
		return
	}

	resp := rec.header()
	// Cloudflare's cache status usually rides on the response from the edge,
	// but when the probe is behind cloudflared it can arrive either way.
	if cfCacheStatus == "" {
		cfCacheStatus = resp.Get("CF-Cache-Status")
	}

	record := probe.Record{
		TS:               arrived.Format(time.RFC3339Nano),
		TSUnix:           float64(arrived.UnixNano()) / 1e9,
		Method:           http.MethodGet,
		URL:              reqURL,
		PageKey:          path,
		StatusCode:       rec.status,
		Bot:              probe.ClassifyBot(ua),
		UserAgent:        ua,
		ClientIP:         ip,
		IfNoneMatch:      ifNoneMatch,
		IfModifiedSince:  ifModifiedSince,
		CacheControlReq:  cacheControlReq,
		ETagSent:         resp.Get("ETag"),
		LastModifiedSent: resp.Get("Last-Modified"),
		CacheControlResp: resp.Get("Cache-Control"),
		ExpiresSent:      resp.Get("Expires"),
		BytesSent:        rec.bytes,
		CFCacheStatus:    cfCacheStatus,
		CFRay:            cfRay,
		Referer:          referer,
		ElapsedS:         elapsed,
		ReqHeaders:       reqHeaders,
	}

	if err := h.logger.Write(record); err != nil {
		fmt.Fprintf(os.Stderr, "[probe] log write error: %v\n", err)
	}

	if !h.quiet {
		mark := "re-downloaded"
		if record.AskedIfChanged() {
			mark = "ASKED FIRST  "
		}
		fmt.Fprintf(h.out, "%s  %d  %-20s  %s\n", mark, record.StatusCode, record.Bot, path)
	}
}
