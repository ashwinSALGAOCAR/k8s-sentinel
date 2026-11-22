// Package httpapi is the service's HTTP surface: health/readiness plus the
// isolate and harden endpoints (see isolate.go and harden.go).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Server holds the dependencies every handler needs.
type Server struct {
	logger    *slog.Logger
	clientset kubernetes.Interface
}

// New wires up the HTTP mux. clientset is an interface (not the concrete
// *kubernetes.Clientset) specifically so tests can substitute
// k8s.io/client-go/kubernetes/fake without touching a real cluster.
func New(logger *slog.Logger, clientset kubernetes.Interface) http.Handler {
	s := &Server{logger: logger, clientset: clientset}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("POST /isolate", s.handleIsolateCreate)
	mux.HandleFunc("GET /isolate", s.handleIsolateList)
	mux.HandleFunc("DELETE /isolate/{id}", s.handleIsolateDelete)
	mux.HandleFunc("POST /harden", s.handleHardenCreate)
	return mux
}

// handleHealthz answers "is this process alive." It deliberately does not
// call out to the Kubernetes API — a slow/unreachable API server should
// make the pod un-ready, not make the kubelet think the process itself is
// dead and restart it.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz answers "can this process actually do its job right now" by
// confirming the Kubernetes API server is reachable. A short timeout keeps
// a wedged API server from making the readiness probe itself hang.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Discovery().ServerVersion() hardcodes context.TODO() internally, so it
	// can't be bounded by our timeout — calling RESTClient() directly hits
	// the same /version endpoint but actually respects ctx.
	if _, err := s.clientset.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Raw(); err != nil {
		s.logger.Warn("readiness check failed: kubernetes API unreachable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: kubernetes api unreachable"))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
