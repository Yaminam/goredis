package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Health reports server state to the HTTP probes.
type Health interface {
	// Ready is true once the AOF is loaded and the listener is accepting,
	// and false again once shutdown begins.
	Ready() bool
	// AOFError returns the current persistence error, if any.
	AOFError() error
}

// Handler returns the admin HTTP handler.
//
//	/metrics  Prometheus exposition.
//	/healthz  Liveness. 200 while the process can serve HTTP. Persistence
//	          failures do NOT fail liveness: a restart would not fix a full
//	          disk, and it would throw away writes still buffered in memory.
//	          They are reported in the body and in metrics instead.
//	/readyz   Readiness. 200 only when ready to serve clients, else 503.
func Handler(m *Metrics, h Health, pprofEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"status": "ok", "ready": h.Ready(), "aof_healthy": true}
		if err := h.AOFError(); err != nil {
			body["aof_healthy"] = false
			body["aof_error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !h.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})
	if pprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// NewServer wraps the handler with timeouts suitable for a probe endpoint.
// WriteTimeout is long enough for a 30 s CPU profile when pprof is enabled.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
