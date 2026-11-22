// Command server is the k8s-sentinel entrypoint. It runs as a long-lived
// HTTP service inside the cluster it manages (see README.md "How an SRE
// triggers actions" for why this is a persistent service, not a one-shot
// CLI).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/httpapi"
	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/k8sclient"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := flag.String("addr", envOrDefault("ADDR", ":8080"), "address to listen on")
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (empty = in-cluster config, falling back to ~/.kube/config)")
	flag.Parse()

	clientset, err := k8sclient.New(*kubeconfig)
	if err != nil {
		logger.Error("failed to build kubernetes client", "error", err)
		os.Exit(1)
	}

	handler := httpapi.New(logger, clientset)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Listen for SIGTERM/SIGINT so the process shuts down gracefully when
	// Kubernetes stops the pod (scale-down, rollout, node drain) instead of
	// dropping in-flight requests — "think about how it behaves over time,"
	// not just the happy path of staying up forever.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		logger.Info("listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received, draining in-flight requests")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
