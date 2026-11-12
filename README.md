# goredis

A Redis-inspired, in-memory key-value server written from scratch in Go, with
append-only-file persistence, Prometheus metrics, health probes, a Docker
image, Kubernetes manifests, CI, a Grafana dashboard, and incident runbooks.

> goredis is an **independent learning and portfolio project**. It is not a
> Redis fork, it implements only a [documented subset](docs/commands.md) of
> Redis commands, and it is **not** a substitute for a security-hardened
> production Redis deployment. There is no TLS and no ACL system.

## Features

- **RESP2 over TCP**, compatible with `redis-cli` and standard client
  libraries for the supported commands. Pipelining and inline commands
  (`nc`/telnet) are supported.
- **21 commands**: `PING ECHO SET GET DEL EXISTS EXPIRE PEXPIRE EXPIREAT
  PEXPIREAT TTL PTTL PERSIST DBSIZE FLUSHDB INFO COMMAND AUTH SELECT QUIT
  BGREWRITEAOF`, including `SET` options (`EX PX EXAT PXAT NX XX KEEPTTL GET`)
  and `EXPIRE` options (`NX XX GT LT`). See [docs/commands.md](docs/commands.md).
- **TTL** with exact expiry (expired keys are never visible), a min-heap so
  `DBSIZE` excludes expired keys cheaply, and one bounded background sweeper.
- **Append-only file** with `always` / `everysec` / `no` fsync, group commit,
  safe recovery from a crash mid-write, refusal to load corrupt files, automatic
  recovery from a full disk, `BGREWRITEAOF` compaction, an OS lock against two
  writers, and a `goredis-check-aof` repair tool.
- **Operations**: flags and `GOREDIS_*` env config, structured logs
  (`log/slog`, text or JSON), Prometheus `/metrics`, `/healthz`, `/readyz`,
  optional pprof, graceful shutdown, a password via file or env, protected
  mode, `maxmemory`, disabled commands, and request size limits.
- **Verified on real infrastructure** by CI: the Docker image is built,
  smoke-tested (including a restart) and scanned (0 vulnerabilities), and
  the Kubernetes manifests are deployed to a kind cluster, where auth, probes,
  the security context and data surviving a pod restart are checked.
- **Delivery**: a multi-stage distroless non-root Docker image, docker-compose
  with Prometheus and Grafana, Kubernetes manifests (Recreate strategy, PVC,
  probes, NetworkPolicy, security context), GitHub Actions CI with SHA-pinned
  actions, a vulnerability scan, an image scan, and manifest validation.

## Quick start

Requirements: Go 1.27.4+ (or Docker).

```bash
go build -o bin/ ./cmd/...
./bin/goredis                          # 127.0.0.1:6379, AOF in ./data, metrics on 127.0.0.1:9379
```

```bash
redis-cli -p 6379 SET greeting hello EX 60     # OK
redis-cli -p 6379 GET greeting                 # "hello"
redis-cli -p 6379 TTL greeting                 # (integer) 60

# No redis-cli? Inline commands work over plain TCP:
printf 'SET k v\r\nGET k\r\nDBSIZE\r\n' | nc 127.0.0.1 6379

curl -s 127.0.0.1:9379/readyz                  # {"status":"ready"}
curl -s 127.0.0.1:9379/metrics | grep ^goredis_
```

With Docker (see [docs/docker.md](docs/docker.md)):

```bash
docker build -t goredis:dev .
docker run --rm -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 \
  -e GOREDIS_PROTECTED_MODE=false -v goredis-data:/data goredis:dev

# Full stack: goredis + Prometheus (127.0.0.1:9090) + Grafana (127.0.0.1:3000)
docker compose -f deploy/docker/docker-compose.yml up --build
```

On Kubernetes, see [docs/kubernetes.md](docs/kubernetes.md):

```bash
kubectl create namespace goredis
kubectl -n goredis create secret generic goredis-auth --from-literal=password="$(openssl rand -base64 32)"
kubectl apply -k deploy/kubernetes
```

## Architecture

```
clients --TCP/RESP2--> server --> protocol --> command engine --> store (map + RWMutex + TTL heap)
                                                     |
                                                     +--> persistence (AOF, group commit) --> appendonly.aof
                                                     +--> metrics --> /metrics /healthz /readyz (HTTP :9379)
```

| Package | Responsibility |
|---------|----------------|
| `internal/protocol` | RESP2 parser and encoder, request limits |
| `internal/store` | In-memory data, transactions, expiry heap, sweeper |
| `internal/command` | Command table, validation, auth, replies, AOF records |
| `internal/persistence` | AOF writer, fsync policies, recovery, rewrite, file lock |
| `internal/server` | TCP accept loop, connections, deadlines, graceful shutdown |
| `internal/metrics` | Prometheus metrics and admin HTTP endpoints |
| `internal/config` | Defaults, env, flags, validation |
| `internal/app` | Wiring, startup and shutdown order |
| `cmd/goredis`, `cmd/goredis-check-aof`, `cmd/goredis-bench` | Binaries |

Details, including concurrency and lock ordering, are in
[docs/architecture.md](docs/architecture.md).

## Durability in one table

| `GOREDIS_AOF_FSYNC` | An acknowledged write survives a process crash | ... and an OS crash or power loss |
|---------------------|-----------------------------------------------|-----------------------------------|
| `always` | yes | yes (if the disk honours fsync) |
| `everysec` (default) | yes | may lose about the last second |
| `no` | yes | may lose up to the OS flush interval |

Full trade-offs and failure handling are in [docs/persistence.md](docs/persistence.md).

## Documentation

| Topic | Document |
|-------|----------|
| Command compatibility table and differences from Redis | [docs/commands.md](docs/commands.md) |
| Architecture, concurrency, startup and shutdown | [docs/architecture.md](docs/architecture.md) |
| Configuration reference | [docs/configuration.md](docs/configuration.md) |
| Persistence and durability | [docs/persistence.md](docs/persistence.md) |
| Docker and docker-compose | [docs/docker.md](docs/docker.md) |
| Kubernetes | [docs/kubernetes.md](docs/kubernetes.md) |
| CI/CD pipeline | [docs/ci-cd.md](docs/ci-cd.md) |
| Metrics, dashboard and alerts | [docs/monitoring.md](docs/monitoring.md) |
| Benchmarks and performance testing | [docs/benchmarks.md](docs/benchmarks.md) |
| Incident simulations and runbooks | [docs/incidents.md](docs/incidents.md) |
| Troubleshooting | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Go primer (47 pages) with 73 interview Q&As on Go and goredis | [docs/Go_Basics.pdf](docs/Go_Basics.pdf) |

## Development

```bash
go test ./...                       # unit, integration and end-to-end tests
go test -race ./...                 # race detector (needs CGO and a C compiler)
go vet ./...
gofmt -l .                          # must print nothing
go test -run='^$' -fuzz=FuzzReadCommand -fuzztime=30s ./internal/protocol
go test -run='^$' -bench=. -benchmem ./internal/store ./internal/protocol ./benchmarks
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

`make help` lists the same tasks (plus Docker, compose and Kubernetes targets)
on Linux and macOS.

Test layout:

| Where | What |
|-------|------|
| `internal/*/..._test.go` | Unit tests per package, including a fuzz test for the parser, a randomized invariant test for the store, and disk-failure simulation for the AOF |
| `internal/server/server_test.go` | Integration over real TCP: pipelining, byte-by-byte input, malformed and oversized requests, max clients, idle timeout, graceful shutdown |
| `tests/` | End-to-end: the whole app on random ports, restart recovery, metrics, health, auth, the file lock, rewrite |
| `benchmarks/` | End-to-end benchmarks over loopback |

## Limitations

- Strings only; database 0 only; no transactions, pub/sub, scripting,
  replication, clustering, `KEYS`/`SCAN`, TLS or ACLs.
- RESP2 only (no `HELLO`/RESP3).
- One lock over the whole dataset. Write-heavy parallel workloads contend on
  it (measured in [docs/benchmarks.md](docs/benchmarks.md)).
- `maxmemory` is an estimate of the dataset, not of process memory.
- No automatic AOF rewrite trigger; run `BGREWRITEAOF` periodically.
- Benchmarks were measured on a Windows laptop and a 2-vCPU GitHub Actions
  Linux runner. They have not been compared with Redis.

## License

MIT, see [LICENSE](LICENSE).
