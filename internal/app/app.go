// Package app wires the packages into a running server: config, store,
// persistence, command engine, TCP server, metrics and the admin HTTP
// endpoints. main is a thin wrapper around it, and end-to-end tests start
// real instances through it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"goredis/internal/command"
	"goredis/internal/config"
	"goredis/internal/metrics"
	"goredis/internal/persistence"
	"goredis/internal/server"
	"goredis/internal/store"
)

// App is one goredis server instance.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	version string
	started time.Time

	st      *store.Store
	aof     *persistence.AOF
	eng     *command.Engine
	srv     *server.Server
	metrics *metrics.Metrics
	admin   *http.Server

	ln      net.Listener
	adminLn net.Listener
	ready   atomic.Bool

	stopSweeper context.CancelFunc
	serveErr    chan error
	adminErr    chan error
	closeOnce   sync.Once
	closeErr    error
}

// New builds an instance: it starts the admin endpoint (so /healthz answers
// during a long AOF replay), loads the AOF, and binds the client listener.
// It does not accept clients until Start.
func New(cfg config.Config, log *slog.Logger, version string) (_ *App, err error) {
	// a is a local, not the named result: "return nil, err" must not nil it
	// out before the deferred cleanup runs.
	a := &App{
		cfg:      cfg,
		log:      log,
		version:  version,
		started:  time.Now(),
		st:       store.New(nil),
		serveErr: make(chan error, 1),
		adminErr: make(chan error, 1),
	}
	defer func() {
		if err != nil {
			a.cleanup()
		}
	}()

	a.metrics = metrics.New(version, cfg.MaxMemory, a.started, a.metricSources())

	if cfg.AdminAddr != "" {
		a.adminLn, err = net.Listen("tcp", cfg.AdminAddr)
		if err != nil {
			return nil, fmt.Errorf("listen on admin address: %w", err)
		}
		a.admin = metrics.NewServer(cfg.AdminAddr, metrics.Handler(a.metrics, a, cfg.EnablePprof))
		go func() { a.adminErr <- a.admin.Serve(a.adminLn) }()
		log.Info("admin endpoint listening", "addr", a.adminLn.Addr().String(), "pprof", cfg.EnablePprof)
	}

	if cfg.AOFEnabled {
		if err := a.loadAOF(); err != nil {
			a.metrics.SetRecovery(false, 0, 0)
			return nil, err
		}
	}

	a.eng = command.New(a.st, command.Options{
		Password:         cfg.Password,
		DisabledCommands: cfg.DisabledCommands,
		MaxMemory:        cfg.MaxMemory,
		AOF:              a.logger(),
		Recorder:         a.metrics,
		Version:          version,
		StartTime:        a.started,
		InfoSections: map[string]func() []command.InfoField{
			"clients":     a.infoClients,
			"persistence": a.infoPersistence,
		},
	})
	a.srv = server.New(server.Config{
		IdleTimeout:  cfg.IdleTimeout,
		WriteTimeout: cfg.WriteTimeout,
		MaxClients:   cfg.MaxClients,
		Limits:       cfg.Limits,
		OnAccept:     a.metrics.ConnectionAccepted,
		OnReject:     a.metrics.ConnectionRejected,
	}, a.eng, log)

	a.ln, err = net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return a, nil
}

func (a *App) loadAOF() error {
	replay := command.New(a.st, command.Options{})
	start := time.Now()
	a.log.Info("loading append-only file", "path", a.cfg.AOFPath)
	a.st.SetLoading(true)
	aof, res, err := persistence.Open(persistence.Options{
		Path:          a.cfg.AOFPath,
		Fsync:         a.cfg.AOFFsync,
		LoadTruncated: a.cfg.AOFLoadTruncated,
		Logger:        a.log,
		ObserveFsync:  a.metrics.ObserveFsync,
	}, replay.Replay)
	a.st.SetLoading(false)
	if err != nil {
		return fmt.Errorf("load AOF: %w", err)
	}
	a.aof = aof
	aof.SetSnapshotFunc(func(begin func()) []store.Item {
		var items []store.Item
		a.st.View(func(tx *store.ReadTx) {
			items = tx.Snapshot()
			begin()
		})
		return items
	})
	a.metrics.SetRecovery(true, res.Commands, res.TruncatedBytes)
	a.log.Info("append-only file loaded",
		"records", res.Commands, "bytes", res.Bytes, "truncated_bytes", res.TruncatedBytes,
		"keys", a.st.Len(), "duration", time.Since(start).Round(time.Millisecond), "fsync", a.cfg.AOFFsync.String())
	return nil
}

// logger returns the AOF as a command.Logger, or nil (an untyped nil, so the
// engine's nil check works) when persistence is off.
func (a *App) logger() command.Logger {
	if a.aof == nil {
		return nil
	}
	return a.aof
}

// Start begins accepting clients and expiring keys.
func (a *App) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	a.stopSweeper = cancel
	go a.st.RunExpiry(ctx, 100*time.Millisecond, 500)
	go func() { a.serveErr <- a.srv.Serve(a.ln) }()
	a.ready.Store(true)
	a.log.Info("goredis ready", "addr", a.ln.Addr().String(), "version", a.version,
		"auth", a.cfg.Password != "", "aof", a.cfg.AOFEnabled)
}

// Run starts the instance and blocks until ctx is cancelled (e.g. SIGTERM)
// or the listener fails, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	a.Start()
	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested")
	case err := <-a.serveErr:
		runErr = fmt.Errorf("client listener failed: %w", err)
	case err := <-a.adminErr:
		runErr = fmt.Errorf("admin listener failed: %w", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	return errors.Join(runErr, a.Shutdown(sctx))
}

// Shutdown drains clients, flushes and fsyncs the AOF, and stops the admin
// endpoint last, so probes and metrics stay visible while draining.
func (a *App) Shutdown(ctx context.Context) error {
	a.closeOnce.Do(func() {
		a.ready.Store(false)
		start := time.Now()
		var errs []error
		if a.srv != nil {
			if err := a.srv.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("drain clients: %w", err))
			}
		}
		if a.stopSweeper != nil {
			a.stopSweeper()
		}
		if a.aof != nil {
			if err := a.aof.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close AOF: %w", err))
			} else {
				a.log.Info("append-only file flushed and closed")
			}
		}
		if a.admin != nil {
			if err := a.admin.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("stop admin endpoint: %w", err))
			}
		}
		a.closeErr = errors.Join(errs...)
		a.log.Info("shutdown complete", "duration", time.Since(start).Round(time.Millisecond), "err", a.closeErr)
	})
	return a.closeErr
}

// cleanup releases whatever New managed to open before failing.
func (a *App) cleanup() {
	if a.ln != nil {
		_ = a.ln.Close()
	}
	if a.aof != nil {
		_ = a.aof.Close()
	}
	if a.admin != nil {
		_ = a.admin.Close()
	}
}

// Addr returns the client listener address (useful with port 0 in tests).
func (a *App) Addr() string { return a.ln.Addr().String() }

// AdminAddr returns the admin listener address, or "" if disabled.
func (a *App) AdminAddr() string {
	if a.adminLn == nil {
		return ""
	}
	return a.adminLn.Addr().String()
}

// Ready implements metrics.Health.
func (a *App) Ready() bool { return a.ready.Load() }

// AOFError implements metrics.Health.
func (a *App) AOFError() error {
	if a.aof == nil {
		return nil
	}
	return a.aof.Err()
}

func (a *App) metricSources() metrics.Sources {
	src := metrics.Sources{
		Keys:        func() float64 { return float64(a.st.Len()) },
		DatasetSize: func() float64 { return float64(a.st.Stats().UsedBytes) },
		ExpiredKeys: func() float64 { return float64(a.st.Stats().ExpiredKeys) },
		DeletedKeys: func() float64 { return float64(a.st.Stats().DeletedKeys) },
		Clients: func() float64 {
			if a.srv == nil {
				return 0
			}
			return float64(a.srv.ActiveConnections())
		},
		AOFEnabled: a.cfg.AOFEnabled,
	}
	if a.cfg.AOFEnabled {
		stat := func(f func(persistence.Stats) float64) func() float64 {
			return func() float64 {
				if a.aof == nil {
					return 0
				}
				return f(a.aof.Stats())
			}
		}
		src.AOFHealthy = stat(func(s persistence.Stats) float64 { return b2f(s.Healthy) })
		src.AOFSize = stat(func(s persistence.Stats) float64 { return float64(s.Size) })
		src.AOFWriteErrors = stat(func(s persistence.Stats) float64 { return float64(s.WriteErrors) })
		src.AOFFsyncErrors = stat(func(s persistence.Stats) float64 { return float64(s.FsyncErrors) })
		src.AOFRewritesSuccess = stat(func(s persistence.Stats) float64 { return float64(s.RewritesOK) })
		src.AOFRewritesFailure = stat(func(s persistence.Stats) float64 { return float64(s.RewritesFailed) })
	}
	return src
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (a *App) infoClients() []command.InfoField {
	return []command.InfoField{
		{Key: "connected_clients", Value: strconv.Itoa(a.srv.ActiveConnections())},
		{Key: "maxclients", Value: strconv.Itoa(a.cfg.MaxClients)},
	}
}

func (a *App) infoPersistence() []command.InfoField {
	if a.aof == nil {
		return []command.InfoField{{Key: "aof_enabled", Value: "0"}}
	}
	s := a.aof.Stats()
	status := "ok"
	if !s.Healthy {
		status = "err"
	}
	last := "never"
	if !s.LastRewrite.IsZero() {
		last = s.LastRewrite.UTC().Format(time.RFC3339)
	}
	return []command.InfoField{
		{Key: "aof_enabled", Value: "1"},
		{Key: "aof_fsync", Value: s.Policy.String()},
		{Key: "aof_last_write_status", Value: status},
		{Key: "aof_current_size", Value: strconv.FormatInt(s.Size, 10)},
		{Key: "aof_buffer_length", Value: strconv.Itoa(s.PendingBytes)},
		{Key: "aof_rewrite_in_progress", Value: strconv.Itoa(int(b2f(s.RewriteInFlight)))},
		{Key: "aof_rewrites_ok", Value: strconv.FormatUint(s.RewritesOK, 10)},
		{Key: "aof_rewrites_failed", Value: strconv.FormatUint(s.RewritesFailed, 10)},
		{Key: "aof_last_rewrite_time", Value: last},
		{Key: "aof_write_errors", Value: strconv.FormatUint(s.WriteErrors, 10)},
		{Key: "aof_fsync_errors", Value: strconv.FormatUint(s.FsyncErrors, 10)},
	}
}
