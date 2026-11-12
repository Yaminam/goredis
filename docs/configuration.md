# Configuration

Every setting can be given as a flag or a `GOREDIS_*` environment variable.
Precedence: built-in default < environment variable < flag. Run
`goredis -h` for the same list. Invalid values stop the server at startup
with a message naming the setting.

| Flag | Environment variable | Default | Meaning |
|------|---------------------|---------|---------|
| `-addr` | `GOREDIS_ADDR` | `127.0.0.1:6379` | RESP listen address. |
| `-admin-addr` | `GOREDIS_ADMIN_ADDR` | `127.0.0.1:9379` | HTTP address for `/metrics`, `/healthz`, `/readyz`. Empty disables it. |
| `-aof-enabled` | `GOREDIS_AOF_ENABLED` | `true` | Append-only file persistence. With `false`, data lives only in memory. |
| `-aof-path` | `GOREDIS_AOF_PATH` | `data/appendonly.aof` | AOF location. The directory is created if missing. A `<path>.lock` file sits next to it. |
| `-aof-fsync` | `GOREDIS_AOF_FSYNC` | `everysec` | `always`, `everysec` or `no`. See [persistence.md](persistence.md). |
| `-aof-load-truncated` | `GOREDIS_AOF_LOAD_TRUNCATED` | `true` | Cut off an incomplete final record at startup instead of refusing to start. |
| `-maxmemory` | `GOREDIS_MAXMEMORY` | `0` (unlimited) | Reject `SET` once the estimated dataset reaches this. Accepts `kb`, `mb`, `gb` (powers of 1024). |
| `-max-clients` | `GOREDIS_MAX_CLIENTS` | `1000` | Concurrent connections. Extra ones get `-ERR max number of clients reached`. |
| `-max-bulk-bytes` | `GOREDIS_MAX_BULK_BYTES` | `8388608` (8 MiB) | Largest single argument. |
| `-max-request-bytes` | `GOREDIS_MAX_REQUEST_BYTES` | `16777216` (16 MiB) | Largest total command size. |
| `-max-args` | `GOREDIS_MAX_ARGS` | `4096` | Most arguments in one command. |
| `-idle-timeout` | `GOREDIS_IDLE_TIMEOUT` | `5m` | Close connections idle this long. `0` disables it. |
| `-write-timeout` | `GOREDIS_WRITE_TIMEOUT` | `10s` | Drop a client that can't accept a reply within this time. `0` disables it. |
| `-shutdown-timeout` | `GOREDIS_SHUTDOWN_TIMEOUT` | `20s` | Max time to drain clients on SIGTERM. Keep it below Kubernetes `terminationGracePeriodSeconds`. |
| `-log-level` | `GOREDIS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `-log-format` | `GOREDIS_LOG_FORMAT` | `text` | `text` or `json` (the container image sets `json`). |
| `-protected-mode` | `GOREDIS_PROTECTED_MODE` | `true` | Refuse a non-loopback `addr` without a password. |
| `-disabled-commands` | `GOREDIS_DISABLED_COMMANDS` | (none) | Comma-separated list, e.g. `FLUSHDB,BGREWRITEAOF`. |
| `-enable-pprof` | `GOREDIS_ENABLE_PPROF` | `false` | Serve `/debug/pprof/` on the admin address. Enable only temporarily, on a private network. |
| `-requirepass-file` | `GOREDIS_REQUIREPASS_FILE` | (none) | File containing the client password. A trailing newline is trimmed. Takes precedence over `GOREDIS_REQUIREPASS`. |
| (none, on purpose) | `GOREDIS_REQUIREPASS` | (none) | Client password. |
| `-healthcheck` | | | Probe mode: GET `/readyz` on the admin address, exit 0 if it returns 200. Used by the Docker `HEALTHCHECK`. |
| `-version` | | | Print the version and exit. |

## Notes

- **Why no password flag?** Command lines are visible to every user on the
  host (`ps`, `/proc/<pid>/cmdline`). Use the file form with Kubernetes or
  Docker secrets.
- **The password is never logged.** The startup log prints `auth_enabled=true`
  instead.
- **maxmemory is approximate.** It counts key and value bytes plus a fixed
  96-byte overhead per key, not the Go heap. Process RSS will be higher
  (Go's GC typically lets the heap grow to about 2x live data). In containers,
  also set `GOMEMLIMIT` to about 90% of the memory limit, and keep `maxmemory`
  well below both. See [kubernetes.md](kubernetes.md) and incident 1 in
  [incidents.md](incidents.md).

## Examples

```bash
# Local development (defaults: loopback, AOF in ./data, everysec)
goredis

# Stricter durability, password from a file, JSON logs
GOREDIS_REQUIREPASS_FILE=/run/secrets/goredis \
GOREDIS_AOF_FSYNC=always GOREDIS_LOG_FORMAT=json \
goredis -addr 0.0.0.0:6379 -admin-addr 0.0.0.0:9379

# Pure in-memory cache with a memory cap and FLUSHDB disabled
goredis -aof-enabled=false -maxmemory 512mb -disabled-commands FLUSHDB
```
