// Package metrics defines goredis's Prometheus metrics and the admin HTTP
// endpoints (/metrics, /healthz, /readyz, optional /debug/pprof).
//
// Label values are always drawn from small, fixed sets (command names known
// to the server, "success"/"failure"), never from client input such as keys,
// so the number of time series stays bounded no matter what clients send.
package metrics

import (
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Sources supplies values that live in other packages. Each func is called
// on every scrape. Nil funcs are skipped.
type Sources struct {
	Keys        func() float64
	DatasetSize func() float64
	ExpiredKeys func() float64
	DeletedKeys func() float64
	Clients     func() float64

	AOFEnabled         bool
	AOFHealthy         func() float64
	AOFSize            func() float64
	AOFWriteErrors     func() float64
	AOFFsyncErrors     func() float64
	AOFRewritesSuccess func() float64
	AOFRewritesFailure func() float64
}

// Metrics holds the registry and the metrics updated on the hot path.
type Metrics struct {
	Registry *prometheus.Registry

	commands        *prometheus.CounterVec
	commandErrors   *prometheus.CounterVec
	commandDuration *prometheus.HistogramVec
	connections     prometheus.Counter
	rejected        prometheus.Counter
	fsyncDuration   prometheus.Histogram

	recoverySuccess   prometheus.Gauge
	recoveredCommands prometheus.Gauge
	truncatedBytes    prometheus.Gauge
}

// New creates a registry with goredis, Go runtime and process metrics.
func New(version string, maxMemory int64, started time.Time, src Sources) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goredis_commands_total", Help: "Commands processed, by command.",
		}, []string{"command"}),
		commandErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goredis_command_errors_total", Help: "Commands that returned an error reply, by command.",
		}, []string{"command"}),
		commandDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goredis_command_duration_seconds",
			Help:    "Command execution time, including waiting for the AOF per the fsync policy.",
			Buckets: []float64{.00001, .000025, .00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"command"}),
		connections: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "goredis_connections_total", Help: "Client connections accepted.",
		}),
		rejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "goredis_rejected_connections_total", Help: "Client connections rejected because max-clients was reached.",
		}),
		fsyncDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "goredis_aof_fsync_duration_seconds",
			Help:    "Time spent in fsync on the append-only file.",
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		recoverySuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "goredis_aof_recovery_success", Help: "1 if the AOF loaded successfully at startup.",
		}),
		recoveredCommands: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "goredis_aof_recovered_commands", Help: "Records replayed from the AOF at startup.",
		}),
		truncatedBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "goredis_aof_truncated_bytes", Help: "Bytes of an incomplete final record discarded at startup.",
		}),
	}
	reg.MustRegister(
		m.commands, m.commandErrors, m.commandDuration, m.connections, m.rejected,
		m.fsyncDuration, m.recoverySuccess, m.recoveredCommands, m.truncatedBytes,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "goredis_build_info", Help: "Build information; always 1.",
	}, []string{"version", "goversion"})
	info.WithLabelValues(version, runtime.Version()).Set(1)
	reg.MustRegister(info)

	gauge := func(name, help string, f func() float64) {
		if f != nil {
			reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f))
		}
	}
	counter := func(name, help string, f func() float64) {
		if f != nil {
			reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, f))
		}
	}
	gauge("goredis_uptime_seconds", "Seconds since the server started.", func() float64 { return time.Since(started).Seconds() })
	gauge("goredis_maxmemory_bytes", "Configured maxmemory; 0 means unlimited.", func() float64 { return float64(maxMemory) })
	gauge("goredis_connected_clients", "Open client connections.", src.Clients)
	gauge("goredis_keys", "Live (non-expired) keys.", src.Keys)
	gauge("goredis_dataset_bytes", "Estimated size of keys and values, including per-key overhead.", src.DatasetSize)
	counter("goredis_expired_keys_total", "Keys removed because their TTL passed.", src.ExpiredKeys)
	counter("goredis_deleted_keys_total", "Keys removed by DEL, FLUSHDB or a non-future expiry.", src.DeletedKeys)

	enabled := 0.0
	if src.AOFEnabled {
		enabled = 1
	}
	gauge("goredis_aof_enabled", "1 if append-only file persistence is enabled.", func() float64 { return enabled })
	if src.AOFEnabled {
		gauge("goredis_aof_healthy", "1 if AOF writes and fsyncs are succeeding; 0 means writes are refused.", src.AOFHealthy)
		gauge("goredis_aof_size_bytes", "Current AOF size.", src.AOFSize)
		counter("goredis_aof_write_errors_total", "Failed writes to the AOF.", src.AOFWriteErrors)
		counter("goredis_aof_fsync_errors_total", "Failed fsyncs of the AOF.", src.AOFFsyncErrors)
		rewrites := prometheus.NewDesc("goredis_aof_rewrites_total", "AOF rewrites (BGREWRITEAOF), by result.", []string{"result"}, nil)
		reg.MustRegister(&funcCollector{desc: rewrites, collect: func(ch chan<- prometheus.Metric) {
			ch <- prometheus.MustNewConstMetric(rewrites, prometheus.CounterValue, src.AOFRewritesSuccess(), "success")
			ch <- prometheus.MustNewConstMetric(rewrites, prometheus.CounterValue, src.AOFRewritesFailure(), "failure")
		}})
	}
	return m
}

// ObserveCommand implements command.Recorder.
func (m *Metrics) ObserveCommand(name string, d time.Duration, failed bool) {
	m.commands.WithLabelValues(name).Inc()
	m.commandDuration.WithLabelValues(name).Observe(d.Seconds())
	if failed {
		m.commandErrors.WithLabelValues(name).Inc()
	}
}

// ObserveFsync records one AOF fsync.
func (m *Metrics) ObserveFsync(d time.Duration) { m.fsyncDuration.Observe(d.Seconds()) }

// ConnectionAccepted counts an accepted client connection.
func (m *Metrics) ConnectionAccepted() { m.connections.Inc() }

// ConnectionRejected counts a connection refused at max-clients.
func (m *Metrics) ConnectionRejected() { m.rejected.Inc() }

// SetRecovery records the result of loading the AOF at startup.
func (m *Metrics) SetRecovery(ok bool, commands int, truncated int64) {
	if ok {
		m.recoverySuccess.Set(1)
	} else {
		m.recoverySuccess.Set(0)
	}
	m.recoveredCommands.Set(float64(commands))
	m.truncatedBytes.Set(float64(truncated))
}

// funcCollector exposes metrics computed at scrape time.
type funcCollector struct {
	desc    *prometheus.Desc
	collect func(chan<- prometheus.Metric)
}

func (c *funcCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c *funcCollector) Collect(ch chan<- prometheus.Metric) { c.collect(ch) }
