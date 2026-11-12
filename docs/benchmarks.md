# Benchmarks and performance testing

All numbers below were measured on one machine while building the project.
They show the relative costs of the design choices (fsync policy,
pipelining, lock contention). **They are not a comparison with Redis**: no
Redis server was run on the same machine with the same workload. Do not quote
them as one.

## Test conditions

| | |
|---|---|
| CPU | 12th Gen Intel Core i5-12500H (12 cores / 16 threads) |
| RAM | 15.6 GB |
| OS | Windows 11 Home (build 26300), laptop, default power plan |
| Go | 1.27.4, windows/amd64 |
| Network | loopback (client and server on the same machine) |
| Disk | the laptop's internal SSD (whatever `%TEMP%` lives on) |
| Background load | not isolated: an editor, Chrome, and another build job were running |

Two limitations of this setup:

1. **Timer resolution.** Go's monotonic clock on Windows has coarse resolution
   (around 0.5 ms), so sub-millisecond latency percentiles from
   `goredis-bench` on Windows are quantised. One early run printed a p50 of
   `0s`. Use Linux for latency work.
2. **Noise.** A laptop with background load is not a benchmark rig. Two runs
   were visibly disturbed by system stalls (details below). Treat everything
   here as indicative and re-run on a quiet Linux host before relying on it.

## How to reproduce

```bash
# Micro and end-to-end Go benchmarks
go test -run='^$' -bench=. -benchmem -benchtime=2s ./internal/store ./internal/protocol ./benchmarks

# Load generator against a running server
go build -o bin/ ./cmd/...
./bin/goredis -aof-path /tmp/bench/appendonly.aof -aof-fsync everysec &
./bin/goredis-bench -clients 50 -requests 200000 -ratio 1:1 -keyspace 10000 -value-size 64
./bin/goredis-bench -clients 50 -requests 500000 -ratio 1:1 -pipeline 16
./bin/goredis-bench -clients 50 -requests 200000 -ratio 9:1
```

`goredis-bench` prints the workload, throughput, latency percentiles
(p50/p90/p99/p99.9/max, per command, or per round trip when pipelining) and
the server's dataset size and Go heap from `INFO` before and after. It also
prints the server's fsync policy and the client host, so pasted results carry
their conditions with them.

For fair numbers: run the client on a different machine (or pin client and
server to different cores), use a fresh AOF directory per run, run each case
at least three times, and report the median.

## Results

### Store (no network), `go test -bench`

| Benchmark | ns/op | allocs/op |
|-----------|-------|-----------|
| `StoreGet` (10k keys) | 72 to 81 (3 runs) | 1 |
| `StoreSet` | 116 to 135 (3 runs) | 1 |
| `StoreSetWithTTL` (heap update) | 154 to 169 | 1 |
| `StoreMixedParallel` (90% GET / 10% SET, 16 threads) | 305 to 355 | 1 |

The parallel case is about 4x slower per operation than a single-threaded GET.
That is contention on the single `RWMutex`: every write blocks all readers.
This is the measured motivation for the documented next step, sharding the
map by key hash.

One `StoreSet` run reported 46 µs/op, with the run taking about 10 minutes of
wall time. That run was discarded as a system stall (the same code measured
116 to 135 ns three times in a row afterwards).

### Protocol

| Benchmark | Result |
|-----------|--------|
| `ReadCommandSet` (parse a 3-arg SET with a 64 B value) | 333 to 356 ns, ~280 to 300 MB/s, 6 allocs |
| `WriteBulkReply` (encode a 64 B bulk reply) | 28 to 32 ns, 0 allocs |

### End to end over loopback TCP, one client, `go test -bench ./benchmarks`

One command per round trip, so this is dominated by loopback latency.

| Benchmark | µs/op |
|-----------|-------|
| `GET` | 36.7 to 40.4 |
| `SET`, AOF off | 39.4 to 40.7 |
| `SET`, AOF `no` | 46.4 to 72.7 |
| `SET`, AOF `everysec` | 44.3 to 82.9 |
| `SET`, AOF `always` | 1,209 to 1,550 |
| `MixedParallel` (80/20, 8 x GOMAXPROCS clients, everysec) | 9.5 to 15.3 per op, aggregate |
| `PipelinedGET100` (100 GETs per round trip) | 380,000 to 638,000 commands/s |

(Ranges cover the two full runs made on Go 1.27.3 and 1.27.4.)

Takeaways:

- With a single client, `always` costs about 30x more per write than
  `everysec`. Every acknowledged write waits for an fsync (about 1.2 ms on this
  SSD).
- Pipelining removes the round trip: 100 GETs per round trip is roughly 15 to
  25 times the single-command rate.

### Load generator, 50 clients, 64 B values, 10k keys

| fsync | Workload | Throughput (commands/s) | p50 | p99 | p99.9 | max | Errors |
|-------|----------|-------------------------|-----|-----|-------|-----|--------|
| everysec | GET:SET 1:1 | 101,035 | 526 µs | 1.72 ms | 2.67 ms | 7.8 ms | 0 |
| everysec | GET:SET 1:1, pipeline 16 (latency per batch) | 199,406 | 3.73 ms | 10.5 ms | 17.4 ms | 22.0 ms | 0 |
| everysec | GET:SET 9:1 | 106,801 | 507 µs | 1.65 ms | 2.72 ms | 5.9 ms | 0 |
| always | SET only (50,000 commands) | 12,874 | 3.50 ms | 8.79 ms | n/a | 112 ms | 0 |
| always | GET:SET 1:1, disturbed run, see below | 2,070 | 3.52 ms | 93.4 ms | 730 ms | 1.70 s | 0 |

Server memory, `everysec` 1:1 run: estimated dataset 0 to 1.66 MiB, Go heap in
use 1.55 to 8.78 MiB.

**Group commit, measured.** In the clean `always` run, the server's own metrics
reported 50,000 SETs served with **2,569 fsyncs** (about 19.5 writes per
fsync), and an average fsync time of 1.24 ms
(`goredis_aof_fsync_duration_seconds_sum / _count`). Under `always`,
concurrency is what makes throughput acceptable: 50 clients reach about 12.9k
durable writes/s, while a single client manages under 1k/s.

**The disturbed run.** An earlier `always` run with a 1:1 mix measured only
2,070 commands/s, with a 1.7 s max latency. It happened during the same window
as the system stalls mentioned above, while Chrome was rendering a large PDF in
the background. While investigating it, an extra re-check was added to the
group-commit path (`AOF.Wait`), so a waiter whose record was already fsynced
returns without touching the file. A regression test
(`TestGroupCommitSharesFsyncs`) asserts that 32 concurrent writers share
fsyncs (640 writes took 43 fsyncs in the test). A before-and-after comparison
showed that the earlier code already batched fsyncs, so the slow run was
environmental, not a code bug. It is reported here rather than hidden.

## Profiling

Enable pprof temporarily and capture a CPU profile under load:

```bash
GOREDIS_ENABLE_PPROF=true ./bin/goredis &
./bin/goredis-bench -clients 50 -requests 2000000 &
go tool pprof -http=:8081 "http://127.0.0.1:9379/debug/pprof/profile?seconds=20"
```

## Known performance limits

- Single `RWMutex` over the whole dataset (see above). Sharding is the next step.
- One goroutine per connection. Fine for thousands of clients, not for
  hundreds of thousands.
- The parser allocates per argument (6 allocations for a 3-argument SET).
  Buffer pooling is possible, but has not been needed yet.
- `DBSIZE` cost grows with the number of *expired but not yet reclaimed* keys
  (normally small, because the sweeper runs every 100 ms).
