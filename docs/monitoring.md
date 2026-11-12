# Monitoring

goredis exposes Prometheus metrics on the admin server: `http://<GOREDIS_ADMIN_ADDR>/metrics` (default `127.0.0.1:9379`, `0.0.0.0:9379` in the container). The same server serves `/healthz` (liveness, JSON body) and `/readyz` (readiness).

| Path | Purpose |
|------|---------|
| `monitoring/prometheus/prometheus.yml` | Scrape config for the compose stack (`goredis:9379`, job `goredis`) |
| `monitoring/prometheus/alerts.yml` | Alert rules |
| `monitoring/grafana/provisioning/` | Grafana datasource (uid `prometheus`) and dashboard provider |
| `monitoring/grafana/dashboards/goredis.json` | The "goredis" dashboard |

In Kubernetes, scrape through the Service/pod `prometheus.io/*` annotations or `deploy/kubernetes/servicemonitor.yaml`, and load `alerts.yml` into your Prometheus (with prometheus-operator, wrap the `groups:` in a `PrometheusRule` object). Import `goredis.json` through Grafana's "Import dashboard"; it expects a Prometheus datasource with uid `prometheus`, or pick yours during import.

The admin port has no authentication. Keep it on loopback or a private network; the Kubernetes NetworkPolicy only admits the `monitoring` namespace. `/debug/pprof/` is mounted only when `GOREDIS_ENABLE_PPROF=true`.

## Metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `goredis_build_info` | gauge | `version`, `goversion` | Always 1. Join on it to see which version runs. |
| `goredis_uptime_seconds` | gauge | | Seconds since the process started. |
| `goredis_connected_clients` | gauge | | Currently open client connections. |
| `goredis_connections_total` | counter | | Accepted connections since start. |
| `goredis_rejected_connections_total` | counter | | Connections refused because `max-clients` was reached. |
| `goredis_commands_total` | counter | `command` | Commands processed. |
| `goredis_command_errors_total` | counter | `command` | Commands that returned an error reply (for example `-ERR`, `-OOM`, `-MISCONF`). |
| `goredis_command_duration_seconds` | histogram | `command` | Server-side command execution time. Buckets from 10µs to 1s. |
| `goredis_keys` | gauge | | Number of keys. |
| `goredis_dataset_bytes` | gauge | | Estimated size of keys and values. Lower than the real heap usage. |
| `goredis_maxmemory_bytes` | gauge | | Configured `maxmemory`; 0 means unlimited. |
| `goredis_expired_keys_total` | counter | | Keys removed because their TTL passed. |
| `goredis_deleted_keys_total` | counter | | Keys removed by commands, as opposed to TTL expiry. |
| `goredis_aof_enabled` | gauge | | 1 if the AOF is enabled. |
| `goredis_aof_healthy` | gauge | | 1 if AOF writes and fsyncs succeed. 0 means write commands are rejected with `-MISCONF`. |
| `goredis_aof_write_errors_total` | counter | | Failed writes to the AOF. |
| `goredis_aof_fsync_errors_total` | counter | | Failed fsyncs of the AOF. |
| `goredis_aof_size_bytes` | gauge | | Current AOF size. |
| `goredis_aof_fsync_duration_seconds` | histogram | | Time spent in fsync. |
| `goredis_aof_rewrites_total` | counter | `result` (`success`, `failure`) | `BGREWRITEAOF` runs. |
| `goredis_aof_recovery_success` | gauge | | 1 if loading the AOF at startup succeeded. |
| `goredis_aof_recovered_commands` | gauge | | Commands replayed from the AOF at startup. |
| `goredis_aof_truncated_bytes` | gauge | | Bytes of an incomplete trailing record dropped at startup (`aof-load-truncated`). Non-zero means the previous run ended abruptly. |
| `process_resident_memory_bytes` | gauge | | RSS of the process (standard process collector). |
| `go_memstats_heap_inuse_bytes` | gauge | | Bytes in in-use heap spans (standard Go collector). |
| `go_goroutines` | gauge | | Goroutines; roughly 1 per client plus a fixed number of background goroutines. |

Other standard `go_*` and `process_*` metrics (GC pauses, open file descriptors, CPU seconds) are also exported.

## Cardinality

Every label value has a fixed, small set of possible values:

- `command` is one of the supported command names or `unknown`. A client sending random command names does not create new series; they all count as `unknown`.
- `result` is `success` or `failure`.
- `version` and `goversion` change only on deploy.

Keys, values, client addresses, client IDs and error messages are **never** used as labels. Each of these is unbounded, and one series per key or per client would grow Prometheus memory without limit and could be triggered by any client. Use logs for per-client or per-key investigations.

The worst case is (number of supported commands + 1) x (2 counters + one series per histogram bucket) per instance: a few hundred series, independent of traffic.

## Dashboard panels and their queries

The dashboard has an `instance` variable (`label_values(goredis_build_info, instance)`, multi-select, "All" by default). Every query filters on `instance=~"$instance"`; it is omitted below for readability. Paste the queries into the Prometheus UI (http://127.0.0.1:9090 in the compose stack) to inspect them.

| Panel | Type | PromQL |
|-------|------|--------|
| Uptime | stat | `goredis_uptime_seconds` |
| AOF healthy | stat, value mapping 1 = Healthy (green), 0 = FAILING (red) | `goredis_aof_healthy` |
| Connected clients | stat | `goredis_connected_clients` |
| Keys | stat | `goredis_keys` |
| Estimated dataset | stat | `goredis_dataset_bytes` |
| Version | stat (shows the label) | `goredis_build_info` |
| Command rate by command | time series | `sum by (command) (rate(goredis_commands_total[5m]))` |
| Error rate (%) | time series | `100 * sum(rate(goredis_command_errors_total[5m])) / sum(rate(goredis_commands_total[5m]))` |
| Command latency p50 / p95 / p99 | time series | `histogram_quantile(0.99, sum by (le) (rate(goredis_command_duration_seconds_bucket[5m])))` (and 0.50, 0.95) |
| Errors by command | time series | `sum by (command) (rate(goredis_command_errors_total[5m]))` |
| Connected clients | time series | `sum(goredis_connected_clients)` and `sum(rate(goredis_rejected_connections_total[5m]))` |
| Memory | time series | `process_resident_memory_bytes`, `go_memstats_heap_inuse_bytes`, `goredis_dataset_bytes`, `goredis_maxmemory_bytes > 0` |
| Keys | time series | `goredis_keys` |
| Expired / deleted keys rate | time series | `sum(rate(goredis_expired_keys_total[5m]))`, `sum(rate(goredis_deleted_keys_total[5m]))` |
| AOF write / fsync errors | time series | `sum(increase(goredis_aof_write_errors_total[5m]))`, `sum(increase(goredis_aof_fsync_errors_total[5m]))` |
| AOF size | time series | `goredis_aof_size_bytes` |
| AOF fsync latency p99 | time series | `histogram_quantile(0.99, sum by (le) (rate(goredis_aof_fsync_duration_seconds_bucket[5m])))` |
| Goroutines | time series | `go_goroutines` |
| Uptime | time series | `goredis_uptime_seconds` |

Notes:

- `histogram_quantile` must aggregate with `sum by (le)` (plus any label you want to keep, such as `instance`) over `rate(..._bucket)`. Averaging per-instance quantiles is mathematically wrong.
- The quantile is interpolated inside a bucket, so a p99 of "8ms" means "between the 5ms and 10ms bucket bounds". With buckets ending at 1s, anything slower reports as 1s.
- The latency histogram measures time inside the server. Network time and client-side queueing are not included.
- `RSS > heap in use > estimated dataset` is normal. The gap between the dataset estimate and the heap is Go map and string overhead; the gap between heap and RSS is GC headroom, stacks and memory not yet returned to the OS.

Useful ad-hoc queries:

```promql
# Top commands by p99 latency
topk(5, histogram_quantile(0.99, sum by (command, le) (rate(goredis_command_duration_seconds_bucket[5m]))))

# Error ratio per command
sum by (command) (rate(goredis_command_errors_total[5m])) / sum by (command) (rate(goredis_commands_total[5m]))

# AOF rewrite failures
increase(goredis_aof_rewrites_total{result="failure"}[1h])

# Did the last startup drop a truncated record?
goredis_aof_truncated_bytes > 0
```

## Alerts

Defined in `monitoring/prometheus/alerts.yml`. Each alert's `runbook` annotation points at its section in [incidents.md](incidents.md#alert-runbooks).

| Alert | Expression (summary) | For | Severity |
|-------|----------------------|-----|----------|
| GoredisDown | `up{job="goredis"} == 0` | 1m | critical |
| GoredisHighErrorRate | errors / commands > 5% | 5m | warning |
| GoredisHighLatencyP99 | p99 from `goredis_command_duration_seconds_bucket` > 10ms | 10m | warning |
| GoredisAOFUnhealthy | `goredis_aof_healthy == 0` (with AOF enabled) | 1m | critical |
| GoredisAOFWriteErrors | `increase(goredis_aof_write_errors_total[5m]) > 0` or the same for fsync errors | none | warning |
| GoredisMemoryNearLimit | `goredis_dataset_bytes / goredis_maxmemory_bytes > 0.9` and maxmemory > 0 | 5m | warning |
| GoredisTooManyClients | `goredis_connected_clients > 800` or rejected connections in the last 5m | 5m | warning |
| GoredisRecentRestart | `goredis_uptime_seconds < 300` | none | info |

`alerts.yml` also contains, as a comment, a Kubernetes variant of the memory alert based on `container_memory_working_set_bytes / kube_pod_container_resource_limits{resource="memory"}` (needs cAdvisor and kube-state-metrics). Use it when `maxmemory` is 0 or to catch heap overhead the dataset estimate misses.

Thresholds to revisit for your deployment: the 800-client threshold assumes the default `max-clients` of 1000, and the 10ms p99 assumes in-memory commands with `aof-fsync=everysec`.

The `GoredisDown` expression assumes the scrape job is named `goredis`. With a ServiceMonitor, the job label is the Service name (also `goredis` with the provided manifests).

Validate changes with:

```bash
promtool check rules monitoring/prometheus/alerts.yml
promtool check config monitoring/prometheus/prometheus.yml
```
