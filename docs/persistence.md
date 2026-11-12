# Persistence: the append-only file (AOF)

goredis keeps all data in memory and records every successful write in an
append-only file. On startup the file is replayed to rebuild the dataset.

## What gets written

Each record is a RESP array, the same encoding clients send. Records are
always written in a **resolved** form, so replaying them gives the same
result no matter when the replay happens:

| Client command | AOF record | Why |
|----------------|------------|-----|
| `SET k v` | `SET k v` | |
| `SET k v EX 10` (or `PX`, `EXAT`) | `SET k v PXAT <absolute unix ms>` | A relative TTL would restart from zero on every replay. |
| `SET k v NX` that is blocked | nothing | Only changes are logged. |
| `SET k v KEEPTTL` | `SET k v PXAT <the kept expiry>` | The kept TTL is made explicit. |
| `DEL a b c` (only `a` existed) | `DEL a` | Only keys actually removed. A DEL that removes nothing is not logged. |
| `EXPIRE k 10` (any variant) | `PEXPIREAT k <absolute ms>` | |
| `EXPIRE k -1` (deletes the key) | `DEL k` | |
| `PERSIST k` (when it removed a TTL) | `PERSIST k` | |
| `FLUSHDB` | `FLUSHDB` | Replay clears everything logged before it. |

### Expiration across restarts

Because expiry times are absolute, a key set with `EX 10` expires ten seconds
after it was **set**, however long the server was down. During replay, expiry
checks are switched off (`store.SetLoading`), so records rebuild exactly the
state at shutdown: a key that was given a TTL and later `PERSIST`ed is not
lost just because its original TTL has passed by the time of the replay. Once
loading finishes, expiry is evaluated against the real clock.

This relies on the server clock. If a restarted container lands on a node
whose clock is far off, keys may expire early or late by that skew.

## Fsync policies and what an acknowledged write means

`GOREDIS_AOF_FSYNC` / `-aof-fsync`:

| Policy | Before the client gets `+OK` | Survives a process crash | Survives an OS crash or power loss |
|--------|------------------------------|--------------------------|------------------------------------|
| `always` | written **and fsynced** | yes | yes (if the disk honours fsync) |
| `everysec` (default) | written to the OS; fsync runs once per second | yes | may lose about the last second |
| `no` | written to the OS; the OS decides when to flush | yes | may lose up to the OS flush interval (often ~30 s on Linux) |

"Survives a process crash" holds for all policies because the reply is only
sent after `write(2)` returns. The data is then in the kernel's page cache,
which outlives the process.

### Cost (measured, see [benchmarks.md](benchmarks.md))

On the development machine, a single client doing sequential `SET`s over
loopback measured about 41 µs per command with the AOF off, 73 µs with `no`,
83 µs with `everysec` and 1.55 ms with `always`. With `always`, concurrent
clients share fsyncs (group commit, below), so total throughput scales much
better than that single-client figure suggests.

### Group commit

Writes are appended to an in-memory buffer **while the store lock is held**,
which guarantees that the order of records in the file matches the order in
which changes were applied in memory. The disk write happens after the lock is
released: the first client waiting for durability takes the file lock and
writes everyone's pending records in one `write(2)` call (plus one fsync under
`always`). Other waiters find their record already covered and return.

## Startup recovery

| What the file contains | Behaviour |
|------------------------|-----------|
| Nothing / no file | Starts empty. |
| Valid records | Replayed in order. |
| Valid records, then an **incomplete final record** (the process died mid-write) | With `aof-load-truncated=true` (default): the partial record is cut off, a warning is logged, and `goredis_aof_truncated_bytes` reports how much was discarded. With `false`: the server refuses to start. |
| A **malformed record** before the end of the file | Refuses to start, reporting the byte offset. Skipping it could silently resurrect deleted keys or lose writes. |
| A well-formed record that is not a valid command (unknown command, wrong arity, bad timestamp) | Refuses to start, reporting the byte offset. |

Repair with the bundled tool (stop the server first):

```bash
goredis-check-aof /data/appendonly.aof        # report only; exit 1 if damaged
goredis-check-aof -fix /data/appendonly.aof   # writes appendonly.aof.bak-<UTC time>, then truncates
```

The checker replays every record through the same command engine the server
uses, so "OK" means exactly "the server will load this file".

## Write failures

| Failure | What happens | How it recovers |
|---------|--------------|-----------------|
| `write(2)` fails (e.g. disk full, `ENOSPC`) | The command that hit it gets `-ERR AOF write failed; the change is applied in memory but is not durable`. The unwritten bytes stay buffered. Later writes get `-MISCONF` **before** anything is applied. Reads keep working. `goredis_aof_healthy` drops to 0. | A background loop retries every second. Once the buffered bytes are written (space freed), the AOF is healthy again and writes resume automatically. A partially written record is completed by the retry, so the file stays valid. |
| `fsync` fails | Same reporting, but the error is **sticky**. | After a failed fsync the kernel may already have dropped the dirty pages, so a later successful fsync proves nothing. Health returns only after `BGREWRITEAOF` writes a complete, fsynced file from memory, or after a restart. |

Liveness probes do **not** fail on persistence errors: restarting would not
free disk space, and it would throw away writes still buffered in memory.

## Compaction (BGREWRITEAOF)

The AOF grows with every write, including overwrites of the same key.
`BGREWRITEAOF` replaces it with one `SET` per live key:

1. Under the store's read lock, take a snapshot of all live keys and start
   copying new records into a rewrite buffer as well as the normal buffer.
2. Without any lock, write the snapshot to `appendonly.aof.rewrite.tmp` and fsync it.
3. Briefly block writers: append the rewrite buffer, fsync, close the old
   file, atomically rename the temp file over it, fsync the directory
   (not supported on Windows), and reopen for appending.

If anything fails, the temp file is removed and the old AOF stays in use.
Writes continue during steps 1 and 2. Only one rewrite runs at a time.

There is no automatic rewrite trigger. Run `BGREWRITEAOF` from a cron job or
when `goredis_aof_size_bytes` grows well beyond the dataset size.

## Single writer

Two processes appending to the same file would interleave records and corrupt
it. goredis takes an exclusive OS lock on `<aof>.lock` (`flock` on Unix,
an exclusive handle on Windows) and refuses to start if another process holds
it. The OS releases the lock automatically when the process dies, so stale
locks are impossible. On Kubernetes the Deployment also uses
`strategy: Recreate` so an old and a new pod never run at the same time.
Note that `flock` semantics on network filesystems (NFS) vary.

## Not implemented

RDB snapshots, multi-part AOF, automatic rewrite triggers, checksums on
records, and replication.
