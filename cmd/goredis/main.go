// Command goredis runs the goredis server.
//
// Configuration comes from flags and GOREDIS_* environment variables; run
// with -h for the full list. SIGINT and SIGTERM trigger a graceful shutdown:
// stop accepting, drain clients, flush and fsync the append-only file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"goredis/internal/app"
	"goredis/internal/config"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Load(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "goredis: configuration error:", err)
		return 2
	}
	if cfg.Version {
		fmt.Fprintln(stderr, "goredis", version)
		return 0
	}
	if cfg.HealthCheck {
		return healthCheck(cfg.AdminAddr, stderr)
	}

	log := newLogger(cfg, stderr)
	log.Info("starting goredis", "version", version, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(cfg, log, version)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		log.Error("exited with error", "err", err)
		return 1
	}
	return 0
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// healthCheck lets a distroless container (no shell, no curl) probe itself:
// GET /readyz on the admin address, exit 0 if it returns 200.
func healthCheck(adminAddr string, stderr io.Writer) int {
	if adminAddr == "" {
		fmt.Fprintln(stderr, "healthcheck: admin endpoint is disabled")
		return 1
	}
	host, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "healthcheck: /readyz returned", resp.Status)
		return 1
	}
	return 0
}
