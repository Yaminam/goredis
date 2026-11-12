// Package config loads server settings from defaults, environment variables
// (GOREDIS_*) and command-line flags, in increasing order of precedence.
//
// The password is deliberately not a flag: command lines are visible to every
// user on the host via ps and /proc. It comes from GOREDIS_REQUIREPASS or,
// preferably, from a file (GOREDIS_REQUIREPASS_FILE / -requirepass-file),
// which is how Kubernetes Secrets and Docker secrets are mounted.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"goredis/internal/persistence"
	"goredis/internal/protocol"
)

// Config holds every server setting.
type Config struct {
	Addr      string
	AdminAddr string

	AOFEnabled       bool
	AOFPath          string
	AOFFsync         persistence.FsyncPolicy
	AOFLoadTruncated bool

	MaxMemory       int64
	MaxClients      int
	Limits          protocol.Limits
	IdleTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration

	LogLevel  slog.Level
	LogFormat string

	Password         string // never logged
	ProtectedMode    bool
	DisabledCommands []string
	EnablePprof      bool

	// Modes that do not start the server.
	HealthCheck bool
	Version     bool
}

// Default returns the development defaults: loopback only, AOF on.
func Default() Config {
	return Config{
		Addr:             "127.0.0.1:6379",
		AdminAddr:        "127.0.0.1:9379",
		AOFEnabled:       true,
		AOFPath:          "data/appendonly.aof",
		AOFFsync:         persistence.FsyncEverySec,
		AOFLoadTruncated: true,
		MaxClients:       1000,
		Limits:           protocol.DefaultLimits(),
		IdleTimeout:      5 * time.Minute,
		WriteTimeout:     10 * time.Second,
		ShutdownTimeout:  20 * time.Second,
		LogLevel:         slog.LevelInfo,
		LogFormat:        "text",
		ProtectedMode:    true,
	}
}

// Load parses args (without the program name) on top of env and defaults.
// getenv is os.Getenv in production and a map lookup in tests.
func Load(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	c := Default()
	fs := flag.NewFlagSet("goredis", flag.ContinueOnError)
	fs.SetOutput(stderr)

	// Each setting is registered once, with its env var as the default and
	// a custom flag.Value that validates input the same way for both.
	var errs []error
	env := func(name string) (string, bool) {
		v := getenv("GOREDIS_" + name)
		return v, v != ""
	}
	bind := func(name, envName, usage string, v flag.Value) {
		if s, ok := env(envName); ok {
			if err := v.Set(s); err != nil {
				errs = append(errs, fmt.Errorf("GOREDIS_%s: %w", envName, err))
			}
		}
		fs.Var(v, name, usage+" (env GOREDIS_"+envName+")")
	}

	bind("addr", "ADDR", "TCP address for RESP clients", (*stringValue)(&c.Addr))
	bind("admin-addr", "ADMIN_ADDR", "HTTP address for /metrics, /healthz, /readyz; empty disables", (*stringValue)(&c.AdminAddr))
	bind("aof-enabled", "AOF_ENABLED", "enable append-only file persistence", (*boolValue)(&c.AOFEnabled))
	bind("aof-path", "AOF_PATH", "append-only file path", (*stringValue)(&c.AOFPath))
	bind("aof-fsync", "AOF_FSYNC", "fsync policy: always, everysec or no", (*fsyncValue)(&c.AOFFsync))
	bind("aof-load-truncated", "AOF_LOAD_TRUNCATED", "truncate an incomplete final AOF record at startup instead of refusing to start", (*boolValue)(&c.AOFLoadTruncated))
	bind("maxmemory", "MAXMEMORY", "reject SET when the estimated dataset exceeds this (bytes, or with kb/mb/gb suffix); 0 = unlimited", (*bytesValue)(&c.MaxMemory))
	bind("max-clients", "MAX_CLIENTS", "maximum concurrent client connections", (*intValue)(&c.MaxClients))
	bind("max-bulk-bytes", "MAX_BULK_BYTES", "largest single argument accepted", (*int64Value)(&c.Limits.MaxBulkBytes))
	bind("max-request-bytes", "MAX_REQUEST_BYTES", "largest total command size accepted", (*int64Value)(&c.Limits.MaxRequestBytes))
	bind("max-args", "MAX_ARGS", "most arguments in one command", (*intValue)(&c.Limits.MaxArgs))
	bind("idle-timeout", "IDLE_TIMEOUT", "close connections idle this long; 0 disables", (*durationValue)(&c.IdleTimeout))
	bind("write-timeout", "WRITE_TIMEOUT", "max time to flush a reply to a slow client; 0 disables", (*durationValue)(&c.WriteTimeout))
	bind("shutdown-timeout", "SHUTDOWN_TIMEOUT", "max time to drain clients on shutdown", (*durationValue)(&c.ShutdownTimeout))
	bind("log-level", "LOG_LEVEL", "debug, info, warn or error", (*levelValue)(&c.LogLevel))
	bind("log-format", "LOG_FORMAT", "text or json", (*choiceValue)(&choice{&c.LogFormat, []string{"text", "json"}}))
	bind("protected-mode", "PROTECTED_MODE", "refuse to listen on a non-loopback address without a password", (*boolValue)(&c.ProtectedMode))
	bind("disabled-commands", "DISABLED_COMMANDS", "comma-separated commands to refuse, e.g. FLUSHDB", (*listValue)(&c.DisabledCommands))
	bind("enable-pprof", "ENABLE_PPROF", "serve /debug/pprof on the admin address", (*boolValue)(&c.EnablePprof))

	passFile, _ := env("REQUIREPASS_FILE")
	fs.StringVar(&passFile, "requirepass-file", passFile, "file containing the client password (env GOREDIS_REQUIREPASS_FILE; or set GOREDIS_REQUIREPASS)")
	fs.BoolVar(&c.HealthCheck, "healthcheck", false, "probe the admin /readyz endpoint and exit 0 if ready (for container health checks)")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		errs = append(errs, fmt.Errorf("unexpected arguments: %v", fs.Args()))
	}

	if pw, ok := env("REQUIREPASS"); ok {
		c.Password = pw
	}
	if passFile != "" {
		b, err := os.ReadFile(passFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("read password file: %w", err))
		} else {
			c.Password = strings.TrimRight(string(b), "\r\n")
		}
	}
	if err := errors.Join(errs...); err != nil {
		return c, err
	}
	if c.HealthCheck || c.Version {
		return c, nil
	}
	return c, c.Validate()
}

// Validate checks settings that depend on each other.
func (c Config) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		errs = append(errs, fmt.Errorf("addr %q: %w", c.Addr, err))
	}
	if c.AdminAddr != "" {
		if _, _, err := net.SplitHostPort(c.AdminAddr); err != nil {
			errs = append(errs, fmt.Errorf("admin-addr %q: %w", c.AdminAddr, err))
		}
	}
	if c.AOFEnabled && c.AOFPath == "" {
		errs = append(errs, errors.New("aof-path must be set when the AOF is enabled"))
	}
	if c.MaxClients < 1 {
		errs = append(errs, errors.New("max-clients must be at least 1"))
	}
	if c.Limits.MaxBulkBytes < 1 || c.Limits.MaxRequestBytes < c.Limits.MaxBulkBytes || c.Limits.MaxArgs < 3 {
		errs = append(errs, errors.New("request limits must satisfy max-bulk-bytes >= 1, max-request-bytes >= max-bulk-bytes, max-args >= 3"))
	}
	if c.MaxMemory < 0 || c.IdleTimeout < 0 || c.WriteTimeout < 0 || c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("maxmemory and timeouts must not be negative; shutdown-timeout must be positive"))
	}
	if c.ProtectedMode && c.Password == "" && !IsLoopback(c.Addr) {
		errs = append(errs, fmt.Errorf("protected mode: refusing to listen on %s without a password. "+
			"Set GOREDIS_REQUIREPASS or GOREDIS_REQUIREPASS_FILE, bind to 127.0.0.1, "+
			"or (only behind a firewall you control) set GOREDIS_PROTECTED_MODE=false", c.Addr))
	}
	return errors.Join(errs...)
}

// IsLoopback reports whether addr binds only to the loopback interface.
// An empty host (":6379") means all interfaces, so it is not loopback.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LogValue keeps the password out of logs even if the whole Config is logged.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.Addr),
		slog.String("admin_addr", c.AdminAddr),
		slog.Bool("aof_enabled", c.AOFEnabled),
		slog.String("aof_path", c.AOFPath),
		slog.String("aof_fsync", c.AOFFsync.String()),
		slog.Int64("maxmemory", c.MaxMemory),
		slog.Int("max_clients", c.MaxClients),
		slog.Bool("auth_enabled", c.Password != ""),
		slog.Bool("protected_mode", c.ProtectedMode),
		slog.Any("disabled_commands", c.DisabledCommands),
	)
}

// ParseBytes parses "1048576", "512kb", "200mb" or "2gb" (case-insensitive,
// powers of 1024).
func ParseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return n * mult, nil
}

// flag.Value implementations shared by flags and env vars.

type stringValue string

func (v *stringValue) Set(s string) error { *v = stringValue(s); return nil }
func (v *stringValue) String() string     { return string(*v) }

type boolValue bool

func (v *boolValue) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("invalid boolean %q", s)
	}
	*v = boolValue(b)
	return nil
}
func (v *boolValue) String() string   { return strconv.FormatBool(bool(*v)) }
func (v *boolValue) IsBoolFlag() bool { return true }

type intValue int

func (v *intValue) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*v = intValue(n)
	return nil
}
func (v *intValue) String() string { return strconv.Itoa(int(*v)) }

type int64Value int64

func (v *int64Value) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*v = int64Value(n)
	return nil
}
func (v *int64Value) String() string { return strconv.FormatInt(int64(*v), 10) }

type bytesValue int64

func (v *bytesValue) Set(s string) error {
	n, err := ParseBytes(s)
	*v = bytesValue(n)
	return err
}
func (v *bytesValue) String() string { return strconv.FormatInt(int64(*v), 10) }

type durationValue time.Duration

func (v *durationValue) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use e.g. 10s, 5m)", s)
	}
	*v = durationValue(d)
	return nil
}
func (v *durationValue) String() string { return time.Duration(*v).String() }

type fsyncValue persistence.FsyncPolicy

func (v *fsyncValue) Set(s string) error {
	p, err := persistence.ParseFsyncPolicy(s)
	*v = fsyncValue(p)
	return err
}
func (v *fsyncValue) String() string { return persistence.FsyncPolicy(*v).String() }

type levelValue slog.Level

func (v *levelValue) Set(s string) error {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return fmt.Errorf("invalid log level %q", s)
	}
	*v = levelValue(l)
	return nil
}
func (v *levelValue) String() string { return slog.Level(*v).String() }

type choice struct {
	dst     *string
	allowed []string
}
type choiceValue choice

func (v *choiceValue) Set(s string) error {
	for _, a := range v.allowed {
		if s == a {
			*v.dst = s
			return nil
		}
	}
	return fmt.Errorf("invalid value %q (want one of %s)", s, strings.Join(v.allowed, ", "))
}
func (v *choiceValue) String() string {
	if v == nil || v.dst == nil {
		return ""
	}
	return *v.dst
}

type listValue []string

func (v *listValue) Set(s string) error {
	*v = nil
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*v = append(*v, strings.ToUpper(p))
		}
	}
	return nil
}
func (v *listValue) String() string { return strings.Join(*v, ",") }
