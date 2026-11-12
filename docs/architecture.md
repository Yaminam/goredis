# Architecture

## Components

```
                      clients (redis-cli, go-redis, nc ...)
                                     |  TCP :6379 (RESP2)
   +---------------------------------v----------------------------------+
   | internal/server                                                    |
   |  accept loop -> one goroutine per connection                       |
   |  idle/write deadlines, max-clients, graceful shutdown              |
   |  read command -> Execute -> buffer reply -> flush when input empty |
   +-------------+--------------------------------------+---------------+
                 | bytes                                ^ replies
   +-------------v-------------+                        |
   | internal/protocol         |  RESP2 parser/encoder, request limits
   +-------------+-------------+
                 | [][]byte args
   +-------------v------------------------------------------------------+
   | internal/command (Engine)                                          |
   |  lookup, auth, disabled commands, arity, MISCONF/OOM checks        |
   |  handler runs inside ONE store transaction (atomic per command)    |
   +------+-------------------------------+------------------+----------+
          | View / Update                 | Append (under    | ObserveCommand
          |                               | store lock)      |
   +------v-------------+     +-----------v-----------+  +---v------------------+
   | internal/store     |     | internal/persistence  |  | internal/metrics     |
   |  map + RWMutex     |     |  AOF buffer, group    |  |  Prometheus registry |
   |  expiry min-heap   |     |  commit, fsync policy,|  |  /metrics /healthz   |
   |  1 sweeper goroutine|    |  retry loop, rewrite, |  |  /readyz /debug/pprof|
   +--------------------+     |  load + truncation,   |  +---------^------------+
                              |  OS file lock         |            | HTTP :9379
                              +-----------+-----------+            |
                                          | write/fsync       Prometheus, kubelet
                                    appendonly.aof

   internal/config: defaults < GOREDIS_* env < flags; validation; protected mode
   internal/app:    wires everything; startup and shutdown order
   cmd/goredis, cmd/goredis-check-aof, cmd/goredis-bench: thin binaries
```

Each package can be tested without the ones above it: the store has no idea
about RESP, the engine has no idea about sockets, and the server has no idea
what any command does.

## Life of a write command

`SET user:1 alice EX 60` from a client:

1. **server**: `ReadCommand` parses `["SET","user:1","alice","EX","60"]` from
   the connection's `bufio.Reader`. It doesn't matter whether that arrived in
   one TCP segment or ten.
2. **command**: looks up `set`, checks auth, disabled commands and arity,
   refuses with `-MISCONF` if the AOF is unhealthy, and parses the options
   (outside any lock).
3. **store**: `Update` takes the write lock. The handler checks NX/XX, turns
   `EX 60` into an absolute expiry (`now + 60000`), stores the value, and calls
   `AOF.Append("SET user:1 alice PXAT 1762…")`, which only copies bytes into a
   buffer. The lock is released.
4. **persistence**: `Wait(seq)` writes the buffer to the file (shared with
   any other waiting clients: group commit), and fsyncs under `always`.
5. **metrics**: the duration, including the AOF wait, is recorded under
   `command="set"`.
6. **server**: the `+OK` reply is buffered and flushed once there is no more
   pipelined input waiting.

## Concurrency model

| Thing | Concurrency control |
|-------|---------------------|
| Client connections | One goroutine each; bounded by `max-clients`. |
| Dataset | One `sync.RWMutex`. Reads share it; writes are exclusive. Every command runs in a single transaction, so each command is atomic. |
| Expiry | One sweeper goroutine. It pops due keys from the min-heap in batches of 500, releasing the lock between batches. |
| AOF buffer | `mu` (short critical sections, no I/O). Taken inside the store lock by `Append`. |
| AOF file | `writeMu` serialises write, fsync and the rewrite swap. Never held together with the store lock. |
| Lock order | store lock -> AOF `mu`; AOF `writeMu` -> AOF `mu`. No cycles. |

Rules followed everywhere: no network or disk I/O while holding the store
lock, and no unbounded goroutine creation (a fixed number of background
goroutines: the sweeper, the AOF retry/fsync loop, at most one rewrite, and the
admin HTTP server).

The single lock is a deliberate first design: simple, and verifiably correct
with the race detector. The benchmarks show its cost: a 90/10 read/write mix
across 16 threads runs about 4x slower per operation than a single-threaded GET (305 to 355 ns vs 72 to 81 ns)
(see [benchmarks.md](benchmarks.md)). Sharding the map by key hash is the
documented next step.

## Startup and shutdown order

Startup (`internal/app`):

1. Parse config. Refuse to start in protected mode on a public address
   without a password.
2. Start the admin HTTP server first, so `/healthz` answers and `/readyz`
   says 503 during a long AOF replay.
3. Take the AOF lock, replay the file with expiry checks off, truncate an
   incomplete tail if allowed, and open it for appending.
4. Bind the client listener, start the sweeper, and mark the server ready.

Shutdown (SIGTERM/SIGINT):

1. Mark not ready (`/readyz` returns 503).
2. Stop accepting; wake idle connections; let in-flight commands finish and
   their replies flush; force-close after `shutdown-timeout`.
3. Stop the sweeper.
4. Flush and fsync the AOF (whatever the policy), close it, release the lock.
5. Stop the admin server last, so metrics stay visible while draining.

## Security posture

- Binds to `127.0.0.1` by default. Protected mode refuses a non-loopback bind
  without a password unless explicitly disabled.
- The password comes from an environment variable or, preferably, a file;
  there is no password flag (flags are visible in `ps`). It is compared in
  constant time and never logged (`Config.LogValue` redacts it).
- Request size limits are checked before allocating; large bulk strings are
  read in 64 KiB chunks.
- Dangerous commands can be disabled (`GOREDIS_DISABLED_COMMANDS=FLUSHDB`).
- The container runs as non-root with a read-only root filesystem.
- **No TLS and no ACLs.** goredis is a learning and portfolio project, not a
  substitute for a hardened Redis deployment. Put it on a private network,
  behind a NetworkPolicy or firewall, and never expose it to the internet.
