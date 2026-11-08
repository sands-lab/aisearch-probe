// probe-proxy: transparent HTTP logging reverse proxy for AI crawler research.
//
// Sits between your ingress (Cloudflare tunnel, nginx, anything) and your
// existing origin service. Logs one JSON line per GET — recording whether the
// client asked "has this changed?" and whether we gave it anything to ask
// with — then forwards the request untouched.
//
// Build:
//
//	go build -o bin/probe-proxy ./proxy
//
// Usage:
//
//	./probe-proxy -proxyTo http://localhost:8080 -port 8899
//	./probe-proxy -proxyTo http://localhost:8080 -port 8899 -logOnlyPath /docs
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

var (
	originFlag       = flag.String("proxyTo", "", "Origin URL to proxy to, e.g. http://localhost:8080 (required)")
	portFlag         = flag.Int("port", 8899, "Port this proxy listens on")
	logFlag          = flag.String("logFile", "logs/probe.jsonl", "Path to the JSONL log file")
	filterFlag       = flag.String("logOnlyPath", "", "Only log requests whose path starts with this prefix (empty = log all)")
	quietFlag        = flag.Bool("quiet", false, "Suppress the live per-request console line")
	noCheckFlag      = flag.Bool("skipSelfCheck", false, "Skip the origin validator self-check on startup")
	serveFlag        = flag.String("serveDir", "", "Serve pages from this directory instead of proxying (use instead of -proxyTo)")
	ccFlag           = flag.String("cacheControl", "", "Cache-Control sent with the static probe-site/ pages (-serveDir mode only). Default: none, the normal page (DECISIONS.md § D13). Generated pages ignore it")
	robotsSecretFlag = flag.String("controlSecret", os.Getenv("PROBE_CONTROL_SECRET"),
		"Password for the page controls, POST /control/correction and /control/heal (-serveDir mode only; empty = controls disabled)")
	pageCodeSecretFlag = flag.String("pageCodeSecret", os.Getenv("PROBE_PAGE_CODE_SECRET"),
		"Secret key for the unforgeable per-page codes on /cpage/ (-serveDir mode only; empty = /cpage/ route disabled)")
	versionStateFlag = flag.String("correction-serverStateFile", "logs/verpage-state.json",
		"File holding each changeable page's current version (/correction/html/, /correction/txt/), so a restart keeps it (-serveDir mode only; missing = empty start; \"\" = memory only)")
	healStateFlag = flag.String("heal-serverStateFile", "logs/heal-state.json",
		"File holding each healable page's state (/heal/: broken with which failure, or fixed and when), so a restart keeps it (-serveDir mode only; missing = empty start; \"\" = memory only)")
)

func newStderrLogger() *log.Logger { return log.New(os.Stderr, "[upstream] ", 0) }

// selfCheck fetches the origin once and reports whether it stamps its
// responses. Without an ETag or Last-Modified no client can ever ask "has this
// changed?", so the whole experiment would record a false negative — every bot
// would look like it never revalidates when in fact we never gave it the
// chance. Catching that on day one is worth one request at startup.
func selfCheck(origin string) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, origin, nil)
	if err != nil {
		log.Printf("[probe] self-check skipped: %v", err)
		return
	}
	req.Header.Set("User-Agent", "ai-crawler-cache-probe/selfcheck")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[probe] ⚠ self-check could not reach origin %s: %v", origin, err)
		return
	}
	defer resp.Body.Close()

	etag := resp.Header.Get("ETag")
	lastMod := resp.Header.Get("Last-Modified")
	if etag == "" && lastMod == "" {
		log.Printf("[probe] ⚠ ORIGIN SENDS NO ETag AND NO Last-Modified (status %d)", resp.StatusCode)
		log.Printf("[probe] ⚠ No client can revalidate against this. Every bot will look")
		log.Printf("[probe] ⚠ like it never caches, and that conclusion would be YOUR bug.")
		log.Printf("[probe] ⚠ Fix the origin's headers before trusting any results.")
		return
	}
	log.Printf("[probe] self-check ok — origin stamps responses (ETag=%q Last-Modified=%q)", etag, lastMod)
}

func main() {
	flag.Parse()

	if (*originFlag == "") == (*serveFlag == "") {
		fmt.Fprintln(os.Stderr, "error: give exactly one of -proxyTo (proxy an existing site) or -serveDir (serve a directory)")
		flag.Usage()
		os.Exit(1)
	}

	logger, err := NewLogger(*logFlag)
	if err != nil {
		log.Fatalf("cannot open log file: %v", err)
	}
	defer logger.Close()

	var handler *Handler
	addr := fmt.Sprintf(":%d", *portFlag)

	if *serveFlag != "" {
		dirInfo, err := os.Stat(*serveFlag)
		if err != nil || !dirInfo.IsDir() {
			log.Fatalf("-serveDir %q is not a directory", *serveFlag)
		}
		st := newStaticServer(*serveFlag, *ccFlag)
		st.controlSecret = *robotsSecretFlag
		st.pageCodeSecret = *pageCodeSecretFlag
		versions, err := newVersionState(*versionStateFlag)
		if err != nil {
			log.Fatalf("%v", err)
		}
		st.versions = versions
		healPages, err := newHealPageState(*healStateFlag)
		if err != nil {
			log.Fatalf("%v", err)
		}
		st.healPages = healPages
		handler = NewHandlerWithBackend(st, logger, *filterFlag)
		log.Printf("[probe] serving %s → listening on %s", *serveFlag, addr)
		log.Printf("[probe] pages carry ETag + Last-Modified and answer conditional requests")
		if *ccFlag == "" {
			log.Printf("[probe] Cache-Control on static pages: none (the normal page)")
		} else {
			log.Printf("[probe] Cache-Control on static pages: %s", *ccFlag)
		}
		if st.controlSecret != "" {
			log.Printf("[probe] changeable-page version control ENABLED at POST %s (state: %q, %d page(s) loaded)",
				controlVersionPath, *versionStateFlag, len(versions.pages))
			log.Printf("[probe] healable-page control ENABLED at POST %s (state: %q, %d page(s) loaded)",
				controlHealPath, *healStateFlag, len(healPages.pages))
		}
		if st.pageCodeSecret != "" {
			log.Printf("[probe] depth pages ENABLED (per-page unforgeable codes)")
		}
	} else {
		originURL, err := url.Parse(*originFlag)
		if err != nil || originURL.Scheme == "" || originURL.Host == "" {
			log.Fatalf("invalid -proxyTo URL %q (want e.g. http://localhost:8080)", *originFlag)
		}
		handler = NewHandler(originURL, logger, *filterFlag)
		if !*noCheckFlag {
			selfCheck(*originFlag)
		}
		log.Printf("[probe] proxying %s → listening on %s", *originFlag, addr)
	}
	handler.quiet = *quietFlag
	log.Printf("[probe] logging to %s", *logFlag)
	if *filterFlag != "" {
		log.Printf("[probe] logging only paths under %s", *filterFlag)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // streaming/SSE origins must not be cut off
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
