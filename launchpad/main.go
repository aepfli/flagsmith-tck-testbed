package main

// launchpad -- the provider-TCK control API for a Flagsmith Edge Proxy.
//
// It runs as PID 1 and does two jobs:
//
//  1. Serves the TCK control API (Appendix F, specification/assets/provider-tck/openapi).
//  2. Acts as the Edge Proxy's *upstream*, serving GET /api/v1/environment-document/ in place of
//     the real Flagsmith API.
//
// Point (2) is the whole design. The evaluation engine under test is real: the Edge Proxy runs
// Flagsmith's own flag_engine over a real environment document. Only the *delivery* of that
// document is synthetic -- which is exactly what flagd-testbed already does with a JSON file on
// disk, and what goff-testbed does with a file retriever. Fake the config source, never the
// evaluation.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// Fixed keys. The control API has no way to communicate connection parameters -- /start
	// returns a bare 200 with no body -- so the adoption hardcodes these exactly as a flagd
	// adoption hardcodes a port. See FINDINGS.md #1.
	serverSideKey = "ser.provider-tck-server-key"
	clientSideKey = "provider-tck-client-key"

	changingFlag = "changing-flag"
)

type launchpad struct {
	mu       sync.RWMutex
	baseline *environmentDocument
	current  *environmentDocument

	proxy         *edgeProxy
	canonicalPath string
	configDir     string
}

func main() {
	var (
		controlPort   = flag.Int("port", 8080, "control API port")
		canonicalPath = flag.String("flags", "/flags/canonical-flags.json", "canonical flag set")
		configDir     = flag.String("configs", "/configs", "named configuration directory")
		pollSeconds   = flag.Int("poll", 1, "edge proxy api_poll_frequency_seconds")
	)
	flag.Parse()

	upstream := fmt.Sprintf("http://127.0.0.1:%d/api/v1", *controlPort)
	lp := &launchpad{
		proxy:         newEdgeProxy(serverSideKey, clientSideKey, upstream, *pollSeconds),
		canonicalPath: *canonicalPath,
		configDir:     *configDir,
	}

	if err := lp.proxy.writeConfig(); err != nil {
		log.Fatalf("writing edge proxy config: %v", err)
	}

	mux := http.NewServeMux()
	// Upstream: what the Edge Proxy polls.
	mux.HandleFunc("/api/v1/environment-document/", lp.handleEnvironmentDocument)
	mux.HandleFunc("/api/v1/environment-document", lp.handleEnvironmentDocument)
	// Control API.
	mux.HandleFunc("/start", lp.handleStart)
	mux.HandleFunc("/stop", lp.handleStop)
	mux.HandleFunc("/restart", lp.handleRestart)
	mux.HandleFunc("/change", lp.handleChange)
	mux.HandleFunc("/reset", lp.handleReset)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *controlPort), Handler: logRequests(mux)}

	go func() {
		log.Printf("launchpad listening on :%d (upstream %s)", *controlPort, upstream)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("control API: %v", err)
		}
	}()

	// Boot into the default configuration so the container is usable without an explicit /start.
	if err := lp.start("default"); err != nil {
		log.Printf("initial start failed: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = lp.proxy.stop()
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Edge Proxy polls the document once a second; logging that would drown everything
		// else. Log only the control API.
		if r.URL.Path != "/api/v1/environment-document/" && r.URL.Path != "/api/v1/environment-document" {
			log.Printf("%s %s%s", r.Method, r.URL.Path, queryString(r))
		}
		next.ServeHTTP(w, r)
	})
}

func queryString(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// handleEnvironmentDocument is the Edge Proxy's upstream.
//
// The proxy sends If-Modified-Since derived from the cached document's updated_at and tolerates a
// 304. We always answer 200 with the current document: it is correct, it is one fewer thing to get
// wrong, and the proxy's own cache.put_environment compares before clearing its endpoint caches.
func (lp *launchpad) handleEnvironmentDocument(w http.ResponseWriter, r *http.Request) {
	if key := r.Header.Get("X-Environment-Key"); key != serverSideKey {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "unknown key"})
		return
	}
	lp.mu.RLock()
	doc := lp.current
	lp.mu.RUnlock()
	if doc == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// start seeds flag state to the named configuration's baseline and ensures the proxy is running,
// returning only once the seeded state is actually being served.
func (lp *launchpad) start(config string) error {
	path := lp.canonicalPath
	if config != "" && config != "default" {
		candidate := fmt.Sprintf("%s/%s.json", lp.configDir, config)
		if _, err := os.Stat(candidate); err != nil {
			return fmt.Errorf("unknown configuration %q", config)
		}
		path = candidate
	}

	doc, err := buildDocument(path, clientSideKey)
	if err != nil {
		return err
	}

	lp.mu.Lock()
	lp.baseline = doc.clone()
	lp.current = doc
	lp.mu.Unlock()

	if err := lp.proxy.start(); err != nil {
		return err
	}

	// Derive the probe from the document just seeded rather than hardcoding a key.
	want, ok := doc.value(changingFlag)
	if !ok {
		return fmt.Errorf("configuration %q has no %s to probe", config, changingFlag)
	}
	return lp.proxy.probeServing(changingFlag, want, probeTimeout)
}

func (lp *launchpad) handleStart(w http.ResponseWriter, r *http.Request) {
	config := r.URL.Query().Get("config")
	if config == "" {
		config = "default"
	}
	if err := lp.start(config); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (lp *launchpad) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := lp.proxy.stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (lp *launchpad) handleRestart(w http.ResponseWriter, r *http.Request) {
	seconds := 0
	if s := r.URL.Query().Get("seconds"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("seconds: %w", err))
			return
		}
		seconds = n
	}
	if err := lp.proxy.stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	time.Sleep(time.Duration(seconds) * time.Second)
	if err := lp.proxy.start(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// /restart preserves flag state, so probe for whatever is current -- not for the baseline.
	lp.mu.RLock()
	doc := lp.current
	lp.mu.RUnlock()
	want, _ := doc.value(changingFlag)
	if err := lp.proxy.probeServing(changingFlag, want, probeTimeout); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleChange mutates changing-flag and returns only once the new value is served.
//
// Note what this does NOT do: it does not rewrite a raw input file. flagd-testbed's /change
// rewrites rawflags/changing-flag.json in place, so the toggled value survives a later /start --
// state leaks across scenarios. Here /start rebuilds the document from the canonical source, so
// the leak is structurally impossible.
func (lp *launchpad) handleChange(w http.ResponseWriter, r *http.Request) {
	lp.mu.Lock()
	if lp.current == nil {
		lp.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("not started"))
		return
	}
	cur, _ := lp.current.value(changingFlag)
	next := "bar"
	if fmt.Sprint(cur) == "bar" {
		next = "foo"
	}
	lp.current.setValue(changingFlag, next)
	lp.mu.Unlock()

	if err := lp.proxy.probeServing(changingFlag, next, probeTimeout); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleReset restores the baseline without restarting the proxy.
//
// OPTIONAL in the control API, but it matters here for the same reason it matters for GO Feature
// Flag: this is a polling architecture on two hops, so a /start blip is expensive and hard for a
// provider to tell apart from a real event.
func (lp *launchpad) handleReset(w http.ResponseWriter, r *http.Request) {
	lp.mu.Lock()
	if lp.baseline == nil {
		lp.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("not started"))
		return
	}
	lp.current = lp.baseline.clone()
	lp.current.UpdatedAt = timestamp()
	want, _ := lp.current.value(changingFlag)
	lp.mu.Unlock()

	if err := lp.proxy.probeServing(changingFlag, want, probeTimeout); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeError(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
